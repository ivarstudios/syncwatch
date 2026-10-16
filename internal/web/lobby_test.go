package web

import (
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/engine"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/notify"
	"github.com/ivarstudios/syncwatch/internal/store"
)

// lobbySites has 2 sites and 3 servers: AKKA and ODIN at "IVAR Office", SOL
// at "Branch office".
const lobbySites = `servers:
  - {name: AKKA, url: "https://a:1", site: "IVAR Office", gui_url: "https://akka.invalid:8384/"}
  - {name: SOL, url: "https://b:1", site: "Branch office"}
  - {name: ODIN, url: "https://c:1", site: "IVAR Office"}
timezone: Europe/Stockholm
api: {token: swt_test_token}
`

// The complete device IDs of the lobby tests: the page shows none of them.
const (
	lobbyAkkaID = "AKKAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA"
	lobbySolID  = "SOLSOLS-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB-BBBBBBB"
	lobbyOdinID = "ODINODI-CCCCCCC-CCCCCCC-CCCCCCC-CCCCCCC-CCCCCCC-CCCCCCC-CCCCCCC"
	lobbyAnnaID = "WSANNAA-DDDDDDD-DDDDDDD-DDDDDDD-DDDDDDD-DDDDDDD-DDDDDDD-DDDDDDD"
	lobbyCarlID = "WSCARLL-EEEEEEE-EEEEEEE-EEEEEEE-EEEEEEE-EEEEEEE-EEEEEEE-EEEEEEE"
	lobbyDaniID = "LAPTOPD-FFFFFFF-FFFFFFF-FFFFFFF-FFFFFFF-FFFFFFF-FFFFFFF-FFFFFFF"
)

var lobbyDeviceIDRe = regexp.MustCompile(`[A-Z2-7]{7}(-[A-Z2-7]{7}){7}`)

// lobbyHub sets the servers of lobbySites: AKKA and ODIN are up, with the PCs
// WS-ANNA (on both) and LAPTOP-DANI (on AKKA, through a relay) connected;
// SOL is unreachable from an hour ago.
func lobbyHub(e *env) {
	now := time.Now()
	dev := func(ids ...string) map[string]*model.Device {
		names := map[string]string{lobbyAkkaID: "AKKA", lobbySolID: "SOL", lobbyOdinID: "ODIN", lobbyAnnaID: "WS-ANNA", lobbyCarlID: "WS-CARL", lobbyDaniID: "LAPTOP-DANI"}
		m := map[string]*model.Device{}
		for _, id := range ids {
			m[id] = &model.Device{ID: id, Name: names[id]}
		}
		return m
	}
	e.hub.Update("akka", func(s *model.Server) {
		s.Status, s.Loaded, s.MyID, s.LastContact, s.LastError = model.StatusUp, true, lobbyAkkaID, now, ""
		s.GUIURL = "https://akka.invalid:8384/"
		s.Devices = dev(lobbySolID, lobbyAnnaID, lobbyCarlID, lobbyDaniID)
		s.Connections = map[string]*model.Connection{lobbySolID: {Connected: true}, lobbyAnnaID: {Connected: true}, lobbyDaniID: {Connected: true, Type: "relay-client"}}
		s.Folders = map[string]*model.Folder{"f1": {ID: "f1", Label: "projects", State: "idle"}, "f2": {ID: "f2", Label: "finals", State: "syncing", NeedItems: 4}}
	})
	e.hub.Update("sol", func(s *model.Server) {
		s.Status, s.Loaded, s.MyID, s.LastContact, s.LastError = model.StatusDown, true, lobbySolID, now.Add(-time.Hour), "no route to host"
		s.Devices = dev(lobbyAkkaID, lobbyCarlID)
		s.Connections = map[string]*model.Connection{lobbyCarlID: {Connected: true}}
		s.Folders = map[string]*model.Folder{"f1": {ID: "f1", Label: "projects", State: "idle"}, "f3": {ID: "f3", Label: "renders", State: "idle"}}
	})
	e.hub.Update("odin", func(s *model.Server) {
		s.Status, s.Loaded, s.MyID, s.LastContact, s.LastError = model.StatusUp, true, lobbyOdinID, now, ""
		s.Devices = dev(lobbyAnnaID)
		s.Connections = map[string]*model.Connection{lobbyAnnaID: {Connected: true}}
		s.Folders = map[string]*model.Folder{"f4": {ID: "f4", Label: "archive", State: "idle"}}
	})
	e.eng.Evaluate()
}

// lobbyView returns a copy of the engine's view with hand-made links and,
// when open is not nil, hand-made findings. The engine of these tests sets
// no links (the rules fill them).
func lobbyView(e *env, links []model.ServerLink, siteLinks []model.SiteLink, open []*store.Record) *engine.View {
	v := *e.eng.View()
	v.Snapshot = v.Snapshot.Clone()
	v.Snapshot.Links, v.Snapshot.SiteLinks = links, siteLinks
	if open != nil {
		v.Open = open
	}
	return &v
}

