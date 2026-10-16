package web

import (
	"bufio"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/notify"
)

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
	if strings.Contains(ov, "✔ 2 idle") || !strings.Contains(ov, "2 folders · last known <span class=\"nowrap\">2026-10-10 17:34</span>: 2 idle") {
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
// again (at the time of the next try), and only a refused webhook
// needs a new URL.
func TestNotificationBannerText(t *testing.T) {
	e := adminEnv(t, twoServers+"access: {viewer: open}\ntimezone: Europe/Stockholm\n")
	now := time.Date(2026, 10, 11, 21, 50, 0, 0, time.UTC) // 23:50 in Stockholm
	e.web.now = func() time.Time { return now }
	e.setDelivery(notify.Delivery{Failing: true, Since: now.Add(-6 * time.Minute), Error: "discord: HTTP 500"})
	_, body := e.get("/")
	if !strings.Contains(body, "✖ <strong>Notifications fail</strong> from 2026-10-11 23:44: discord: HTTP 500. syncwatch tries again automatically. If Discord refuses the webhook (HTTP 401, 403 or 404), type a new webhook URL in <a href=\"/settings#notifications\">") {
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
	// The next try today shows the time; after midnight, also the date.
	for try, want := range map[time.Time]string{
		now.Add(2 * time.Minute):  "syncwatch tries again at 23:52. If Discord refuses the webhook",
		now.Add(16 * time.Minute): "syncwatch tries again at 2026-10-12 00:06. If Discord refuses the webhook",
	} {
		e.setDelivery(notify.Delivery{Failing: true, Since: now.Add(-6 * time.Minute), Error: "discord: HTTP 500", NextTry: try})
		if _, body := e.get("/frag/alert"); !strings.Contains(body, want) {
			t.Errorf("next try %v: want %q in %s", try, want, body)
		}
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

// In the grid, a PC that is not connected is "offline" only when every
// server that knows it is up; else its state is unknown (the PCs tile rule).
func TestGridPCUnknownWhileServerDown(t *testing.T) {
	e := adminEnv(t, twoServers)
	shared := func() map[string]*model.Folder {
		return map[string]*model.Folder{"f1": {ID: "f1", Label: "projects", State: "idle", SharedWith: []string{"PCBOTHS-1", "PCAKKAO-2"},
			Completion: map[string]*model.Completion{"PCBOTHS-1": {Percent: 100, RemoteState: "valid"}, "PCAKKAO-2": {Percent: 100, RemoteState: "valid"}}}}
	}
	e.hub.Update("akka", func(s *model.Server) {
		s.Status, s.Loaded, s.MyID, s.LastContact = model.StatusUp, true, "AKKAAAA-1111111", time.Now()
		s.Devices = map[string]*model.Device{"PCBOTHS-1": {ID: "PCBOTHS-1", Name: "BOTH"}, "PCAKKAO-2": {ID: "PCAKKAO-2", Name: "AKKA-ONLY"}}
		s.Folders = shared()
	})
	e.hub.Update("sol", func(s *model.Server) {
		s.Status, s.Loaded, s.MyID, s.LastContact = model.StatusDown, true, "SOLLLLL-2222222", time.Now().Add(-time.Hour)
		s.Devices = map[string]*model.Device{"PCBOTHS-1": {ID: "PCBOTHS-1", Name: "BOTH"}}
		s.Folders = shared()
	})
	e.eng.Evaluate()
	vm := e.web.grid(e.eng.View(), RoleAdmin)
	cells := map[string]gridCell{}
	for i, d := range vm.Devices {
		cells[d.Name] = vm.Groups[0].Items[0].Cells[i]
	}
	if c := cells["BOTH"]; c.Text != "unknown" || !strings.Contains(c.Title, "not up") {
		t.Errorf("PC seen by a server that is down: %+v", c)
	}
	if c := cells["AKKA-ONLY"]; c.Class != "off" || c.Icon != "○" || !strings.Contains(c.Title, "Offline") || !strings.Contains(c.Title, "100%") {
		t.Errorf("PC seen only by a server that is up: %+v", c)
	}
}
