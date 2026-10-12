package web

// Tests of the pre-release review (stage pp, area auth): the last use of an
// API token during a replace (tokens-2), the login limit for many addresses
// (sessions-2), the length of an administrator session (sessions-3) and the
// login of an administrator in a browser with a viewer session (sessions-4).

import (
	"fmt"
	"net/http"
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