// lobbyHTML renders the lobby body of a view, as /frag/lobby does.
func lobbyHTML(e *env, v *engine.View, site string) string {
	rec := httptest.NewRecorder()
	e.web.fragment(rec, "lobby-body", requestInfo{Role: RoleViewer}, map[string]any{"VM": e.web.lobby(v, site)})
	return html.UnescapeString(rec.Body.String())
}

func lobbyRecord(id, check string, sev model.Severity, server string, related []string, urgent bool, msg string, opened time.Time) *store.Record {
	return &store.Record{Finding: model.Finding{ID: id, Check: check, Checks: []string{check}, Severity: sev, Server: server, Related: related,
		Urgent: urgent, Subject: msg, Message: msg}, OpenedAt: opened}
}

// lobbyClock sets the clock of the web server to 2026-10-11 14:00 in the
// configured time zone and returns it.
func lobbyClock(e *env) time.Time {
	now := time.Date(2026, 10, 11, 14, 0, 0, 0, e.cfg.Get().Location())
	e.web.now = func() time.Time { return now }
	return now
}

func lobbyGroupNames(vm lobbyVM) []string {
	var out []string
	for _, g := range vm.Groups {
		var names []string
		for _, s := range g.Servers {
			names = append(names, s.Name)
		}
		out = append(out, g.Name+": "+strings.Join(names, ", "))
	}
	return out
}

// Viewers open the lobby: with open access without a login, with a viewer
// password after one login. With access for the administrator only, the
// page asks for the login.
func TestLobbyAccess(t *testing.T) {
	h, _ := HashPassword("correct horse battery")

	open := newEnv(t, lobbySites+"access: {viewer: open}\n")
	_ = open.st.SetSetting(KeyAdminHash, h)
	resp, body := open.get("/lobby")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "<h3>AKKA</h3>") || !strings.Contains(body, `id="lobby-body"`) {
		t.Fatalf("open access: %d %s", resp.StatusCode, body)
	}
	if resp, frag := open.get("/frag/lobby"); resp.StatusCode != http.StatusOK || !strings.HasPrefix(strings.TrimSpace(frag), `<div id="lobby-body"`) ||
		strings.Contains(frag, "<html") || strings.Contains(frag, "<head>") || strings.Contains(frag, "<body") || !strings.Contains(frag, "<h3>AKKA</h3>") {
		t.Fatalf("open access, fragment: %d %s", resp.StatusCode, frag)
	}

	closed := newEnv(t, lobbySites)
	_ = closed.st.SetSetting(KeyAdminHash, h)
	if resp, _ := closed.get("/lobby?site=Branch%20office"); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login?next=/lobby%3Fsite=Branch%2520office" {
		t.Fatalf("administrator only: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, _ := closed.get("/frag/lobby"); resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("HX-Redirect") != "/login" {
		t.Fatalf("administrator only, fragment: %d", resp.StatusCode)
	}

	viewer := newEnv(t, lobbySites+"access: {viewer: password}\n")
	_ = viewer.st.SetSetting(KeyAdminHash, h)
	vh, _ := HashPassword("viewer password 1")
	_ = viewer.st.SetSetting(KeyViewerHash, vh)
	if resp, _ := viewer.get("/lobby"); resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login?next=/lobby") {
		t.Fatalf("viewer password, no login: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	r := viewer.post("/login", url.Values{"csrf": {viewer.csrf("/login")}, "password": {"viewer password 1"}, "next": {"/lobby"}})
	if r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/lobby" {
		t.Fatalf("viewer login: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	if resp, body := viewer.get("/lobby"); resp.StatusCode != http.StatusOK || !strings.Contains(body, "<h3>SOL</h3>") {
		t.Fatalf("viewer password, after the login: %d", resp.StatusCode)
	}

	proxy := newEnv(t, lobbySites+"access: {mode: proxy}\n")
	if resp, _ := proxy.get("/lobby"); resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy mode: %d", resp.StatusCode)
	}
}

