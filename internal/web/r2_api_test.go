package web

// Tests of wave 3 (r2) for /api/v1/status, /api/v1/ha and the API tokens:
// W-053 and W-031 (named API tokens with a scope and a last use), W-048 (no
// API without an API token or a login).

import (
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/store"
)

var tokenValueRe = regexp.MustCompile(`<p class="mono token">(swt_[^<]+)</p>`)

// addToken adds an API token with the form of the API card and returns the
// value that the answer shows.
func (e *env) addToken(name, scope string) string {
	e.t.Helper()
	resp, body := e.postPage("/settings/api-tokens", url.Values{"csrf": {e.csrf("/settings")}, "op": {"add"}, "name": {name}, "scope": {scope}})
	m := tokenValueRe.FindStringSubmatch(body)
	if resp.StatusCode != http.StatusOK || m == nil {
		e.t.Fatalf("add the API token %q: %d", name, resp.StatusCode)
	}
	return m[1]
}

// tokenID returns the ID of the API token with the name.
func (e *env) tokenID(name string) string {
	e.t.Helper()
	tok, err := e.st.APITokenByName(name)
	if err != nil || tok == nil {
		e.t.Fatalf("no API token %q: %v", name, err)
	}
	return tok.ID
}

// loginClient sets the password of role and logs in with a client of its
// own (a browser of another person).
func (e *env) loginClient(password, role string) *http.Client {
	e.t.Helper()
	key := KeyAdminHash
	if role == RoleViewer {
		key = KeyViewerHash
	}
	h, _ := HashPassword(password)
	_ = e.st.SetSetting(key, h)
	if role == RoleViewer {
		if a, _ := e.st.Setting(KeyAdminHash); a == "" {
			_ = e.st.SetSetting(KeyAdminHash, h+"x") // a viewer login needs an administrator password
		}
	}
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(e.srv.URL + "/login")
	if err != nil {
		e.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	resp, err = c.PostForm(e.srv.URL+"/login", url.Values{"csrf": {csrfRe.FindStringSubmatch(string(b))[1]}, "password": {password}})
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		e.t.Fatalf("login as %s: %d", role, resp.StatusCode)
	}
	return c
}

