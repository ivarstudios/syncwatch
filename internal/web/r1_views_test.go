package web

import (
	"bufio"
	"bytes"
	"encoding/json"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/notify"
)

// syncBuffer is a log destination that the server's goroutines can share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// apiGet sends an API request with the given Authorization header ("" for
// none) and Host ("" for the test server's), and decodes a JSON answer.
func (e *env) apiGet(path, auth, host string) (*http.Response, map[string]any) {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+path, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if host != "" {
		req.Host = host
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		e.t.Fatalf("%s: %d, not JSON: %q", path, resp.StatusCode, b)
	}
	return resp, out
}

// apiServers returns the servers[] entries of /api/v1/status by ID.
func apiServers(st map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	list, _ := st["servers"].([]any)
	for _, x := range list {
		sv, _ := x.(map[string]any)
		id, _ := sv["id"].(string)
		out[id] = sv
	}
	return out
}

// The machine values of /api/v1/status stay as they were in 78ef2d8 when the
// dashboard text changes: severity ERROR/WARN/INFO (never the display name
// WARNING), lower-case counts keys, and the raw detail keys and UTC times.
func TestAPIMachineValuesStay(t *testing.T) {
	e := adminEnv(t, twoServers+"timezone: Europe/Stockholm\n") // two unreachable servers: two H1 errors
	at := time.Date(2026, 10, 10, 15, 30, 0, 0, time.UTC)
	e.hub.Update("akka", func(s *model.Server) {
		s.Status, s.Loaded, s.MyID, s.LastContact = model.StatusUp, true, "AAAAAAA-BBBBBBB", time.Now()
		s.PendingDevices = []model.PendingDevice{{DeviceID: "CCCCCCC-DDDDDDD", Name: "LAPTOP-X", Address: "10.0.0.9:22000", Time: at}}
	})
	e.eng.Evaluate()
	csrf := e.csrf("/problems")
	for _, r := range e.eng.View().Open {
		if r.Check == "H9" {
			e.post("/problems/"+r.ID+"/ignore", url.Values{"csrf": {csrf}, "reason": {"known"}})
		}
	}
	_, st := e.apiGet("/api/v1/status", "Bearer swt_test_token", "")
	counts, _ := st["counts"].(map[string]any)
	for _, k := range []string{"error", "warn", "info", "snoozed", "ignored", "urgent"} {
		if _, ok := counts[k]; !ok {
			t.Errorf("counts.%s is missing: %v", k, counts)
		}
	}
	for k := range counts {
		if k != strings.ToLower(k) || strings.Contains(k, "warning") {
			t.Errorf("counts key %q", k)
		}
	}
	if counts["snoozed"] != 1.0 || counts["ignored"] != 1.0 || counts["error"] != 1.0 || counts["warn"] != 0.0 {
		t.Errorf("counts %v: want error 1 (H1 of sol), snoozed 1 and ignored 1 (H9)", counts)
	}
	seen := map[string]bool{}
	findings, _ := st["findings"].([]any)
	for _, x := range findings {
		f, _ := x.(map[string]any)
		sev, _ := f["severity"].(string)
		if sev != "ERROR" && sev != "WARN" && sev != "INFO" {
			t.Errorf("findings[].severity %q: want ERROR, WARN or INFO", sev)
		}
		seen[sev] = true
		if _, ok := f["ignored"]; !ok {
			t.Errorf("finding %v has no ignored field", f["id"])
		}
		if f["ignored"] == true && f["snoozed"] != true {
			t.Errorf("an ignored finding must stay snoozed too, as in 78ef2d8: %v", f)
		}
		if f["check"] == "H9" {
			ds, _ := f["details"].([]any)
			found := false
			for _, y := range ds {
				d, _ := y.(map[string]any)
				if d["key"] == "First offered" && d["value"] == "2026-10-10T15:30:00Z" {
					found = true
				}
			}
			if !found {
				t.Errorf("H9 details in the API: %v, want the key First offered with the UTC time", ds)
			}
		}
	}
	if !seen["ERROR"] || !seen["WARN"] {
		t.Errorf("severities seen: %v", seen)
	}
	// The dashboard shows the label and the time in the configured zone.
	_, body := e.get("/problems?snoozed=1")
	if !strings.Contains(body, "<th>Last request</th><td class=\"mono\">2026-10-10 17:30</td>") {
		t.Error("problems page: H9 detail is not 'Last request' at 17:30 Stockholm time")
	}
	if strings.Contains(body, "First offered") || strings.Contains(body, "2026-10-10T15:30:00Z") {
		t.Error("problems page shows the raw detail key or UTC time")
	}
	for _, want := range []string{">Started</th>", ">Reported</th>"} {
		if !strings.Contains(body, want) {
			t.Errorf("problems page lacks the row %s", want)
		}
	}
}

