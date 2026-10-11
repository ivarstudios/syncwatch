package web

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLimiterSlowsDownInsteadOfLockingOut(t *testing.T) {
	l := newLimiter()
	now := time.Now()
	// Failures spread over a few addresses reach the global threshold.
	crossed := false
	for i := 0; i < l.slowAll; i++ {
		crossed = l.fail(fmt.Sprintf("10.0.0.%d", i/l.maxIP), now, RoleAdmin) || crossed
	}
	if !crossed {
		t.Fatal("crossing the threshold was not reported")
	}
	// A fresh address can still log in, only more slowly.
	ok, delay := l.check("10.9.9.9", now)
	if !ok || delay <= 0 || delay > l.maxDelay {
		t.Fatalf("fresh address: ok=%v delay=%v", ok, delay)
	}
	// One address is still capped hard.
	if ok, _ := l.check("10.0.0.0", now); ok {
		t.Fatal("per-address limit not applied")
	}
	// A flood only raises the delay to its maximum and keeps memory bounded.
	for i := 0; i < 10000; i++ {
		l.fail(fmt.Sprintf("10.1.%d.%d", i/250, i%250), now, RoleAdmin)
	}
	if ok, delay := l.check("10.9.9.9", now); !ok || delay != l.maxDelay {
		t.Fatalf("after a flood: ok=%v delay=%v", ok, delay)
	}
	if len(l.global) > l.slowAll+int(l.maxDelay/l.step)+1 {
		t.Fatalf("global failure list grew to %d", len(l.global))
	}
	// After the window, everything is back to normal.
	if ok, delay := l.check("10.0.0.0", now.Add(l.window+time.Second)); !ok || delay != 0 {
		t.Fatalf("after the window: ok=%v delay=%v", ok, delay)
	}
}

func TestSecureCookiesOverHTTPS(t *testing.T) {
	e := newEnv(t, twoServers)
	srv := httptest.NewTLSServer(e.web)
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	found := false
	for _, c := range resp.Cookies() {
		if c.Name == csrfCookie {
			found = true
			if !c.Secure {
				t.Fatal("CSRF cookie not Secure over HTTPS")
			}
		}
	}
	if !found {
		t.Fatal("no CSRF cookie")
	}
}

// Attempts sent at the same moment must not all pass the per-address limit
// before the first failure is recorded.
func TestLimiterReservesAttempts(t *testing.T) {
	l := newLimiter()
	now := time.Now()
	for i := 0; i < l.maxIP; i++ {
		if ok, _ := l.check("10.0.0.1", now); !ok {
			t.Fatalf("attempt %d refused with nothing failed yet", i)
		}
	}
	if ok, _ := l.check("10.0.0.1", now); ok {
		t.Fatal("a sixth attempt passed while five were still being checked")
	}
	if ok, _ := l.check("10.0.0.2", now); !ok {
		t.Fatal("another address was affected")
	}
	l.success("10.0.0.2", RoleAdmin)
	// Attempts that end free their slot; failures keep it taken.
	l.fail("10.0.0.1", now, RoleAdmin)
	if ok, _ := l.check("10.0.0.1", now); ok {
		t.Fatal("attempt passed with four in flight and one failed")
	}
	for i := 0; i < l.maxIP-1; i++ {
		l.success("10.0.0.1", RoleAdmin)
	}
	if ok, _ := l.check("10.0.0.1", now); !ok {
		t.Fatal("attempt refused after the others ended successfully")
	}
	l.fail("10.0.0.1", now, RoleAdmin)
	if _, ok := l.perIP["10.0.0.2"]; ok {
		t.Fatal("an address with nothing against it is remembered")
	}
}

// A successful viewer login clears the viewer failures only: knowing the
// viewer password buys no extra guesses at the admin password.
func TestLimiterCountsRolesSeparately(t *testing.T) {
	l := newLimiter()
	now := time.Now()
	for i := 0; i < l.maxIP-1; i++ {
		l.check("10.0.0.1", now)
		l.fail("10.0.0.1", now, RoleAdmin, RoleViewer)
	}
	l.check("10.0.0.1", now)
	l.success("10.0.0.1", RoleViewer)
	l.check("10.0.0.1", now)
	l.fail("10.0.0.1", now, RoleAdmin, RoleViewer)
	if ok, _ := l.check("10.0.0.1", now); ok {
		t.Fatal("five failed admin guesses did not block the address after a viewer login")
	}
	// The admin's own success forgets the guesses at the admin password.
	l2 := newLimiter()
	for i := 0; i < l2.maxIP-1; i++ {
		l2.check("10.0.0.1", now)
		l2.fail("10.0.0.1", now, RoleAdmin)
	}
	l2.check("10.0.0.1", now)
	l2.success("10.0.0.1", RoleAdmin)
	for i := 0; i < l2.maxIP-1; i++ {
		if ok, _ := l2.check("10.0.0.1", now); !ok {
			t.Fatalf("attempt %d after an admin login refused", i)
		}
		l2.fail("10.0.0.1", now, RoleAdmin)
	}
}

