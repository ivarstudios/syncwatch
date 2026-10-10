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

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/notify"
	"github.com/ivarstudios/syncwatch/internal/settings"
	"github.com/ivarstudios/syncwatch/internal/store"
)

// flash returns the flash message a redirect set, and whether it is an error.
func flash(resp *http.Response) (string, bool) {
	for _, c := range resp.Cookies() {
		switch c.Name {
		case "sw_flash":
			return flashDecode(c.Value), false
		case "sw_flash_err":
			return flashDecode(c.Value), true
		}
	}
	return "", false
}

func TestServerURLChangeNeedsKey(t *testing.T) {
	e := adminEnv(t, twoServers)
	csrf := e.csrf("/settings")
	_ = e.st.SetPin("akka", "pinned-fingerprint")
	save := func(f url.Values) (string, bool) {
		f.Set("csrf", csrf)
		f.Set("orig_id", "akka")
		f.Set("name", "AKKA")
		return flash(e.post("/settings/servers", f))
	}

	// Pointing the server elsewhere without entering the key is refused, so
	// the stored key can't be redirected to another host.
	if msg, isErr := save(url.Values{"url": {"https://evil.example:8384"}}); !isErr || !strings.Contains(msg, "API key") {
		t.Fatalf("URL change without a key: %q %v", msg, isErr)
	}
	if sv, _ := e.cfg.Get().ServerByKey("akka"); sv.URL != "https://a:1" {
		t.Fatalf("URL changed to %s", sv.URL)
	}
	// Plain http:// needs the explicit opt-in, even with a key.
	if _, isErr := save(url.Values{"url": {"http://a:1"}, "api_key": {"k"}}); !isErr {
		t.Fatal("http:// accepted without allow_http")
	}
	// Other edits without a key are fine.
	if msg, isErr := save(url.Values{"url": {"https://A:1/"}, "disabled": {"on"}}); isErr {
		t.Fatalf("edit without URL change refused: %s", msg)
	}
	if key, bound, _ := e.st.ServerKey("akka"); key != "secret-key-akka" || bound != "https://a:1" {
		t.Fatalf("key or binding changed: %q %q", key, bound)
	}
	if pin, _ := e.st.Pin("akka"); pin != "pinned-fingerprint" {
		t.Fatal("pin changed by an unrelated edit")
	}

	// With a new key the server moves; the key is bound to the new URL and
	// the old URL's certificate pin is forgotten.
	if msg, isErr := save(url.Values{"url": {"https://192.0.2.7:8384"}, "api_key": {"new-key"}}); isErr {
		t.Fatalf("URL change with key: %s", msg)
	}
	if key, bound, _ := e.st.ServerKey("akka"); key != "new-key" || bound != "https://192.0.2.7:8384" {
		t.Fatalf("new key: %q %q", key, bound)
	}
	if pin, _ := e.st.Pin("akka"); pin != "" {
		t.Fatal("old pin kept for the new URL")
	}
	if n := e.noticesText(); !strings.Contains(n, "AKKA: API URL changed") || !strings.Contains(n, "https://192.0.2.7:8384") {
		t.Fatalf("no notice for the URL change: %q", n)
	}
	if _, body := e.get("/settings"); strings.Contains(body, "new-key") {
		t.Fatal("settings page shows the key")
	}
}

func TestPinChangesNeedConfirmation(t *testing.T) {
	e := adminEnv(t, twoServers)
	csrf := e.csrf("/settings")
	accept := func(confirm string) (string, bool) {
		return flash(e.post("/settings/servers/akka/accept-cert", url.Values{"csrf": {csrf}, "fingerprint": {"ff"}, "confirm": {confirm}}))
	}
	reset := func(confirm string) (string, bool) {
		return flash(e.post("/settings/servers", url.Values{"csrf": {csrf}, "orig_id": {"akka"}, "name": {"AKKA"},
			"url": {"https://a:1"}, "reset_pin": {"on"}, "confirm": {confirm}}))
	}
	for _, bad := range []string{"", "wrong"} {
		if _, isErr := accept(bad); !isErr {
			t.Fatalf("certificate accepted with confirmation %q", bad)
		}
		if _, isErr := reset(bad); !isErr {
			t.Fatalf("pin reset with confirmation %q", bad)
		}
	}
	e.mu.Lock()
	n := len(e.accepted) + len(e.resets)
	e.mu.Unlock()
	if n != 0 {
		t.Fatal("hooks called without confirmation")
	}
	// The admin password or the server's own API key confirms.
	if msg, isErr := accept("correct horse battery"); isErr {
		t.Fatalf("accept with the admin password: %s", msg)
	}
	if msg, isErr := reset("secret-key-akka"); isErr {
		t.Fatalf("reset with the API key: %s", msg)
	}
	e.mu.Lock()
	accepted, resets := len(e.accepted), len(e.resets)
	e.mu.Unlock()
	if accepted != 1 || resets != 1 {
		t.Fatalf("accepted %d, reset %d", accepted, resets)
	}
	notices := e.noticesText()
	if !strings.Contains(notices, "new certificate accepted") || !strings.Contains(notices, "Pinned certificate forgotten") {
		t.Fatalf("notices: %q", notices)
	}
}