// The display label of a detail key works for the keys of 78ef2d8 and for
// the STE labels, so the order of the merges does not matter.
func TestDetailLabels(t *testing.T) {
	e := newEnv(t, twoServers+"timezone: Europe/Stockholm\n")
	rows := e.web.detailRows([]model.Detail{
		{Key: "Watch error", Value: "inotify limit"}, {Key: "File watcher error", Value: "inotify limit"},
		{Key: "Failing items", Value: "3"}, {Key: "Items left", Value: "4"}, {Key: "Bytes left", Value: "5"},
		{Key: "Type", Value: "relay-client"}, {Key: "URL", Value: "https://a:1"}, {Key: "Down since", Value: "2026-01-10T15:30:00Z"},
		{Key: "First offered", Value: "never"}, {Key: "Error", Value: ""}, {Key: "Folder ID", Value: "abcd-1234"},
	})
	want := []detailRow{
		{"File watcher error", "inotify limit"}, {"File watcher error", "inotify limit"},
		{"Failed items", "3"}, {"Remaining items", "4"}, {"Remaining bytes", "5"},
		{"Connection type", "relay-client"}, {"API URL", "https://a:1"}, {"Down from", "2026-01-10 16:30"},
		{"Last request", "never"}, {"Folder ID", "abcd-1234"},
	}
	if len(rows) != len(want) {
		t.Fatalf("rows %v, want %v", rows, want)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("row %d = %v, want %v", i, rows[i], want[i])
		}
	}
}

// Names with direction controls (U+202E reverses the rest of a line) show
// the control as a visible marker on every page. The API keeps the raw text.
func TestBidiControlsShownVisibly(t *testing.T) {
	e := adminEnv(t, twoServers)
	const evil = "PC-\u200b\u202egnp.exe"
	e.hub.Update("akka", func(s *model.Server) {
		s.Status, s.Loaded, s.MyID, s.LastContact = model.StatusUp, true, "AAAAAAA-BBBBBBB", time.Now()
		s.PendingDevices = []model.PendingDevice{{DeviceID: "CCCCCCC-DDDDDDD", Name: evil, Address: "10.0.0.9:22000", Time: time.Now()}}
		s.Folders = map[string]*model.Folder{"f1": {ID: "f1", Label: "LIVE-\u202eECRUOS1-TSET", State: "error", Error: "folder marker missing", Path: "/share/LIVE-\u2067x"}}
	})
	e.eng.Evaluate()
	for _, page := range []string{"/", "/problems", "/grid"} {
		_, body := e.get(page)
		if i := strings.IndexFunc(body, hiddenControl); i >= 0 {
			t.Errorf("%s: raw control character U+%04X near %q", page, []rune(body[i:])[0], body[max(0, i-40):i])
		}
		if !strings.Contains(html.UnescapeString(body), "‹U+202E›") {
			t.Errorf("%s: no visible marker for U+202E", page)
		}
	}
	_, st := e.apiGet("/api/v1/status", "Bearer swt_test_token", "")
	b, _ := json.Marshal(st)
	if !strings.Contains(string(b), "\u202e") || strings.Contains(string(b), "U+202E") {
		t.Error("the API must keep the raw text")
	}
	if got := showControls("a\u202ab\u2069c\u200dd\ufeff"); got != "a‹U+202A›b‹U+2069›c\u200dd‹U+FEFF›" {
		t.Errorf("showControls: %q", got)
	}
}

