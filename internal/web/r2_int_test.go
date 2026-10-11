package web

// Tests of the integrator of wave 3 (r2): contracts between the assignments,
// end to end through the dashboard, the store and the JSON API.

import (
	"html"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/store"
)

// An api.token in an imported file (the 2 steps of fixer 1) becomes the
// value of the API token "API token" (fixer 5's ImportSecrets): the API
// accepts the new value with the full scope and refuses the old one, the
// import card does not call api.token an encrypted secret, and the notice
// names the API token.
func TestImportedAPITokenIsTheAPITokenNamedAPIToken(t *testing.T) {
	e := adminEnv(t, twoServers)
	_, page := e.get("/settings")
	card := html.UnescapeString(section(t, page, "import"))
	if !strings.Contains(card, `The value of <code>api.token</code> becomes the value of the API token "API token".`) ||
		strings.Contains(card, "<code>discord_notices_webhook</code>, <code>api.token</code>)") {
		t.Fatalf("the import card must not list api.token as an encrypted secret:\n%s", card)
	}
	csrf := e.csrf("/settings")
	if msg, isErr := impFile(e, csrf, "servers: [{name: AKKA, url: 'https://a:1'}, {name: SOL, url: 'https://b:1'}]\napi: {token: swt_imported_value}\n", nil); isErr {
		t.Fatalf("import: %s", msg)
	}
	if n := e.noticesText(); !strings.Contains(n, `The API token "API token" is replaced.`) || strings.Contains(n, "swt_imported") {
		t.Fatalf("import notice: %q", n)
	}
	for path, want := range map[string]int{"/api/v1/status": http.StatusOK, "/api/v1/ha": http.StatusOK} {
		if resp, _ := e.apiGet(path, "Bearer swt_imported_value", ""); resp.StatusCode != want {
			t.Errorf("%s with the imported value: %d, want %d", path, resp.StatusCode, want)
		}
		if resp, _ := e.apiGet(path, "Bearer swt_test_token", ""); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s with the old value: %d, want 401", path, resp.StatusCode)
		}
	}
	tokens, err := e.st.APITokens()
	if err != nil || len(tokens) != 1 || tokens[0].Name != "API token" || tokens[0].Scope != "full" {
		t.Fatalf("API tokens after the import: %+v %v", tokens, err)
	}
}

