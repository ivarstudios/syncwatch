package web

// Tests of wave 3 (r2) for /api/v1/status, /api/v1/ha and the API tokens:
// W-053 and W-031 (named API tokens with a scope and a last use), W-048 (no
// API without an API token or a login), W-049 (one time format and stable
// status values), W-054 (HA status error only with H4), W-052 and W-057.api
// (counts, notifications, sites and links), W-036 (snooze end and reason).

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
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
		if tok == nil {
			t.Fatal("the API token does not exist")
		}
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
	if !strings.Contains(section(t, page, "api"), `<p class="c-err">The database has an API token of an older version. syncwatch does not accept it. Revoke it.</p>`) {
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

// timeKeys are the keys of time values in both answers.
var timeKeys = map[string]bool{"generated_at": true, "last_contact": true, "cert_expires": true, "last_seen": true, "since": true,
	"opened_at": true, "next_try": true, "snooze_until": true, "notifications_failing_since": true}

var utcSecond = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$`)

// checkTimes walks a JSON answer: each time value is UTC with whole seconds
// or null. It returns the number of time values that are not null.
func checkTimes(t *testing.T, where string, v any) int {
	t.Helper()
	n := 0
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if timeKeys[k] {
				if val == nil {
					continue
				}
				s, ok := val.(string)
				if !ok || !utcSecond.MatchString(s) {
					t.Errorf("%s.%s = %#v, want YYYY-MM-DDTHH:MM:SSZ or null", where, k, val)
				}
				n++
				continue
			}
			n += checkTimes(t, where+"."+k, val)
		}
	case []any:
		for _, val := range x {
			n += checkTimes(t, where+"[]", val)
		}
	}
	return n
}

// W-049: every time in both answers is UTC with whole seconds; a time that
// is not known is null, not a missing key. last_contact and last_seen were
// local times with an offset.
func TestAPITimesAreUTC(t *testing.T) {
	e := adminEnv(t, twoServers+"timezone: Europe/Stockholm\n")
	local := time.FixedZone("CEST", 2*3600)
	contact := time.Date(2026, 10, 11, 17, 29, 1, 784844188, local)
	e.hub.Update("akka", func(s *model.Server) {
		s.Status, s.Loaded, s.MyID, s.LastContact = model.StatusUp, true, "AKKAAAA-1111111", contact
		s.StartTime = contact.Add(-time.Hour)
		s.Cert = model.CertInfo{Fingerprint: "AA", NotAfter: time.Date(2027, 1, 2, 3, 4, 5, 6, local)}
		s.Devices = map[string]*model.Device{"PCAAAAA-1": {ID: "PCAAAAA-1", Name: "PC", LastSeen: contact}}
	})
	e.eng.Evaluate()
	r := e.eng.View().Open[0]
	_ = e.eng.Snooze(r.ID, time.Now().Add(24*time.Hour+123*time.Millisecond), "VPN work", RoleAdmin)
	e.setDelivery(notify.Delivery{Failing: true, Since: contact, NextTry: contact.Add(90*time.Second + 5*time.Millisecond), Error: "x"})
	for _, p := range []string{"/api/v1/status", "/api/v1/ha"} {
		_, body := e.apiGet(p, "Bearer swt_test_token", "")
		n := checkTimes(t, p, body)
		if p == "/api/v1/status" && n < 9 || p == "/api/v1/ha" && n < 3 {
			t.Errorf("%s: only %d time values are set", p, n)
		}
	}
	_, st := e.apiGet("/api/v1/status", "Bearer swt_test_token", "")
	akka := apiServers(st)["akka"]
	if akka["last_contact"] != "2026-10-11T15:29:01Z" || akka["cert_expires"] != "2027-01-02T01:04:05Z" {
		t.Errorf("akka: last_contact %v, cert_expires %v", akka["last_contact"], akka["cert_expires"])
	}
	if sol := apiServers(st)["sol"]; sol["last_contact"] != nil || sol["cert_expires"] != nil {
		t.Errorf("sol: %v", sol)
	} else if _, ok := sol["last_contact"]; !ok {
		t.Error("sol: the key last_contact is missing; want null")
	}
	devices, _ := st["devices"].([]any)
	seen := false
	for _, x := range devices {
		if d, _ := x.(map[string]any); d["id"] == "AKKAAAA-1111111" || d["id"] == "PCAAAAA-1" {
			seen = true
			if d["last_seen"] != "2026-10-11T15:29:01Z" {
				t.Errorf("devices[] %v: last_seen %v", d["id"], d["last_seen"])
			}
		}
	}
	if !seen {
		t.Error("no devices")
	}
	n, _ := st["notifications"].(map[string]any)
	if n["since"] != "2026-10-11T15:29:01Z" || n["next_try"] != "2026-10-11T15:30:31Z" {
		t.Errorf("notifications: %v", n)
	}
	_, ha := e.apiGet("/api/v1/ha", "Bearer swt_test_token", "")
	if ha["notifications_failing"] != true || ha["notifications_failing_since"] != "2026-10-11T15:29:01Z" {
		t.Errorf("ha notifications: %v %v", ha["notifications_failing"], ha["notifications_failing_since"])
	}
	servers, _ := ha["servers"].(map[string]any)
	if a, _ := servers["akka"].(map[string]any); a["last_contact"] != "2026-10-11T15:29:01Z" {
		t.Errorf("ha akka last_contact: %v", a["last_contact"])
	}
	// While notifications work: null, not a missing key.
	e.setDelivery(notify.Delivery{})
	_, st = e.apiGet("/api/v1/status", "Bearer swt_test_token", "")
	n, _ = st["notifications"].(map[string]any)
	if _, ok := n["next_try"]; !ok || n["since"] != nil || n["next_try"] != nil || n["failing"] != false {
		t.Errorf("notifications while they work: %v", n)
	}
}

// view is a hand-made engine view: a snapshot with the servers and the open
// findings, for the answers that the engine of another assignment fills.
func view(servers []*model.Server, open ...*store.Record) *engine.View {
	return &engine.View{Snapshot: &model.Snapshot{At: time.Now(), Servers: servers}, Open: open, Snoozes: map[string]store.Snooze{}}
}

func up(id string, folders ...*model.Folder) *model.Server {
	sv := &model.Server{ID: id, Name: strings.ToUpper(id), Status: model.StatusUp, Loaded: true, LastContact: time.Now(), Folders: map[string]*model.Folder{}}
	for _, f := range folders {
		sv.Folders[f.ID] = f
	}
	return sv
}

func rec(check, server string, sev model.Severity, urgent bool, related ...string) *store.Record {
	id := model.FindingID(check, server, check+"-"+server+strings.Join(related, ","))
	return &store.Record{Finding: model.Finding{ID: id, Check: check, Checks: []string{check}, Severity: sev, Server: server, Related: related, Urgent: urgent,
		Subject: check, Message: check}, OpenedAt: time.Now(), Since: time.Now()}
}

// W-049, W-054: the status values of /api/v1/ha have no spaces, and error
// needs an open H4 finding with severity ERROR.
func TestHAStatusValues(t *testing.T) {
	e := newEnv(t, `servers: [{name: A, url: "https://a:1"}, {name: B, url: "https://b:1", disabled: true}]`)
	status := func(v *engine.View, id string) string {
		t.Helper()
		return e.web.haStatusOf(v, nil, nil).Servers[id].Status
	}
	failed := &model.Folder{ID: "f", State: "idle", PullErrors: 3, FileErrors: []model.FileError{{Path: "x", Error: "denied"}}}
	watch := &model.Folder{ID: "w", State: "idle", WatchError: "inotify limit"}
	both := &model.Folder{ID: "fw", State: "idle", PullErrors: 1, WatchError: "inotify limit"}
	for _, c := range []struct {
		name string
		v    *engine.View
		want string
	}{
		{"idle", view([]*model.Server{up("a", &model.Folder{ID: "f", State: "idle"})}), "up_to_date"},
		{"no folders", view([]*model.Server{up("a")}), "up_to_date"},
		{"syncing", view([]*model.Server{up("a", &model.Folder{ID: "f", State: "syncing", NeedItems: 3})}), "syncing"},
		{"scanning", view([]*model.Server{up("a", &model.Folder{ID: "f", State: "scanning"})}), "scanning"},
		{"paused", view([]*model.Server{up("a", &model.Folder{ID: "f", Paused: true})}), "paused"},
		{"failed items, no H4", view([]*model.Server{up("a", failed)}), "syncing"},
		{"failed items and a watcher error, no H4", view([]*model.Server{up("a", both)}), "watch_error"},
		{"failed items, H4 ERROR", view([]*model.Server{up("a", failed)}, rec("H4", "a", model.Error, true)), "error"},
		{"H4 ERROR of another server", view([]*model.Server{up("a", failed)}, rec("H4", "c", model.Error, true)), "syncing"},
		{"watcher error, watcher H4 WARN", view([]*model.Server{up("a", watch)}, rec("H4", "a", model.Warn, false)), "watch_error"},
		{"failed items, only the watcher H4 WARN", view([]*model.Server{up("a", both)}, rec("H4", "a", model.Warn, false)), "watch_error"},
		{"unreachable", view([]*model.Server{{ID: "a", Status: model.StatusDown, Loaded: true}}), "unreachable"},
		{"certificate changed", view([]*model.Server{{ID: "a", Status: model.StatusCertChanged, Loaded: true}}), "certificate_changed"},
		{"not contacted", view([]*model.Server{{ID: "a", Status: model.StatusUnknown}}), "not_contacted"},
		{"up, no full check yet", view([]*model.Server{{ID: "a", Status: model.StatusUp}}), "not_contacted"},
	} {
		if got := status(c.v, "a"); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
		if got := status(c.v, "b"); got != "disabled" {
			t.Errorf("%s: the disabled server %q", c.name, got)
		}
	}
	// A snoozed H4 still counts; folders_error keeps the raw count.
	h4 := rec("H4", "a", model.Error, true)
	v := view([]*model.Server{up("a", failed)}, h4)
	v.Snoozes[h4.ID] = store.Snooze{FindingID: h4.ID, Kind: store.SnoozeKind, Until: time.Now().Add(time.Hour)}
	if s := e.web.haStatusOf(v, nil, nil).Servers["a"]; s.Status != "error" || s.FoldersError != 1 {
		t.Errorf("snoozed H4: %+v", s)
	}
	if s := e.web.haStatusOf(view([]*model.Server{up("a", failed)}), nil, nil).Servers["a"]; s.FoldersError != 1 || s.FoldersSyncing != 0 {
		t.Errorf("failed items without H4: folders_error %d, folders_syncing %d", s.FoldersError, s.FoldersSyncing)
	}
}

// W-054 with the engine: failed items without H4 (failing_files waits 1
// hour) give syncing and folders_error 1; a folder error opens H4 at once and
// gives error.
func TestHAErrorFollowsH4(t *testing.T) {
	e := newEnv(t, twoServers)
	e.hub.Update("akka", func(s *model.Server) {
		s.Status, s.Loaded, s.LastContact = model.StatusUp, true, time.Now()
		s.Folders = map[string]*model.Folder{"f": {ID: "f", Label: "LIVE", State: "idle", PullErrors: 2}}
	})
	e.eng.Evaluate()
	ha := func() map[string]any {
		t.Helper()
		_, body := e.apiGet("/api/v1/ha", "Bearer swt_test_token", "")
		servers, _ := body["servers"].(map[string]any)
		a, _ := servers["akka"].(map[string]any)
		return a
	}
	if a := ha(); a["status"] != "syncing" || a["folders_error"] != 1.0 {
		t.Errorf("failed items without H4: %v %v", a["status"], a["folders_error"])
	}
	e.hub.Update("akka", func(s *model.Server) {
		s.Folders["f"] = &model.Folder{ID: "f", Label: "LIVE", State: "error", Error: "folder marker missing"}
	})
	e.eng.Evaluate()
	if a := ha(); a["status"] != "error" || a["folders_error"] != 1.0 {
		t.Errorf("folder error with H4: %v %v", a["status"], a["folders_error"])
	}
}

// W-052: the counts of /api/v1/ha are the counts of /api/v1/status; each
// server counts its findings and the findings that relate to it; the
// notification state is in the answer.
func TestHACountsAndNotifications(t *testing.T) {
	e := adminEnv(t, twoServers) // two unreachable servers: two urgent H1 errors
	e.hub.Update("akka", func(s *model.Server) {
		s.Status, s.Loaded, s.LastContact = model.StatusUp, true, time.Now()
		s.PendingDevices = []model.PendingDevice{{DeviceID: "CCCCCCC-DDDDDDD", Name: "LAPTOP-X", Address: "10.0.0.9:22000", Time: time.Now()}}
	})
	e.eng.Evaluate()
	for _, r := range e.eng.View().Open {
		if r.Check == "H9" {
			_ = e.eng.Ignore(r.ID, "known", RoleAdmin)
		}
	}
	_, st := e.apiGet("/api/v1/status", "Bearer swt_test_token", "")
	_, ha := e.apiGet("/api/v1/ha", "Bearer swt_test_token", "")
	counts, _ := st["counts"].(map[string]any)
	for k, want := range map[string]any{"findings_error": counts["error"], "findings_warn": counts["warn"], "findings_info": counts["info"], "findings_urgent": counts["urgent"]} {
		if ha[k] != want {
			t.Errorf("ha %s = %v, counts say %v", k, ha[k], want)
		}
	}
	if open := counts["error"].(float64) + counts["warn"].(float64) + counts["info"].(float64); ha["findings_open"] != open || open < 1 {
		t.Errorf("ha findings_open = %v, counts say %v", ha["findings_open"], open)
	}
	if ha["notifications_failing"] != false || ha["notifications_failing_since"] != nil {
		t.Errorf("notifications while they work: %v %v", ha["notifications_failing"], ha["notifications_failing_since"])
	}
	for _, k := range []string{"links", "site_links"} {
		if l, ok := ha[k].([]any); !ok || len(l) != 0 {
			t.Errorf("%s without links: %#v, want []", k, ha[k])
		}
	}

	// A finding of a PC that both servers know counts for both; a snoozed
	// one counts for none.
	pc := rec("H7", "", model.Warn, false, "akka", "sol")
	cross := rec("X1", "", model.Info, false)
	hidden := rec("H4", "akka", model.Error, true)
	v := view([]*model.Server{up("akka"), up("sol")}, rec("H1", "sol", model.Error, true), pc, cross, hidden)
	v.Snoozes[hidden.ID] = store.Snooze{FindingID: hidden.ID, Kind: store.IgnoreKind, Reason: "known"}
	out := e.web.haStatusOf(v, nil, nil)
	if a, s := out.Servers["akka"], out.Servers["sol"]; a.FindingsOpen != 1 || a.FindingsUrgent != 0 || s.FindingsOpen != 2 || s.FindingsUrgent != 1 {
		t.Errorf("per server: akka %d/%d, sol %d/%d; want 1/0 and 2/1", a.FindingsOpen, a.FindingsUrgent, s.FindingsOpen, s.FindingsUrgent)
	}
	if out.FindingsOpen != 3 || out.FindingsUrgent != 1 || out.FindingsError != 1 || out.FindingsWarn != 1 || out.FindingsInfo != 1 {
		t.Errorf("top: %+v", out)
	}
	if a := out.Servers["akka"]; a.Status != "up_to_date" {
		t.Errorf("an ignored H4 without a folder in error: %s", a.Status)
	}
	e.setDelivery(notify.Delivery{Failing: true, Since: time.Date(2026, 10, 11, 21, 4, 0, 0, time.UTC), Error: "discord: HTTP 404"})
	_, ha = e.apiGet("/api/v1/ha", "Bearer swt_test_token", "")
	if ha["notifications_failing"] != true || ha["notifications_failing_since"] != "2026-10-11T21:04:00Z" {
		t.Errorf("notifications fail: %v %v", ha["notifications_failing"], ha["notifications_failing_since"])
	}
	if b, _ := json.Marshal(ha); strings.Contains(string(b), "HTTP 404") {
		t.Error("/api/v1/ha has the error text")
	}
}

// W-057.api, W-052: the sites and the links in both answers, from a
// hand-made view (the engine fills the links).
func TestAPISitesAndLinks(t *testing.T) {
	e := adminEnv(t, `servers: [{name: AKKA, url: "https://a:1", site: IVAR Office}, {name: SOL, url: "https://b:1", site: Branch office}, {name: SIRIUS, url: "https://c:1", disabled: true}]
site: IVAR Office
api: {token: swt_test_token}
`)
	since := time.Date(2026, 10, 11, 6, 12, 0, 0, time.UTC)
	v := view([]*model.Server{up("akka"), up("sol")})
	v.Snapshot.Links = []model.ServerLink{{A: "akka", B: "sol", State: model.LinkConnected, Since: since}}
	v.Snapshot.SiteLinks = []model.SiteLink{{A: "IVAR Office", B: "Branch office", State: model.LinkConnected, Since: since, Servers: [][2]string{{"akka", "sol"}}}}
	b, _ := json.Marshal(e.web.apiStatusOf(v, false))
	var st map[string]any
	_ = json.Unmarshal(b, &st)
	if got := string(mustJSON(t, st["links"])); got != `[{"a":"akka","b":"sol","since":"2026-10-11T06:12:00Z","site_a":"IVAR Office","site_b":"Branch office","state":"connected"}]` {
		t.Errorf("links: %s", got)
	}
	if got := string(mustJSON(t, st["site_links"])); got != `[{"a":"IVAR Office","b":"Branch office","since":"2026-10-11T06:12:00Z","state":"connected"}]` {
		t.Errorf("site_links: %s", got)
	}
	svs := apiServers(st)
	if svs["akka"]["site"] != "IVAR Office" || svs["sol"]["site"] != "Branch office" || svs["sirius"]["site"] != "" {
		t.Errorf("servers[].site: %v %v %v", svs["akka"]["site"], svs["sol"]["site"], svs["sirius"]["site"])
	}
	v.Snapshot.Links[0].State, v.Snapshot.Links[0].Since = model.LinkUnreachable, time.Time{}
	ha := e.web.haStatusOf(v, nil, nil)
	if len(ha.Links) != 1 || ha.Links[0].State != "unreachable" || ha.Links[0].Since != nil || ha.Links[0].SiteB != "Branch office" || len(ha.SiteLinks) != 1 {
		t.Errorf("ha links: %+v %+v", ha.Links, ha.SiteLinks)
	}
	if ha.Servers["akka"].Site != "IVAR Office" || ha.Servers["sirius"].Site != "" || ha.Servers["sirius"].Status != "disabled" {
		t.Errorf("ha sites: %+v", ha.Servers)
	}
	// Through the handlers: the keys are there without links too.
	_, body := e.apiGet("/api/v1/status", "Bearer swt_test_token", "")
	if _, ok := body["links"].([]any); !ok {
		t.Errorf("links without links: %#v", body["links"])
	}
	if _, ok := apiServers(body)["sol"]["site"]; !ok {
		t.Error("servers[].site is missing")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// W-036: findings[].snooze_until and snooze_reason: the end and the reason
// of a snooze; null and the reason for an ignore; null and "" without a
// snooze. snoozed, ignored and counts do not change.
func TestAPISnoozeUntilAndReason(t *testing.T) {
	e := adminEnv(t, twoServers) // two H1 findings
	// The H1 of SOL stays open: AKKA comes up below.
	var sol string
	for _, r := range e.eng.View().Open {
		if r.Check == "H1" && r.Server == "sol" {
			sol = r.ID
		}
	}
	until := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	if err := e.eng.Snooze(sol, until, "VPN work tonight", RoleAdmin); err != nil {
		t.Fatal(err)
	}
	e.hub.Update("akka", func(s *model.Server) {
		s.Status, s.Loaded, s.LastContact = model.StatusUp, true, time.Now()
		s.PendingDevices = []model.PendingDevice{{DeviceID: "CCCCCCC-DDDDDDD", Name: "LAPTOP-X", Address: "10.0.0.9:22000", Time: time.Now()}}
	})
	e.eng.Evaluate()
	var h9 string
	for _, r := range e.eng.View().Open {
		if r.Check == "H9" {
			h9 = r.ID
			_ = e.eng.Ignore(r.ID, "a guest laptop", RoleAdmin)
		}
	}
	_, st := e.apiGet("/api/v1/status", "Bearer swt_test_token", "")
	findings, _ := st["findings"].([]any)
	got := map[string]map[string]any{}
	for _, x := range findings {
		f, _ := x.(map[string]any)
		got[f["id"].(string)] = f
	}
	if f := got[sol]; f == nil || f["snoozed"] != true || f["ignored"] != false || f["snooze_until"] != until.UTC().Format(time.RFC3339) || f["snooze_reason"] != "VPN work tonight" {
		t.Errorf("snoozed: %v", f)
	}
	if f := got[h9]; f == nil || f["snoozed"] != true || f["ignored"] != true || f["snooze_until"] != nil || f["snooze_reason"] != "a guest laptop" {
		t.Errorf("ignored: %v", f)
	} else if _, ok := f["snooze_until"]; !ok {
		t.Error("ignored: no key snooze_until")
	}
	for id, f := range got {
		if id == sol || id == h9 {
			continue
		}
		if f["snoozed"] != false || f["snooze_until"] != nil || f["snooze_reason"] != "" {
			t.Errorf("not snoozed: %v", f)
		}
	}
	counts, _ := st["counts"].(map[string]any)
	if counts["snoozed"] != 2.0 || counts["ignored"] != 1.0 {
		t.Errorf("counts: %v", counts)
	}
}