// An unreachable server keeps its last folder counts, but marked as the
// last known state: a grey tile line, grey grid cells, no "no faults" from
// them, and folders.stale in the API. A server that syncwatch never
// contacted shows no folder state at all.
func TestStaleFolderState(t *testing.T) {
	e := adminEnv(t, twoServers)
	contact := time.Date(2026, 10, 10, 17, 34, 0, 0, time.Local)
	e.hub.Update("akka", func(s *model.Server) {
		s.Status, s.Loaded, s.MyID, s.LastContact = model.StatusDown, true, "AAAAAAA-BBBBBBB", contact
		s.Folders = map[string]*model.Folder{
			"f1": {ID: "f1", Label: "one", State: "idle"}, "f2": {ID: "f2", Label: "two", State: "idle"},
		}
	})
	e.hub.Update("sol", func(s *model.Server) { s.Status, s.LastError = model.StatusUnknown, "" })
	e.eng.Evaluate()
	vm := e.web.overview(e.eng.View())
	tiles := map[string]serverTile{}
	for _, tile := range vm.Servers {
		tiles[tile.ID] = tile
	}
	if a := tiles["akka"]; !a.FoldersStale || a.NoContact || a.Idle != 2 {
		t.Errorf("akka tile: %+v", a)
	}
	if s := tiles["sol"]; !s.FoldersStale || !s.NoContact {
		t.Errorf("sol tile: %+v", s)
	}
	_, ov := e.get("/frag/overview")
	if strings.Contains(ov, "✔ 2 idle") || !strings.Contains(ov, "2 folders · last known 2026-10-10 17:34: 2 idle") {
		t.Error("overview: the tile of the unreachable server shows its folders as current")
	}
	if !strings.Contains(ov, "<dt>Folders</dt><dd>—</dd>") {
		t.Error("overview: the tile of the server that was never contacted claims a folder state")
	}
	_, grid := e.get("/frag/grid")
	if strings.Contains(grid, "c-ok\" title=\"Up to date\"") || strings.Contains(grid, "no faults") {
		t.Error("grid: the unreachable server's folders show as up to date")
	}
	if !strings.Contains(grid, "AKKA unreachable, last known state 2026-10-10 17:34: Up to date") || !strings.Contains(grid, "2 with an unknown state") {
		t.Error("grid: no grey last known state for the unreachable server")
	}
	_, st := e.apiGet("/api/v1/status", "Bearer swt_test_token", "")
	svs := apiServers(st)
	f, _ := svs["akka"]["folders"].(map[string]any)
	if f["stale"] != true || f["idle"] != 2.0 || f["total"] != 2.0 {
		t.Errorf("API akka folders %v: want the last counts with stale true", f)
	}
	if f, _ := svs["sol"]["folders"].(map[string]any); f["stale"] != true {
		t.Errorf("API sol folders %v: want stale true", f)
	}
	for id, sv := range svs {
		if v, ok := sv["uptime_s"]; !ok || v != nil {
			t.Errorf("API %s uptime_s = %v (present %v), want null", id, v, ok)
		}
	}
	// Up again after a full check: the counts are current, uptime_s is a number.
	e.hub.Update("akka", func(s *model.Server) { s.Status, s.StartTime = model.StatusUp, time.Now().Add(-time.Hour) })
	e.eng.Evaluate()
	_, st = e.apiGet("/api/v1/status", "Bearer swt_test_token", "")
	svs = apiServers(st)
	if f, _ := svs["akka"]["folders"].(map[string]any); f["stale"] != false {
		t.Errorf("API akka folders after the server is up: %v", f)
	}
	if u, _ := svs["akka"]["uptime_s"].(float64); u < 3500 {
		t.Errorf("API akka uptime_s = %v", svs["akka"]["uptime_s"])
	}
	if _, ov := e.get("/frag/overview"); !strings.Contains(ov, "✔ 2 idle") {
		t.Error("overview: the server is up, but its tile does not show the current state")
	}
}

