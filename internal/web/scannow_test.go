package web

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/collector"
	"github.com/ivarstudios/syncwatch/internal/model"
)

// W-056: "Scan now" for the administrator and the time of the last
// structure scan. Hooks.ScanNow is a fake collector here; the scan itself
// (the path of the timed scan, one scan at a time, the 30 seconds, the end
// time) is tested in internal/collector (scannow_test.go).

const scanYAML = `servers: [{name: AKKA, url: "https://a:1"}, {name: SOL, url: "https://b:1"}, {name: SIRIUS, url: "https://c:1", disabled: true}]
timezone: Europe/Stockholm
api: {token: swt_test_token}
structure: {project_pattern: '^LIVE-'}
`

// scanEnv is a dashboard with the administrator logged in, AKKA and SOL
// unreachable and SIRIUS disabled. As with the collector manager, the hub
// has no disabled server.
func scanEnv(t *testing.T, extra string) *env {
	t.Helper()
	e := adminEnv(t, scanYAML+extra)
	e.hub.SetServers([]*model.Server{{ID: "akka", Name: "AKKA"}, {ID: "sol", Name: "SOL"}})
	e.eng.Evaluate()
	return e
}

// scanHook is the fake collector behind Hooks.ScanNow: it records the
// servers that the dashboard asks to scan and answers with reply.
type scanHook struct {
	mu    sync.Mutex
	calls []string
	reply func(id string) collector.ScanStart
}

func (h *scanHook) scanNow(id string) collector.ScanStart {
	h.mu.Lock()
	h.calls = append(h.calls, id)
	reply := h.reply
	h.mu.Unlock()
	return reply(id)
}

func (h *scanHook) called() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.calls...)
}

// withScanHook puts a fake collector behind "Scan now" of e.
func withScanHook(e *env, reply func(id string) collector.ScanStart) *scanHook {
	h := &scanHook{reply: reply}
	e.web.hooks.ScanNow = h.scanNow
	return h
}

// shareWith makes AKKA up with 1 share that has the directories. A name that
// starts with LIVE- is a project folder in a Syncthing folder; a different
// name gets S4. The last structure scan ended at scanned, and it listed the
// share.
func shareWith(e *env, scanned time.Time, dirs ...string) {
	e.hub.Update("akka", func(s *model.Server) {
		s.Status, s.Loaded, s.MyID, s.PathSep, s.LastContact, s.LastError = model.StatusUp, true, "AKKAAAA-1111111", "/", time.Now(), ""
		sh := &model.Share{Path: "/share/AKKA-LIVE-1SOURCE", Name: "AKKA-LIVE-1SOURCE", ScannedAt: scanned, ListedAt: scanned}
		for _, d := range dirs {
			dir := &model.Dir{Name: d, Path: sh.Path + "/" + d}
			if strings.HasPrefix(d, "LIVE-") {
				dir.FolderIDs = []string{strings.ToLower(d)}
			}
			sh.Dirs = append(sh.Dirs, dir)
		}
		s.Structure = &model.Structure{ScannedAt: scanned, Shares: []*model.Share{sh}}
		s.LastStructureScan = scanned
	})
}

// structureOpen gives the check and the subject of each open structure
// finding.
func structureOpen(e *env) []string {
	var out []string
	for _, r := range e.eng.View().Open {
		if r.Category == model.CatStructure {
			out = append(out, r.Check+" "+r.Subject)
		}
	}
	return out
}

// findingRow returns the list item of the open finding of the check and
// the subject on the page.
func findingRow(t *testing.T, e *env, page, check, subject string) string {
	t.Helper()
	for _, r := range e.eng.View().Open {
		if r.Check != check || r.Subject != subject {
			continue
		}
		i := strings.Index(page, `<li id="f-`+r.ID+`"`)
		if i < 0 {
			t.Fatalf("no row of %s %s on the page", check, subject)
		}
		if j := strings.Index(page[i:], "</li>"); j >= 0 {
			return page[i : i+j]
		}
		return page[i:]
	}
	t.Fatalf("no open finding %s %s: %v", check, subject, structureOpen(e))
	return ""
}

// flashes returns the green and the red flash message of an answer.
func flashes(resp *http.Response) (ok, failed string) {
	for _, c := range resp.Cookies() {
		switch c.Name {
		case "sw_flash":
			ok = flashDecode(c.Value)
		case "sw_flash_err":
			failed = flashDecode(c.Value)
		}
	}
	return ok, failed
}