// A stolen admin session must not be able to guess the admin password
// through the password-change form.
func TestPasswordChangeIsRateLimited(t *testing.T) {
	e := adminEnv(t, twoServers)
	csrf := e.csrf("/settings")
	change := func(current string) (string, bool) {
		return flash(e.post("/settings/password", url.Values{"csrf": {csrf}, "current": {current},
			"password": {"a new long password"}, "password2": {"a new long password"}}))
	}
	for i := 0; i < 5; i++ {
		if msg, isErr := change(fmt.Sprintf("guess %d", i)); !isErr || !strings.Contains(msg, "wrong") {
			t.Fatalf("guess %d: %q %v", i, msg, isErr)
		}
	}
	if msg, isErr := change("correct horse battery"); !isErr || !strings.Contains(msg, "Too many") {
		t.Fatalf("right password after 5 guesses: %q %v", msg, isErr)
	}
	if h, _ := e.st.Setting(KeyAdminHash); !CheckPassword("correct horse battery", h) {
		t.Fatal("password changed while rate limited")
	}
}

// The webhook only goes to Discord, and changes that can silence alerting
// are announced.
func TestNotificationChangesAreAnnounced(t *testing.T) {
	e := adminEnv(t, `servers: [{name: AKKA, url: "https://a:1", api_key: secret-key-akka}, {name: SOL, url: "https://b:1"}]
notifications: {discord_webhook: "https://discord.com/api/webhooks/1/old", discord_role_id: "420000000000000000"}
`)
	csrf := e.csrf("/settings")
	notify := func(f url.Values) (string, bool) {
		f.Set("csrf", csrf)
		f.Set("weekly_day", "monday")
		f.Set("weekly_time", "08:00")
		f.Set("timezone", "Local")
		return flash(e.post("/settings/notifications", f))
	}
	if msg, isErr := notify(url.Values{"discord_webhook": {"http://10.0.0.5:8080/api/webhooks/1/x"}, "discord_role_id": {"420000000000000000"}}); !isErr || !strings.Contains(msg, "discord.com") {
		t.Fatalf("internal webhook URL: %q %v", msg, isErr)
	}
	if v, _ := e.st.Secret(store.SecretDiscordWebhook); v != "https://discord.com/api/webhooks/1/old" {
		t.Fatalf("webhook changed to %q", v)
	}
	if msg, isErr := notify(url.Values{"discord_webhook": {"https://discord.com/api/webhooks/2/new"}}); isErr {
		t.Fatalf("Discord webhook refused: %s", msg)
	}
	if n := e.noticesText(); !strings.Contains(n, "Discord webhook replaced") || !strings.Contains(n, "no longer mention a role") {
		t.Fatalf("notices: %q", n)
	}
	notify(url.Values{"discord_webhook_clear": {"on"}})
	if n := e.noticesText(); !strings.Contains(n, "Discord webhook removed") {
		t.Fatalf("notices: %q", n)
	}

	form := url.Values{"csrf": {csrf}}
	for _, c := range config.Catalog {
		if c.ID != "H1" {
			form.Set("enabled_"+c.ID, "on")
		}
		if c.Urgent && c.ID != "H4" {
			form.Set("urgent_"+c.ID, "on")
		}
		form.Set("debounce_"+c.ID, config.Duration(c.Debounce).String())
	}
	form.Set("debounce_S1", "30d")
	for _, k := range []string{"failing_files", "stuck_sync", "paused", "pc_offline", "conflict_window", "inactive_project", "old_device", "dead_share"} {
		form.Set(k, "2d")
	}
	e.post("/settings/checks", form)
	n := e.noticesText()
	for _, want := range []string{"H1 (Server unreachable) switched off", "H4 (Folder sync stopped) no longer sends urgent pings", "S1 (Overlapping Syncthing folders) waits 30d"} {
		if !strings.Contains(n, want) {
			t.Errorf("no notice %q in %q", want, n)
		}
	}

	if msg, _ := flash(e.post("/settings/import", url.Values{"csrf": {csrf}, "yaml": {"servers: [{name: AKKA, url: 'https://a:1'}, {name: SOL, url: 'https://b:1'}]\nchecks: {H3: {enabled: false}}\nnotifications: {discord_webhook: 'https://evil.example/hook'}\n"}})); !strings.Contains(msg, "discord_webhook") {
		t.Fatalf("import with an internal webhook: %q", msg)
	}
	e.post("/settings/import", url.Values{"csrf": {csrf}, "yaml": {"servers: [{name: AKKA, url: 'https://a:1'}, {name: SOL, url: 'https://b:1'}]\nchecks: {H3: {enabled: false}}\n"}})
	if n := e.noticesText(); !strings.Contains(n, "H3 (Server certificate changed) switched off") {
		t.Fatalf("import notice: %q", n)
	}
}

