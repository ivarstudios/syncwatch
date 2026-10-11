package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/engine"
	"github.com/ivarstudios/syncwatch/internal/notify"
	"github.com/ivarstudios/syncwatch/internal/store"
)

// healthz returns the status and the body of /healthz.
func (e *env) healthz() (int, string) {
	e.t.Helper()
	resp, body := e.get("/healthz")
	return resp.StatusCode, body
}

// /healthz answers 503 when the notifications fail for more than the limit
// (SYNCWATCH_HEALTH_NOTIFY_LIMIT), with the time and without the error text
// of Discord: the address needs no login. A shorter failure is healthy.
func TestHealthzNotificationFailure(t *testing.T) {
	e := newEnv(t, twoServers)
	e.web.SetHealth(HealthOptions{Started: time.Now().Add(-time.Hour), NotifyLimit: 15 * time.Minute, EvalLimit: 5 * time.Minute})
	if code, body := e.healthz(); code != http.StatusOK || body != "ok\n" {
		t.Fatalf("healthy: %d %q", code, body)
	}
	since := time.Now().Add(-16 * time.Minute)
	e.setDelivery(notify.Delivery{Failing: true, Since: since, Error: "Discord: HTTP 404 Unknown Webhook (secret-hook)"})
	code, body := e.healthz()
	if code != http.StatusServiceUnavailable || body != "notifications fail from "+e.web.fmtTime(since)+"\n" {
		t.Fatalf("16 minutes of failed notifications: %d %q", code, body)
	}
	if strings.Contains(body, "Discord") || strings.Contains(body, "secret-hook") {
		t.Fatalf("the error text of Discord is in the answer: %q", body)
	}
	e.setDelivery(notify.Delivery{Failing: true, Since: time.Now().Add(-14 * time.Minute), Error: "Discord: HTTP 500"})
	if code, body := e.healthz(); code != http.StatusOK {
		t.Fatalf("14 minutes of failed notifications: %d %q", code, body)
	}
	// The rule is off without a limit (tests and older callers).
	e.web.SetHealth(HealthOptions{})
	e.setDelivery(notify.Delivery{Failing: true, Since: time.Now().Add(-48 * time.Hour)})
	if code, body := e.healthz(); code != http.StatusOK {
		t.Fatalf("without the rule: %d %q", code, body)
	}
}

// /healthz answers 503 when no evaluation of the checks completed for more
// than the limit: counted from the last evaluation, and from the start until
// the first one.
func TestHealthzEvaluations(t *testing.T) {
	e := newEnv(t, twoServers)
	evaluated := e.eng.View().Evaluated
	if evaluated.IsZero() {
		t.Fatal("the test engine did not evaluate")
	}
	e.web.SetHealth(HealthOptions{Started: evaluated.Add(-time.Minute), NotifyLimit: 15 * time.Minute, EvalLimit: 5 * time.Minute})
	e.web.now = func() time.Time { return evaluated.Add(4 * time.Minute) }
	if code, body := e.healthz(); code != http.StatusOK {
		t.Fatalf("4 minutes after the last evaluation: %d %q", code, body)
	}
	e.web.now = func() time.Time { return evaluated.Add(6 * time.Minute) }
	if code, body := e.healthz(); code != http.StatusServiceUnavailable || body != "no evaluation of the checks from "+e.web.fmtTime(evaluated)+"\n" {
		t.Fatalf("6 minutes after the last evaluation: %d %q", code, body)
	}

	// An engine that did not evaluate yet: counted from the start.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng, err := engine.New(e.st, e.hub, e.cfg, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(e.cfg, e.st, eng, Hooks{}, "test", log)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	s.SetHealth(HealthOptions{Started: started, EvalLimit: 5 * time.Minute})
	ctx := t.Context()
	s.now = func() time.Time { return started.Add(4 * time.Minute) }
	if r := s.Ready(ctx); len(r) != 0 {
		t.Fatalf("4 minutes after the start without an evaluation: %q", r)
	}
	s.now = func() time.Time { return started.Add(6 * time.Minute) }
	if r := s.Ready(ctx); len(r) != 1 || r[0] != "no evaluation of the checks from "+s.fmtTime(started) {
		t.Fatalf("6 minutes after the start without an evaluation: %q", r)
	}
}