// Forty wrong passwords sent at once from one address: at most five are
// checked, the rest are refused.
func TestParallelLoginsAreLimited(t *testing.T) {
	e := newEnv(t, twoServers)
	h, _ := HashPassword("correct horse battery")
	_ = e.st.SetSetting(KeyAdminHash, h)
	csrf := e.csrf("/login")
	const n = 40
	var wg sync.WaitGroup
	var mu sync.Mutex
	codes := map[int]int{}
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			resp, err := e.http.PostForm(e.srv.URL+"/login", url.Values{"csrf": {csrf}, "password": {fmt.Sprintf("guess %d", i)}})
			if err != nil {
				return
			}
			resp.Body.Close()
			mu.Lock()
			codes[resp.StatusCode]++
			mu.Unlock()
		}(i)
	}
	close(start)
	wg.Wait()
	if codes[http.StatusUnauthorized] > 5 || codes[http.StatusUnauthorized]+codes[http.StatusTooManyRequests] != n {
		t.Fatalf("status counts after %d parallel wrong logins: %v", n, codes)
	}
}

// With a viewer password, guesses at the admin password are still capped at
// five per address: logging in as viewer in between doesn't reset them.
func TestViewerLoginKeepsAdminFailures(t *testing.T) {
	e := newEnv(t, twoServers+"access: {viewer: password}\n")
	ah, _ := HashPassword("correct horse battery")
	vh, _ := HashPassword("viewer password 1")
	_ = e.st.SetSetting(KeyAdminHash, ah)
	_ = e.st.SetSetting(KeyViewerHash, vh)
	csrf := e.csrf("/login")
	login := func(pw string) int {
		return e.post("/login", url.Values{"csrf": {csrf}, "password": {pw}}).StatusCode
	}
	for i := 0; i < 4; i++ {
		if code := login(fmt.Sprintf("admin guess %d", i)); code != http.StatusUnauthorized {
			t.Fatalf("guess %d: %d", i, code)
		}
	}
	if code := login("viewer password 1"); code != http.StatusSeeOther {
		t.Fatalf("viewer login: %d", code)
	}
	if code := login("admin guess 4"); code != http.StatusUnauthorized {
		t.Fatalf("fifth guess: %d", code)
	}
	if code := login("correct horse battery"); code != http.StatusTooManyRequests {
		t.Fatalf("after five wrong guesses and a viewer login: %d, want 429", code)
	}
}

// A page on a host name an attacker controls (DNS rebinding) is same-origin
// with its own name, so the Origin check alone doesn't stop it. Only
// addresses, localhost, the host of public_url and the configured names are
// answered; everything else gets 421 with a hint.
func TestUnknownHostNameRefused(t *testing.T) {
	e := newEnv(t, twoServers+"access: {mode: proxy}\npublic_url: https://dash.home.invalid:8080\n")
	e.web.AllowHosts("nas.lan.invalid:8080", "syncwatch", " Watch.Example.Invalid ")
	get := func(host, path string) (*http.Response, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+path, nil)
		req.Host = host
		resp, err := e.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	for _, host := range []string{"rebind.attacker.invalid", "rebind.attacker.invalid:8080", "localhost.attacker.invalid",
		"dash.home.invalid.attacker.invalid", "nas.lan.invalid.attacker.invalid", "127.0.0.1.attacker.invalid"} {
		for _, path := range []string{"/settings", "/healthz", "/api/v1/status", "/assets/app.js"} {
			resp, body := get(host, path)
			if resp.StatusCode != http.StatusMisdirectedRequest {
				t.Errorf("Host %q, %s: %d", host, path, resp.StatusCode)
			}
			if !strings.Contains(body, "public_url") || !strings.Contains(body, "SYNCWATCH_TLS_HOSTS") {
				t.Errorf("Host %q: no hint in %q", host, body)
			}
		}
	}
	for _, host := range []string{"127.0.0.1:8080", "127.0.0.1", "[::1]:8080", "[::1]", "192.0.2.7:443", "localhost", "LOCALHOST:8080", "localhost.",
		"dash.home.invalid", "DASH.home.invalid:8080", "nas.lan.invalid", "syncwatch:8080", "watch.example.invalid"} {
		if resp, _ := get(host, "/settings"); resp.StatusCode != http.StatusOK {
			t.Errorf("Host %q: %d, want 200", host, resp.StatusCode)
		}
	}
	// A form sent from the rebinding page with its own Origin is refused too.
	csrf := e.csrf("/settings")
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/settings/api-token", strings.NewReader(url.Values{"csrf": {csrf}, "op": {"generate"}}.Encode()))
	req.Host = "rebind.attacker.invalid:8080"
	req.Header.Set("Origin", "http://rebind.attacker.invalid:8080")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := e.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if kept := e.hasAPIToken("swt_test_token"); resp.StatusCode != http.StatusMisdirectedRequest || !kept {
		t.Fatalf("POST from a rebinding page: %d, the API token works: %v", resp.StatusCode, kept)
	}
}