// call sends a GET with an optional bearer token through c and returns the
// status code and the body.
func call(t *testing.T, c *http.Client, u, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// W-053 acceptance: an ha API token reads /api/v1/ha and gets 403 on
// /api/v1/status; a full API token reads both; revoking 1 API token leaves
// the others working; the API token of the configuration file (the single
// API token of an older version) works as "API token" with the scope full.
func TestAPITokenScopes(t *testing.T) {
	e := adminEnv(t, twoServers)
	ha := e.addToken("Home Assistant", store.ScopeHA)
	full := e.addToken("Scripts", store.ScopeFull)
	status, hapath := e.srv.URL+"/api/v1/status", e.srv.URL+"/api/v1/ha"
	for _, c := range []struct {
		token    string
		status   int
		ha       int
		bodyPart string
	}{
		{ha, http.StatusForbidden, http.StatusOK, `{"error":"the API token permits only /api/v1/ha"}`},
		{full, http.StatusOK, http.StatusOK, `"health":`},
		{"swt_test_token", http.StatusOK, http.StatusOK, `"health":`}, // "API token" from the file, scope full
		{"swt_unknown", http.StatusUnauthorized, http.StatusUnauthorized, `{"error":"invalid token"}`},
	} {
		code, body := call(t, http.DefaultClient, status, c.token)
		if code != c.status || !strings.Contains(body, c.bodyPart) {
			t.Errorf("%s on /api/v1/status: %d %s", c.token, code, body)
		}
		if code, body := call(t, http.DefaultClient, hapath, c.token); code != c.ha {
			t.Errorf("%s on /api/v1/ha: %d %s", c.token, code, body)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, status, nil)
	req.Header.Set("Authorization", "Bearer "+ha)
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.Header.Get("Content-Type") != "application/json" || !strings.Contains(resp.Header.Get("WWW-Authenticate"), `error="insufficient_scope"`) {
		t.Errorf("403 headers: %v", resp.Header)
	}
	if list, _ := e.st.APITokens(); len(list) != 3 || list[0].Name != store.LegacyTokenName || list[0].Scope != store.ScopeFull {
		t.Fatalf("API tokens: %+v", list)
	}

	// Revoking 1 API token leaves the others working.
	e.post("/settings/api-tokens", url.Values{"csrf": {e.csrf("/settings")}, "op": {"revoke"}, "id": {e.tokenID("Scripts")}})
	if code, _ := call(t, http.DefaultClient, status, full); code != http.StatusUnauthorized {
		t.Errorf("the revoked API token: %d", code)
	}
	if code, _ := call(t, http.DefaultClient, hapath, ha); code != http.StatusOK {
		t.Errorf("the ha API token after the revoke of another: %d", code)
	}
	if code, _ := call(t, http.DefaultClient, status, "swt_test_token"); code != http.StatusOK {
		t.Errorf("the API token of the file after the revoke of another: %d", code)
	}
	// A new value: the old one stops, the scope stays.
	resp2, body := e.postPage("/settings/api-tokens", url.Values{"csrf": {e.csrf("/settings")}, "op": {"replace"}, "id": {e.tokenID("Home Assistant")}})
	m := tokenValueRe.FindStringSubmatch(body)
	if resp2.StatusCode != http.StatusOK || m == nil {
		t.Fatalf("replace: %d", resp2.StatusCode)
	}
	if code, _ := call(t, http.DefaultClient, hapath, ha); code != http.StatusUnauthorized {
		t.Errorf("the old value after the replace: %d", code)
	}
	if code, _ := call(t, http.DefaultClient, status, m[1]); code != http.StatusForbidden {
		t.Errorf("the new value of the ha API token on /api/v1/status: %d", code)
	}
}

// W-053: the value of a new API token shows one time, in the panel at the top
// of the answer, with the name and the addresses of its scope. The list shows
// the name, the scope, the creation time and "never" for the last use.
func TestNewAPITokenShownOnce(t *testing.T) {
	e := adminEnv(t, twoServers+"timezone: Europe/Stockholm\n")
	at := time.Date(2026, 10, 11, 13, 5, 0, 0, time.UTC)
	e.web.now = func() time.Time { return at }
	resp, body := e.postPage("/settings/api-tokens", url.Values{"csrf": {e.csrf("/settings")}, "op": {"add"}, "name": {"  Home Assistant "}, "scope": {"ha"}})
	m := tokenValueRe.FindStringSubmatch(body)
	if resp.StatusCode != http.StatusOK || m == nil {
		t.Fatalf("add: %d", resp.StatusCode)
	}
	panel := body[strings.Index(body, "<h2>New API token</h2>"):strings.Index(body, `<section class="card" id="servers">`)]
	for _, want := range []string{`Copy the value of the API token "Home Assistant" now. syncwatch does not show it again.`,
		"Use it as <code>Authorization: Bearer &lt;token&gt;</code> for <code>GET /api/v1/ha</code>."} {
		if !strings.Contains(panel, want) {
			t.Errorf("the panel lacks %q: %s", want, panel)
		}
	}
	if strings.Contains(panel, "/api/v1/status") {
		t.Error("the panel of an ha API token names /api/v1/status")
	}
	if !strings.HasPrefix(m[1], "swt_") || len(m[1]) < 30 {
		t.Errorf("value %q", m[1])
	}
	_, page := e.get("/settings")
	if strings.Contains(page, m[1]) || strings.Contains(page, "New API token") {
		t.Fatal("the value shows again")
	}
	api := section(t, page, "api")
	if !strings.Contains(api, "<td><strong>Home Assistant</strong></td><td>Home Assistant: /api/v1/ha only</td><td class=\"nowrap\">2026-10-11 15:05</td><td class=\"nowrap\">never</td>") {
		t.Errorf("the list: %s", api)
	}
	stored, _ := e.st.APITokenByValue(m[1])
	if stored == nil || stored.Name != "Home Assistant" || stored.Scope != store.ScopeHA {
		t.Fatalf("stored: %+v", stored)
	}
	// A full API token: both addresses in the panel.
	_, body = e.postPage("/settings/api-tokens", url.Values{"csrf": {e.csrf("/settings")}, "op": {"add"}, "name": {"Scripts"}, "scope": {"full"}})
	if !strings.Contains(body, "for <code>GET /api/v1/status</code> and <code>GET /api/v1/ha</code>.") {
		t.Error("the panel of a full API token does not name both addresses")
	}
}

// W-031: the last use of an API token shows in the list, at most 1 minute
// late. syncwatch writes it at most once a minute for each API token.
func TestAPITokenLastUsed(t *testing.T) {
	e := adminEnv(t, twoServers+"timezone: UTC\n")
	ha := e.addToken("Home Assistant", store.ScopeHA)
	now := time.Date(2026, 10, 11, 8, 0, 0, 0, time.UTC)
	e.web.now = func() time.Time { return now }
	used := func() time.Time {
		t.Helper()
		tok, _ := e.st.APITokenByValue(ha)
		return tok.LastUsed
	}
	call(t, http.DefaultClient, e.srv.URL+"/api/v1/ha", ha)
	if !used().Equal(now) {
		t.Fatalf("last use after the first request: %v", used())
	}
	first := now
	for i := 0; i < 5; i++ {
		now = now.Add(10 * time.Second)
		call(t, http.DefaultClient, e.srv.URL+"/api/v1/ha", ha)
	}
	if !used().Equal(first) {
		t.Fatalf("syncwatch wrote the last use more than once a minute: %v", used())
	}
	now = first.Add(61 * time.Second)
	call(t, http.DefaultClient, e.srv.URL+"/api/v1/ha", ha)
	if !used().Equal(now) {
		t.Fatalf("last use after a minute: %v, want %v", used(), now)
	}
	// A request that the scope refuses is also a use of the API token.
	now = now.Add(2 * time.Minute)
	if code, _ := call(t, http.DefaultClient, e.srv.URL+"/api/v1/status", ha); code != http.StatusForbidden || !used().Equal(now) {
		t.Fatalf("a refused request: %d, last use %v", code, used())
	}
	_, page := e.get("/settings")
	if !strings.Contains(section(t, page, "api"), `<td class="nowrap">2026-10-11 08:03</td>`) {
		t.Errorf("the list does not show the last use: %s", section(t, page, "api"))
	}
}

// W-053: at most 5 API tokens. With 5, the card has no "Add API token" form
// and says why; a sixth one is refused with the message in the card. Names
// must be unique without regard to case; the typed name stays in the form.
func TestAPITokenLimitAndNames(t *testing.T) {
	e := adminEnv(t, twoServers) // 1 API token from the file
	for _, name := range []string{"Home Assistant", "Lobby", "Scripts"} {
		e.addToken(name, store.ScopeHA)
	}
	csrf := e.csrf("/settings")
	resp, body := e.postPage("/settings/api-tokens", url.Values{"csrf": {csrf}, "op": {"add"}, "name": {"home assistant"}, "scope": {"full"}})
	api := section(t, body, "api")
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(api, `<p id="api-error" class="flash err" role="alert" tabindex="-1" autofocus>✖ The name of an API token must have 1 to 40 characters and no control characters. Each API token needs a different name.</p>`) ||
		!strings.Contains(api, `name="name" value="home assistant"`) || !strings.Contains(api, `<option value="full" selected>`) {
		t.Fatalf("a name in use: %d %s", resp.StatusCode, api)
	}
	for _, bad := range []url.Values{{"op": {"add"}, "name": {"x"}, "scope": {"admin"}}, {"op": {"replace"}, "id": {"nope"}}, {"op": {"nothing"}}} {
		bad.Set("csrf", csrf)
		if resp, body := e.postPage("/settings/api-tokens", bad); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, `id="api-error"`) {
			t.Errorf("%v: %d", bad, resp.StatusCode)
		}
	}
	e.addToken("Backup", store.ScopeFull)
	_, page := e.get("/settings")
	api = section(t, page, "api")
	if strings.Contains(api, `value="add"`) || !strings.Contains(api, "There are 5 API tokens. Revoke an API token before you add an API token.") {
		t.Errorf("the card with 5 API tokens: %s", api)
	}
	resp, body = e.postPage("/settings/api-tokens", url.Values{"csrf": {csrf}, "op": {"add"}, "name": {"Sixth"}, "scope": {"ha"}})
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "✖ There are 5 API tokens. Revoke an API token first.") {
		t.Fatalf("a sixth API token: %d", resp.StatusCode)
	}
	if list, _ := e.st.APITokens(); len(list) != 5 {
		t.Fatalf("%d API tokens", len(list))
	}
	// The old route cannot add the API token "API token" either.
	e.post("/settings/api-token", url.Values{"csrf": {csrf}, "op": {"revoke"}})
	e.addToken("Sixth", store.ScopeHA)
	if resp, body := e.postPage("/settings/api-token", url.Values{"csrf": {csrf}, "op": {"generate"}}); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "There are 5 API tokens.") {
		t.Fatalf("generate with 5 API tokens: %d", resp.StatusCode)
	}
}