// The lobby is read-only, also for an administrator: no form, no button, no
// navigation, no link (also no "Open in Syncthing"), no device ID and no
// finding ID.
func TestLobbyPageIsReadOnly(t *testing.T) {
	e := adminEnv(t, lobbySites+"access: {viewer: open}\n")
	lobbyHub(e)
	if len(e.eng.View().Open) == 0 {
		t.Fatal("the test needs open findings")
	}
	anon, _ := http.Get(e.srv.URL + "/lobby")
	b, _ := io.ReadAll(anon.Body)
	anon.Body.Close()
	_, admin := e.get("/lobby")
	_, frag := e.get("/frag/lobby")
	for name, page := range map[string]string{"open access": string(b), "administrator": admin, "fragment": frag} {
		for _, bad := range []string{"<form", "<button", "<input", "<nav", "<a ", "Open in Syncthing", "akka.invalid", "Log out", "/settings"} {
			if strings.Contains(page, bad) {
				t.Errorf("%s: the lobby has %q", name, bad)
			}
		}
		if m := lobbyDeviceIDRe.FindString(page); m != "" {
			t.Errorf("%s: the lobby shows the device ID %s", name, m)
		}
		for _, r := range e.eng.View().Open {
			if strings.Contains(page, r.ID) {
				t.Errorf("%s: the lobby shows the finding ID %s", name, r.ID)
			}
		}
		if !strings.Contains(page, "Server SOL is unreachable") {
			t.Errorf("%s: no H1 for SOL in the list", name)
		}
	}
	// A device ID in a message (a name that a device sends) is shortened.
	v := lobbyView(e, nil, nil, []*store.Record{lobbyRecord("x1", "H9", model.Error, "akka", nil, true, "Unknown device "+lobbyCarlID+" requests a connection to AKKA", time.Now())})
	if got := lobbyHTML(e, v, ""); strings.Contains(got, lobbyCarlID) || !strings.Contains(got, "Unknown device WSCARLL… requests a connection to AKKA") {
		t.Errorf("device ID in a message: %s", got)
	}
}

// The page passes the dashboard's Content-Security-Policy: only scripts and
// styles from /assets, no inline script or style, no style attribute, no
// event handler attribute. The assets exist.
func TestLobbyContentSecurityPolicy(t *testing.T) {
	e := newEnv(t, lobbySites+"access: {viewer: open}\n")
	lobbyHub(e)
	resp, page := e.get("/lobby?theme=dark")
	csp := resp.Header.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'self'", "script-src 'self'", "style-src 'self'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %s", csp, want)
		}
	}
	if strings.Contains(csp, "unsafe") || resp.Header.Get("X-Frame-Options") != "DENY" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("headers: %v", resp.Header)
	}
	if strings.Contains(page, "<style") || regexp.MustCompile(`(?i)\sstyle=`).MatchString(page) || regexp.MustCompile(`(?i)\son[a-z]+=`).MatchString(page) {
		t.Error("the lobby has an inline style or an event handler attribute")
	}
	scripts := regexp.MustCompile(`<script[^>]*>`).FindAllString(page, -1)
	if len(scripts) != 4 {
		t.Errorf("scripts: %v", scripts)
	}
	for _, s := range scripts {
		if !regexp.MustCompile(`^<script src="/assets/[a-z.-]+\.js\?v=[0-9a-f]{10}" defer>$`).MatchString(s) {
			t.Errorf("script %s is not a file from /assets", s)
		}
	}
	if strings.Count(page, "</script>") != len(scripts) || regexp.MustCompile(`<script[^>]*>[^<]`).MatchString(page) {
		t.Error("the lobby has an inline script")
	}
	refs := regexp.MustCompile(`(?:src|href)="([^"]*)"`).FindAllStringSubmatch(page, -1)
	for _, m := range refs {
		if !strings.HasPrefix(m[1], "/assets/") {
			t.Errorf("reference %q is not a file from /assets", m[1])
			continue
		}
		if r, _ := e.get(m[1]); r.StatusCode != http.StatusOK {
			t.Errorf("%s: %d", m[1], r.StatusCode)
		}
	}
	for _, want := range []string{`href="/assets/lobby.css?v=`, `src="/assets/htmx.min.js?v=`, `src="/assets/htmx-ext-sse.js?v=`, `src="/assets/app.js?v=`, `src="/assets/lobby.js?v=`,
		`"includeIndicatorStyles":false`} {
		if !strings.Contains(page, want) {
			t.Errorf("the lobby lacks %s", want)
		}
	}
}