// today gives a time of the current day in the configured zone of e.
func today(e *env, h, m, s int) time.Time {
	loc := e.cfg.Get().Location()
	n := time.Now().In(loc)
	return time.Date(n.Year(), n.Month(), n.Day(), h, m, s, 0, loc)
}

func tooSoon(string) collector.ScanStart {
	return collector.ScanStart{State: collector.ScanTooSoon, Started: time.Now(), Next: time.Now().Add(30 * time.Second)}
}

// Only an administrator sees "Scan now" and starts a scan. A viewer, the
// open dashboard and a browser without a login see the time of the last
// scan, but no button, and the dashboard refuses their requests: no scan
// starts.
func TestScanNowOnlyForAdministrators(t *testing.T) {
	e := scanEnv(t, "access: {viewer: password}\n")
	vh, _ := HashPassword("viewer password 1")
	_ = e.st.SetSetting(KeyViewerHash, vh)
	shareWith(e, time.Now(), "Wrong", "LIVE-RIGHT")
	e.eng.Evaluate()
	hook := withScanHook(e, tooSoon)

	_, page := e.get("/problems")
	if n := strings.Count(page, `action="/structure/scan"`); n != 1 {
		t.Fatalf("the administrator sees %d forms of Scan now, want 1: the S4 of Wrong (the H1 of SOL has none)", n)
	}
	if row := findingRow(t, e, page, "S4", "AKKA-LIVE-1SOURCE/Wrong on AKKA"); !strings.Contains(row, `<input type="hidden" name="server" value="akka">`) ||
		!strings.Contains(row, `<button type="submit">Scan now</button>`) {
		t.Fatalf("the S4 row has no Scan now for AKKA:\n%s", row)
	}

	viewer := newClient(t, e.srv.URL, &http.Client{})
	if r, _ := viewer.do(http.MethodPost, "/login", url.Values{"csrf": {viewer.token("/login")}, "password": {"viewer password 1"}}, nil); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("viewer login: %d", r.StatusCode)
	}
	_, vpage := viewer.do(http.MethodGet, "/problems", nil, nil)
	if !strings.Contains(vpage, "Directory Wrong in share") || !strings.Contains(vpage, "· last scan ") || strings.Contains(vpage, "/structure/scan") {
		t.Fatal("the viewer must see the finding and its last scan, but no Scan now")
	}
	if r, _ := viewer.do(http.MethodPost, "/structure/scan", url.Values{"csrf": {viewer.token("/problems")}, "server": {"akka"}}, nil); r.StatusCode != http.StatusForbidden {
		t.Fatalf("Scan now of a viewer: %d, want 403", r.StatusCode)
	}
	nobody := newClient(t, e.srv.URL, &http.Client{})
	if r, _ := nobody.do(http.MethodPost, "/structure/scan", url.Values{"csrf": {nobody.token("/login")}, "server": {"akka"}}, nil); r.StatusCode != http.StatusSeeOther ||
		r.Header.Get("Location") != "/login" {
		t.Fatalf("Scan now without a login: %d %q, want the login page", r.StatusCode, r.Header.Get("Location"))
	}

	// Open access: each person can view, only the administrator can scan.
	open := scanEnv(t, "access: {viewer: open}\n")
	shareWith(open, time.Now(), "Wrong")
	open.eng.Evaluate()
	openHook := withScanHook(open, tooSoon)
	anon := newClient(t, open.srv.URL, &http.Client{})
	_, opage := anon.do(http.MethodGet, "/problems", nil, nil)
	if !strings.Contains(opage, "· last scan ") || strings.Contains(opage, "/structure/scan") {
		t.Fatal("open access: the page must show the last scan, but no Scan now")
	}
	if r, _ := anon.do(http.MethodPost, "/structure/scan", url.Values{"csrf": {anon.token("/login")}, "server": {"akka"}}, nil); r.StatusCode != http.StatusForbidden {
		t.Fatalf("Scan now with open access and no login: %d, want 403", r.StatusCode)
	}
	if calls := append(hook.called(), openHook.called()...); len(calls) != 0 {
		t.Fatalf("refused requests started scans: %v", calls)
	}

	resp := e.post("/structure/scan", url.Values{"csrf": {e.csrf("/problems")}, "server": {"akka"}, "back": {"/problems?category=structure#f-x"}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/problems?category=structure#f-x" {
		t.Fatalf("Scan now of the administrator: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if calls := hook.called(); len(calls) != 1 || calls[0] != "akka" {
		t.Fatalf("scans: %v, want [akka]", calls)
	}
}

// "Scan now" has the rules of the other forms of the administrator (wave 3):
// the CSRF token of the session, and an Origin of this dashboard.
func TestScanNowNeedsTheCSRFTokenOfTheSession(t *testing.T) {
	e := scanEnv(t, "")
	hook := withScanHook(e, tooSoon)
	own := e.csrf("/problems")
	send := func(form url.Values, header map[string]string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/structure/scan", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		for k, v := range header {
			req.Header.Set(k, v)
		}
		resp, err := e.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for name, c := range map[string]struct {
		form   url.Values
		header map[string]string
	}{
		"no CSRF token":         {url.Values{"server": {"akka"}}, nil},
		"a different token":     {url.Values{"server": {"akka"}, "csrf": {"x" + own}}, nil},
		"Origin of a different": {url.Values{"server": {"akka"}, "csrf": {own}}, map[string]string{"Origin": "https://evil.invalid"}},
		"Origin null":           {url.Values{"server": {"akka"}, "csrf": {own}}, map[string]string{"Origin": "null"}},
	} {
		if got := send(c.form, c.header); got != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", name, got)
		}
	}
	// A CSRF pair that a page got without a login does not work with the
	// session of the administrator.
	other := newClient(t, e.srv.URL, &http.Client{})
	anon := other.token("/login")
	if r, _ := other.do(http.MethodPost, "/structure/scan", url.Values{"csrf": {anon}, "server": {"akka"}},
		map[string]string{"Cookie": sessionCookie + "=" + e.sessionOf() + "; " + csrfCookie + "=" + anon}); r.StatusCode != http.StatusForbidden {
		t.Errorf("an anonymous CSRF pair with the session of the administrator: %d, want 403", r.StatusCode)
	}
	if calls := hook.called(); len(calls) != 0 {
		t.Fatalf("refused requests started scans: %v", calls)
	}
	u, _ := url.Parse(e.srv.URL)
	if got := send(url.Values{"server": {"akka"}, "csrf": {own}}, map[string]string{"Origin": "http://" + u.Host}); got != http.StatusSeeOther {
		t.Fatalf("the token of the session: %d, want 303", got)
	}
	if calls := hook.called(); len(calls) != 1 {
		t.Fatalf("scans: %v, want 1", calls)
	}
}

