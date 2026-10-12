package web

// Tests of the pre-release review (stage pp, area auth): the last use of an
// API token during a replace (tokens-2), the login limit for many addresses
// (sessions-2), the length of an administrator session (sessions-3) and the
// login of an administrator in a browser with a viewer session (sessions-4).

import (
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/store"
)

// tokens-2: a request reads the API token with the old value. Then the
// administrator selects Replace, and then the request records its use. The
// use must not go to the new value: no program sent it, and the card is
// what the administrator reads to see if Home Assistant has the new value.
func TestAPITokenUseDuringReplace(t *testing.T) {
	e := adminEnv(t, twoServers+"timezone: UTC\n")
	old := e.addToken("Home Assistant", store.ScopeHA)
	id := e.tokenID("Home Assistant")
	at := time.Date(2026, 10, 12, 9, 6, 0, 0, time.UTC)
	const value = "swt_new_value_of_the_replace"
	// The first time that a request with an API token asks for the time is
	// the record of its use (touchAPIToken), after the read of the row.
	var replaced atomic.Bool
	e.web.now = func() time.Time {
		if replaced.CompareAndSwap(false, true) {
			if err := e.st.ReplaceAPIToken(id, value, at); err != nil {
				t.Error(err)
			}
		}
		return at
	}
	if code, body := call(t, http.DefaultClient, e.srv.URL+"/api/v1/ha", old); code != http.StatusOK || !replaced.Load() {
		t.Fatalf("the request with the old value: %d %s, replace during the request %v", code, body, replaced.Load())
	}
	if tok, err := e.st.APITokenByValue(value); err != nil || tok == nil || !tok.LastUsed.IsZero() {
		t.Fatalf("the new value after the request with the old value: %+v %v, want no last use", tok, err)
	}
	_, page := e.get("/settings")
	if api := section(t, page, "api"); !strings.Contains(api, `<dt>Last used</dt><dd>never</dd>`) {
		t.Errorf("the card after the replace: %s", api)
	}
	if code, _ := call(t, http.DefaultClient, e.srv.URL+"/api/v1/ha", old); code != http.StatusUnauthorized {
		t.Errorf("the old value after the replace: %d", code)
	}
	// The new value records its own use.
	if code, _ := call(t, http.DefaultClient, e.srv.URL+"/api/v1/ha", value); code != http.StatusOK {
		t.Fatalf("the new value: %d", code)
	}
	if tok, _ := e.st.APITokenByValue(value); tok == nil || !tok.LastUsed.Equal(at) {
		t.Errorf("the new value after its own request: %+v", tok)
	}
}

// sessions-2: guesses from many addresses at the same moment. Each attempt
// waits its delay, but the attempts wait at the same time: the delay alone
// does not slow them down. While the slow-down is active, each address gets
// 1 attempt in the window. A burst after a quiet window has no failures yet:
// the attempts still being checked count too.
func TestLimiterSlowsDownGuessesFromManyAddresses(t *testing.T) {
	now := time.Now()
	addr := func(i int) string { return fmt.Sprintf("10.1.%d.%d", i/250, i%250) }
	l := newLimiter()
	for i := 0; i < l.slowAll; i++ {
		ip := fmt.Sprintf("10.0.0.%d", i/l.maxIP)
		l.check(ip, now)
		l.fail(ip, now, RoleAdmin)
	}
	passed := 0
	for i := 0; i < 250; i++ {
		for range 4 {
			if ok, _ := l.check(addr(i), now); ok {
				passed++
			}
		}
	}
	if passed != 250 {
		t.Fatalf("250 addresses with 4 attempts each while the slow-down is active: %d attempts passed, want 250 (1 for each address)", passed)
	}
	for i := 0; i < 250; i++ {
		l.fail(addr(i), now, RoleAdmin)
	}
	later := now.Add(time.Minute)
	if ok, _ := l.check(addr(7), later); ok {
		t.Fatal("a second attempt after a failure while the slow-down is active")
	}
	// The message of a refused confirmation gives the wait of this limit.
	if d := l.retryAfter(addr(7), later); d != 9*time.Minute {
		t.Fatalf("retry after %v, want 9m", d)
	}
	// The administrator at an address without a failure gets the attempt.
	if ok, delay := l.check("10.9.9.9", later); !ok || delay != l.maxDelay {
		t.Fatalf("an address without a failure: ok=%v delay=%v", ok, delay)
	}
	l.success("10.9.9.9", RoleAdmin)

	// A burst after a quiet window: 250 addresses with 5 attempts each.
	l = newLimiter()
	passed = 0
	for range 5 {
		for i := 0; i < 250; i++ {
			if ok, _ := l.check(addr(i), now); ok {
				passed++
			}
		}
	}
	if passed > l.slowAll+250 {
		t.Fatalf("a burst of 250 addresses with 5 attempts each after a quiet window: %d attempts passed, want at most %d", passed, l.slowAll+250)
	}
	// When the limiter forgets the addresses (more than 10000), the count
	// of the attempts still being checked starts again at 0. Else the
	// attempts that end after it would keep the limit at 1 for ever.
	for i := 0; i <= 10000; i++ {
		l.fail(fmt.Sprintf("10.2.%d.%d", i/250, i%250), now, RoleAdmin)
	}
	for i := 0; i < 250; i++ {
		l.fail(addr(i), now, RoleAdmin)
	}
	after := now.Add(l.window + time.Second)
	for i := range l.maxIP {
		if ok, _ := l.check("10.9.9.9", after); !ok {
			t.Fatalf("attempt %d after the window: refused", i+1)
		}
	}
}

