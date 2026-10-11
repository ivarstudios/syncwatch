package web

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/engine"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/store"
)

// r2vFullID matches a complete Syncthing device ID (8 groups of 7).
var r2vFullID = regexp.MustCompile(`[A-Z2-7]{7}(-[A-Z2-7]{7}){7}`)

const (
	r2vIDAkka    = "AKKAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA"
	r2vIDAnna    = "PCANNAA-BBBBBBB-CCCCCCC-DDDDDDD-EEEEEEE-FFFFFFF-GGGGGGG-HHHHHHH"
	r2vIDUnknown = "XUNKNWN-2222222-3333333-4444444-5555555-6666666-7777777-QQQQQQQ"
)

// r2vViewerGet fetches a page without a session: with open access, as a viewer.
func (e *env) r2vViewerGet(path string) string {
	e.t.Helper()
	resp, err := http.Get(e.srv.URL + path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("%s as a viewer: %d", path, resp.StatusCode)
	}
	return string(b)
}

// W-048 (c), C12: viewers (also with open access) see device IDs only in
// their short form, and no "Open in Syncthing" links. Administrators see
// everything as before.
func TestViewersSeeShortDeviceIDs(t *testing.T) {
	e := adminEnv(t, twoServers+"access: {viewer: open}\n")
	now := time.Now()
	e.hub.Update("akka", func(s *model.Server) {
		s.Status, s.Loaded, s.MyID, s.LastContact, s.GUIURL = model.StatusUp, true, r2vIDAkka, now, "https://akka.invalid:8384"
		s.Devices = map[string]*model.Device{r2vIDAkka: {ID: r2vIDAkka, Name: "AKKA"}, r2vIDAnna: {ID: r2vIDAnna, Name: "ANNA-PC"}}
		s.Connections = map[string]*model.Connection{r2vIDAnna: {Connected: true, StartedAt: now}}
		s.Folders = map[string]*model.Folder{"f1": {ID: "f1", Label: "projects", State: "idle", SharedWith: []string{r2vIDAnna},
			Completion: map[string]*model.Completion{r2vIDAnna: {Percent: 100, RemoteState: "valid"}}}}
		s.PendingDevices = []model.PendingDevice{{DeviceID: r2vIDUnknown, Address: "10.0.0.9:22000", Time: now}}
		s.PendingFolders = []model.PendingFolder{{FolderID: "f2", Label: "offered", OfferedBy: r2vIDAnna, Time: now}}
	})
	e.eng.Evaluate()

	// The administrator: the full IDs in the details and the column titles,
	// and the links to Syncthing.
	_, problems := e.get("/problems")
	for _, want := range []string{r2vIDUnknown, r2vIDAnna, "Open in Syncthing"} {
		if !strings.Contains(problems, want) {
			t.Errorf("administrator, /problems: no %q", want)
		}
	}
	if _, grid := e.get("/grid"); !strings.Contains(grid, `title="`+r2vIDAnna+`"`) || !strings.Contains(grid, `data-key="dev:`+r2vIDAnna+`"`) {
		t.Error("administrator, /grid: no full device ID in the column title")
	}
	if _, ov := e.get("/"); !strings.Contains(ov, `href="https://akka.invalid:8384"`) || !strings.Contains(ov, "Open in Syncthing") {
		t.Error("administrator, /: no link to Syncthing on the AKKA tile")
	}

	// A viewer: no complete device ID and no link to Syncthing on any page or
	// live update.
	for _, p := range []string{"/", "/problems", "/grid", "/frag/overview", "/frag/problems", "/frag/grid"} {
		body := e.r2vViewerGet(p)
		if m := r2vFullID.FindString(body); m != "" {
			t.Errorf("viewer, %s: full device ID %s", p, m)
		}
		if strings.Contains(body, "Open in Syncthing") || strings.Contains(body, "akka.invalid:8384") || strings.Contains(body, `href="https://b:1"`) {
			t.Errorf("viewer, %s: a link to Syncthing", p)
		}
	}
	vp := e.r2vViewerGet("/problems")
	for _, want := range []string{"<th>Device ID</th><td class=\"mono\">XUNKNWN…</td>", "<th>Offered by</th><td class=\"mono\">PCANNAA…</td>"} {
		if !strings.Contains(vp, want) {
			t.Errorf("viewer, /problems: no %q", want)
		}
	}
	if vg := e.r2vViewerGet("/grid"); !strings.Contains(vg, `title="PCANNAA…"`) || !strings.Contains(vg, `title="AKKAAAA…"`) {
		t.Error("viewer, /grid: the column titles do not show the short device IDs")
	}

	// The view models: only the administrator's form has the full values.
	vm := e.web.problems(e.eng.View(), problemFilter{}, RoleViewer)
	for _, r := range vm.Rows {
		if r.Link != "" {
			t.Errorf("viewer row %s has the link %q", r.Check, r.Link)
		}
	}
	if got := shortDeviceID(r2vIDAnna); got != "PCANNAA…" {
		t.Errorf("shortDeviceID = %q", got)
	}
	if got := shortDeviceID("PCANNAA"); got != "PCANNAA" {
		t.Errorf("shortDeviceID of a short value = %q, want it unchanged", got)
	}
}