// The answer comes after the scan and the evaluation of its result: the
// finding of a corrected mistake is resolved on the next page, and the
// flash message says so. An INFO log line names the administrator, the
// address and the server.
func TestScanNowResolvesACorrectedMistake(t *testing.T) {
	e := scanEnv(t, "")
	logs := &syncBuffer{}
	e.web.log = slog.New(slog.NewTextHandler(logs, nil))
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		e.eng.Run(ctx)
	}()
	defer func() { cancel(); <-stopped }()
	shareWith(e, time.Now().Add(-40*time.Minute), "Wrong", "LIVE-RIGHT")
	if !e.eng.Settle(ctx, 5*time.Second) || len(structureOpen(e)) != 1 {
		t.Fatalf("open structure findings: %v, want the S4 of Wrong", structureOpen(e))
	}

	// The data steward removed the directory. The scan of the fake
	// collector reads the share again.
	hook := withScanHook(e, func(string) collector.ScanStart {
		started, end := time.Now(), make(chan struct{})
		go func() {
			time.Sleep(50 * time.Millisecond)
			shareWith(e, time.Now(), "LIVE-RIGHT")
			close(end)
		}()
		return collector.ScanStart{State: collector.ScanStarted, Started: started, Done: end}
	})
	resp := e.post("/structure/scan", url.Values{"csrf": {e.csrf("/problems")}, "server": {"akka"}})
	ok, failed := flashes(resp)
	want := "The structure scan of server AKKA is complete. syncwatch resolved 1 finding. The server has no open structure findings."
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/problems" || ok != want || failed != "" {
		t.Fatalf("answer %d %q: %q / %q, want %q", resp.StatusCode, resp.Header.Get("Location"), ok, failed, want)
	}
	if open := structureOpen(e); len(open) != 0 {
		t.Fatalf("after the answer: open structure findings %v", open)
	}
	// The page after the answer has the flash message, and the finding is
	// in the list of resolved findings only.
	if _, page := e.get("/problems"); strings.Contains(page, `action="/structure/scan"`) || !strings.Contains(page, "✔ "+want) ||
		!strings.Contains(page, "Directory Wrong in share AKKA-LIVE-1SOURCE on AKKA: name does not match the project pattern <span class=\"muted\">· ") {
		t.Fatalf("the page after the answer:\n%s", page)
	}
	line := `level=INFO msg="administrator started a structure scan" server=AKKA role=admin login=password ip=127.0.0.1`
	if !strings.Contains(logs.String(), line) {
		t.Fatalf("no %q in the log:\n%s", line, logs)
	}
	if calls := hook.called(); len(calls) != 1 || calls[0] != "akka" {
		t.Fatalf("scans: %v", calls)
	}

	// A scan that opens a finding: a new project folder that is not in
	// Syncthing (S7).
	withScanHook(e, func(string) collector.ScanStart {
		started, end := time.Now(), make(chan struct{})
		go func() {
			shareWith(e, time.Now(), "LIVE-RIGHT", "LIVE-NEWPROJECT")
			e.hub.Update("akka", func(s *model.Server) { s.Structure.Shares[0].Dirs[1].FolderIDs = nil })
			close(end)
		}()
		return collector.ScanStart{State: collector.ScanStarted, Started: started, Done: end}
	})
	ok, _ = flashes(e.post("/structure/scan", url.Values{"csrf": {e.csrf("/problems")}, "server": {"akka"}}))
	if want := "The structure scan of server AKKA is complete. syncwatch opened 1 new finding. 1 structure finding of the server is open."; ok != want {
		t.Fatalf("scan with a new finding: %q, want %q", ok, want)
	}
	if open := structureOpen(e); len(open) != 1 || open[0] != "S7 AKKA-LIVE-1SOURCE/LIVE-NEWPROJECT on AKKA" {
		t.Fatalf("open structure findings: %v", open)
	}
}