// A disabled server stays in the status API (status disabled) and on the
// overview (a grey tile), and health_counts gives the health line numbers.
// A PC counts as offline only when every server that knows it is up.
func TestDisabledServerAndHealthCounts(t *testing.T) {
	e := adminEnv(t, `servers: [{name: AKKA, url: "https://a:1"}, {name: SOL, url: "https://b:1"}, {name: SIRIUS, url: "https://c:1", disabled: true}]
api: {token: swt_test_token}
thresholds: {pc_offline: 1h}
`)
	e.hub.SetServers([]*model.Server{{ID: "akka", Name: "AKKA"}, {ID: "sol", Name: "SOL"}})
	old := time.Now().Add(-3 * time.Hour)
	pc := func(id, name string) *model.Device { return &model.Device{ID: id, Name: name, LastSeen: old} }
	e.hub.Update("akka", func(s *model.Server) {
		s.Status, s.Loaded, s.MyID, s.LastContact, s.StartTime = model.StatusUp, true, "AKKAAAA-1111111", time.Now(), time.Now().Add(-time.Hour)
		s.Devices = map[string]*model.Device{"PCONLIN-1": pc("PCONLIN-1", "ONLINE"), "PCOFFLN-2": pc("PCOFFLN-2", "OFFLINE"), "PCBOTHS-3": pc("PCBOTHS-3", "BOTH")}
		s.Connections = map[string]*model.Connection{"PCONLIN-1": {Connected: true, StartedAt: time.Now()}}
	})
	e.hub.Update("sol", func(s *model.Server) {
		s.Status, s.Loaded, s.MyID, s.LastContact = model.StatusDown, true, "SOLLLLL-2222222", old
		s.Devices = map[string]*model.Device{"PCBOTHS-3": pc("PCBOTHS-3", "BOTH"), "PCSOLON-4": pc("PCSOLON-4", "SOL-ONLY")}
	})
	e.eng.Evaluate()
	_, st := e.apiGet("/api/v1/status", "Bearer swt_test_token", "")
	svs := apiServers(st)
	if s := svs["sirius"]; s == nil || s["status"] != "disabled" || s["name"] != "SIRIUS" {
		t.Errorf("API: disabled server %v", s)
	}
	hc, _ := st["health_counts"].(map[string]any)
	want := map[string]float64{"servers": 3, "servers_up": 1, "servers_disabled": 1, "pcs": 4, "pcs_online": 1, "pcs_offline": 1, "pcs_unknown": 2}
	for k, v := range want {
		if hc[k] != v {
			t.Errorf("health_counts.%s = %v, want %v", k, hc[k], v)
		}
	}
	vm := e.web.overview(e.eng.View())
	if vm.PCTotal != 4 || vm.PCOnline != 1 || vm.PCOff != 1 || vm.PCUnknown != 2 {
		t.Errorf("PCs tile: total %d online %d offline %d unknown %d", vm.PCTotal, vm.PCOnline, vm.PCOff, vm.PCUnknown)
	}
	_, ov := e.get("/")
	if !strings.Contains(ov, "<h2>SIRIUS</h2>") || !strings.Contains(ov, "○ Disabled") || !strings.Contains(ov, "syncwatch does not poll this server.") {
		t.Error("overview: no grey tile for the disabled server")
	}
	if !strings.Contains(ov, "<dt>Unknown</dt>") {
		t.Error("overview: the PCs tile does not show the unknown PCs")
	}
}

// The token check accepts the scheme in any case (RFC 7235), answers JSON
// errors as JSON, and logs unknown tokens once a minute per address with a
// count. The Host check answers API clients in JSON and logs the same way,
// with at most 100 characters of the Host.
func TestAPIHygieneAndRefusalLogs(t *testing.T) {
	e := newEnv(t, twoServers)
	var logs syncBuffer
	e.web.log = slog.New(slog.NewTextHandler(&logs, nil))
	now := time.Now()
	e.web.now = func() time.Time { return now }
	if resp, _ := e.apiGet("/api/v1/status", "bearer swt_test_token", ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("lower-case bearer: %d", resp.StatusCode)
	}
	for _, auth := range []string{"", "Bearer swt_guess"} {
		resp, body := e.apiGet("/api/v1/status", auth, "")
		if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("Content-Type") != "application/json" || body["error"] == nil {
			t.Errorf("auth %q: %d %q %v", auth, resp.StatusCode, resp.Header.Get("Content-Type"), body)
		}
	}
	for i := 0; i < 11; i++ {
		e.apiGet("/api/v1/ha", "Bearer swt_guess", "")
	}
	if n := strings.Count(logs.String(), "API request with an unknown token"); n != 1 {
		t.Errorf("%d log lines for 12 unknown tokens in a minute, want 1:\n%s", n, logs.String())
	}
	now = now.Add(61 * time.Second)
	e.apiGet("/api/v1/status", "Bearer swt_guess", "")
	if !strings.Contains(logs.String(), "count=12") {
		t.Errorf("the line after a minute does not count the requests: %s", logs.String())
	}

	host := strings.Repeat("x", 5000) + ".attacker.invalid"
	for i := 0; i < 300; i++ {
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/login", nil)
		req.Host = host
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}
	text := logs.String()
	if n := strings.Count(text, "request for an unknown host name refused"); n != 1 {
		t.Errorf("%d log lines for 300 refused requests in a minute, want 1", n)
	}
	if strings.Contains(text, strings.Repeat("x", 101)) {
		t.Error("the log has more than 100 characters of the Host")
	}
	resp, body := e.apiGet("/api/v1/status", "Bearer swt_test_token", "rebind.attacker.invalid")
	msg, _ := body["error"].(string)
	if resp.StatusCode != http.StatusMisdirectedRequest || resp.Header.Get("Content-Type") != "application/json" || !strings.Contains(msg, "SYNCWATCH_TLS_HOSTS") {
		t.Errorf("unknown host on the API: %d %q %v", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}

	// The login limit: one line a minute per address.
	h, _ := HashPassword("correct horse battery")
	_ = e.st.SetSetting(KeyAdminHash, h)
	csrf := e.csrf("/login")
	for i := 0; i < 9; i++ {
		e.post("/login", url.Values{"csrf": {csrf}, "password": {"nope"}})
	}
	if n := strings.Count(logs.String(), "login rate limit reached"); n != 1 {
		t.Errorf("%d log lines for 4 refused logins in a minute, want 1", n)
	}
}

// The event stream sends "ping" as an event that the page can see (an SSE
// comment never reaches the page), and an update after a change.
func TestEventStreamPing(t *testing.T) {
	old := ssePing
	ssePing = 50 * time.Millisecond
	t.Cleanup(func() { ssePing = old })
	e := adminEnv(t, twoServers)
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/events", nil)
	resp, err := e.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type %q", ct)
	}
	lines := make(chan string, 100)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	wait := func(want string) {
		t.Helper()
		timeout := time.After(5 * time.Second)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("stream ended before %q", want)
				}
				if strings.HasPrefix(l, ":") {
					t.Fatalf("comment line %q: the page cannot see it", l)
				}
				if l == want {
					return
				}
			case <-timeout:
				t.Fatalf("no %q", want)
			}
		}
	}
	wait("event: ping")
	e.hub.Update("akka", func(s *model.Server) { s.Status, s.Loaded = model.StatusUp, true })
	e.eng.Evaluate()
	wait("event: update")
}

