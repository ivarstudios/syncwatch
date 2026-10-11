package web

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/notify"
	"github.com/ivarstudios/syncwatch/internal/store"
)

func TestAPIStatus(t *testing.T) {
	e := newEnv(t, twoServers)
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/v1/status", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	req.Header.Set("Authorization", "Bearer wrong")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", resp.StatusCode)
	}
	req.Header.Set("Authorization", "Bearer swt_test_token")
	resp, _ = http.DefaultClient.Do(req)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	body := string(b)
	if resp.StatusCode != 200 || !strings.Contains(body, `"check": "H1"`) || !strings.Contains(body, "0 of 2 servers up") {
		t.Fatalf("status: %d %s", resp.StatusCode, body)
	}
	for _, secret := range []string{"secret-key-akka", "secret-hook", "swt_test_token"} {
		if strings.Contains(body, secret) {
			t.Fatalf("API leaks %q", secret)
		}
	}
}

// /api/v1/ha gives Home Assistant per-server sensors: running, overall
// status, data moved in 24 h and 7 days, current rate.
func TestAPIForHomeAssistant(t *testing.T) {
	e := newEnv(t, `servers: [{name: AKKA, url: "https://a:1"}, {name: SOL, url: "https://b:1"}, {name: SIRIUS, url: "https://c:1", disabled: true}]
api: {token: swt_test_token}
`)
	// Like the collector manager, the hub only has the enabled servers.
	e.hub.SetServers([]*model.Server{{ID: "akka", Name: "AKKA"}, {ID: "sol", Name: "SOL", Status: model.StatusDown}})
	now := time.Now()
	e.hub.Update("akka", func(s *model.Server) {
		s.Status, s.Loaded = model.StatusUp, true
		s.Folders = map[string]*model.Folder{
			"a": {ID: "a", State: "idle"}, "b": {ID: "b", State: "syncing", NeedItems: 4}, "c": {ID: "c", Paused: true},
		}
		s.Rate = model.Rate{In: 12_500_000, Out: 250_000, At: now}
	})
	e.eng.Evaluate() // the engine's view follows the hub within a second
	_, _ = e.st.RecordTraffic("akka", "s", 0, 0, now.Add(-6*24*time.Hour), 7*24*time.Hour)
	_, _ = e.st.RecordTraffic("akka", "s", 4_000_000_000, 1_000_000_000, now.Add(-3*24*time.Hour), 7*24*time.Hour) // inside the week only
	_, _ = e.st.RecordTraffic("akka", "s", 5_500_000_000, 1_250_000_000, now.Add(-time.Hour), 7*24*time.Hour)

	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/v1/ha", nil)
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("without a token: %d", resp.StatusCode)
	}
	req.Header.Set("Authorization", "Bearer swt_test_token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("with the token: %v %v", err, resp.StatusCode)
	}
	var got struct {
		Servers map[string]map[string]any `json:"servers"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	akka, sol := got.Servers["akka"], got.Servers["sol"]
	want := map[string]any{
		"name": "AKKA", "running": true, "status": "syncing", "folders": 3.0, "folders_syncing": 1.0, "folders_paused": 1.0,
		"received_24h_gb": 1.5, "sent_24h_gb": 0.25, "moved_24h_gb": 1.75,
		"received_7d_gb": 5.5, "sent_7d_gb": 1.25, "moved_7d_gb": 6.75,
		"download_mb_s": 12.5, "upload_mb_s": 0.25,
	}
	for k, v := range want {
		if akka[k] != v {
			t.Errorf("akka %s = %v, want %v", k, akka[k], v)
		}
	}
	if sol["running"] != false || sol["status"] != "unknown" || sol["download_mb_s"] != 0.0 {
		t.Errorf("sol (down): %v", sol)
	}
	// A disabled server is listed too, so Home Assistant's templates find it.
	if sirius := got.Servers["sirius"]; sirius["running"] != false || sirius["status"] != "disabled" || sirius["name"] != "SIRIUS" {
		t.Errorf("sirius (disabled): %v", sirius)
	}
}

// The grid, the overview tile, /api/v1/status and /api/v1/ha count folders
// by the same state function: a watcher error is a warning everywhere and a
// scanning folder is never "syncing".
func TestFolderStateAgreesEverywhere(t *testing.T) {
	e := adminEnv(t, twoServers)
	e.hub.Update("akka", func(s *model.Server) {
		s.Status, s.Loaded, s.MyID = model.StatusUp, true, "AAAAAAA-BBBBBBB"
		s.Folders = map[string]*model.Folder{
			"f1": {ID: "f1", Label: "idle-folder", State: "idle"},
			"f2": {ID: "f2", Label: "sync-folder", State: "syncing", NeedItems: 4},
			"f3": {ID: "f3", Label: "scan-folder", State: "scanning"},
			"f4": {ID: "f4", Label: "err-folder", State: "error", Error: "folder marker missing"},
			"f5": {ID: "f5", Label: "watch-folder", State: "idle", WatchError: "inotify limit"},
			"f6": {ID: "f6", Label: "paused-folder", Paused: true},
		}
	})
	e.eng.Evaluate()
	_, ov := e.get("/frag/overview")
	for _, want := range []string{"✔ 1 idle", "⟳ 1 syncing", "⟳ 1 scanning", "✖ 1 error", "⚠ 1 file watcher error", "⏸ 1 paused"} {
		if !strings.Contains(ov, want) {
			t.Errorf("overview tile lacks %q", want)
		}
	}
	_, grid := e.get("/frag/grid")
	if !strings.Contains(grid, `c-warn" title="file watcher error: inotify limit"`) || !strings.Contains(grid, "file watcher error</span>") {
		t.Error("grid: the watcher error is not shown as a warning cell")
	}
	if n := strings.Count(grid, `cell c-err" title=`); n != 2 { // one per view (wide table and narrow list)
		t.Errorf("grid: %d error cells, want 2 (the folder error only)", n)
	}
	if !strings.Contains(grid, `c-sync" title="Scanning"`) {
		t.Error("grid: the scanning folder is not shown as scanning")
	}

	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/v1/status", nil)
	req.Header.Set("Authorization", "Bearer swt_test_token")
	resp, _ := http.DefaultClient.Do(req)
	var st struct {
		Servers []struct {
			ID      string         `json:"id"`
			Folders map[string]int `json:"folders"`
		} `json:"servers"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	wantCounts := map[string]int{"total": 6, "idle": 1, "syncing": 1, "scanning": 1, "error": 1, "watch_error": 1, "paused": 1}
	for _, sv := range st.Servers {
		if sv.ID != "akka" {
			continue
		}
		for k, v := range wantCounts {
			if sv.Folders[k] != v {
				t.Errorf("status API folders.%s = %d, want %d", k, sv.Folders[k], v)
			}
		}
	}

	req, _ = http.NewRequest(http.MethodGet, e.srv.URL+"/api/v1/ha", nil)
	req.Header.Set("Authorization", "Bearer swt_test_token")
	resp, _ = http.DefaultClient.Do(req)
	var ha struct {
		Servers map[string]map[string]any `json:"servers"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&ha)
	resp.Body.Close()
	akka := ha.Servers["akka"]
	for k, v := range map[string]any{"status": "error", "folders": 6.0, "folders_syncing": 1.0, "folders_scanning": 1.0, "folders_error": 1.0, "folders_watch_error": 1.0, "folders_paused": 1.0} {
		if akka[k] != v {
			t.Errorf("HA %s = %v, want %v", k, akka[k], v)
		}
	}
	// With the folder error gone, the watcher error is the server's status.
	e.hub.Update("akka", func(s *model.Server) { delete(s.Folders, "f4") })
	e.eng.Evaluate()
	resp, _ = http.DefaultClient.Do(req)
	_ = json.NewDecoder(resp.Body).Decode(&ha)
	resp.Body.Close()
	if ha.Servers["akka"]["status"] != "watch error" {
		t.Errorf("HA status with a watcher error = %v, want \"watch error\"", ha.Servers["akka"]["status"])
	}

	// Unreachable, and never contacted (sol): the tile, the grid and the
	// status API show the last known state as such, and HA says unknown.
	e.hub.Update("akka", func(s *model.Server) { s.Status, s.LastContact = model.StatusDown, time.Now() })
	e.hub.Update("sol", func(s *model.Server) { s.Status = model.StatusUnknown })
	e.eng.Evaluate()
	_, ov = e.get("/frag/overview")
	if strings.Contains(ov, "✔ 1 idle") || !strings.Contains(ov, "5 folders · last known ") || !strings.Contains(ov, "<dt>Folders</dt><dd>—</dd>") {
		t.Error("overview: the tiles of an unreachable and a not-contacted server show current folder states")
	}
	_, grid = e.get("/frag/grid")
	if strings.Contains(grid, `c-ok" title=`) || strings.Count(grid, `cell c-off" title="AKKA unreachable, last known state `) != 10 { // 5 folders, two views
		t.Error("grid: the cells of the unreachable server are not grey with their last known state")
	}
	req, _ = http.NewRequest(http.MethodGet, e.srv.URL+"/api/v1/status", nil)
	req.Header.Set("Authorization", "Bearer swt_test_token")
	resp, _ = http.DefaultClient.Do(req)
	var st2 struct {
		Servers []struct {
			ID      string         `json:"id"`
			Folders map[string]any `json:"folders"`
		} `json:"servers"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&st2)
	resp.Body.Close()
	for _, sv := range st2.Servers {
		if sv.Folders["stale"] != true {
			t.Errorf("status API %s: folders.stale = %v, want true", sv.ID, sv.Folders["stale"])
		}
		if sv.ID == "akka" && (sv.Folders["total"] != 5.0 || sv.Folders["idle"] != 1.0) {
			t.Errorf("status API akka: %v, want the last known counts", sv.Folders)
		}
	}
	req, _ = http.NewRequest(http.MethodGet, e.srv.URL+"/api/v1/ha", nil)
	req.Header.Set("Authorization", "Bearer swt_test_token")
	resp, _ = http.DefaultClient.Do(req)
	_ = json.NewDecoder(resp.Body).Decode(&ha)
	resp.Body.Close()
	if ha.Servers["akka"]["status"] != "unknown" || ha.Servers["sol"]["status"] != "unknown" {
		t.Errorf("HA status of unreachable servers: %v, %v", ha.Servers["akka"]["status"], ha.Servers["sol"]["status"])
	}
}

// The JSON status API and the Home Assistant endpoint count what the
// dashboard counts: the overview's open, urgent and snoozed numbers, and
// each tile's folder states.
func TestAPICountsMatchOverview(t *testing.T) {
	e := adminEnv(t, twoServers) // two unreachable servers: two H1 errors
	e.hub.Update("akka", func(s *model.Server) {
		s.Folders = map[string]*model.Folder{
			"f1": {ID: "f1", State: "idle"}, "f2": {ID: "f2", State: "syncing", NeedItems: 4},
			"f3": {ID: "f3", State: "idle", WatchError: "inotify limit"}, "f4": {ID: "f4", Paused: true},
		}
	})
	e.hub.Update("sol", func(s *model.Server) { s.Status = model.StatusUnknown }) // not contacted
	e.eng.Evaluate()
	csrf := e.csrf("/problems")
	e.post("/problems/"+e.eng.View().Open[0].ID+"/ignore", url.Values{"csrf": {csrf}, "reason": {"known"}})
	vm := e.web.overview(e.eng.View())
	if vm.OpenCount != 1 || vm.SnoozedCount != 1 {
		t.Fatalf("overview open=%d snoozed=%d, want 1 and 1", vm.OpenCount, vm.SnoozedCount)
	}
	tiles := map[string]serverTile{}
	for _, tile := range vm.Servers {
		tiles[tile.ID] = tile
	}

	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/v1/status", nil)
	req.Header.Set("Authorization", "Bearer swt_test_token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		Counts       map[string]int `json:"counts"`
		HealthCounts map[string]int `json:"health_counts"`
		Servers      []struct {
			ID      string         `json:"id"`
			Folders map[string]any `json:"folders"`
		} `json:"servers"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if open := st.Counts["error"] + st.Counts["warn"] + st.Counts["info"]; open != vm.OpenCount || st.Counts["urgent"] != vm.UrgentOpen || st.Counts["snoozed"] != vm.SnoozedCount {
		t.Errorf("status API counts %v; overview says open=%d urgent=%d snoozed=%d", st.Counts, vm.OpenCount, vm.UrgentOpen, vm.SnoozedCount)
	}
	if hc := st.HealthCounts; hc["pcs"] != vm.PCTotal || hc["pcs_online"] != vm.PCOnline || hc["pcs_offline"] != vm.PCOff || hc["pcs_unknown"] != vm.PCUnknown || hc["servers"] != len(vm.Servers) {
		t.Errorf("status API health_counts %v; the overview has %d servers and PCs %d/%d/%d/%d", hc, len(vm.Servers), vm.PCTotal, vm.PCOnline, vm.PCOff, vm.PCUnknown)
	}
	for _, sv := range st.Servers {
		tile := tiles[sv.ID]
		for k, n := range map[string]int{"total": tile.Folders, "idle": tile.Idle, "syncing": tile.Syncing, "scanning": tile.Scanning, "error": tile.Errors, "watch_error": tile.WatchErrors, "paused": tile.Paused} {
			if sv.Folders[k] != float64(n) {
				t.Errorf("status API %s folders.%s = %v, the tile says %d", sv.ID, k, sv.Folders[k], n)
			}
		}
		// Down (akka) and not contacted (sol): the counts are the last known state.
		if sv.Folders["stale"] != tile.FoldersStale || !tile.FoldersStale {
			t.Errorf("status API %s folders.stale = %v, the tile says %v", sv.ID, sv.Folders["stale"], tile.FoldersStale)
		}
	}

	req, _ = http.NewRequest(http.MethodGet, e.srv.URL+"/api/v1/ha", nil)
	req.Header.Set("Authorization", "Bearer swt_test_token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var ha struct {
		Servers map[string]map[string]any `json:"servers"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&ha)
	resp.Body.Close()
	for id, tile := range tiles {
		for k, n := range map[string]int{"folders": tile.Folders, "folders_syncing": tile.Syncing, "folders_scanning": tile.Scanning, "folders_error": tile.Errors, "folders_watch_error": tile.WatchErrors, "folders_paused": tile.Paused} {
			if ha.Servers[id][k] != float64(n) {
				t.Errorf("HA %s %s = %v, the tile says %d", id, k, ha.Servers[id][k], n)
			}
		}
	}
	if tile := tiles["akka"]; tile.Folders != 4 || tile.Idle != 1 || tile.Syncing != 1 || tile.WatchErrors != 1 || tile.Paused != 1 {
		t.Errorf("akka tile: %+v", tile)
	}
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

// The delivery error text can quote the channel's answer, so the API shows
// it to admins only; viewer sessions and token holders get failing and
// since. (Open viewing without a token gets no answer at all: W-048.)
func TestAPIDeliveryErrorForAdminsOnly(t *testing.T) {
	e := adminEnv(t, twoServers+"access: {viewer: password}\n")
	viewer := e.loginClient("viewer password 1", RoleViewer)
	e.setDelivery(notify.Delivery{Failing: true, Since: time.Now(), Error: "discord: HTTP 404: Unknown Webhook"})
	status := func(c *http.Client, bearer string) string {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/v1/status", nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || !strings.Contains(string(b), `"failing": true`) {
			t.Fatalf("API: %d %s", resp.StatusCode, b)
		}
		return string(b)
	}
	if body := status(e.http, ""); !strings.Contains(body, "Unknown Webhook") {
		t.Fatalf("admin session: no error text in %s", body)
	}
	if body := status(viewer, ""); strings.Contains(body, "Unknown Webhook") {
		t.Fatal("a viewer session sees the error text")
	}
	if body := status(http.DefaultClient, "swt_test_token"); strings.Contains(body, "Unknown Webhook") {
		t.Fatal("token holder sees the error text")
	}
	e2 := newEnv(t, twoServers+"access: {mode: proxy}\n")
	e2.setDelivery(notify.Delivery{Failing: true, Since: time.Now(), Error: "discord: HTTP 404: Unknown Webhook"})
	req, _ := http.NewRequest(http.MethodGet, e2.srv.URL+"/api/v1/status", nil)
	resp, _ := http.DefaultClient.Do(req)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "Unknown Webhook") {
		t.Fatal("proxy mode (everyone is admin): no error text")
	}
}

// W-032, for each API token (W-053): Replace and Revoke ask first, in the
// form of their own button, with the name of the API token. The API text
// names both endpoints and what the answer contains. The replace notice says
// that the old value stops working. Add API token asks nothing.
func TestAPITokenFormsAskFirst(t *testing.T) {
	e := adminEnv(t, twoServers) // the API token "API token" of the file
	_, page := e.get("/settings")
	csrf := csrfRe.FindStringSubmatch(page)[1]
	api := section(t, page, "api")
	const replace = `data-confirm="Replace the API token &#34;API token&#34;? The programs with this API token lose access immediately."`
	const revoke = `data-confirm="Revoke the API token &#34;API token&#34;? The programs with this API token lose access immediately."`
	for _, want := range []string{replace, revoke, "<code>GET /api/v1/status</code> or <code>GET /api/v1/ha</code>",
		"The answer contains device IDs, server URLs and folder paths. It does not contain API keys, passwords or the webhook URL."} {
		if !strings.Contains(api, want) {
			t.Errorf("API section lacks %s", want)
		}
	}
	// Each confirmation is on the form of its own button.
	if a, b, c, d := strings.Index(api, replace), strings.Index(api, `value="replace"`), strings.Index(api, revoke), strings.Index(api, `value="revoke"`); !(a >= 0 && a < b && b < c && c < d) {
		t.Errorf("forms and buttons are not paired: %d %d %d %d", a, b, c, d)
	}
	if i := strings.Index(api, `value="add"`); i < 0 || strings.Contains(api[strings.LastIndex(api[:i], "<form"):i], "data-confirm") {
		t.Error("Add API token asks for a confirmation")
	}
	id := e.tokenID("API token")
	resp, body := e.postPage("/settings/api-tokens", url.Values{"csrf": {csrf}, "op": {"replace"}, "id": {id}})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "for <code>GET /api/v1/status</code> and <code>GET /api/v1/ha</code>.") {
		t.Fatalf("new token panel: %d", resp.StatusCode)
	}
	if n := e.noticesText(); !strings.Contains(n, `API token replaced: The API token "API token" is replaced. syncwatch refuses the old value now.`) {
		t.Fatalf("notice: %q", n)
	}
}

// The API token is generated once and shown once (the API token part of
// TestSettingsForms).
func TestAPITokenGenerateForm(t *testing.T) {
	e := adminEnv(t, twoServers)
	csrf := e.csrf("/settings")
	resp, err := e.http.PostForm(e.srv.URL+"/settings/api-token", url.Values{"csrf": {csrf}, "op": {"generate"}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	m := regexp.MustCompile(`<p class="mono token">(swt_[^<]+)</p>`).FindSubmatch(b)
	if m == nil {
		t.Fatal("token not shown after generation")
	}
	tok := string(m[1])
	if !e.hasAPIToken(tok) || e.hasAPIToken("swt_test_token") {
		t.Fatal("the shown token is not the API token that works")
	}
	if _, body := e.get("/settings"); strings.Contains(body, tok) {
		t.Fatal("token shown again")
	}
}

// Each change of an API token sends one settings notice about security
// (C5: Security true), with the name of the API token (the API token part of
// TestEverySettingsChangeIsAnnounced; W-053).
func TestAPITokenChangesAreAnnounced(t *testing.T) {
	e := adminEnv(t, twoServers)
	csrf := e.csrf("/settings")
	seen := len(e.noticeValues())
	expect := func(step, title, summary string) {
		t.Helper()
		all := e.noticeValues()
		if len(all) != seen+1 {
			t.Fatalf("%s: %d notices, want 1: %+v", step, len(all)-seen, all[seen:])
		}
		n := all[seen]
		if n.Title != title || !strings.HasPrefix(n.Summary, summary+"\nChanged from ") || !n.Security {
			t.Errorf("%s: notice %+v, want %q, %q and Security", step, n, title, summary)
		}
		seen = len(all)
	}
	e.post("/settings/api-token", url.Values{"csrf": {csrf}, "op": {"generate"}})
	expect("old route: generate", "API token replaced", `The API token "API token" is replaced. syncwatch refuses the old value now.`)
	if msg, isErr := flash(e.post("/settings/api-token", url.Values{"csrf": {csrf}, "op": {"revoke"}})); isErr {
		t.Fatalf("POST /settings/api-token: %s", msg)
	}
	expect("old route: revoke", "API token revoked", `The API token "API token" is revoked. syncwatch refuses its value now.`)
	e.addToken("Home Assistant", store.ScopeHA)
	expect("add ha", "API token added", `The API token "Home Assistant" is added. It can read /api/v1/ha only.`)
	e.addToken("Scripts", store.ScopeFull)
	expect("add full", "API token added", `The API token "Scripts" is added. It can read /api/v1/status and /api/v1/ha.`)
	e.postPage("/settings/api-tokens", url.Values{"csrf": {csrf}, "op": {"replace"}, "id": {e.tokenID("Home Assistant")}})
	expect("replace", "API token replaced", `The API token "Home Assistant" is replaced. syncwatch refuses the old value now.`)
	if msg, isErr := flash(e.post("/settings/api-tokens", url.Values{"csrf": {csrf}, "op": {"revoke"}, "id": {e.tokenID("Scripts")}})); isErr || msg != `The API token "Scripts" is revoked.` {
		t.Fatalf("revoke: %q %v", msg, isErr)
	}
	expect("revoke", "API token revoked", `The API token "Scripts" is revoked. syncwatch refuses its value now.`)
	// A refused form changes nothing and says nothing.
	if resp, _ := e.postPage("/settings/api-tokens", url.Values{"csrf": {csrf}, "op": {"revoke"}, "id": {"00000000"}}); resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("revoke of an unknown ID: %d", resp.StatusCode)
	}
	if all := e.noticeValues(); len(all) != seen {
		t.Fatalf("a refused form sent a notice: %+v", all[seen:])
	}
}

// W-030 for the API token: after the secret key changed, Settings showed the
// API token as "set" although syncwatch could not decrypt it (the API token
// part of TestUndecryptableSecretsAreNotShownAsSet). The store keeps an API
// token of an older version that it cannot decrypt (it cannot move it into
// api_tokens): the API card says so and has a button that deletes it (W-053).
func TestUndecryptableAPITokenIsNotShownAsSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "syncwatch.db")
	keyA, keyB := make([]byte, 32), make([]byte, 32)
	keyB[0] = 1
	st, err := store.Open(path, keyA)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SetSecret(store.SecretAPIToken, "swt_x")
	st.Close()
	st, err = store.Open(path, keyB) // another secret key
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := newEnv(t, twoServers)
	e.web.st, e.st = st, st
	h, _ := HashPassword("correct horse battery")
	_ = st.SetSetting(KeyAdminHash, h)
	if r := e.post("/login", url.Values{"csrf": {e.csrf("/login")}, "password": {"correct horse battery"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", r.StatusCode)
	}
	_, page := e.get("/settings")
	a := section(t, page, "api")
	if !strings.Contains(a, `<p class="c-err">An API token of an older version is stored, but syncwatch cannot decrypt it. Revoke it.</p>`) ||
		!strings.Contains(a, `value="revoke-old" class="danger">Revoke old API token</button>`) || !strings.Contains(a, "<p>There are no API tokens.</p>") {
		t.Errorf("API token: %s", a)
	}
	if resp, _ := e.apiGet("/api/v1/status", "Bearer swt_x", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("the value of the old API token: %d", resp.StatusCode)
	}
	msg, isErr := flash(e.post("/settings/api-tokens", url.Values{"csrf": {e.csrf("/settings")}, "op": {"revoke-old"}}))
	if isErr || msg != "The API token of an older version is revoked." || st.HasSecret(store.SecretAPIToken) {
		t.Fatalf("revoke the old API token: %q %v, secret kept %v", msg, isErr, st.HasSecret(store.SecretAPIToken))
	}
	if n := e.noticeValues(); len(n) != 1 || n[0].Title != "API token revoked" || !n[0].Security {
		t.Fatalf("notice: %+v", n)
	}
	if _, page = e.get("/settings"); strings.Contains(section(t, page, "api"), "older version") {
		t.Error("the API card still shows the old API token")
	}
	// An API token that syncwatch can decrypt is in the list, without the
	// line of the old API token.
	_, page = adminEnv(t, twoServers).get("/settings")
	if a := section(t, page, "api"); strings.Contains(a, "older version") || !strings.Contains(a, "<td><strong>API token</strong></td><td>Full: /api/v1/status and /api/v1/ha</td>") {
		t.Errorf("a readable API token is not in the list: %s", a)
	}
}