// The answers when syncwatch starts no scan: the 30 seconds (with the start
// of the last scan and the time of the next one), a scan that runs, a
// server that is not up, and a server that is not in the settings. The
// times have seconds, and the date when they are not today.
func TestScanNowAnswers(t *testing.T) {
	old := scanWait
	scanWait = 200 * time.Millisecond
	defer func() { scanWait = old }()
	e := scanEnv(t, "")
	logs := &syncBuffer{}
	e.web.log = slog.New(slog.NewTextHandler(logs, nil))
	e.web.now = func() time.Time { return today(e, 10, 0, 20) }
	shareWith(e, today(e, 10, 0, 6), "LIVE-RIGHT")
	e.eng.Evaluate()
	running := make(chan struct{}) // a scan that does not end in the test
	defer close(running)
	press := func(server string, reply collector.ScanStart) (ok, failed string) {
		t.Helper()
		hook := withScanHook(e, func(string) collector.ScanStart { return reply })
		resp := e.post("/structure/scan", url.Values{"csrf": {e.csrf("/problems")}, "server": {server}})
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("%s: %d", server, resp.StatusCode)
		}
		if calls := hook.called(); len(calls) != 1 || calls[0] != server {
			t.Fatalf("scans: %v, want [%s]", calls, server)
		}
		return flashes(resp)
	}
	yesterday := today(e, 0, 0, 0).Add(-10 * time.Second)
	for _, c := range []struct {
		name, server string
		reply        collector.ScanStart
		ok, failed   string
	}{
		{"a second press after a complete scan", "akka",
			collector.ScanStart{State: collector.ScanTooSoon, Started: today(e, 10, 0, 5), Next: today(e, 10, 0, 35)},
			"", "The last structure scan of server AKKA started at 10:00:05. The result is on this page. You can start the next scan at 10:00:35."},
		{"a second press while the scan runs", "akka",
			collector.ScanStart{State: collector.ScanTooSoon, Started: today(e, 10, 0, 5), Next: today(e, 10, 0, 35), Done: running},
			"", "The last structure scan of server AKKA started at 10:00:05. The result follows on this page. You can start the next scan at 10:00:35."},
		{"a scan of yesterday", "akka",
			collector.ScanStart{State: collector.ScanTooSoon, Started: yesterday, Next: yesterday.Add(30 * time.Second)},
			"", "The last structure scan of server AKKA started at " + yesterday.Format("2006-01-02 15:04:05") + ". The result is on this page. You can start the next scan at 00:00:20."},
		{"a timed scan runs longer than the wait", "akka",
			collector.ScanStart{State: collector.ScanRunning, Started: today(e, 10, 0, 5), Done: running},
			"The structure scan of server AKKA started at 10:00:05. The result follows on this page.", ""},
		{"a server that is down", "sol", collector.ScanStart{State: collector.ScanNotUp},
			"", "Server SOL is unreachable: refused. syncwatch can scan it when it is up again."},
	} {
		ok, failed := press(c.server, c.reply)
		if ok != c.ok || failed != c.failed {
			t.Errorf("%s: %q / %q, want %q / %q", c.name, ok, failed, c.ok, c.failed)
		}
	}
	for _, c := range []struct {
		name   string
		update func(*model.Server)
		want   string
	}{
		{"first full check", func(s *model.Server) { s.Loaded = false }, "The first full check of server AKKA is not complete. Try again in 1 minute."},
		{"no session", func(s *model.Server) {}, "syncwatch is not connected to server AKKA now. Try again in 1 minute."},
		{"certificate", func(s *model.Server) { s.Status = model.StatusCertChanged },
			"The certificate of server AKKA changed. Accept the certificate in Settings → Servers. Then start the scan again."},
		{"not contacted", func(s *model.Server) { s.Status = model.StatusUnknown }, "syncwatch is not connected to server AKKA now. Try again in 1 minute."},
	} {
		shareWith(e, today(e, 10, 0, 6), "LIVE-RIGHT")
		e.hub.Update("akka", c.update)
		e.eng.Evaluate()
		if ok, failed := press("akka", collector.ScanStart{State: collector.ScanNotUp}); ok != "" || failed != c.want {
			t.Errorf("not up, %s: %q / %q, want %q", c.name, ok, failed, c.want)
		}
	}

	// A server that is not in the settings, or that is disabled: no scan.
	for _, id := range []string{"sirius", "nope"} {
		hook := withScanHook(e, tooSoon)
		ok, failed := flashes(e.post("/structure/scan", url.Values{"csrf": {e.csrf("/problems")}, "server": {id}}))
		if want := "syncwatch cannot scan this server. The server is not in Settings → Servers, or it is disabled."; ok != "" || failed != want {
			t.Errorf("%s: %q / %q", id, ok, failed)
		}
		if calls := hook.called(); len(calls) != 0 {
			t.Errorf("%s: scans %v", id, calls)
		}
	}
	if strings.Contains(logs.String(), "administrator started a structure scan") {
		t.Fatalf("a log line for a scan that did not start:\n%s", logs)
	}
}