// The line of the import notice for api.token follows the rule of the import
// itself (settings.ImportSecrets): "replaced" only when an API token named
// "API token" exists. A secret api.token of an older version that the store
// did not move (a downgrade made it after the migration) does not count: the
// import adds the API token "API token" next to it. The CLI uses the same
// rule (internal/app).
func TestImportTokenLineFollowsTheImportRule(t *testing.T) {
	e := adminEnv(t, `servers: [{name: AKKA, url: "https://a:1"}]`)
	if _, err := e.st.AddAPIToken("Home Assistant", "ha", "swt_ha_value", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetSecret(store.SecretAPIToken, "swt_old_version"); err != nil {
		t.Fatal(err)
	}
	csrf := e.csrf("/settings")
	if msg, isErr := impFile(e, csrf, "servers: [{name: AKKA, url: 'https://a:1'}]\napi: {token: swt_imported_value}\n", nil); isErr {
		t.Fatalf("import: %s", msg)
	}
	if n := e.noticesText(); !strings.Contains(n, `The API token "API token" is added.`) {
		t.Fatalf("import notice: %q", n)
	}
	tokens, _ := e.st.APITokens()
	var names []string
	for _, tok := range tokens {
		names = append(names, tok.Name+"/"+tok.Scope)
	}
	if strings.Join(names, ",") != "Home Assistant/ha,API token/full" {
		t.Fatalf("API tokens after the import: %v", names)
	}
}

// C3/C7/C11 end to end: the engine (fixer 2) computes the links between the
// servers and the sites; /api/v1/status and /api/v1/ha (fixer 5) give them
// with the keys of the plan, and the Overview (fixer 6) and the lobby page
// (fixer 7) show the same rows.
func TestLinksEndToEnd(t *testing.T) {
	e := adminEnv(t, `servers:
  - {name: AKKA, url: "https://a:1", site: "IVAR Office"}
  - {name: SOL, url: "https://b:1", site: "Branch office"}
  - {name: ODEN, url: "https://c:1", site: "IVAR Office"}
site: IVAR Office
timezone: Europe/Stockholm
api: {token: swt_test_token}
`)
	start := time.Now().Add(-time.Hour).Truncate(time.Second)
	const akkaID, solID, odenID = "AKKAAAA-AAAAAAA", "SOLSOLS-BBBBBBB", "ODENODE-CCCCCCC"
	server := func(id, my string, peers ...string) {
		e.hub.Update(id, func(s *model.Server) {
			s.Status, s.Loaded, s.MyID, s.LastContact, s.LastError = model.StatusUp, true, my, time.Now(), ""
			s.Devices, s.Connections = map[string]*model.Device{}, map[string]*model.Connection{}
			for _, p := range peers {
				s.Devices[p] = &model.Device{ID: p, Name: p}
				s.Connections[p] = &model.Connection{Connected: true, StartedAt: start}
			}
		})
	}
	server("akka", akkaID, solID, odenID)
	server("sol", solID, akkaID)
	server("oden", odenID, akkaID)
	e.eng.Evaluate()

	snap := e.eng.View().Snapshot
	if len(snap.Links) != 2 || len(snap.SiteLinks) != 1 || snap.SiteLinks[0].State != model.LinkConnected {
		t.Fatalf("engine links %+v, site links %+v", snap.Links, snap.SiteLinks)
	}
	keys := func(m map[string]any) string {
		var out []string
		for k := range m {
			out = append(out, k)
		}
		slices.Sort(out)
		return strings.Join(out, ",")
	}
	for _, path := range []string{"/api/v1/status", "/api/v1/ha"} {
		resp, body := e.apiGet(path, "Bearer swt_test_token", "")
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
		links, _ := body["links"].([]any)
		siteLinks, _ := body["site_links"].([]any)
		if len(links) != 2 || len(siteLinks) != 1 {
			t.Fatalf("%s: links %v, site_links %v", path, body["links"], body["site_links"])
		}
		l0, _ := links[0].(map[string]any)
		if keys(l0) != "a,b,since,site_a,site_b,state" || l0["a"] != "akka" || l0["b"] != "sol" || l0["site_a"] != "IVAR Office" ||
			l0["site_b"] != "Branch office" || l0["state"] != "connected" || l0["since"] != start.UTC().Format("2006-01-02T15:04:05Z") {
			t.Errorf("%s: links[0] %v", path, l0)
		}
		s0, _ := siteLinks[0].(map[string]any)
		if keys(s0) != "a,b,since,state" || s0["a"] != "IVAR Office" || s0["b"] != "Branch office" || s0["state"] != "connected" {
			t.Errorf("%s: site_links[0] %v", path, s0)
		}
	}
	_, ha := e.apiGet("/api/v1/ha", "Bearer swt_test_token", "")
	if servers, _ := ha["servers"].(map[string]any); servers == nil || servers["sol"].(map[string]any)["site"] != "Branch office" {
		t.Errorf("/api/v1/ha servers: %v", ha["servers"])
	}
	_, st := e.apiGet("/api/v1/status", "Bearer swt_test_token", "")
	if sv := apiServers(st); sv["oden"]["site"] != "IVAR Office" {
		t.Errorf("/api/v1/status servers: %v", st["servers"])
	}

	// The Overview and the lobby show the same rows: connected, then SOL is
	// unreachable.
	same := func(step, want string) {
		t.Helper()
		v := e.eng.View()
		var ov, lb []string
		for _, r := range e.web.overview(v).Links {
			ov = append(ov, r.Text())
		}
		for _, l := range e.web.lobby(v, "").Links {
			lb = append(lb, l.Text)
		}
		if strings.Join(ov, " | ") != want || strings.Join(lb, " | ") != want {
			t.Errorf("%s: Overview %q, lobby %q, want %q", step, ov, lb, want)
		}
	}
	same("connected", "IVAR Office ↔ Branch office: connected from "+e.web.fmtClock(start))
	e.hub.Update("sol", func(s *model.Server) {
		s.Status, s.DownSince, s.LastError = model.StatusDown, time.Now(), "no route to host"
	})
	e.eng.Evaluate()
	same("SOL down", "IVAR Office ↔ Branch office: state unknown: SOL is unreachable")
	_, st = e.apiGet("/api/v1/status", "Bearer swt_test_token", "")
	if sl, _ := st["site_links"].([]any); len(sl) != 1 || sl[0].(map[string]any)["state"] != "unreachable" || sl[0].(map[string]any)["since"] != nil {
		t.Errorf("site_links with SOL down: %v", st["site_links"])
	}
}

// C11: the Overview and the lobby page write the rows of the connections
// with one helper: 2 servers whose state is not known are joined with "and",
// and a time of another day has its date.
func TestOverviewAndLobbyWriteTheSameLinkRows(t *testing.T) {
	e := adminEnv(t, r2vSites)
	e.web.now = func() time.Time { return r2vNow }
	yesterday := time.Date(2026, 10, 10, 19, 4, 0, 0, time.UTC)
	links := []model.ServerLink{
		{A: "akka", B: "oden", State: model.LinkDisconnected, Since: yesterday},
		{A: "sol", B: "oden", State: model.LinkUnreachable},
	}
	siteLinks := []model.SiteLink{{A: "IVAR Office", B: "Branch office", State: model.LinkUnreachable, Servers: [][2]string{{"akka", "sol"}, {"oden", "sol"}}}}
	for _, tc := range []struct {
		links     []model.ServerLink
		siteLinks []model.SiteLink
		want      string
	}{
		{links, nil, "AKKA ↔ ODEN: not connected from 2026-10-10 21:04 | SOL ↔ ODEN: state unknown: SOL is unreachable and the full check of ODEN is not complete"},
		{links, siteLinks, "IVAR Office ↔ Branch office: state unknown: SOL is unreachable and the full check of ODEN is not complete"},
	} {
		v := r2vView(tc.links, tc.siteLinks, nil)
		var ov, lb []string
		for _, r := range e.web.overview(v).Links {
			ov = append(ov, r.Text())
		}
		for _, l := range e.web.lobby(v, "").Links {
			lb = append(lb, l.Text)
		}
		if strings.Join(ov, " | ") != tc.want || strings.Join(lb, " | ") != tc.want {
			t.Errorf("Overview %q, lobby %q, want %q", ov, lb, tc.want)
		}
	}
}

// C1 end to end: the keys that the Settings forms write (fixer 1) are the
// keys that the checks read (fixer 2). The Devices card sets devices[].relay:
// no H11 for the device, and H12 after thresholds.relay_lost of the Checks
// form. The site of a server (Servers form) and the site of syncwatch
// (Notifications form) go into the H1 message and its causes, and into
// /api/v1/status (fixer 5).
func TestSettingsKeysReachTheChecks(t *testing.T) {
	e := adminEnv(t, twoServers)
	csrf := e.csrf("/settings")
	const laptop = "LAPTOPD-AAAAAAA-BBBBBBB-CCCCCCC-DDDDDDD-EEEEEEE-FFFFFFF-GGGGGGG"
	save := func(path string, form url.Values) {
		t.Helper()
		form.Set("csrf", csrf)
		if msg, isErr := flash(e.post(path, form)); isErr {
			t.Fatalf("POST %s: %s", path, msg)
		}
	}
	laptopOn := func(connected bool, lastSeen time.Time) {
		e.hub.Update("akka", func(s *model.Server) {
			s.Status, s.Loaded, s.MyID, s.LastContact, s.LastError = model.StatusUp, true, "AKKAAAA-1111111", time.Now(), ""
			s.Devices = map[string]*model.Device{laptop: {ID: laptop, Name: "LAPTOP-DANI", LastSeen: lastSeen}}
			s.Connections = map[string]*model.Connection{laptop: {Connected: connected, Type: "relay-client", Address: "relay://10.0.0.9:22067"}}
		})
		e.eng.Evaluate()
	}
	open := func(check, part string) bool {
		for _, r := range e.eng.View().Open {
			if r.Check == check && strings.Contains(r.Subject+" "+r.Message, part) {
				return true
			}
		}
		return false
	}
	laptopOn(true, time.Now())
	if !open("H11", "LAPTOP-DANI") {
		t.Fatal("a device that connects through a relay must have H11 before the Devices form")
	}
	save("/settings/devices", url.Values{"relay": {laptop}})
	if d := e.cfg.Get().Devices; len(d) != 1 || d[0].ID != laptop || !d[0].Relay {
		t.Fatalf("devices: %+v", d)
	}
	laptopOn(true, time.Now())
	if open("H11", "LAPTOP-DANI") {
		t.Fatal("H11 for a device with devices[].relay")
	}
	// The Checks form sets relay_lost: 2 hours, the laptop was seen 1 hour
	// ago: no H12. 30 minutes: H12.
	f := checksForm(e.cfg.Get(), csrf)
	f.Set("relay_lost", "2h")
	save("/settings/checks", f)
	laptopOn(false, time.Now().Add(-time.Hour))
	if open("H12", "LAPTOP-DANI") {
		t.Fatal("H12 before relay_lost (2 hours)")
	}
	f.Set("relay_lost", "30m")
	save("/settings/checks", f)
	if e.eng.Evaluate(); !open("H12", "LAPTOP-DANI") {
		t.Fatalf("no H12 after relay_lost (30 minutes): %v", e.cfg.Get().Thresholds.RelayLost)
	}

	// Sites.
	save("/settings/servers", url.Values{"orig_id": {"sol"}, "name": {"SOL"}, "url": {"https://b:1"}, "site": {"Branch office"}})
	nf := notifyForm(e.cfg.Get(), csrf)
	nf.Set("site", "IVAR Office")
	save("/settings/notifications", nf)
	e.hub.Update("sol", func(s *model.Server) {
		s.Status, s.DownSince, s.LastError = model.StatusDown, time.Now(), "dial tcp 10.0.0.2:8384: i/o timeout"
	})
	e.eng.Evaluate()
	var h1 *store.Record
	for _, r := range e.eng.View().Open {
		if r.Check == "H1" && r.Server == "sol" {
			h1 = r
		}
	}
	if h1 == nil || !strings.HasPrefix(h1.Message, "Server SOL (Branch office) is unreachable from IVAR Office: ") {
		t.Fatalf("H1 of SOL: %+v", h1)
	}
	causes := ""
	for _, d := range h1.Details {
		if d.Key == "Possible causes" {
			causes = d.Value
		}
	}
	if causes != "The VPN between IVAR Office and Branch office is down, or the server is off or does not answer." {
		t.Errorf("H1 causes: %q", causes)
	}
	_, st := e.apiGet("/api/v1/status", "Bearer swt_test_token", "")
	if sv := apiServers(st); sv["sol"]["site"] != "Branch office" || sv["akka"]["site"] != "" {
		t.Errorf("/api/v1/status sites: %v", st["servers"])
	}
}