// A database that does not answer makes /healthz answer 503, with one line
// for each reason.
func TestHealthzDatabaseAndReasons(t *testing.T) {
	e := newEnv(t, twoServers)
	evaluated := e.eng.View().Evaluated
	e.web.SetHealth(HealthOptions{Started: evaluated, NotifyLimit: 15 * time.Minute, EvalLimit: 5 * time.Minute})
	e.web.now = func() time.Time { return evaluated.Add(20 * time.Minute) }
	e.setDelivery(notify.Delivery{Failing: true, Since: evaluated.Add(time.Minute)})
	if err := e.st.Close(); err != nil {
		t.Fatal(err)
	}
	code, body := e.healthz()
	want := "database not available\nno evaluation of the checks from " + e.web.fmtTime(evaluated) + "\nnotifications fail from " + e.web.fmtTime(evaluated.Add(time.Minute)) + "\n"
	if code != http.StatusServiceUnavailable || body != want {
		t.Fatalf("closed database: %d %q, want %q", code, body, want)
	}
}

// client is a browser of its own for a dashboard URL: a cookie jar, no
// redirects followed.
type client struct {
	t    *testing.T
	base string
	c    *http.Client
}

func newClient(t *testing.T, base string, c *http.Client) *client {
	c.Jar, _ = cookiejar.New(nil)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client{t: t, base: base, c: c}
}

// tlsClient serves the dashboard of e over HTTPS (a TLS listener of
// httptest) and returns a browser for it.
func tlsClient(t *testing.T, e *env) *client {
	t.Helper()
	srv := httptest.NewTLSServer(e.web)
	t.Cleanup(srv.Close)
	return newClient(t, srv.URL, srv.Client())
}