// W-053: the route of the single API token of an older version, POST
// /settings/api-token, works on the API token "API token": generate gives
// it a new value (its scope stays) or adds it with the scope full; revoke
// revokes it. The other API tokens do not change.
func TestOldAPITokenRoute(t *testing.T) {
	e := adminEnv(t, twoServers)
	ha := e.addToken("Home Assistant", store.ScopeHA)
	csrf := e.csrf("/settings")
	id := e.tokenID(store.LegacyTokenName)
	_, body := e.postPage("/settings/api-token", url.Values{"csrf": {csrf}, "op": {"generate"}})
	m := tokenValueRe.FindStringSubmatch(body)
	if m == nil || e.hasAPIToken("swt_test_token") {
		t.Fatal("generate did not replace the value")
	}
	if tok, _ := e.st.APITokenByValue(m[1]); tok == nil || tok.ID != id || tok.Scope != store.ScopeFull {
		t.Fatalf("the new value: %+v", tok)
	}
	if msg, isErr := flash(e.post("/settings/api-token", url.Values{"csrf": {csrf}, "op": {"revoke"}})); isErr || msg != `The API token "API token" is revoked.` {
		t.Fatalf("revoke: %q %v", msg, isErr)
	}
	if e.hasAPIToken(m[1]) || !e.hasAPIToken(ha) {
		t.Fatal("revoke: wrong API token")
	}
	_, body = e.postPage("/settings/api-token", url.Values{"csrf": {csrf}, "op": {"generate"}})
	if m = tokenValueRe.FindStringSubmatch(body); m == nil {
		t.Fatal("generate without the API token")
	}
	if tok, _ := e.st.APITokenByValue(m[1]); tok == nil || tok.Name != store.LegacyTokenName || tok.Scope != store.ScopeFull {
		t.Fatalf("generate added %+v", tok)
	}
}