// r2vSites has AKKA and ODEN at "IVAR Office" and SOL at "Branch office".
const r2vSites = `servers: [{name: AKKA, url: "https://a:1", site: "IVAR Office"}, {name: SOL, url: "https://b:1", site: "Branch office"}, {name: ODEN, url: "https://c:1", site: "IVAR Office"}]
api: {token: swt_test_token}
timezone: Europe/Stockholm
`

// r2vNow is 12:00 in Stockholm.
var r2vNow = time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC)

// r2vView is a hand-made view: AKKA up, SOL unreachable, ODEN up without a
// complete full check.
func r2vView(links []model.ServerLink, siteLinks []model.SiteLink, open []*store.Record) *engine.View {
	return &engine.View{Snapshot: &model.Snapshot{At: r2vNow, Servers: []*model.Server{
		{ID: "akka", Name: "AKKA", Status: model.StatusUp, Loaded: true},
		{ID: "sol", Name: "SOL", Status: model.StatusDown},
		{ID: "oden", Name: "ODEN", Status: model.StatusUp},
	}, Links: links, SiteLinks: siteLinks}, Open: open}
}

// r2vRender renders the Overview body of a view model for the administrator.
func r2vRender(t *testing.T, e *env, vm overviewVM) string {
	t.Helper()
	var b bytes.Buffer
	if err := e.web.tmpl.ExecuteTemplate(&b, "overview-body", pageData{Role: RoleAdmin, Data: map[string]any{"VM": vm}}); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// r2vRow is a Connections row as the page shows it, with its sites.
type r2vRow struct{ text, class, sites string }

func r2vSameRows(t *testing.T, what string, got []linkRow, want []r2vRow) {
	t.Helper()
	var g []r2vRow
	for _, r := range got {
		g = append(g, r2vRow{r.A + " ↔ " + r.B + ": " + r.State, r.Class, strings.Join(r.Sites, "|")})
	}
	if len(g) != len(want) {
		t.Fatalf("%s: %d rows %q, want %d", what, len(g), g, len(want))
	}
	for i := range want {
		if g[i] != want[i] {
			t.Errorf("%s row %d:\n got %+v\nwant %+v", what, i, g[i], want[i])
		}
	}
}

// W-057 (a), C11: the Connections section of the Overview has one row for
// each site link (2 or more sites), else one row for each server link, from
// the links of the engine (here a hand-made view). A time of today shows
// without the date. Without links there is no section.
func TestConnectionRowsFromLinks(t *testing.T) {
	e := adminEnv(t, r2vSites)
	e.web.now = func() time.Time { return r2vNow }
	at0812 := time.Date(2026, 10, 11, 6, 12, 0, 0, time.UTC)    // 08:12 in Stockholm
	yesterday := time.Date(2026, 10, 10, 19, 4, 0, 0, time.UTC) // 21:04 in Stockholm
	links := []model.ServerLink{
		{A: "akka", B: "sol", State: model.LinkConnected, Since: at0812},
		{A: "akka", B: "oden", State: model.LinkDisconnected, Since: yesterday},
		{A: "sol", B: "oden", State: model.LinkUnreachable},
		{A: "akka", B: "oden", State: model.LinkConnected}, // the start is not known
	}
	r2vSameRows(t, "server links", e.web.overview(r2vView(links, nil, nil)).Links, []r2vRow{
		{"AKKA ↔ SOL: connected from 08:12", "ok", "IVAR Office|Branch office"},
		{"AKKA ↔ ODEN: not connected from 2026-10-10 21:04", "err", "IVAR Office"},
		{"SOL ↔ ODEN: state unknown: SOL is unreachable and the full check of ODEN is not complete", "off", "Branch office|IVAR Office"},
		{"AKKA ↔ ODEN: connected", "ok", "IVAR Office"},
	})
	siteLinks := []model.SiteLink{
		{A: "IVAR Office", B: "Branch office", State: model.LinkConnected, Since: at0812, Servers: [][2]string{{"akka", "sol"}, {"sol", "oden"}}},
		{A: "IVAR Office", B: "Branch office", State: model.LinkUnreachable, Servers: [][2]string{{"akka", "sol"}, {"oden", "sol"}}},
		{A: "IVAR Office", B: "Branch office", State: model.LinkDisconnected, Since: yesterday, Servers: [][2]string{{"akka", "sol"}}},
	}
	vm := e.web.overview(r2vView(links, siteLinks, nil))
	r2vSameRows(t, "site links", vm.Links, []r2vRow{
		{"IVAR Office ↔ Branch office: connected from 08:12", "ok", "IVAR Office|Branch office"},
		{"IVAR Office ↔ Branch office: state unknown: SOL is unreachable and the full check of ODEN is not complete", "off", "IVAR Office|Branch office"},
		{"IVAR Office ↔ Branch office: not connected from 2026-10-10 21:04", "err", "IVAR Office|Branch office"},
	})

	// The section is between the server tiles and the findings, green, red
	// or grey, with the sites of each row for the site filter.
	page := r2vRender(t, e, vm)
	i, j, k := strings.Index(page, `aria-label="Servers"`), strings.Index(page, "<h2>Connections</h2>"), strings.Index(page, "Most important open findings")
	if i < 0 || j < i || k < j {
		t.Errorf("Connections at %d, want between the tiles (%d) and the findings (%d)", j, i, k)
	}
	for _, want := range []string{
		"<li class=\"lk-ok\" data-sites=\"IVAR Office\nBranch office\"><span class=\"lk-state\" aria-hidden=\"true\">✔</span> <span class=\"lk-name\">IVAR Office</span> ↔ <span class=\"lk-name\">Branch office</span>: <span class=\"lk-state\">connected from 08:12</span></li>",
		`<li class="lk-off" data-sites=`, `<li class="lk-err" data-sites=`, `<span class="lk-state">not connected from 2026-10-10 21:04</span>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("the Overview body lacks %q", want)
		}
	}

	// No links (1 server, or links that the engine did not compute): no section.
	vm = e.web.overview(r2vView(nil, nil, nil))
	if vm.Links != nil || strings.Contains(r2vRender(t, e, vm), "Connections") {
		t.Errorf("no links: rows %v, or a Connections section", vm.Links)
	}
	if _, ov := e.get("/"); strings.Contains(ov, "Connections") {
		t.Error("a Connections section without links")
	}
}

// W-057 (a), C11: with 2 or more sites the Overview and the Folders page have
// the site filter, and each element of a server has its site: the tiles (with
// the site name), the finding rows, the server columns and cells of the grid
// and the device lists of the narrow view. A finding between servers has the
// sites of all its servers. The PCs have no site.
func TestSiteFilterMarkup(t *testing.T) {
	e := adminEnv(t, r2vSites)
	now := time.Now()
	e.hub.Update("akka", func(s *model.Server) {
		s.Status, s.Loaded, s.MyID, s.LastContact, s.Version = model.StatusUp, true, "AKKAAAA-1111111", now, "v2.1.5"
		s.Devices = map[string]*model.Device{"PCANNAA-3333333": {ID: "PCANNAA-3333333", Name: "ANNA-PC"}}
		s.Folders = map[string]*model.Folder{"f1": {ID: "f1", Label: "projects", State: "idle", SharedWith: []string{"PCANNAA-3333333"}}}
	})
	e.hub.Update("sol", func(s *model.Server) {
		s.Status, s.Loaded, s.MyID, s.LastContact, s.Version = model.StatusUp, true, "SOLLLLL-2222222", now, "v2.1.6"
		s.Folders = map[string]*model.Folder{"f1": {ID: "f1", Label: "projects", State: "error", Error: "folder marker missing"}}
	})
	e.eng.Evaluate()

	_, ov := e.get("/")
	for _, want := range []string{
		`<select id="site-filter" autocomplete="off">`, `<option value="">All sites</option>`,
		`<option value="IVAR Office">IVAR Office</option>`, `<option value="Branch office">Branch office</option>`,
		`<article class="tile st-ok" data-site="IVAR Office">`, `<article class="tile st-ok" data-site="Branch office">`,
		`<article class="tile st-err" data-site="IVAR Office">`, `<p class="tile-site">Branch office</p>`,
		`data-limit="8"`, `<p class="c-ok site-empty" hidden>`, `data-day="`,
	} {
		if !strings.Contains(ov, want) {
			t.Errorf("Overview lacks %q", want)
		}
	}
	rowSites := map[string]string{} // check → the site attribute of its row
	for _, m := range regexp.MustCompile(`<li class="sev-row-\w+[^"]*"([^>]*)>\s*<span class="sev[^>]*>[^<]*</span> <span class="chk">(\w+)</span>`).FindAllStringSubmatch(ov, -1) {
		rowSites[m[2]] = strings.TrimSpace(m[1])
	}
	for check, want := range map[string]string{
		"H1": `data-site="IVAR Office"`,                   // ODEN is unreachable
		"H4": `data-site="Branch office"`,                 // the folder error on SOL
		"H8": "data-sites=\"IVAR Office\nBranch office\"", // AKKA and SOL run different versions
	} {
		if got, ok := rowSites[check]; !ok || got != want {
			t.Errorf("Overview row %s: %q (found %v), want %q", check, got, ok, want)
		}
	}

	_, grid := e.get("/grid")
	for _, want := range []string{
		`<select id="site-filter" autocomplete="off">`,
		`title="SOLLLLL-2222222" data-site="Branch office">SOL<span class="muted"> (server)</span></th>`,
		`<td data-site="Branch office"><span class="cell c-err"`, `<td data-site="IVAR Office"><span class="cell c-ok"`,
		`data-key="dev:SOLLLLL-2222222" data-site="Branch office"`, `data-key="dev:AKKAAAA-1111111" data-site="IVAR Office"`,
	} {
		if !strings.Contains(grid, want) {
			t.Errorf("Folders lacks %q", want)
		}
	}
	if !strings.Contains(grid, `title="PCANNAA-3333333">ANNA-PC</th>`) || !strings.Contains(grid, `data-key="dev:PCANNAA-3333333" >`) {
		t.Error("Folders: the PC column or the PC list has a site")
	}

	// With fewer than 2 sites, no filter and no site attributes; the tile
	// still names its site.
	e1 := adminEnv(t, `servers: [{name: AKKA, url: "https://a:1", site: "IVAR Office"}, {name: SOL, url: "https://b:1"}]`)
	for _, p := range []string{"/", "/grid", "/frag/overview", "/frag/grid"} {
		if _, body := e1.get(p); strings.Contains(body, "site-filter") || strings.Contains(body, "data-site") || strings.Contains(body, "data-limit") {
			t.Errorf("%s with 1 site: a site filter or site attributes", p)
		}
	}
	if _, body := e1.get("/"); !strings.Contains(body, `<p class="tile-site">IVAR Office</p>`) {
		t.Error("the tile does not show the site")
	}
}

// W-057 (a): the Overview list shows 8 findings. With the site filter, each
// site gets its own first 8: the rows past the 8 of all sites are in the
// page, hidden until the filter selects one of their sites.
func TestOverviewListForEachSite(t *testing.T) {
	e := adminEnv(t, r2vSites)
	e.web.now = func() time.Time { return r2vNow }
	rec := func(id, server string, related ...string) *store.Record {
		return &store.Record{Finding: model.Finding{ID: id, Check: "H4", Severity: model.Error, Server: server, Related: related, Message: "finding " + id}, OpenedAt: r2vNow}
	}
	var open []*store.Record
	for i := 0; i < 10; i++ {
		open = append(open, rec(fmt.Sprintf("akka%d", i), "akka"))
	}
	open = append(open, rec("sol0", "sol"), rec("pc0", "", "sol"), rec("x0", ""))
	vm := e.web.overview(r2vView(nil, nil, open))
	var got []string
	for _, r := range vm.Top {
		got = append(got, fmt.Sprintf("%s over=%v sites=%s", r.ID, r.Over, strings.Join(r.Sites, "|")))
	}
	want := []string{
		"akka0 over=false sites=IVAR Office", "akka1 over=false sites=IVAR Office", "akka2 over=false sites=IVAR Office",
		"akka3 over=false sites=IVAR Office", "akka4 over=false sites=IVAR Office", "akka5 over=false sites=IVAR Office",
		"akka6 over=false sites=IVAR Office", "akka7 over=false sites=IVAR Office",
		"sol0 over=true sites=Branch office", "pc0 over=true sites=Branch office", "x0 over=true sites=",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("Overview list:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	page := r2vRender(t, e, vm)
	for _, want := range []string{`<li class="sev-row-error over-limit" data-site="Branch office">`, `<li class="sev-row-error over-limit">`, `<li class="sev-row-error" data-site="IVAR Office">`} {
		if !strings.Contains(page, want) {
			t.Errorf("the list lacks %q", want)
		}
	}
	// Without sites, the list is the first 8, as before.
	e1 := adminEnv(t, twoServers)
	e1.web.now = e.web.now
	if vm := e1.web.overview(r2vView(nil, nil, open)); len(vm.Top) != 8 || vm.Top[7].ID != "akka7" || vm.Top[7].Over || vm.Sites != nil {
		t.Errorf("without sites: %d rows, sites %v", len(vm.Top), vm.Sites)
	}
}

// The site filter runs in the browser (checked live, see docs/changes/r2-6.md);
// this pins the shipped script and styles: the filter, its key in the browser
// storage, the list limit, the call at load and after each live update, and
// the new fetch of sections with times of today after midnight.
func TestSiteFilterShipped(t *testing.T) {
	e := newEnv(t, twoServers)
	_, js := e.get("/assets/app.js")
	for _, want := range []string{`localStorage.getItem("sw-site")`, `localStorage.setItem("sw-site", sel.value)`, `getElementById("site-filter")`,
		`"[data-site], [data-sites]"`, `"site-hidden"`, `"over-limit"`, `"site-empty"`, `"[data-limit]"`, `"[data-day]"`, `split("\n")`} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %s", want)
		}
	}
	if i := strings.Index(js, `"htmx:afterSwap"`); i < 0 || !strings.Contains(js[i:i+200], "applySiteFilter();") {
		t.Error("app.js does not apply the site filter after a live update")
	}
	if i := strings.Index(js, `"DOMContentLoaded"`); i < 0 || !strings.Contains(js[i:], "initSiteFilter();\n    applySiteFilter();") {
		t.Error("app.js does not apply the site filter at load")
	}
	_, css := e.get("/assets/app.css")
	for _, want := range []string{".site-hidden, .over-limit { display: none !important; }", ".tile-site", ".links li.lk-ok", ".links li.lk-err", ".links li.lk-off", ".sw-offline .links"} {
		if !strings.Contains(css, want) {
			t.Errorf("app.css lacks %s", want)
		}
	}
}
