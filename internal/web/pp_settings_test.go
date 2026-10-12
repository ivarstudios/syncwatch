package web

// Tests of the Settings area: the viewer and administrator passwords, the
// API token of an imported file, the spelling of the sites and the
// direction controls in the import preview and the notification log.

import (
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/store"
)

const (
	ppAdminPW  = "correct horse battery" // the administrator password of adminEnv
	ppViewerPW = "studio lobby 2026"     // the staff and the lobby screen use it
)

// ppLogin logs in with pw from a browser of its own and returns the role of
// the session that syncwatch stored for it ("" when the login failed).
func ppLogin(t *testing.T, e *env, pw string) string {
	t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(e.srv.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	m := csrfRe.FindStringSubmatch(string(b))
	if m == nil {
		t.Fatal("no csrf token on /login")
	}
	if resp, err = c.PostForm(e.srv.URL+"/login", url.Values{"csrf": {m[1]}, "password": {pw}}); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	u, _ := url.Parse(e.srv.URL)
	for _, ck := range jar.Cookies(u) {
		if ck.Name == sessionCookie {
			if z, err := e.st.Session(hashToken(ck.Value), time.Now()); err == nil && z != nil {
				return z.Role
			}
		}
	}
	return ""
}

// The viewer password and the administrator password are
// different. The login checks the administrator password first: with one
// password for both, each login with the viewer password (the staff, the
// lobby screen) was an administrator session.
func TestViewerAndAdministratorPasswordsDiffer(t *testing.T) {
	e := adminEnv(t, twoServers)
	csrf := e.csrf("/settings")
	access := func(f url.Values) (string, bool) {
		f.Set("csrf", csrf)
		f.Set("mode", "builtin")
		return flash(e.post("/settings/access", f))
	}

	// The administrator password as the viewer password.
	msg, isErr := access(url.Values{"viewer": {"password"}, "viewer_password": {ppAdminPW}, "confirm": {ppAdminPW}})
	if !isErr || msg != "The viewer password must be different from the administrator password." {
		t.Fatalf("the administrator password as the viewer password: %q (error %v)", msg, isErr)
	}
	if _, ok := e.web.setting(KeyViewerHash); ok || e.cfg.Get().Access.Viewer != config.ViewerNone {
		t.Fatalf("the refused change was stored: viewer hash %v, viewer access %q", ok, e.cfg.Get().Access.Viewer)
	}

	// A different viewer password: the lobby screen is a viewer.
	if msg, isErr := access(url.Values{"viewer": {"password"}, "viewer_password": {ppViewerPW}, "confirm": {ppAdminPW}}); isErr {
		t.Fatalf("a viewer password: %q", msg)
	}
	if role := ppLogin(t, e, ppViewerPW); role != RoleViewer {
		t.Fatalf("login with the viewer password: role %q", role)
	}

	// The viewer password as the new administrator password.
	notices := len(e.noticeList())
	msg, isErr = flash(e.post("/settings/password", url.Values{"csrf": {csrf}, "current": {ppAdminPW}, "password": {ppViewerPW}, "password2": {ppViewerPW}}))
	if !isErr || msg != "The administrator password must be different from the viewer password." {
		t.Fatalf("the viewer password as the administrator password: %q (error %v)", msg, isErr)
	}
	if h, _ := e.web.setting(KeyAdminHash); !CheckPassword(ppAdminPW, h) {
		t.Fatal("the refused administrator password was stored")
	}
	if n := e.noticeList(); len(n) != notices {
		t.Fatalf("a refused change sent a settings notice: %q", n[notices:])
	}
	if role := ppLogin(t, e, ppViewerPW); role != RoleViewer {
		t.Fatalf("login with the viewer password after the refused change: role %q", role)
	}
	if role := ppLogin(t, e, ppAdminPW); role != RoleAdmin {
		t.Fatalf("login with the administrator password: role %q", role)
	}
}

// The check of a new viewer password must not tell a session
// that does not know the administrator password (an unlocked browser, a
// copied cookie) whether a guess is the administrator password. Thus a new
// viewer password needs the administrator password first, also when it has
// no effect now (viewer access "administrator only"), and the comparison
// comes after that confirmation, inside its rate limit.
func TestViewerPasswordIsNoAdministratorPasswordOracle(t *testing.T) {
	e := adminEnv(t, twoServers)
	csrf := e.csrf("/settings")
	answer := func(guess string) string {
		msg, isErr := flash(e.post("/settings/access", url.Values{"csrf": {csrf}, "mode": {"builtin"}, "viewer": {"none"}, "viewer_password": {guess}}))
		return fmt.Sprintf("%q (error %v)", msg, isErr)
	}
	wrong, right := answer("a wrong guess 01"), answer(ppAdminPW)
	if wrong != right {
		t.Fatalf("a wrong guess gives %s, the administrator password gives %s", wrong, right)
	}
	if want := fmt.Sprintf("%q (error true)", "This change permits access to more people. Type the administrator password to accept the change."); right != want {
		t.Fatalf("a new viewer password without the administrator password: %s, want %s", right, want)
	}
	if _, ok := e.web.setting(KeyViewerHash); ok {
		t.Fatal("a viewer password was stored without the administrator password")
	}
	// With the administrator password, a viewer password for later is saved.
	msg, isErr := flash(e.post("/settings/access", url.Values{"csrf": {csrf}, "mode": {"builtin"}, "viewer": {"none"}, "viewer_password": {ppViewerPW}, "confirm": {ppAdminPW}}))
	if h, ok := e.web.setting(KeyViewerHash); isErr || !ok || !CheckPassword(ppViewerPW, h) {
		t.Fatalf("a viewer password with the administrator password: %q (error %v)", msg, isErr)
	}
}

// The first start (/setup) refuses an administrator password
// that is the viewer password. A viewer password without an administrator
// password exists when it was set in proxy mode; syncwatch import-config
// can then set built-in login to on.
func TestSetupRefusesTheViewerPassword(t *testing.T) {
	e := newEnv(t, twoServers)
	vh, _ := HashPassword(ppViewerPW)
	if err := e.st.SetSetting(KeyViewerHash, vh); err != nil {
		t.Fatal(err)
	}
	token := e.web.SetupToken()
	csrf := e.csrf("/setup")
	resp, body := e.postPage("/setup", url.Values{"csrf": {csrf}, "token": {token}, "password": {ppViewerPW}, "password2": {ppViewerPW}})
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "The administrator password must be different from the viewer password.") {
		t.Fatalf("setup with the viewer password: %d %s", resp.StatusCode, body)
	}
	if e.web.adminConfigured() {
		t.Fatal("the viewer password is the administrator password")
	}
	if resp, _ := e.postPage("/setup", url.Values{"csrf": {csrf}, "token": {token}, "password": {ppAdminPW}, "password2": {ppAdminPW}}); resp.StatusCode != http.StatusSeeOther || !e.web.adminConfigured() {
		t.Fatalf("setup with a different password: %d", resp.StatusCode)
	}
}