func TestImportCannotRedirectKeys(t *testing.T) {
	e := adminEnv(t, twoServers)
	csrf := e.csrf("/settings")
	imp := func(yaml string) string {
		msg, _ := flash(e.post("/settings/import", url.Values{"csrf": {csrf}, "yaml": {yaml}}))
		return msg
	}
	// AKKA moves to another host without an api_key in the file; SOL is dropped.
	if msg := imp("servers: [{name: AKKA, url: 'https://evil.example:8384'}]\n"); !strings.Contains(msg, "Enter the API key again for AKKA") {
		t.Fatalf("import message: %q", msg)
	}
	sv, _ := e.cfg.Get().ServerByKey("akka")
	if settings.KeyUsable(e.st, sv) {
		t.Fatal("the stored key is usable for the imported URL")
	}
	if n := e.noticesText(); !strings.Contains(n, "AKKA: API URL changed") || !strings.Contains(n, "SOL removed") {
		t.Fatalf("notices: %q", n)
	}
	// A removed server's key doesn't survive for a later server with the same ID.
	imp("servers: [{name: AKKA, url: 'https://a:1'}, {name: SOL, url: 'https://b:1'}]\n")
	_ = e.st.SetServerKey("sol", "sol-key", "https://b:1")
	imp("servers: [{name: AKKA, url: 'https://a:1'}]\n")
	if e.st.HasSecret(store.ServerAPIKeySecret("sol")) {
		t.Fatal("a removed server's key was kept")
	}
	// Back at the URL it was entered for, AKKA's key works again.
	sv, _ = e.cfg.Get().ServerByKey("akka")
	if !settings.KeyUsable(e.st, sv) {
		t.Fatal("key not usable at its own URL")
	}
	// Switching login off by import is announced too.
	e.post("/settings/import", url.Values{"csrf": {csrf}, "confirm": {"correct horse battery"},
		"yaml": {"servers: [{name: AKKA, url: 'https://a:1'}]\naccess: {mode: proxy}\n"}})
	if !strings.Contains(e.noticesText(), "Built-in login switched off") {
		t.Fatalf("notices: %q", e.noticesText())
	}
}