// Settings → Structure has "Scan now" for each enabled server and the last
// scan of each server. Each server gets its line in the answer.
func TestScanNowInSettings(t *testing.T) {
	old := scanWait
	scanWait = 200 * time.Millisecond
	defer func() { scanWait = old }()
	e := scanEnv(t, "")
	e.web.now = func() time.Time { return today(e, 9, 8, 0) }
	shareWith(e, today(e, 9, 5, 0), "LIVE-RIGHT")
	e.eng.Evaluate()
	_, page := e.get("/settings")
	card := section(t, page, "structure")
	for _, want := range []string{`<form method="post" action="/structure/scan" class="inline">`, `<input type="hidden" name="back" value="/settings#structure">`,
		`<p class="small muted">Last scan: AKKA 09:05 (<span data-ago="180">3 minutes ago</span>) · SOL no scan after the start of syncwatch</p>`} {
		if !strings.Contains(card, want) {
			t.Errorf("Settings → Structure has no %q:\n%s", want, card)
		}
	}
	if strings.Contains(card, `name="server"`) || strings.Contains(card, "SIRIUS") {
		t.Errorf("Settings → Structure: Scan now is for all enabled servers, without SIRIUS:\n%s", card)
	}

	running := make(chan struct{})
	defer close(running)
	hook := withScanHook(e, func(id string) collector.ScanStart {
		if id == "akka" {
			return collector.ScanStart{State: collector.ScanStarted, Started: today(e, 9, 8, 0), Done: running}
		}
		return collector.ScanStart{State: collector.ScanNotUp}
	})
	resp := e.post("/structure/scan", url.Values{"csrf": {e.csrf("/settings")}, "back": {"/settings#structure"}})
	ok, failed := flashes(resp)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/settings#structure" {
		t.Fatalf("answer %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if calls := hook.called(); len(calls) != 2 || calls[0] != "akka" || calls[1] != "sol" {
		t.Fatalf("scans %v, want [akka sol]", calls)
	}
	if ok != "The structure scan of server AKKA started at 09:08:00. The result follows on this page." ||
		failed != "Server SOL is unreachable: refused. syncwatch can scan it when it is up again." {
		t.Fatalf("answer: %q / %q", ok, failed)
	}
}