// The page shows when it has no live stream and fetches its sections again
// when the stream works again. The behaviour runs in the browser; this pins
// the shipped script, styles and banner.
func TestConnectionLossShipped(t *testing.T) {
	e := adminEnv(t, twoServers+"timezone: Asia/Kolkata\n")
	_, js := e.get("/assets/app.js")
	for _, want := range []string{`"htmx:sseError"`, `"htmx:sseOpen"`, `"visibilitychange"`, `"sw-offline"`, `"ping"`, `[hx-trigger~='sse:update']`, "s.onerror(new Event(\"error\"))"} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %s", want)
		}
	}
	_, css := e.get("/assets/app.css")
	for _, want := range []string{".sw-offline .live-on", ".sw-offline .health", "unicode-bidi: isolate"} {
		if !strings.Contains(css, want) {
			t.Errorf("app.css lacks %s", want)
		}
	}
	_, page := e.get("/")
	for _, want := range []string{`id="sw-conn"`, `role="status" hidden`, "No connection to syncwatch</strong> from <span class=\"sw-conn-since\"></span>. The page shows old data.",
		`data-tz-offset="330"`, `<span class="live-on">Real-time view</span>`} {
		if !strings.Contains(page, want) {
			t.Errorf("overview page lacks %s", want)
		}
	}
}

// The notification banner is true when Discord itself fails: syncwatch tries
// again, and only a refused webhook needs a new URL.
func TestNotificationBannerText(t *testing.T) {
	e := adminEnv(t, twoServers+"access: {viewer: open}\n")
	e.setDelivery(notify.Delivery{Failing: true, Since: time.Now(), Error: "discord: HTTP 500"})
	_, body := e.get("/")
	if !strings.Contains(body, "syncwatch tries again automatically. If Discord refuses the webhook (HTTP 401, 403 or 404), type a new webhook URL in <a href=\"/settings#notifications\">") {
		t.Error("admin banner text")
	}
	if strings.Contains(body, "until you correct this") {
		t.Error("the banner still asks for a correction in all cases")
	}
	anon, _ := http.Get(e.srv.URL + "/")
	b, _ := io.ReadAll(anon.Body)
	anon.Body.Close()
	if !strings.Contains(string(b), "an administrator must type a new webhook URL in Settings → Notifications.") {
		t.Error("viewer banner text")
	}
}

// With every open finding hidden, the Findings list says so, and links to
// the hidden ones.
func TestEmptyListWithHiddenFindings(t *testing.T) {
	e := adminEnv(t, twoServers) // two H1 errors
	csrf := e.csrf("/problems")
	for _, r := range e.eng.View().Open {
		e.post("/problems/"+r.ID+"/ignore", url.Values{"csrf": {csrf}, "reason": {"known"}})
	}
	_, body := e.get("/problems")
	if !strings.Contains(body, "There are no open findings. <a href=\"#snoozed\">2 findings are hidden (snoozed or ignored).</a>") {
		t.Error("empty list with hidden findings")
	}
	if strings.Contains(body, "No findings match the filter.") {
		t.Error("the list talks about a filter that is not set")
	}
	if _, body := e.get("/problems?severity=info"); !strings.Contains(body, "No findings match the filter.") {
		t.Error("filtered empty list")
	}
}