// An anonymous POST to /login or /setup is read only up to 64 KiB before the
// CSRF check, so nobody can make the server read (and spool to disk) a
// large upload without logging in.
func TestLoginBodyIsLimited(t *testing.T) {
	e := newEnv(t, twoServers)
	h, _ := HashPassword("correct horse battery")
	_ = e.st.SetSetting(KeyAdminHash, h)
	for _, path := range []string{"/login", "/setup"} {
		pr, pw := io.Pipe()
		var sent atomic.Int64
		go func() {
			defer pw.Close()
			fmt.Fprintf(pw, "--b\r\nContent-Disposition: form-data; name=\"f\"; filename=\"a.bin\"\r\nContent-Type: application/octet-stream\r\n\r\n")
			chunk := make([]byte, 64<<10)
			for i := 0; i < 256; i++ { // 16 MiB
				n, err := pw.Write(chunk)
				sent.Add(int64(n))
				if err != nil {
					return
				}
			}
			fmt.Fprintf(pw, "\r\n--b--\r\n")
		}()
		req, _ := http.NewRequest(http.MethodPost, e.srv.URL+path, pr)
		req.Header.Set("Content-Type", "multipart/form-data; boundary=b")
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusSeeOther {
				t.Fatalf("%s: %d for a body without a CSRF token", path, resp.StatusCode)
			}
		}
		if n := sent.Load(); n > 4<<20 {
			t.Fatalf("%s: the server read %d MiB of an anonymous upload", path, n>>20)
		}
	}
	// A normal login still works.
	csrf := e.csrf("/login")
	if r := e.post("/login", url.Values{"csrf": {csrf}, "password": {"correct horse battery"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", r.StatusCode)
	}
}

// A link clicked in Discord is a cross-site visit. With SameSite=Strict the
// browser kept the session cookie at home and showed the login page, and a
// fresh CSRF cookie then broke the forms in other tabs. Lax sends both on
// such top-level visits and still keeps them off cross-site POSTs.
func TestCookiesAreSameSiteLax(t *testing.T) {
	e := newEnv(t, twoServers)
	h, _ := HashPassword("correct horse battery")
	_ = e.st.SetSetting(KeyAdminHash, h)
	got := map[string]http.SameSite{}
	resp, _ := e.get("/login")
	for _, c := range resp.Cookies() {
		got[c.Name] = c.SameSite
	}
	csrf := e.csrf("/login")
	for _, c := range e.post("/login", url.Values{"csrf": {csrf}, "password": {"correct horse battery"}}).Cookies() {
		got[c.Name] = c.SameSite
	}
	for _, name := range []string{csrfCookie, sessionCookie} {
		if got[name] != http.SameSiteLaxMode {
			t.Errorf("%s: SameSite %v, want Lax", name, got[name])
		}
	}
	for _, c := range e.post("/logout", url.Values{"csrf": {csrf}}).Cookies() {
		if c.Name == sessionCookie && c.SameSite != http.SameSiteLaxMode {
			t.Errorf("logout cookie: SameSite %v, want Lax", c.SameSite)
		}
	}
}

func TestCSRFRequired(t *testing.T) {
	e := adminEnv(t, twoServers)
	v := e.eng.View()
	id := v.Open[0].ID
	if r := e.post("/problems/"+id+"/ignore", url.Values{"reason": {"x"}}); r.StatusCode != http.StatusForbidden {
		t.Fatalf("POST without CSRF: %d", r.StatusCode)
	}
	if r := e.post("/problems/"+id+"/ignore", url.Values{"reason": {"x"}, "csrf": {"forged"}}); r.StatusCode != http.StatusForbidden {
		t.Fatalf("POST with forged CSRF: %d", r.StatusCode)
	}
	csrf := e.csrf("/problems")
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/problems/"+id+"/ignore", strings.NewReader(url.Values{"reason": {"x"}, "csrf": {csrf}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	resp, _ := e.http.Do(req)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin POST: %d", resp.StatusCode)
	}
	if r := e.post("/problems/"+id+"/ignore", url.Values{"reason": {"known"}, "csrf": {csrf}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("valid POST: %d", r.StatusCode)
	}
	if _, ok := e.eng.View().Snoozed(id, time.Now()); !ok {
		t.Fatal("ignore not applied")
	}
}