// An API token of an older version that syncwatch can decrypt but did not
// move into api_tokens (a downgrade made it after the migration): the API
// does not accept it, and the card says so.
func TestOldAPITokenSecretIsNotAccepted(t *testing.T) {
	e := adminEnv(t, twoServers)
	if err := e.st.SetSecret(store.SecretAPIToken, "swt_old_version"); err != nil {
		t.Fatal(err)
	}
	if code, _ := call(t, http.DefaultClient, e.srv.URL+"/api/v1/status", "swt_old_version"); code != http.StatusUnauthorized {
		t.Errorf("the secret api.token: %d", code)
	}
	_, page := e.get("/settings")
	if !strings.Contains(section(t, page, "api"), `<p class="c-err">An API token of an older version is stored. syncwatch does not accept it. Revoke it.</p>`) {
		t.Errorf("card: %s", section(t, page, "api"))
	}
	// The old route revokes the API token "API token", not the secret.
	e.post("/settings/api-token", url.Values{"csrf": {e.csrf("/settings")}, "op": {"revoke"}})
	if !e.st.HasSecret(store.SecretAPIToken) {
		t.Fatal("the old route revoked the secret first")
	}
	e.post("/settings/api-token", url.Values{"csrf": {e.csrf("/settings")}, "op": {"revoke"}})
	if e.st.HasSecret(store.SecretAPIToken) {
		t.Fatal("the old route without the API token does not revoke the secret")
	}
}