// Switching login off, opening up viewing or setting a viewer password needs
// the admin password, on the Access form and by import.
func TestWeakeningAccessNeedsAdminPassword(t *testing.T) {
	e := adminEnv(t, twoServers)
	csrf := e.csrf("/settings")
	access := func(f url.Values) (string, bool) {
		f.Set("csrf", csrf)
		return flash(e.post("/settings/access", f))
	}
	viewer := func() string { return e.cfg.Get().Access.Viewer }

	for _, confirm := range []string{"", "wrong password"} {
		if msg, isErr := access(url.Values{"mode": {"builtin"}, "viewer": {"open"}, "confirm": {confirm}}); !isErr || !strings.Contains(msg, "admin password") {
			t.Fatalf("open viewing with confirm %q: %q %v", confirm, msg, isErr)
		}
	}
	if msg, isErr := access(url.Values{"mode": {"proxy"}, "confirm_proxy": {"on"}, "viewer": {"none"}}); !isErr || !strings.Contains(msg, "admin password") {
		t.Fatalf("proxy mode without the password: %q %v", msg, isErr)
	}
	if msg, isErr := access(url.Values{"mode": {"builtin"}, "viewer": {"password"}, "viewer_password": {"viewer password 1"}}); !isErr {
		t.Fatalf("viewer password without the admin password: %q", msg)
	}
	if _, ok := e.web.setting(KeyViewerHash); ok {
		t.Fatal("viewer password stored although the change was refused")
	}
	if e.cfg.Get().Access.Mode != "builtin" || viewer() != "none" {
		t.Fatalf("access changed: %+v", e.cfg.Get().Access)
	}

	if msg, isErr := access(url.Values{"mode": {"builtin"}, "viewer": {"open"}, "confirm": {"correct horse battery"}}); isErr || viewer() != "open" {
		t.Fatalf("open viewing with the password: %q %v", msg, isErr)
	}
	// Tightening access needs no password.
	if msg, isErr := access(url.Values{"mode": {"builtin"}, "viewer": {"none"}}); isErr || viewer() != "none" {
		t.Fatalf("closing viewing: %q %v", msg, isErr)
	}

	// Import: refused before anything from the file is stored.
	imp := func(confirm, yaml string) (string, bool) {
		return flash(e.post("/settings/import", url.Values{"csrf": {csrf}, "confirm": {confirm}, "yaml": {yaml}}))
	}
	file := "servers: [{name: AKKA, url: 'https://a:1'}]\naccess: {mode: proxy}\napi: {token: swt_from_the_file}\n"
	if msg, isErr := imp("", file); !isErr || !strings.Contains(msg, "Not imported") {
		t.Fatalf("import to proxy mode without the password: %q %v", msg, isErr)
	}
	if tok, _ := e.st.Secret(store.SecretAPIToken); tok == "swt_from_the_file" {
		t.Fatal("secret from a refused import was stored")
	}
	if e.cfg.Get().Access.Mode != "builtin" || len(e.cfg.Get().Servers) != 2 {
		t.Fatal("refused import changed the configuration")
	}
	if msg, isErr := imp("", "servers: [{name: AKKA, url: 'https://a:1'}]\n"); isErr {
		t.Fatalf("import that keeps access as it is: %q", msg)
	}
	if msg, isErr := imp("correct horse battery", file); isErr || e.cfg.Get().Access.Mode != "proxy" {
		t.Fatalf("import to proxy mode with the password: %q %v", msg, isErr)
	}
}

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
	if tok, _ := e.st.Secret(store.SecretAPIToken); resp.StatusCode != http.StatusMisdirectedRequest || tok != "swt_test_token" {
		t.Fatalf("POST from a rebinding page: %d, token %q", resp.StatusCode, tok)
	}
}

// The delivery error text can quote the channel's answer, so the API shows
// it to admins only; viewers, open viewing and token holders get failing
// and since.
func TestAPIDeliveryErrorForAdminsOnly(t *testing.T) {
	e := adminEnv(t, twoServers+"access: {viewer: open}\n")
	e.setDelivery(notify.Delivery{Failing: true, Since: time.Now(), Error: "discord: HTTP 404: Unknown Webhook"})
	status := func(c *http.Client, bearer string) string {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/v1/status", nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 || !strings.Contains(string(b), `"failing": true`) {
			t.Fatalf("API: %d %s", resp.StatusCode, b)
		}
		return string(b)
	}
	if body := status(e.http, ""); !strings.Contains(body, "Unknown Webhook") {
		t.Fatalf("admin session: no error text in %s", body)
	}
	if body := status(http.DefaultClient, ""); strings.Contains(body, "Unknown Webhook") {
		t.Fatal("open viewing sees the error text")
	}
	if body := status(http.DefaultClient, "swt_test_token"); strings.Contains(body, "Unknown Webhook") {
		t.Fatal("token holder sees the error text")
	}
	e2 := newEnv(t, twoServers+"access: {mode: proxy}\n")
	e2.setDelivery(notify.Delivery{Failing: true, Since: time.Now(), Error: "discord: HTTP 404: Unknown Webhook"})
	req, _ := http.NewRequest(http.MethodGet, e2.srv.URL+"/api/v1/status", nil)
	resp, _ := http.DefaultClient.Do(req)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "Unknown Webhook") {
		t.Fatal("proxy mode (everyone is admin): no error text")
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