// syncwatch makes the API token of an imported file (api.token)
// the API token "API token", with the scope full when it adds it. A weak
// value is refused at the preview, before syncwatch stores anything. The
// preview and the settings notice name the scope.
func TestImportedAPITokenIsCheckedAndShowsItsScope(t *testing.T) {
	const servers = "servers: [{name: AKKA, url: 'https://a:1'}]\n"
	e := adminEnv(t, servers)
	csrf := e.csrf("/settings")
	resp, _, id := impPreview(e, csrf, servers+"api: {token: x}\n")
	msg, isErr := flash(resp)
	if want := "Import failed. API token (api.token): the value must have at least 10 characters and no spaces or control characters. Make the API token in Settings → API."; id != "" || !isErr || msg != want {
		t.Fatalf("a 1-character API token: preview %v, %q (error %v)", id != "", msg, isErr)
	}
	if e.hasAPIToken("x") {
		t.Fatal("the refused API token was stored")
	}

	for _, tc := range []struct {
		scope, value, preview, line string
	}{
		{store.ScopeFull, "swt_imported_value_1", `the API token "API token" (Full: /api/v1/status and /api/v1/ha)`,
			`The API token "API token" is added. It can read /api/v1/status and /api/v1/ha.`},
		{store.ScopeHA, "swt_imported_value_2", `the API token "API token" (Home Assistant: /api/v1/ha only)`,
			`The API token "API token" is replaced. It can read /api/v1/ha only.`},
	} {
		if tc.scope == store.ScopeHA {
			// The scope of the API token "API token" stays: an administrator
			// gave it the scope Home Assistant.
			t0, _ := e.st.APITokenByName(store.LegacyTokenName)
			if err := e.st.DeleteAPIToken(t0.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := e.st.AddAPIToken(store.LegacyTokenName, store.ScopeHA, "swt_ha_value_0", time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		_, page, id := impPreview(e, csrf, servers+"api: {token: "+tc.value+"}\n")
		if id == "" {
			t.Fatalf("%s: no preview: %s", tc.scope, impError(page))
		}
		if p := html.UnescapeString(section(t, page, "import")); !strings.Contains(p, "<strong>Secrets in the file:</strong> "+tc.preview+".") {
			t.Fatalf("%s: the preview does not name the scope: %s", tc.scope, p)
		}
		n := len(e.noticeList())
		if resp, _ := impConfirm(e, csrf, id, nil); resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("%s: import %d", tc.scope, resp.StatusCode)
		}
		if all := e.noticeList(); len(all) != n+1 || !strings.Contains(all[n], tc.line) {
			t.Fatalf("%s: the notice does not name the scope: %q", tc.scope, all[n:])
		}
		if tok, _ := e.st.APITokenByValue(tc.value); tok == nil || tok.Scope != tc.scope {
			t.Fatalf("%s: the imported API token: %+v", tc.scope, tok)
		}
	}
}

// One site typed in two spellings is one site. The Servers and
// Notifications forms make each white space a space (a no-break space from
// Option-Space or a paste looks the same as a space). The spelling that the
// administrator types is the spelling of the site everywhere: a site is the
// same site without regard to case. H1 then names no VPN between a site and
// itself.
func TestOneSiteTypedInTwoSpellings(t *testing.T) {
	e := adminEnv(t, twoServers)
	csrf := e.csrf("/settings")
	save := func(path string, f url.Values) {
		t.Helper()
		f.Set("csrf", csrf)
		if msg, isErr := flash(e.post(path, f)); isErr {
			t.Fatalf("%s: %s", path, msg)
		}
	}
	h1 := func() string {
		t.Helper()
		e.eng.Evaluate()
		for _, r := range e.eng.View().Open {
			if r.Check == "H1" && r.Server == "akka" {
				return r.Message + " / " + r.DetailValue("Possible causes")
			}
		}
		t.Fatal("no open H1 for AKKA")
		return ""
	}
	now := time.Now()
	e.hub.Update("akka", func(s *model.Server) {
		s.Status, s.LastError, s.DownSince, s.LastContact = model.StatusDown, "timeout", now.Add(-10*time.Minute), now.Add(-11*time.Minute)
	})
	save("/settings/servers", url.Values{"orig_id": {"akka"}, "name": {"AKKA"}, "url": {"https://a:1"}, "site": {"IVAR Office"}})
	save("/settings/servers", url.Values{"orig_id": {"sol"}, "name": {"SOL"}, "url": {"https://b:1"}, "site": {"Branch office"}})
	f := notifyForm(e.cfg.Get(), csrf)
	f.Set("site", "IVAR\u00a0Office")
	save("/settings/notifications", f)
	if cfg := e.cfg.Get(); cfg.Site != "IVAR Office" || cfg.SiteOf("akka") != "IVAR Office" {
		t.Fatalf("no-break space: syncwatch %+q, AKKA %+q", cfg.Site, cfg.SiteOf("akka"))
	}
	if got := h1(); !strings.HasPrefix(got, "Server AKKA is unreachable: timeout / ") || strings.Contains(got, "VPN between IVAR Office and IVAR Office") {
		t.Fatalf("H1 with a no-break space: %+q", got)
	}
	_, page := e.get("/settings")
	if i := strings.Index(page, `<datalist id="site-names">`); i < 0 || strings.Count(page[i:i+strings.Index(page[i:], "</datalist>")], "<option") != 2 {
		t.Fatalf("the datalist does not offer each site one time: %s", page[i:])
	}

	// A case variant on a server: the typed spelling is the spelling of the
	// site, also for syncwatch. The notice names both changes.
	n := len(e.noticeList())
	save("/settings/servers", url.Values{"orig_id": {"akka"}, "name": {"AKKA"}, "url": {"https://a:1"}, "site": {"IVAR office"}})
	if cfg := e.cfg.Get(); cfg.Site != "IVAR office" || cfg.SiteOf("akka") != "IVAR office" {
		t.Fatalf("case variant: syncwatch %+q, AKKA %+q", cfg.Site, cfg.SiteOf("akka"))
	}
	if all := e.noticeList(); len(all) != n+1 || !strings.Contains(all[n], "The site of the server AKKA changed from IVAR Office to IVAR office.") ||
		!strings.Contains(all[n], "Site where syncwatch runs: IVAR Office → IVAR office") {
		t.Fatalf("notice of the case variant: %q", all[n:])
	}
	if got := h1(); !strings.HasPrefix(got, "Server AKKA is unreachable: timeout / ") {
		t.Fatalf("H1 with a case variant: %+q", got)
	}

	// A new server at the site of SOL, typed in capitals: one site, and the
	// notice of the new server names the change of SOL.
	n = len(e.noticeList())
	save("/settings/servers", url.Values{"name": {"SIRIUS"}, "url": {"https://c:1"}, "api_key": {"key-sirius"}, "site": {"BRANCH\u00a0OFFICE"}})
	if cfg := e.cfg.Get(); cfg.SiteOf("sol") != "BRANCH OFFICE" || cfg.SiteOf("sirius") != "BRANCH OFFICE" || cfg.Site != "IVAR office" {
		t.Fatalf("new server: SOL %+q, SIRIUS %+q, syncwatch %+q", cfg.SiteOf("sol"), cfg.SiteOf("sirius"), cfg.Site)
	}
	if all := e.noticeList(); len(all) != n+1 || !strings.Contains(all[n], "Server SIRIUS added: The API URL is https://c:1.\nThe site of the server is BRANCH OFFICE.\nThe site of the server SOL changed from Branch office to BRANCH OFFICE.\n") {
		t.Fatalf("notice of the new server: %q", all[n:])
	}
}

// The import preview shows the names of devices and servers as the
// other pages do: a character that changes the direction of text shows as
// ‹U+202E›. Raw, it showed the rest of a line in reverse ("gnp.exe" as
// "exe.png"). The names come from Syncthing (devices) and from the file
// or the configuration (servers).
func TestImportPreviewShowsDirectionControls(t *testing.T) {
	e := adminEnv(t, `servers: [{name: AKKA, url: "https://a:1", api_key: secret-key-akka}, {name: "\u202eLOS", url: "https://b:1"}]`+"\n")
	e.hub.Update("akka", func(s *model.Server) {
		s.Status, s.Loaded, s.MyID, s.LastContact = model.StatusUp, true, "AKKAAAA-1111111", time.Now()
		s.Devices = map[string]*model.Device{relayPC: {ID: relayPC, Name: "\u202egnp.exe\u202c"}}
		s.Connections = map[string]*model.Connection{relayPC: {Connected: true, Type: "relay-client", StartedAt: time.Now()}}
	})
	e.eng.Evaluate()
	csrf := e.csrf("/settings")
	file := `servers: [{name: AKKA, url: "https://a:1"}, {name: "\u202eKEY", url: "https://k:1", api_key: key-k}, {name: "\u202eNOKEY", url: "https://n:1"}]
devices: [{id: ` + relayPC + `, relay: true}]
`
	_, page, id := impPreview(e, csrf, file)
	if id == "" {
		t.Fatalf("no preview: %s", impError(page))
	}
	check := func(step, p string, want ...string) {
		t.Helper()
		if strings.ContainsFunc(p, hiddenControl) {
			t.Errorf("%s: the card Export and import has a raw direction control: %q", step, p)
		}
		p = html.UnescapeString(p)
		for _, w := range want {
			if !strings.Contains(p, w) {
				t.Errorf("%s: no %q", step, w)
			}
		}
	}
	check("preview", section(t, page, "import"),
		"The device ‹U+202E›gnp.exe‹U+202C› (LAPTOPD) always connects through a relay.", // Changes
		"syncwatch removes the server ‹U+202E›LOS and deletes",                          // Removed
		"Remove the server ‹U+202E›LOS (syncwatch deletes its API key)",                 // DeleteLabel
		"The server ‹U+202E›NOKEY has no API key for its API URL.",                      // KeyAgain
		"the API key of the server ‹U+202E›KEY")                                         // Secrets
	// A refused step 2 shows the preview again with the reason.
	_, page = impConfirm(e, csrf, id, nil)
	check("refused import", section(t, page, "import"), "Not imported. Select the checkbox to remove the server ‹U+202E›LOS.")
}

// The table "Recent notifications" shows the titles as the other
// pages do. The title of an urgent message has the folder label or the
// device name from Syncthing; the error can have the answer of Discord.
func TestNotificationLogShowsDirectionControls(t *testing.T) {
	e := adminEnv(t, twoServers)
	if err := e.st.LogNotification(time.Now(), "urgent", "discord", true, "", "🚨 Folder sync stopped: LIVE-\u202eECRUOS1 on AKKA"); err != nil {
		t.Fatal(err)
	}
	if err := e.st.LogNotification(time.Now(), "urgent", "discord", false, "HTTP 400: \u202eLIVE", "🚨 Server unreachable: \u202eLOS"); err != nil {
		t.Fatal(err)
	}
	_, page := e.get("/settings")
	i := strings.Index(page, `data-key="notify-log"`)
	if i < 0 {
		t.Fatal("no table Recent notifications")
	}
	log := page[i : i+strings.Index(page[i:], "</details>")]
	if strings.ContainsFunc(log, hiddenControl) {
		t.Errorf("the table Recent notifications has a raw direction control: %q", log)
	}
	for _, want := range []string{"Folder sync stopped: LIVE-‹U+202E›ECRUOS1 on AKKA", "Server unreachable: ‹U+202E›LOS", "✖ HTTP 400: ‹U+202E›LIVE"} {
		if !strings.Contains(html.UnescapeString(log), want) {
			t.Errorf("no %q in %q", want, log)
		}
	}
}