// The lobby has one group for each site, in the order of the configuration,
// and a group "Other servers" for the servers without a site. Without any
// site there is one group without a heading. A disabled server keeps a grey
// card in its group.
func TestLobbyGroupsBySite(t *testing.T) {
	e := newEnv(t, lobbySites)
	lobbyHub(e)
	vm := e.web.lobby(e.eng.View(), "")
	if got, want := strings.Join(lobbyGroupNames(vm), " | "), "IVAR Office: AKKA, ODIN | Branch office: SOL"; got != want {
		t.Errorf("groups %q, want %q", got, want)
	}
	page := lobbyHTML(e, e.eng.View(), "")
	if i, j := strings.Index(page, "<h2>IVAR Office</h2>"), strings.Index(page, "<h2>Branch office</h2>"); i < 0 || j < i {
		t.Errorf("site headings: %s", page)
	}

	e2 := newEnv(t, `servers:
  - {name: AKKA, url: "https://a:1"}
  - {name: SOL, url: "https://b:1", site: "Branch office"}
  - {name: ODIN, url: "https://c:1", site: "IVAR Office"}
  - {name: SIRIUS, url: "https://d:1", site: "Branch office", disabled: true}
`)
	e2.hub.SetServers([]*model.Server{{ID: "akka", Name: "AKKA"}, {ID: "sol", Name: "SOL"}, {ID: "odin", Name: "ODIN"}})
	e2.eng.Evaluate()
	vm = e2.web.lobby(e2.eng.View(), "")
	if got, want := strings.Join(lobbyGroupNames(vm), " | "), "Branch office: SOL, SIRIUS | IVAR Office: ODIN | Other servers: AKKA"; got != want {
		t.Errorf("groups %q, want %q", got, want)
	}
	page = lobbyHTML(e2, e2.eng.View(), "")
	if !strings.Contains(page, "<h2>Other servers</h2>") || !strings.Contains(page, "<h3>SIRIUS</h3>") || !strings.Contains(page, "○ Disabled") ||
		!strings.Contains(page, "syncwatch does not poll this server.") {
		t.Errorf("other servers and the disabled server: %s", page)
	}

	e3 := newEnv(t, twoServers)
	vm = e3.web.lobby(e3.eng.View(), "")
	if got := strings.Join(lobbyGroupNames(vm), " | "); got != ": AKKA, SOL" {
		t.Errorf("without sites: %q", got)
	}
	page = lobbyHTML(e3, e3.eng.View(), "")
	sites := page[strings.Index(page, `<div class="sites">`):strings.Index(page, `<section class="lobby-findings">`)]
	if strings.Contains(sites, "<h2") || strings.Contains(page, "Other servers") || !strings.Contains(sites, "<h3>AKKA</h3>") {
		t.Errorf("without sites, the group has a heading: %s", sites)
	}
}

// ?site=<name> shows one site: its servers, the links that touch it and its
// findings. The live updates keep the site. A site that no server has gives
// a text with the sites.
func TestLobbySiteFilter(t *testing.T) {
	e := newEnv(t, lobbySites+"access: {viewer: open}\n")
	lobbyHub(e)
	_, page := e.get("/lobby?site=Branch+office")
	page = html.UnescapeString(page)
	if !strings.Contains(page, "<h2>Branch office</h2>") || !strings.Contains(page, "<h3>SOL</h3>") || strings.Contains(page, "<h3>AKKA</h3>") ||
		strings.Contains(page, "<h3>ODIN</h3>") || strings.Contains(page, "<h2>IVAR Office</h2>") {
		t.Errorf("?site=Branch office: %s", page)
	}
	if !strings.Contains(page, `hx-get="/frag/lobby?site=Branch+office"`) || !strings.Contains(page, "<title>Branch office · Lobby · syncwatch</title>") {
		t.Error("the page does not keep the site for the live updates")
	}
	if _, frag := e.get("/frag/lobby?site=Branch+office"); !strings.Contains(frag, "<h3>SOL</h3>") || strings.Contains(frag, "<h3>AKKA</h3>") {
		t.Errorf("fragment with the site: %s", frag)
	}
	if _, all := e.get("/lobby"); !strings.Contains(all, `hx-get="/frag/lobby"`) || !strings.Contains(all, "<h3>AKKA</h3>") || !strings.Contains(all, "<h3>SOL</h3>") {
		t.Error("the lobby without a site does not show all servers")
	}
	_, page = e.get("/lobby?site=Nowhere")
	page = html.UnescapeString(page)
	if !strings.Contains(page, `There is no server with the site "Nowhere".`) || !strings.Contains(page, "The sites are: IVAR Office, Branch office.") ||
		strings.Contains(page, "<h3>") {
		t.Errorf("unknown site: %s", page)
	}
	e2 := newEnv(t, twoServers+"access: {viewer: open}\n")
	if _, page := e2.get("/lobby?site=IVAR+Office"); !strings.Contains(page, "No server has a site.") {
		t.Errorf("unknown site without sites: %s", page)
	}

	// The links and the findings of the site, from a hand-made view: 3 sites.
	e3 := newEnv(t, `servers:
  - {name: AKKA, url: "https://a:1", site: "IVAR Office"}
  - {name: SOL, url: "https://b:1", site: "Branch office"}
  - {name: ODIN, url: "https://c:1", site: "IVAR Office"}
  - {name: DANI, url: "https://d:1", site: "Remote"}
timezone: Europe/Stockholm
`)
	lobbyClock(e3)
	at := time.Date(2026, 10, 11, 8, 12, 0, 0, e3.cfg.Get().Location())
	siteLinks := []model.SiteLink{
		{A: "IVAR Office", B: "Branch office", State: model.LinkConnected, Since: at, Servers: [][2]string{{"akka", "sol"}, {"sol", "odin"}}},
		{A: "IVAR Office", B: "Remote", State: model.LinkConnected, Since: at, Servers: [][2]string{{"akka", "dani"}}},
		{A: "Branch office", B: "Remote", State: model.LinkConnected, Since: at, Servers: [][2]string{{"sol", "dani"}}},
	}
	open := []*store.Record{
		lobbyRecord("r1", "H1", model.Error, "sol", nil, true, "Server SOL is unreachable", at),
		lobbyRecord("r2", "H2", model.Error, "akka", []string{"sol"}, true, "Servers AKKA and SOL are not connected to each other", at),
		lobbyRecord("r3", "H4", model.Error, "odin", nil, true, "Folder archive on ODIN cannot sync", at),
		lobbyRecord("r4", "X1", model.Error, "", nil, false, "A finding of no server", at),
	}
	vm := e3.web.lobby(lobbyView(e3, nil, siteLinks, open), "Remote")
	var texts []string
	for _, l := range vm.Links {
		texts = append(texts, l.Text)
	}
	if got, want := strings.Join(texts, " | "), "IVAR Office ↔ Remote: connected from 08:12 | Branch office ↔ Remote: connected from 08:12"; got != want {
		t.Errorf("links of Remote: %q, want %q", got, want)
	}
	vm = e3.web.lobby(lobbyView(e3, nil, siteLinks, open), "Branch office")
	var msgs []string
	for _, f := range vm.Findings {
		msgs = append(msgs, f.Message)
	}
	if got, want := strings.Join(msgs, " | "), "Server SOL is unreachable | Servers AKKA and SOL are not connected to each other | A finding of no server"; got != want {
		t.Errorf("findings of Branch office: %q, want %q", got, want)
	}
	if len(vm.Links) != 2 || len(vm.Groups) != 1 || vm.Groups[0].Name != "Branch office" {
		t.Errorf("Branch office: links %v, groups %v", vm.Links, lobbyGroupNames(vm))
	}
}

