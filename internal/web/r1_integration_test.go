package web

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/store"
)

// Integration of r1-3 (C4: a disabled server's device is a server), r1-4
// (the health line, C3) and r1-6 (health_counts and the PCs tile, C3): the
// three count the same PCs. SIRIUS is disabled; its stored device ID is
// configured on AKKA, so before C4 it counted as a fifth, offline PC.
func TestHealthLineAndCountsAgree(t *testing.T) {
	e := adminEnv(t, `servers: [{name: AKKA, url: "https://a:1"}, {name: SOL, url: "https://b:1"}, {name: SIRIUS, url: "https://c:1", disabled: true}]
api: {token: swt_test_token}
thresholds: {pc_offline: 1h}
`)
	if err := e.st.SetServerDeviceID("sirius", "SIRIUSS-3333333"); err != nil {
		t.Fatal(err)
	}
	e.hub.SetServers([]*model.Server{{ID: "akka", Name: "AKKA"}, {ID: "sol", Name: "SOL"}})
	old := time.Now().Add(-3 * time.Hour)
	pc := func(id, name string) *model.Device { return &model.Device{ID: id, Name: name, LastSeen: old} }
	e.hub.Update("akka", func(s *model.Server) {
		s.Status, s.Loaded, s.MyID, s.LastContact, s.StartTime = model.StatusUp, true, "AKKAAAA-1111111", time.Now(), time.Now().Add(-time.Hour)
		s.Devices = map[string]*model.Device{
			"PCONLIN-1": pc("PCONLIN-1", "ONLINE"), "PCOFFLN-2": pc("PCOFFLN-2", "OFFLINE"), "PCBOTHS-3": pc("PCBOTHS-3", "BOTH"),
			"SIRIUSS-3333333": pc("SIRIUSS-3333333", "SIRIUS"),
		}
		s.Connections = map[string]*model.Connection{"PCONLIN-1": {Connected: true, StartedAt: time.Now()}}
	})
	e.hub.Update("sol", func(s *model.Server) {
		s.Status, s.Loaded, s.MyID, s.LastContact = model.StatusDown, true, "SOLLLLL-2222222", old
		s.Devices = map[string]*model.Device{"PCBOTHS-3": pc("PCBOTHS-3", "BOTH"), "PCSOLON-4": pc("PCSOLON-4", "SOL-ONLY")}
	})
	e.eng.Evaluate()

	_, st := e.apiGet("/api/v1/status", "Bearer swt_test_token", "")
	hc, _ := st["health_counts"].(map[string]any)
	want := map[string]float64{"servers": 3, "servers_up": 1, "servers_disabled": 1, "pcs": 4, "pcs_online": 1, "pcs_offline": 1, "pcs_unknown": 2}
	for k, v := range want {
		if hc[k] != v {
			t.Errorf("health_counts.%s = %v, want %v", k, hc[k], v)
		}
	}
	health, _ := st["health"].(string)
	for _, s := range []string{"1 of 3 servers up (SOL down, SIRIUS disabled)", "4 PCs, 1 offline more than 60 minutes, 2 unknown"} {
		if !strings.Contains(health, s) {
			t.Errorf("health line %q does not contain %q", health, s)
		}
	}
	vm := e.web.overview(e.eng.View())
	if vm.PCTotal != 4 || vm.PCOnline != 1 || vm.PCOff != 1 || vm.PCUnknown != 2 {
		t.Errorf("PCs tile: total %d online %d offline %d unknown %d", vm.PCTotal, vm.PCOnline, vm.PCOff, vm.PCUnknown)
	}
	devs, _ := st["devices"].([]any)
	found := false
	for _, d := range devs {
		if m, _ := d.(map[string]any); m["id"] == "SIRIUSS-3333333" || m["name"] == "SIRIUS" {
			found = true
			if m["server"] != true {
				t.Errorf("devices[] SIRIUS: server = %v, want true", m["server"])
			}
		}
	}
	if !found {
		t.Errorf("devices[] has no entry for SIRIUS: %v", devs)
	}
	// The Folders grid has no column for the disabled server (it would
	// have only "–" cells), and its device is not a PC column either.
	for _, d := range e.web.grid(e.eng.View(), RoleAdmin).Devices {
		if d.ID == "SIRIUSS-3333333" {
			t.Errorf("grid: a column for the disabled server (server=%v)", d.Server)
		}
	}
}

// W-030 for server API keys (r1-5 request 2, integrator follow-up in the
// plan): a stored API key that the secret key cannot decrypt is not shown as
// "for a different URL"; a key entered for another URL still is.
func TestServerKeyThatCannotBeDecrypted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "syncwatch.db")
	keyA, keyB := make([]byte, 32), make([]byte, 32)
	keyB[0] = 1
	st, err := store.Open(path, keyA)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SetServerKey("akka", "secret-key-akka", "https://a:1")
	st.Close()
	st, err = store.Open(path, keyB) // another secret key
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	_ = st.SetServerKey("sol", "secret-key-sol", "https://other:1") // readable, another URL
	e := newEnv(t, twoServers)
	e.web.st, e.st = st, st
	h, _ := HashPassword("correct horse battery")
	_ = st.SetSetting(KeyAdminHash, h)
	if r := e.post("/login", url.Values{"csrf": {e.csrf("/login")}, "password": {"correct horse battery"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", r.StatusCode)
	}
	_, page := e.get("/settings")
	sv := section(t, page, "servers")
	lost := "(Set, but syncwatch cannot decrypt it: the secret key changed. Type it again or restore the old secret key.)"
	other := "(The API key is for a different URL. Type it again.)"
	if strings.Count(sv, lost) != 1 || strings.Count(sv, other) != 1 {
		t.Errorf("servers section: %d cannot-decrypt marks and %d other-URL marks, want 1 and 1", strings.Count(sv, lost), strings.Count(sv, other))
	}
	if i, j := strings.Index(sv, lost), strings.Index(sv, other); i < 0 || j < 0 || i > j {
		t.Error("the cannot-decrypt mark is not on AKKA (first) or the other-URL mark is not on SOL")
	}
}

// r1-5 request 5: a session over the confirmation limit logs "password
// check rate limit reached" once a minute with a count (fixer 6's log
// limiter, W-025), not once for each refused confirmation.
func TestConfirmationLimitLogsOnceAMinute(t *testing.T) {
	e := adminEnv(t, twoServers)
	var logs syncBuffer
	e.web.log = slog.New(slog.NewTextHandler(&logs, nil))
	now := time.Now()
	e.web.now = func() time.Time { return now }
	csrf := e.csrf("/settings")
	accept := func(confirm string) {
		e.post("/settings/servers/akka/accept-cert", url.Values{"csrf": {csrf}, "fingerprint": {"ff"}, "confirm": {confirm}})
	}
	for i := 0; i < 5; i++ {
		accept(fmt.Sprintf("guess %d", i))
	}
	for i := 0; i < 4; i++ {
		accept("correct horse battery") // refused by the limit
	}
	if n := strings.Count(logs.String(), "password check rate limit reached"); n != 1 {
		t.Fatalf("%d lines for 4 refused checks in a minute, want 1:\n%s", n, logs.String())
	}
	now = now.Add(61 * time.Second)
	accept("correct horse battery")
	if n := strings.Count(logs.String(), "password check rate limit reached"); n != 2 || !strings.Contains(logs.String(), "count=4") {
		t.Fatalf("after a minute: %d lines, want 2, the second with count=4:\n%s", n, logs.String())
	}
}