// sessions-2 through the login form: 40 addresses send 3 wrong passwords
// each at the same moment while the slow-down is active. syncwatch checks 1
// password for each address and refuses the others (429). The administrator
// at another address logs in.
func TestLoginSlowDownWithParallelGuessesFromManyAddresses(t *testing.T) {
	e := newEnv(t, twoServers)
	h, _ := HashPassword("correct horse battery")
	_ = e.st.SetSetting(KeyAdminHash, h)
	l := e.web.limiter
	l.step, l.maxDelay = time.Millisecond, 5*time.Millisecond // short delays for the test; the rule stays
	now := time.Now()
	for i := 0; i < l.slowAll; i++ {
		ip := fmt.Sprintf("10.0.0.%d", i/l.maxIP)
		l.check(ip, now)
		l.fail(ip, now, RoleAdmin)
	}
	// Each request has its own client address: a device on the LAN with
	// many IPv4 addresses.
	login := func(ip, password string) int {
		form := url.Values{"csrf": {"pp-csrf-token"}, "password": {password}}
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
		req.Host, req.RemoteAddr = "127.0.0.1", ip+":40000"
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: csrfCookie, Value: "pp-csrf-token"})
		rec := httptest.NewRecorder()
		e.web.ServeHTTP(rec, req)
		return rec.Code
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[int]int{}
	start := make(chan struct{})
	for i := range 40 {
		for j := range 3 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				code := login(fmt.Sprintf("10.1.0.%d", i), fmt.Sprintf("guess %d %d", i, j))
				mu.Lock()
				codes[code]++
				mu.Unlock()
			}()
		}
	}
	close(start)
	wg.Wait()
	if codes[http.StatusUnauthorized] != 40 || codes[http.StatusTooManyRequests] != 80 {
		t.Fatalf("40 addresses with 3 wrong passwords each at the same moment: %v, want 40 checked (401) and 80 refused (429)", codes)
	}
	if code := login("10.9.9.9", "correct horse battery"); code != http.StatusSeeOther {
		t.Fatalf("the administrator at another address: %d", code)
	}
}