// Each structure finding shows the end of the last structure scan that
// listed its share (S3 to S7), or of the last structure scan of its server
// (S1, S2): the clock time today, with the date on another day (the
// configured zone), and how long ago. The details say when no scan
// completed after the start. Other findings have no last scan.
func TestStructureFindingShowsTheLastScan(t *testing.T) {
	e := scanEnv(t, "")
	e.web.now = func() time.Time { return today(e, 9, 8, 0) }
	shareWith(e, today(e, 9, 5, 0), "Wrong")
	// S2: a Syncthing folder (outer) is the root of a share (the parent of
	// inner). The last scan of the server ended after the listing of the
	// share of Wrong.
	e.hub.Update("akka", func(s *model.Server) {
		s.Folders = map[string]*model.Folder{"outer": {ID: "outer", Path: "/data/outer"}, "inner": {ID: "inner", Path: "/data/outer/inner"}}
		s.LastStructureScan = today(e, 9, 6, 0)
	})
	e.eng.Evaluate()
	_, page := e.get("/problems")
	row := findingRow(t, e, page, "S4", "AKKA-LIVE-1SOURCE/Wrong on AKKA")
	for _, want := range []string{`<span class="muted nowrap">· last scan 09:05 (<span data-ago="180">3 minutes ago</span>)</span>`,
		`<th title="The end of the last structure scan that listed the share">Last scan</th><td>09:05 (<span data-ago="180">3 minutes ago</span>)</td>`} {
		if !strings.Contains(row, want) {
			t.Errorf("the S4 row has no %q:\n%s", want, row)
		}
	}
	s2 := findingRow(t, e, page, "S2", "outer on AKKA")
	for _, want := range []string{`<span class="muted nowrap">· last scan 09:06 (<span data-ago="120">2 minutes ago</span>)</span>`,
		`<th title="The end of the last structure scan of the server">Last scan</th><td>09:06 (<span data-ago="120">2 minutes ago</span>)</td>`} {
		if !strings.Contains(s2, want) {
			t.Errorf("the S2 row has no %q:\n%s", want, s2)
		}
	}
	if h1 := findingRow(t, e, page, "H1", "SOL"); strings.Contains(h1, "ast scan") || strings.Contains(h1, "Scan now") {
		t.Errorf("the H1 row has a last scan or Scan now:\n%s", h1)
	}

	// 23:50 the day before, seen at 01:30: no change of daylight saving time
	// is between these 2 times (it is at 02:00 or 03:00).
	e.web.now = func() time.Time { return today(e, 1, 30, 0) }
	yesterday := today(e, 0, 0, 0).Add(-10 * time.Minute)
	e.hub.Update("akka", func(s *model.Server) { s.LastStructureScan, s.Structure.Shares[0].ListedAt = yesterday, yesterday })
	e.eng.Evaluate()
	_, page = e.get("/problems")
	if row := findingRow(t, e, page, "S4", "AKKA-LIVE-1SOURCE/Wrong on AKKA"); !strings.Contains(row, "· last scan "+yesterday.Format("2006-01-02 15:04")+` (<span data-ago="6000">1 hour ago</span>)`) {
		t.Errorf("a scan of yesterday must have its date:\n%s", row)
	}

	e.hub.Update("akka", func(s *model.Server) { s.LastStructureScan, s.Structure.Shares[0].ListedAt = time.Time{}, time.Time{} })
	e.eng.Evaluate()
	_, page = e.get("/problems")
	for _, f := range [][2]string{{"S4", "AKKA-LIVE-1SOURCE/Wrong on AKKA"}, {"S2", "outer on AKKA"}} {
		if row := findingRow(t, e, page, f[0], f[1]); strings.Contains(row, "· last scan") || !strings.Contains(row, "<td>no scan after the start of syncwatch</td>") {
			t.Errorf("%s before the first scan:\n%s", f[0], row)
		}
	}
}

func TestAgoWords(t *testing.T) {
	for d, want := range map[time.Duration]string{
		-time.Second: "less than 1 minute ago", 59 * time.Second: "less than 1 minute ago", time.Minute: "1 minute ago",
		59*time.Minute + 59*time.Second: "59 minutes ago", time.Hour: "1 hour ago", 23*time.Hour + 59*time.Minute: "23 hours ago",
		24 * time.Hour: "1 day ago", 50 * time.Hour: "2 days ago",
	} {
		if got := agoWords(d); got != want {
			t.Errorf("agoWords(%v) = %q, want %q", d, got, want)
		}
	}
}