// The link rows: between the sites with 2 or more sites, else
// between the servers; "from" with the clock time today, with the date on
// another day; the reason when the state is not known.
func TestLobbyLinkRows(t *testing.T) {
	e := newEnv(t, lobbySites)
	lobbyHub(e)
	lobbyClock(e)
	loc := e.cfg.Get().Location()
	today := time.Date(2026, 10, 11, 8, 12, 0, 0, loc)
	yesterday := time.Date(2026, 10, 10, 21, 4, 0, 0, loc)
	pairs := [][2]string{{"akka", "sol"}, {"sol", "odin"}}
	for _, tc := range []struct {
		link model.SiteLink
		want string
	}{
		{model.SiteLink{A: "IVAR Office", B: "Branch office", State: model.LinkConnected, Since: today, Servers: pairs}, "IVAR Office ↔ Branch office: connected from 08:12"},
		{model.SiteLink{A: "IVAR Office", B: "Branch office", State: model.LinkDisconnected, Since: yesterday, Servers: pairs}, "IVAR Office ↔ Branch office: not connected from 2026-10-10 21:04"},
		{model.SiteLink{A: "IVAR Office", B: "Branch office", State: model.LinkDisconnected, Servers: pairs}, "IVAR Office ↔ Branch office: not connected"},
		{model.SiteLink{A: "IVAR Office", B: "Branch office", State: model.LinkUnreachable, Since: today, Servers: pairs}, "IVAR Office ↔ Branch office: state unknown: SOL is unreachable"},
	} {
		v := lobbyView(e, []model.ServerLink{{A: "akka", B: "odin", State: model.LinkConnected, Since: today}}, []model.SiteLink{tc.link}, nil)
		vm := e.web.lobby(v, "")
		if len(vm.Links) != 1 || vm.Links[0].Text != tc.want || vm.Links[0].State != tc.link.State {
			t.Errorf("site link %v: rows %+v, want %q", tc.link.State, vm.Links, tc.want)
			continue
		}
		if page := lobbyHTML(e, v, ""); !strings.Contains(page, `<li class="link-`+tc.link.State+`">`) || !strings.Contains(page, tc.want) {
			t.Errorf("site link %v: not on the page", tc.link.State)
		}
	}

	// Fewer than 2 sites: the rows of the server pairs, with the reason of a
	// server whose state is not known.
	e2 := newEnv(t, twoServers+"timezone: Europe/Stockholm\n")
	lobbyClock(e2)
	for _, tc := range []struct {
		status model.ServerStatus
		loaded bool
		want   string
	}{
		{model.StatusDown, true, "AKKA ↔ SOL: state unknown: SOL is unreachable"},
		{model.StatusCertChanged, true, "AKKA ↔ SOL: state unknown: the certificate of SOL changed"},
		{model.StatusUp, false, "AKKA ↔ SOL: state unknown: the full check of SOL is not complete"},
		{model.StatusUnknown, false, "AKKA ↔ SOL: state unknown: syncwatch has no contact with SOL"},
	} {
		e2.hub.Update("akka", func(s *model.Server) { s.Status, s.Loaded = model.StatusUp, true })
		e2.hub.Update("sol", func(s *model.Server) { s.Status, s.Loaded = tc.status, tc.loaded })
		e2.eng.Evaluate()
		links := []model.ServerLink{{A: "akka", B: "sol", State: model.LinkUnreachable}}
		vm := e2.web.lobby(lobbyView(e2, links, nil, nil), "")
		if len(vm.Links) != 1 || vm.Links[0].Text != tc.want {
			t.Errorf("SOL %s: rows %+v, want %q", tc.status, vm.Links, tc.want)
		}
	}
	at := time.Date(2026, 10, 11, 8, 12, 0, 0, e2.cfg.Get().Location())
	vm := e2.web.lobby(lobbyView(e2, []model.ServerLink{{A: "akka", B: "sol", State: model.LinkConnected, Since: at}}, nil, nil), "")
	if len(vm.Links) != 1 || vm.Links[0].Text != "AKKA ↔ SOL: connected from 08:12" {
		t.Errorf("server pair: %+v", vm.Links)
	}
	if vm := e2.web.lobby(lobbyView(e2, nil, nil, nil), ""); len(vm.Links) != 0 || strings.Contains(lobbyHTML(e2, lobbyView(e2, nil, nil, nil), ""), `class="links"`) {
		t.Error("rows without links")
	}
}