// sessions-3: access.session_ttl 365d for a lobby screen (README) gives the
// viewer session 365 days. An administrator session ends after 30 days at
// most: a browser on a shared PC that an administrator did not log out is
// not an administrator for a year. A session of an older version with a
// longer life also ends after 30 days.
func TestAdminSessionLasts30DaysAtMost(t *testing.T) {
	e := newEnv(t, twoServers+"access: {viewer: password, session_ttl: 365d}\n")
	ah, _ := HashPassword("correct horse battery")
	vh, _ := HashPassword("viewer password 1")
	_ = e.st.SetSetting(KeyAdminHash, ah)
	_ = e.st.SetSetting(KeyViewerHash, vh)
	start := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	var clock atomic.Int64
	clock.Store(start.UnixNano())
	e.web.now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	day := func(n int) { clock.Store(start.Add(time.Duration(n) * 24 * time.Hour).UnixNano()) }
	// login logs a new browser in and returns the browser and its session cookie.
	login := func(password string) (*http.Client, *http.Cookie) {
		t.Helper()
		jar, _ := cookiejar.New(nil)
		c := &http.Client{Jar: jar, CheckRedirect: e.http.CheckRedirect}
		_, body := pageOf(t, c, e.srv.URL+"/login")
		resp, err := c.PostForm(e.srv.URL+"/login", url.Values{"csrf": {csrfRe.FindStringSubmatch(body)[1]}, "password": {password}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		for _, ck := range resp.Cookies() {
			if ck.Name == sessionCookie {
				return c, ck
			}
		}
		t.Fatalf("login: %d without a session cookie", resp.StatusCode)
		return nil, nil
	}
	lobby, vc := login("viewer password 1")
	if vc.MaxAge != 365*24*3600 {
		t.Errorf("viewer session cookie: Max-Age %d, want 365 days", vc.MaxAge)
	}
	admin, ac := login("correct horse battery")
	if ac.MaxAge != 30*24*3600 || !ac.Expires.Equal(start.Add(maxAdminSession)) {
		t.Errorf("administrator session cookie: Max-Age %d, Expires %v, want 30 days", ac.MaxAge, ac.Expires)
	}
	if z, err := e.st.Session(hashToken(ac.Value), start); err != nil || z == nil || z.Role != RoleAdmin || !z.ExpiresAt.Equal(start.Add(maxAdminSession)) {
		t.Fatalf("administrator session: %+v %v", z, err)
	}
	// A session that an older version made for 365 days.
	if err := e.st.CreateSession(store.Session{TokenHash: hashToken("older version session"), Role: RoleAdmin, CSRF: "x",
		CreatedAt: start, ExpiresAt: start.Add(365 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	older := func() int {
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/settings", nil)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "older version session"})
		resp, err := e.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	day(29)
	if code, _ := pageOf(t, admin, e.srv.URL+"/settings"); code != http.StatusOK {
		t.Fatalf("day 29, administrator: %d", code)
	}
	if code := older(); code != http.StatusOK {
		t.Fatalf("day 29, administrator session of an older version: %d", code)
	}
	day(31)
	if code, _ := pageOf(t, admin, e.srv.URL+"/settings"); code != http.StatusSeeOther {
		t.Errorf("day 31, administrator: %d, want the login (303)", code)
	}
	if code := older(); code != http.StatusSeeOther {
		t.Errorf("day 31, administrator session of an older version: %d, want the login (303)", code)
	}
	day(364)
	if code, _ := pageOf(t, lobby, e.srv.URL+"/lobby"); code != http.StatusOK {
		t.Errorf("day 364, lobby screen: %d", code)
	}
}

// pageOf sends a GET through the browser c and returns the status code and
// the body.
func pageOf(t *testing.T, c *http.Client, u string) (int, string) {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// sessions-4: a browser with a viewer session (the lobby screen, the PC of
// a colleague) opens an administrator page. syncwatch sends it to the login
// form with the reason, and the administrator logs in there: then the
// browser gets the page that it asked for. The viewer password on that form
// sends the browser back to the form, with the reason. A form of a viewer
// session still gets 403.
func TestViewerSessionGetsTheAdministratorLogin(t *testing.T) {
	e := newEnv(t, twoServers+"access: {viewer: password}\n")
	ah, _ := HashPassword("correct horse battery")
	vh, _ := HashPassword("viewer password 1")
	_ = e.st.SetSetting(KeyAdminHash, ah)
	_ = e.st.SetSetting(KeyViewerHash, vh)
	const reason = "This page needs the administrator password."
	if r := e.post("/login", url.Values{"csrf": {e.csrf("/login")}, "password": {"viewer password 1"}, "next": {"/lobby"}}); r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/lobby" {
		t.Fatalf("viewer login: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	toLogin := func(step string) {
		t.Helper()
		resp, _ := e.get("/settings")
		if msg, isErr := flash(resp); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login?next=/settings" || msg != reason || !isErr {
			t.Fatalf("%s: GET /settings: %d %q, flash %q %v; want the login form with the reason", step, resp.StatusCode, resp.Header.Get("Location"), msg, isErr)
		}
		resp, body := e.get("/login?next=/settings")
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, `<div class="flash err" role="alert">✖ `+reason+`</div>`) ||
			!strings.Contains(body, `name="password"`) || !strings.Contains(body, `<input type="hidden" name="next" value="/settings">`) {
			t.Fatalf("%s: GET /login?next=/settings: %d, want the form with the reason:\n%s", step, resp.StatusCode, body)
		}
	}
	toLogin("viewer session")
	if r := e.post("/login", url.Values{"csrf": {e.csrf("/login?next=/settings")}, "password": {"viewer password 1"}, "next": {"/settings"}}); r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/settings" {
		t.Fatalf("viewer password on the form: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	toLogin("the viewer password on the form")
	if r := e.post("/login", url.Values{"csrf": {e.csrf("/login?next=/settings")}, "password": {"correct horse battery"}, "next": {"/settings"}}); r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/settings" {
		t.Fatalf("administrator password on the form: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	if resp, _ := e.get("/settings"); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /settings after the administrator login: %d", resp.StatusCode)
	}
	if resp, _ := e.get("/login?next=/problems"); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/problems" {
		t.Errorf("/login with an administrator session: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}

	// A form of a viewer session: 403, as before.
	viewer := e.loginClient("viewer password 2", RoleViewer)
	_, page := pageOf(t, viewer, e.srv.URL+"/")
	resp, err := viewer.PostForm(e.srv.URL+"/settings/servers", url.Values{"csrf": {csrfRe.FindStringSubmatch(page)[1]}, "name": {"X"}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(b), reason) {
		t.Errorf("a form of a viewer session: %d, want 403 with the reason", resp.StatusCode)
	}

	// Open access without an administrator password: the first start.
	open := newEnv(t, twoServers+"access: {viewer: open}\n")
	if resp, _ := open.get("/settings"); resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/setup" {
		t.Errorf("open access, no administrator password: GET /settings: %d %q, want /setup", resp.StatusCode, resp.Header.Get("Location"))
	}
}