// app.js counts the age of the last scan on while the page is open, with the
// words of agoWords.
func TestAppJSCountsTheLastScanOn(t *testing.T) {
	e := newEnv(t, scanYAML)
	_, js := e.get("/assets/app.js")
	for _, want := range []string{`document.querySelectorAll("[data-ago]")`, `if (m < 1) return "less than 1 minute ago";`,
		`return plural(m, "minute", "minutes") + " ago";`, `return plural(Math.floor(m / 60), "hour", "hours") + " ago";`,
		`return plural(Math.floor(m / (24 * 60)), "day", "days") + " ago";`, "stampAgo(document);"} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %s", want)
		}
	}
}

// W-056, W-049: servers[].last_structure_scan of /api/v1/status is the end of
// the last structure scan in UTC with whole seconds, null before the first
// scan and for a disabled server.
func TestAPILastStructureScan(t *testing.T) {
	e := scanEnv(t, "")
	end := time.Date(2026, 10, 11, 17, 29, 1, 784844188, time.FixedZone("CEST", 2*3600))
	shareWith(e, end, "LIVE-RIGHT")
	e.eng.Evaluate()
	_, st := e.apiGet("/api/v1/status", "Bearer swt_test_token", "")
	servers := apiServers(st)
	if got := servers["akka"]["last_structure_scan"]; got != "2026-10-11T15:29:01Z" {
		t.Errorf("akka: last_structure_scan %v", got)
	}
	for _, id := range []string{"sol", "sirius"} {
		if v, ok := servers[id]["last_structure_scan"]; !ok || v != nil {
			t.Errorf("%s: last_structure_scan %v (key present: %v), want null", id, v, ok)
		}
	}
	checkTimes(t, "status", st)
}

// A share that the scan cannot list keeps the last known state: its
// findings show the end of its last listing, with the error in the details,
// and the answer to Scan now names the share in red. A scan that listed no
// share is not complete (review of W-056, finding 1).
func TestScanNowShareThatCannotBeListed(t *testing.T) {
	e := scanEnv(t, "")
	e.web.now = func() time.Time { return today(e, 10, 0, 20) }
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		e.eng.Run(ctx)
	}()
	defer func() { cancel(); <-stopped }()
	listed := today(e, 9, 20, 0)
	shareWith(e, listed, "Wrong", "LIVE-RIGHT")
	finals := &model.Share{Path: "/share/AKKA-LIVE-4FINALS", Name: "AKKA-LIVE-4FINALS", ScannedAt: listed, ListedAt: listed,
		Dirs: []*model.Dir{{Name: "LIVE-X-4FINALS", Path: "/share/AKKA-LIVE-4FINALS/LIVE-X-4FINALS", FolderIDs: []string{"live-x-4finals"}}}}
	e.hub.Update("akka", func(s *model.Server) { s.Structure.Shares = append(s.Structure.Shares, finals) })
	if !e.eng.Settle(ctx, 5*time.Second) || len(structureOpen(e)) != 1 {
		t.Fatalf("open structure findings: %v, want the S4 of Wrong", structureOpen(e))
	}

	// The data steward renamed Wrong, but the listing of its share fails
	// (a timeout on a busy NAS). The scan lists the other share, as
	// collector.scan does: the failed share keeps its directories and times.
	scan := func(fail map[string]string, end time.Time) collector.ScanStart {
		done := make(chan struct{})
		go func() {
			e.hub.Update("akka", func(s *model.Server) {
				for _, sh := range s.Structure.Shares {
					if sh.Error = fail[sh.Name]; sh.Error == "" {
						sh.ScannedAt, sh.ListedAt = end.Add(-time.Second), end
					}
				}
				s.Structure.ScannedAt = end.Add(-time.Second)
				if len(fail) < len(s.Structure.Shares) {
					s.LastStructureScan = end
				}
			})
			close(done)
		}()
		return collector.ScanStart{State: collector.ScanStarted, Started: end.Add(-time.Second), Done: done, End: &collector.ScanEnd{}}
	}
	const notMounted = "not found on the server (check that the directory is mounted in the Syncthing container)"
	withScanHook(e, func(string) collector.ScanStart {
		return scan(map[string]string{"AKKA-LIVE-1SOURCE": "timeout"}, today(e, 10, 0, 21))
	})
	ok, failed := flashes(e.post("/structure/scan", url.Values{"csrf": {e.csrf("/problems")}, "server": {"akka"}}))
	if want := "The structure scan of server AKKA is complete for 1 of 2 shares. 1 structure finding of the server is open."; ok != want {
		t.Errorf("1 of 2 shares listed: %q, want %q", ok, want)
	}
	if want := "The findings of share AKKA-LIVE-1SOURCE on server AKKA show the last known state. syncwatch cannot list the share: timeout."; failed != want {
		t.Errorf("1 of 2 shares listed: %q, want %q", failed, want)
	}
	_, page := e.get("/problems")
	row := findingRow(t, e, page, "S4", "AKKA-LIVE-1SOURCE/Wrong on AKKA")
	for _, want := range []string{`<span class="tag stale"`, `<span class="muted nowrap">· last scan 09:20 (<span data-ago="2420">40 minutes ago</span>)</span>`,
		`Last scan</th><td>09:20 (<span data-ago="2420">40 minutes ago</span>) · syncwatch cannot list the share: timeout</td>`} {
		if !strings.Contains(row, want) {
			t.Errorf("the S4 row of the share that the scan cannot list has no %q:\n%s", want, row)
		}
	}
	apiLast := func() any {
		_, st := e.apiGet("/api/v1/status", "Bearer swt_test_token", "")
		return apiServers(st)["akka"]["last_structure_scan"]
	}
	end1 := today(e, 10, 0, 21).UTC().Format("2006-01-02T15:04:05Z")
	if got := apiLast(); got != end1 {
		t.Errorf("1 of 2 shares listed: last_structure_scan %v, want %v", got, end1)
	}

	// No share can be listed: no green answer, a red sentence for each
	// share, and the last scan of the server does not move.
	withScanHook(e, func(string) collector.ScanStart {
		return scan(map[string]string{"AKKA-LIVE-1SOURCE": "timeout", "AKKA-LIVE-4FINALS": notMounted}, today(e, 10, 0, 52))
	})
	ok, failed = flashes(e.post("/structure/scan", url.Values{"csrf": {e.csrf("/problems")}, "server": {"akka"}}))
	if want := "The findings of share AKKA-LIVE-1SOURCE on server AKKA show the last known state. syncwatch cannot list the share: timeout.\n" +
		"The findings of share AKKA-LIVE-4FINALS on server AKKA show the last known state. syncwatch cannot list the share: " + notMounted + "."; ok != "" || failed != want {
		t.Errorf("no share listed: %q / %q, want no green answer and %q", ok, failed, want)
	}
	if got := apiLast(); got != end1 {
		t.Errorf("no share listed: last_structure_scan %v, want %v (no move)", got, end1)
	}
}