// A server card: the status label and icon, the folder summary (the last
// known state in grey when the state is not current, nothing for a server
// that syncwatch never contacted) and the open ERROR and WARNING findings of
// the server, without the snoozed ones.
func TestLobbyServerCards(t *testing.T) {
	e := newEnv(t, lobbySites)
	lobbyHub(e)
	now := lobbyClock(e)
	e.hub.Update("sol", func(s *model.Server) { s.LastContact = now.Add(-time.Hour) })
	e.hub.Update("odin", func(s *model.Server) { s.Status, s.Loaded, s.LastContact = model.StatusUnknown, false, time.Time{} })
	e.eng.Evaluate()
	open := []*store.Record{
		lobbyRecord("r1", "H1", model.Error, "sol", nil, true, "Server SOL is unreachable", now.Add(-3*time.Minute)),
		lobbyRecord("r2", "H2", model.Error, "akka", []string{"sol"}, true, "Servers AKKA and SOL are not connected to each other", now),
		lobbyRecord("r3", "H11", model.Warn, "akka", nil, false, "Device LAPTOP-DANI is connected to AKKA through a relay, which is slow", now),
		lobbyRecord("r4", "H7", model.Warn, "", []string{"akka", "odin"}, false, "PC WS-CARL is offline for 9 days", now),
		lobbyRecord("r5", "H4", model.Error, "akka", nil, true, "Folder finals on AKKA cannot sync", now),
	}
	v := lobbyView(e, nil, nil, open)
	v.Snoozes = map[string]store.Snooze{"r5": {FindingID: "r5", Kind: store.SnoozeKind, Until: now.Add(time.Hour)}}
	vm := e.web.lobby(v, "")
	cards := map[string]lobbyServer{}
	for _, g := range vm.Groups {
		for _, s := range g.Servers {
			cards[s.Name] = s
		}
	}
	if a := cards["AKKA"]; a.StatusLabel != "Up" || a.StatusIcon != "✔" || a.Stale || a.Folders.Total != 2 || a.Folders.Idle != 1 || a.Folders.Syncing != 1 ||
		a.Errors != 1 || a.Warnings != 2 {
		t.Errorf("AKKA: %+v", a)
	}
	if s := cards["SOL"]; s.StatusLabel != "Unreachable" || !s.Stale || s.NoContact || s.Errors != 2 || s.Warnings != 0 {
		t.Errorf("SOL: %+v", s)
	}
	if o := cards["ODIN"]; !o.NoContact || o.Errors != 0 || o.Warnings != 1 {
		t.Errorf("ODIN: %+v", o)
	}
	page := lobbyHTML(e, v, "")
	for _, want := range []string{
		`<span class="badge st-ok">✔ Up</span>`, `<span class="badge st-err">✖ Unreachable</span>`,
		`<dd>2: <span class="c-ok">✔ 1 idle</span> · <span class="c-sync">⟳ 1 syncing</span></dd>`,
		`<dd class="muted">2 folders · last known <span class="nowrap">13:00</span>: 2 idle</dd>`,
		"<dt>Folders</dt><dd>—</dd>",
		`<span class="c-err">ERROR 1</span> · <span class="c-warn">WARNING 2</span>`,
		`<span class="c-err">ERROR 2</span> · <span class="muted">WARNING 0</span>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the cards lack %s", want)
		}
	}
	if strings.Contains(page, "no route to host") {
		t.Error("the card shows the connection error of the server")
	}
}

// Each site shows the PCs that are connected to its servers now. A server
// that is not up or did not finish a full check makes the count a minimum,
// or unknown.
func TestLobbyPCsPerSite(t *testing.T) {
	e := newEnv(t, lobbySites)
	lobbyHub(e)
	pcs := func() map[string]string {
		out := map[string]string{}
		for _, g := range e.web.lobby(e.eng.View(), "").Groups {
			out[g.Name] = g.PCs
		}
		return out
	}
	// WS-ANNA on AKKA and ODIN, LAPTOP-DANI on AKKA; SOL is a server, not a PC.
	if got := pcs(); got["IVAR Office"] != "2" || got["Branch office"] != "unknown" {
		t.Errorf("PCs %v", got)
	}
	if page := lobbyHTML(e, e.eng.View(), ""); !strings.Contains(page, "PCs connected now: <strong>2</strong>") || !strings.Contains(page, "PCs connected now: <strong>unknown</strong>") {
		t.Error("the page lacks the PCs of the sites")
	}
	e.hub.Update("odin", func(s *model.Server) { s.Status = model.StatusDown })
	e.eng.Evaluate()
	if got := pcs(); got["IVAR Office"] != "2 or more" {
		t.Errorf("ODIN down: %v", got)
	}
	e.hub.Update("sol", func(s *model.Server) { s.Status = model.StatusUp })
	e.eng.Evaluate()
	if got := pcs(); got["Branch office"] != "1" {
		t.Errorf("SOL up: %v", got)
	}
}

// The list has the open urgent and ERROR findings that are not snoozed or
// ignored (the message, the check, "open X"), at most 10, then "…and N
// more".
func TestLobbyFindingsList(t *testing.T) {
	e := newEnv(t, lobbySites)
	lobbyHub(e)
	now := lobbyClock(e)
	open := []*store.Record{
		lobbyRecord("r1", "H1", model.Error, "sol", nil, true, "Server SOL is unreachable: no route to host", now.Add(-3*time.Minute)),
		lobbyRecord("r2", "H11", model.Warn, "akka", nil, false, "Device LAPTOP-DANI is connected to AKKA through a relay, which is slow", now),
		lobbyRecord("r3", "H7", model.Warn, "", []string{"akka"}, true, "PC WS-CARL is offline for 9 days", now.Add(-50*time.Hour)),
		lobbyRecord("r4", "H4", model.Error, "akka", nil, true, "Folder finals on AKKA cannot sync", now),
		lobbyRecord("r5", "S3", model.Error, "odin", nil, true, "Project folder X is nested inside project folder Y", now),
	}
	open[0].Stale = true
	v := lobbyView(e, nil, nil, open)
	v.Snoozes = map[string]store.Snooze{"r4": {FindingID: "r4", Kind: store.IgnoreKind, Reason: "known"}}
	vm := e.web.lobby(v, "")
	var got []string
	for _, f := range vm.Findings {
		got = append(got, f.Checks+" "+f.Message+" · open "+f.OpenFor)
	}
	want := []string{"H1 Server SOL is unreachable: no route to host · open 3 minutes", "H7 PC WS-CARL is offline for 9 days · open 2 days",
		"S3 Project folder X is nested inside project folder Y · open 0 seconds"}
	if strings.Join(got, " | ") != strings.Join(want, " | ") || vm.More != 0 {
		t.Errorf("findings %q, want %q", got, want)
	}
	page := lobbyHTML(e, v, "")
	for _, w := range []string{`<span class="sev sev-error">🔴 ERROR</span> <span class="chk">H1</span> <span class="tag urgent">urgent</span> <span class="tag stale">stale</span> <span class="msg">Server SOL is unreachable: no route to host</span> <span class="muted nowrap">· open 3 minutes</span>`,
		`<span class="sev sev-warn">🟠 WARNING</span> <span class="chk">H7</span> <span class="tag urgent">urgent</span>`} {
		if !strings.Contains(page, w) {
			t.Errorf("the list lacks %s", w)
		}
	}
	if strings.Contains(page, "LAPTOP-DANI") || strings.Contains(page, "finals on AKKA") || strings.Contains(page, "…and") {
		t.Error("the list has a finding that is not urgent, an ignored finding or a cut line")
	}

	for i := 0; i < 12; i++ {
		open = append(open, lobbyRecord("e"+string(rune('a'+i)), "H4", model.Error, "akka", nil, false, "Folder f"+string(rune('a'+i))+" on AKKA cannot sync", now))
	}
	vm = e.web.lobby(lobbyView(e, nil, nil, open), "")
	if len(vm.Findings) != lobbyMaxFindings || vm.More != 6 {
		t.Errorf("%d findings and %d more, want 10 and 6", len(vm.Findings), vm.More)
	}
	if page := lobbyHTML(e, lobbyView(e, nil, nil, open), ""); !strings.Contains(page, "…and 6 more") {
		t.Error("no cut line")
	}
	if page := lobbyHTML(e, lobbyView(e, nil, nil, []*store.Record{}), ""); !strings.Contains(page, "✔ There are no open urgent or ERROR findings.") {
		t.Error("no text for an empty list")
	}

	// A long message is cut after 160 characters; a device ID in it is
	// shortened first, thus no part of it shows.
	long := "Folder " + strings.Repeat("ä", 140) + " by " + lobbyAnnaID + " cannot sync" // the device ID starts at character 151
	vm = e.web.lobby(lobbyView(e, nil, nil, []*store.Record{lobbyRecord("l1", "H4", model.Error, "akka", nil, true, long, now)}), "")
	if m := vm.Findings[0].Message; len([]rune(m)) > lobbyMaxMessage+1 || !strings.HasSuffix(m, "…") || !strings.HasPrefix(m, "Folder ääää") ||
		!strings.Contains(m, " by WSANNAA…") || strings.Contains(m, "WSANNAA-") || strings.Contains(m, "cannot sync") {
		t.Errorf("long message: %q", m)
	}
	short := "Device " + lobbyAnnaID + " is paused on AKKA"
	if vm := e.web.lobby(lobbyView(e, nil, nil, []*store.Record{lobbyRecord("l2", "H6", model.Error, "akka", nil, true, short, now)}), ""); vm.Findings[0].Message != "Device WSANNAA… is paused on AKKA" {
		t.Errorf("short message: %q", vm.Findings[0].Message)
	}
}

// The page refreshes through the event stream (and each minute, for the
// times on a screen that stays open), shows the connection-lost banner of
// app.js, the notification failure and the theme of ?theme=.
func TestLobbyLiveUpdatesAndTheme(t *testing.T) {
	e := newEnv(t, lobbySites+"access: {viewer: open}\n")
	_, page := e.get("/lobby")
	for _, want := range []string{`<html lang="en">`, `<body class="lobby" hx-ext="sse" sse-connect="/events" data-tz-offset="`,
		`hx-get="/frag/lobby" hx-trigger="every 60s, sse:update" hx-swap="outerHTML"`,
		`<div id="sw-conn" class="conn-lost" role="status" hidden>✖ <strong>No connection to syncwatch</strong> from <span class="sw-conn-since"></span>. The page shows old data.</div>`,
		`<span class="live-on">Real-time view</span><span class="live-off">No connection to syncwatch</span> · last change`} {
		if !strings.Contains(page, want) {
			t.Errorf("the lobby lacks %s", want)
		}
	}
	// app.js refreshes the sections with [hx-trigger~='sse:update']: the
	// word must stand alone in the attribute.
	if !regexp.MustCompile(`hx-trigger="(?:[^"]*\s)?sse:update(?:\s[^"]*)?"`).MatchString(page) {
		t.Error("app.js cannot find the lobby section after a lost connection")
	}
	_, js := e.get("/assets/app.js")
	for _, want := range []string{`"sw-conn"`, `".sw-conn-since"`, `"sw-offline"`, `[hx-trigger~='sse:update']`} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %s, which the lobby uses", want)
		}
	}
	_, ljs := e.get("/assets/lobby.js")
	for _, want := range []string{`"htmx:beforeOnLoad"`, "xhr.status !== 401", `"/login?next=" + encodeURIComponent(`} {
		if !strings.Contains(ljs, want) {
			t.Errorf("lobby.js lacks %s", want)
		}
	}
	_, css := e.get("/assets/lobby.css")
	for _, want := range []string{"@media (prefers-color-scheme: dark)", ":root:not(.theme-light)", ":root.theme-dark", ".sw-offline .live-on", "unicode-bidi: isolate", "overflow-wrap: anywhere"} {
		if !strings.Contains(css, want) {
			t.Errorf("lobby.css lacks %s", want)
		}
	}
	for theme, want := range map[string]string{"dark": `<html lang="en" class="theme-dark">`, "light": `<html lang="en" class="theme-light">`, "<b>": `<html lang="en">`} {
		if _, page := e.get("/lobby?theme=" + url.QueryEscape(theme)); !strings.Contains(page, want) {
			t.Errorf("?theme=%s: want %s", theme, want)
		}
	}
	if strings.Contains(page, "Notifications fail") {
		t.Error("a notification failure while notifications work")
	}
	e.setDelivery(notify.Delivery{Failing: true, Since: time.Now(), Error: "discord: HTTP 404: Unknown Webhook"})
	if _, frag := e.get("/frag/lobby"); !strings.Contains(frag, "✖ <strong>Notifications fail</strong> from ") || !strings.Contains(frag, ". syncwatch tries again automatically.") ||
		strings.Contains(frag, "Unknown Webhook") {
		t.Errorf("notification failure: %s", frag)
	}
}