// W-048: the JSON API needs an API token or a login, also with open access.
// A logged-in viewer or administrator and proxy mode pass without a token.
func TestAPINeedsTokenOrLogin(t *testing.T) {
	paths := []string{"/api/v1/status", "/api/v1/ha"}
	open := newEnv(t, twoServers+"access: {viewer: open}\n")
	h, _ := HashPassword("correct horse battery")
	_ = open.st.SetSetting(KeyAdminHash, h)
	for _, p := range paths {
		if resp, _ := open.get(p); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("open access without a token, %s: %d", p, resp.StatusCode)
		}
		code, body := call(t, http.DefaultClient, open.srv.URL+p, "")
		if code != http.StatusUnauthorized || !strings.Contains(body, `{"error":"token required"}`) {
			t.Errorf("open access, %s: %d %s", p, code, body)
		}
		if code, _ := call(t, http.DefaultClient, open.srv.URL+p, "swt_test_token"); code != http.StatusOK {
			t.Errorf("open access with the API token, %s: %d", p, code)
		}
	}
	// The dashboard pages stay open.
	if resp, _ := open.get("/"); resp.StatusCode != http.StatusOK {
		t.Errorf("open access, overview: %d", resp.StatusCode)
	}

	e := newEnv(t, twoServers+"access: {viewer: password}\n")
	viewer := e.loginClient("viewer password 1", RoleViewer)
	admin := e.loginClient("correct horse battery", RoleAdmin)
	for _, p := range paths {
		if code, _ := call(t, viewer, e.srv.URL+p, ""); code != http.StatusOK {
			t.Errorf("viewer session, %s: %d", p, code)
		}
		if code, _ := call(t, admin, e.srv.URL+p, ""); code != http.StatusOK {
			t.Errorf("administrator session, %s: %d", p, code)
		}
	}
	// A viewer session after viewing was set to administrator only.
	c := e.cfg.Get().Clone()
	c.Access.Viewer = "none"
	_ = e.cfg.Set(c)
	if code, _ := call(t, viewer, e.srv.URL+"/api/v1/status", ""); code != http.StatusUnauthorized {
		t.Errorf("viewer session with viewer access none: %d", code)
	}

	proxy := newEnv(t, twoServers+"access: {mode: proxy}\n")
	for _, p := range paths {
		if code, _ := call(t, http.DefaultClient, proxy.srv.URL+p, ""); code != http.StatusOK {
			t.Errorf("proxy mode, %s: %d", p, code)
		}
		if code, _ := call(t, http.DefaultClient, proxy.srv.URL+p, "swt_unknown"); code != http.StatusUnauthorized {
			t.Errorf("proxy mode with an unknown API token, %s: %d", p, code)
		}
	}
}