// A scan that stops before the end (its session ends, or the first full
// check of the server is not complete) writes no result. The answer says so
// in red and promises no result, and it does not wait for an evaluation.
// The collector does not count the scan: the next press can start a scan
// (review of W-056, finding 3).
func TestScanNowStoppedScan(t *testing.T) {
	old := scanWait
	scanWait = 2 * time.Second
	defer func() { scanWait = old }()
	e := scanEnv(t, "")
	e.web.now = func() time.Time { return today(e, 10, 0, 20) }
	shareWith(e, today(e, 9, 0, 0), "Wrong", "LIVE-RIGHT")
	e.eng.Evaluate()
	stopped := func(state, why string) collector.ScanStart {
		done := make(chan struct{})
		close(done) // the scan stopped: Done closes, and the hub does not change
		return collector.ScanStart{State: state, Started: today(e, 10, 0, 20), Next: today(e, 10, 0, 50), Done: done, End: &collector.ScanEnd{Stopped: why}}
	}
	const session = "The structure scan of server AKKA stopped before the end: the connection to the server ended. Try again in 1 minute."
	for _, c := range []struct {
		name  string
		reply collector.ScanStart
		want  string
	}{
		{"the session ends during the scan", stopped(collector.ScanStarted, collector.ScanStopSession), session},
		{"the timed scan that runs stops", stopped(collector.ScanRunning, collector.ScanStopSession), session},
		{"the scan of a second press stops", stopped(collector.ScanTooSoon, collector.ScanStopSession), session},
		{"the first full check is not complete", stopped(collector.ScanStarted, collector.ScanStopNotLoaded),
			"The structure scan of server AKKA stopped before the end: the first full check of the server is not complete. Try again in 1 minute."},
	} {
		withScanHook(e, func(string) collector.ScanStart { return c.reply })
		start := time.Now()
		ok, failed := flashes(e.post("/structure/scan", url.Values{"csrf": {e.csrf("/problems")}, "server": {"akka"}}))
		if ok != "" || failed != c.want {
			t.Errorf("%s: %q / %q, want no green answer and %q", c.name, ok, failed, c.want)
		}
		if d := time.Since(start); d >= scanWait {
			t.Errorf("%s: the answer waited %v for an evaluation of a scan without a result", c.name, d.Round(time.Millisecond))
		}
	}
}