// do sends a request with extra headers and returns the answer with its body.
func (b *client) do(method, path string, form url.Values, header map[string]string) (*http.Response, string) {
	b.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, b.base+path, body)
	if err != nil {
		b.t.Fatal(err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, string(out)
}

// token returns the CSRF token of the forms of a page.
func (b *client) token(path string) string {
	b.t.Helper()
	_, body := b.do(http.MethodGet, path, nil, nil)
	m := csrfRe.FindStringSubmatch(body)
	if m == nil {
		b.t.Fatalf("no CSRF token on %s", path)
	}
	return m[1]
}

// login logs in with the administrator password of adminEnv.
func (b *client) login() {
	b.t.Helper()
	if r, _ := b.do(http.MethodPost, "/login", url.Values{"csrf": {b.token("/login")}, "password": {"correct horse battery"}}, nil); r.StatusCode != http.StatusSeeOther {
		b.t.Fatalf("login: %d", r.StatusCode)
	}
}

// cookies returns the cookies that an answer sets, by name.
func cookies(resp *http.Response) map[string]*http.Cookie {
	out := map[string]*http.Cookie{}
	for _, c := range resp.Cookies() {
		out[c.Name] = c
	}
	return out
}

// sessionOf returns the value of the plain session cookie of e's browser.
func (e *env) sessionOf() string {
	e.t.Helper()
	u, _ := url.Parse(e.srv.URL)
	for _, c := range e.jar.Cookies(u) {
		if c.Name == sessionCookie {
			return c.Value
		}
	}
	e.t.Fatal("no plain session cookie over HTTP")
	return ""
}

// X-Forwarded-Proto counts only from a trusted reverse proxy
// (SYNCWATCH_TRUSTED_PROXIES). From another client a plain HTTP dashboard
// keeps plain cookies without Secure, and the forms keep working.
func TestForwardedProtoOnlyFromTrustedProxies(t *testing.T) {
	e := newEnv(t, twoServers)
	h, _ := HashPassword("correct horse battery")
	_ = e.st.SetSetting(KeyAdminHash, h)
	b := newClient(t, e.srv.URL, &http.Client{})
	csrfFor := func(proto string) *http.Cookie {
		t.Helper()
		b.c.Jar, _ = cookiejar.New(nil)
		resp, _ := b.do(http.MethodGet, "/login", nil, map[string]string{"X-Forwarded-Proto": proto})
		got := cookies(resp)
		if c := got[hostPrefix+csrfCookie]; c != nil {
			return c
		}
		return got[csrfCookie]
	}
	for _, trusted := range [][]netip.Prefix{nil, {netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("::1/128")}} {
		e.web.TrustProxies(trusted)
		if c := csrfFor("https"); c == nil || c.Name != csrfCookie || c.Secure {
			t.Fatalf("X-Forwarded-Proto https from an address that is not trusted (%v): %+v", trusted, c)
		}
		// The login still works, with plain cookies.
		resp, _ := b.do(http.MethodPost, "/login", url.Values{"csrf": {b.token("/login")}, "password": {"correct horse battery"}}, map[string]string{"X-Forwarded-Proto": "https"})
		if c := cookies(resp)[sessionCookie]; resp.StatusCode != http.StatusSeeOther || c == nil || c.Secure {
			t.Fatalf("login through a proxy that is not trusted: %d %+v", resp.StatusCode, c)
		}
	}
	e.web.TrustProxies([]netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")})
	if c := csrfFor("https"); c == nil || c.Name != hostPrefix+csrfCookie || !c.Secure {
		t.Fatalf("X-Forwarded-Proto https from a trusted proxy: %+v", c)
	}
	for proto, secure := range map[string]bool{"http": false, "": false, "HTTPS": true, "https, http": false, "http,https": true} {
		if c := csrfFor(proto); c == nil || c.Secure != secure {
			t.Errorf("X-Forwarded-Proto %q from a trusted proxy: %+v, want Secure %v", proto, c, secure)
		}
	}

	// IPv4 clients in IPv6 form, and IPv4 prefixes in IPv6 form.
	s := e.web
	s.TrustProxies([]netip.Prefix{netip.MustParsePrefix("::ffff:192.0.2.0/120")})
	for addr, want := range map[string]string{"192.0.2.7:4000": "https", "[::ffff:192.0.2.7]:4000": "https", "192.0.3.7:4000": "http", "[2001:db8::7]:4000": "http"} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = addr
		r.Header.Set("X-Forwarded-Proto", "https")
		if got := s.requestScheme(r); got != want {
			t.Errorf("client %s: %s, want %s", addr, got, want)
		}
	}
	if r := httptest.NewRequest(http.MethodGet, "/", nil); s.requestScheme(r) != "http" {
		t.Error("a request without the header is not http")
	}
}

// Over HTTPS the cookies are __Host-sw_session and __Host-sw_csrf (Secure,
// Path /, no Domain) and only these names are read: the session of an older
// version (plain name) ends. The login makes a new CSRF token and clears the
// plain names. The logout clears both forms of both cookies.
func TestHostCookiesOverHTTPS(t *testing.T) {
	e := newEnv(t, twoServers)
	h, _ := HashPassword("correct horse battery")
	_ = e.st.SetSetting(KeyAdminHash, h)
	b := tlsClient(t, e)
	resp, body := b.do(http.MethodGet, "/login", nil, nil)
	got := cookies(resp)
	anon := got[hostPrefix+csrfCookie]
	if anon == nil || !anon.Secure || anon.Path != "/" || anon.Domain != "" || !anon.HttpOnly || anon.SameSite != http.SameSiteLaxMode || got[csrfCookie] != nil {
		t.Fatalf("CSRF cookie over HTTPS: %+v (all: %v)", anon, resp.Header.Values("Set-Cookie"))
	}
	if !strings.Contains(body, `value="`+anon.Value+`"`) {
		t.Fatal("the form has another CSRF token than the cookie")
	}
	resp, _ = b.do(http.MethodPost, "/login", url.Values{"csrf": {anon.Value}, "password": {"correct horse battery"}}, nil)
	got = cookies(resp)
	sess, csrf := got[hostPrefix+sessionCookie], got[hostPrefix+csrfCookie]
	if resp.StatusCode != http.StatusSeeOther || sess == nil || !sess.Secure || sess.Path != "/" || sess.Domain != "" || !sess.HttpOnly {
		t.Fatalf("login: %d, session cookie %+v", resp.StatusCode, sess)
	}
	if csrf == nil || !csrf.Secure || csrf.Value == anon.Value {
		t.Fatalf("the login did not make a new CSRF cookie: %+v", csrf)
	}
	for _, name := range []string{sessionCookie, csrfCookie} {
		if c := got[name]; c == nil || c.MaxAge >= 0 || !c.Secure {
			t.Errorf("the login did not clear the old cookie %s: %+v", name, c)
		}
	}
	if z, err := e.st.Session(hashToken(sess.Value), time.Now()); err != nil || z == nil || z.CSRF != csrf.Value {
		t.Fatalf("the session has not the CSRF token of the cookie: %+v %v", z, err)
	}
	if b.token("/settings") != csrf.Value {
		t.Fatal("the forms have another CSRF token than the session")
	}

	// A session of an older version, with the plain cookie name: over HTTPS
	// it does not count.
	old := randomToken(32)
	if err := e.st.CreateSession(store.Session{TokenHash: hashToken(old), Role: RoleAdmin, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	b2 := tlsClient(t, e)
	if r, _ := b2.do(http.MethodGet, "/settings", nil, map[string]string{"Cookie": sessionCookie + "=" + old}); r.StatusCode != http.StatusSeeOther || !strings.HasPrefix(r.Header.Get("Location"), "/login") {
		t.Fatalf("a plain session cookie over HTTPS: %d %q", r.StatusCode, r.Header.Get("Location"))
	}

	// Log out: both forms of both cookies are cleared, the session ends.
	resp, _ = b.do(http.MethodPost, "/logout", url.Values{"csrf": {csrf.Value}}, nil)
	got = cookies(resp)
	for _, name := range []string{sessionCookie, csrfCookie, hostPrefix + sessionCookie, hostPrefix + csrfCookie} {
		if c := got[name]; c == nil || c.MaxAge >= 0 || !c.Secure || c.Path != "/" {
			t.Errorf("logout did not clear %s: %+v", name, c)
		}
	}
	if z, _ := e.st.Session(hashToken(sess.Value), time.Now()); resp.StatusCode != http.StatusSeeOther || z != nil {
		t.Fatalf("logout: %d, the session stays: %+v", resp.StatusCode, z)
	}
}

// Over plain HTTP the cookies keep their names without Secure, and the
// logout clears only the plain names (a browser refuses a __Host- cookie
// over HTTP).
func TestPlainCookiesOverHTTP(t *testing.T) {
	e := adminEnv(t, twoServers)
	sess := e.sessionOf()
	resp := e.post("/logout", url.Values{"csrf": {e.csrf("/")}})
	got := cookies(resp)
	if c := got[sessionCookie]; resp.StatusCode != http.StatusSeeOther || c == nil || c.MaxAge >= 0 || c.Secure {
		t.Fatalf("logout over HTTP: %d %+v", resp.StatusCode, c)
	}
	if c := got[csrfCookie]; c == nil || c.MaxAge >= 0 {
		t.Fatalf("logout over HTTP did not clear the CSRF cookie: %+v", c)
	}
	for name := range got {
		if strings.HasPrefix(name, hostPrefix) {
			t.Errorf("logout over HTTP sets %s", name)
		}
	}
	if z, _ := e.st.Session(hashToken(sess), time.Now()); z != nil {
		t.Fatal("the session stays after the logout")
	}
}

// The Origin rules of a form: Origin null is refused; over HTTPS an http
// Origin is refused (a downgrade); over plain HTTP (or behind a proxy that
// is not trusted) both schemes of the host pass. Another host is refused,
// as before.
func TestCSRFOriginRules(t *testing.T) {
	e := adminEnv(t, twoServers)
	u, _ := url.Parse(e.srv.URL)
	post := func(origin string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/problems/zz-no-such-finding/unsnooze", strings.NewReader(url.Values{"csrf": {e.csrf("/problems")}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := e.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for origin, want := range map[string]int{
		"": http.StatusSeeOther, "http://" + u.Host: http.StatusSeeOther, "https://" + u.Host: http.StatusSeeOther,
		"null": http.StatusForbidden, "https://evil.invalid": http.StatusForbidden, "http://evil.invalid:" + u.Port(): http.StatusForbidden,
		"http://" + u.Host + "/x": http.StatusForbidden, "ftp://" + u.Host: http.StatusForbidden, "http://user@" + u.Host: http.StatusForbidden,
	} {
		if got := post(origin); got != want {
			t.Errorf("plain HTTP, Origin %q: %d, want %d", origin, got, want)
		}
	}

	b := tlsClient(t, e)
	b.login()
	host := strings.TrimPrefix(b.base, "https://")
	for origin, want := range map[string]int{
		"": http.StatusSeeOther, "https://" + host: http.StatusSeeOther,
		"null": http.StatusForbidden, "http://" + host: http.StatusForbidden, "https://evil.invalid": http.StatusForbidden,
	} {
		h := map[string]string{}
		if origin != "" {
			h["Origin"] = origin
		}
		resp, body := b.do(http.MethodPost, "/problems/zz-no-such-finding/unsnooze", url.Values{"csrf": {b.token("/problems")}}, h)
		if resp.StatusCode != want {
			t.Errorf("HTTPS, Origin %q: %d %q, want %d", origin, resp.StatusCode, body, want)
		}
	}
	// A trusted TLS proxy gives the HTTPS rules to a plain HTTP request. (The
	// browser talks HTTPS to the proxy: the test sends its cookie itself.)
	e.web.TrustProxies([]netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")})
	c := newClient(t, e.srv.URL, &http.Client{})
	resp, body := c.do(http.MethodGet, "/login", nil, map[string]string{"X-Forwarded-Proto": "https"})
	pair := cookies(resp)[hostPrefix+csrfCookie]
	if pair == nil || !strings.Contains(body, `value="`+pair.Value+`"`) {
		t.Fatalf("no __Host- CSRF cookie behind a trusted TLS proxy: %v", resp.Header.Values("Set-Cookie"))
	}
	for origin, want := range map[string]int{"http://" + u.Host: http.StatusForbidden, "https://" + u.Host: http.StatusSeeOther} {
		resp, _ := c.do(http.MethodPost, "/login", url.Values{"csrf": {pair.Value}, "password": {"correct horse battery"}},
			map[string]string{"X-Forwarded-Proto": "https", "Origin": origin, "Cookie": pair.Name + "=" + pair.Value})
		if resp.StatusCode != want {
			t.Errorf("behind a trusted TLS proxy, Origin %q: %d, want %d", origin, resp.StatusCode, want)
		}
	}
}

// The CSRF token belongs to the session: a CSRF cookie and form token that
// another page got without a login (an anonymous pair) do not work with an
// administrator session. A session of an older version, without a CSRF
// token, uses the cookie as before.
func TestCSRFTokenBoundToSession(t *testing.T) {
	e := adminEnv(t, twoServers)
	sess := e.sessionOf()
	other := newClient(t, e.srv.URL, &http.Client{})
	anon := other.token("/login")
	send := func(cookie, form string) int {
		t.Helper()
		resp, _ := other.do(http.MethodPost, "/problems/zz-no-such-finding/unsnooze", url.Values{"csrf": {form}},
			map[string]string{"Cookie": sessionCookie + "=" + sess + "; " + csrfCookie + "=" + cookie})
		return resp.StatusCode
	}
	if got := send(anon, anon); got != http.StatusForbidden {
		t.Fatalf("an anonymous CSRF pair with an administrator session: %d, want 403", got)
	}
	own := e.csrf("/problems")
	if got := send(anon, own); got != http.StatusSeeOther {
		t.Fatalf("the token of the session with another CSRF cookie: %d, want 303", got)
	}

	// A session of an older version: the double-submit cookie, as before.
	old := randomToken(32)
	if err := e.st.CreateSession(store.Session{TokenHash: hashToken(old), Role: RoleAdmin, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	sess = old
	if got := send(anon, anon); got != http.StatusSeeOther {
		t.Fatalf("a session without a CSRF token, cookie and form the same: %d, want 303", got)
	}
	if got := send(anon, own); got != http.StatusForbidden {
		t.Fatalf("a session without a CSRF token, cookie and form not the same: %d, want 403", got)
	}
}

// The login makes a new CSRF token: the token of the login page does not
// work after the login, the token of the session does.
func TestCSRFTokenChangesAtLogin(t *testing.T) {
	e := newEnv(t, twoServers)
	h, _ := HashPassword("correct horse battery")
	_ = e.st.SetSetting(KeyAdminHash, h)
	before := e.csrf("/login")
	resp := e.post("/login", url.Values{"csrf": {before}, "password": {"correct horse battery"}})
	after := ""
	if c := cookies(resp)[csrfCookie]; c != nil {
		after = c.Value
	}
	if resp.StatusCode != http.StatusSeeOther || after == "" || after == before {
		t.Fatalf("login: %d, CSRF cookie %q, before %q", resp.StatusCode, after, before)
	}
	if r := e.post("/problems/zz-no-such-finding/unsnooze", url.Values{"csrf": {before}}); r.StatusCode != http.StatusForbidden {
		t.Fatalf("the token from before the login: %d, want 403", r.StatusCode)
	}
	if got := e.csrf("/problems"); got != after {
		t.Fatalf("the forms have %q, the cookie %q", got, after)
	}
	if r := e.post("/problems/zz-no-such-finding/unsnooze", url.Values{"csrf": {after}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("the token of the session: %d, want 303", r.StatusCode)
	}
}
