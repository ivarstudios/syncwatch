package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/engine"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/notify"
	"github.com/ivarstudios/syncwatch/internal/settings"
	"github.com/ivarstudios/syncwatch/internal/store"
)

type env struct {
	t    *testing.T
	srv  *httptest.Server
	web  *Server
	st   *store.Store
	cfg  *config.Holder
	eng  *engine.Engine
	hub  *model.Hub
	jar  http.CookieJar
	http *http.Client

	mu       sync.Mutex
	notices  []string // titles and summaries sent through Hooks.Notice
	accepted []string // AcceptCert calls
	resets   []string // ResetPin calls
	delivery notify.Delivery
}

func (e *env) setDelivery(d notify.Delivery) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.delivery = d
}

func (e *env) noticesText() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return strings.Join(e.notices, "\n")
}

func newEnv(t *testing.T, yaml string) *env {
	t.Helper()
	st, err := store.OpenMemory(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.ParseYAML([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = settings.ImportSecrets(st, c)
	h, _ := config.NewHolder(c)
	hub := model.NewHub(nil)
	var ms []*model.Server
	for _, s := range c.Servers {
		ms = append(ms, &model.Server{ID: s.ID, Name: s.Name, Status: model.StatusDown, LastError: "refused", DownSince: time.Now()})
	}
	hub.SetServers(ms)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng, _ := engine.New(st, hub, h, nil, log)
	eng.Evaluate()
	e := &env{t: t, st: st, cfg: h, eng: eng, hub: hub}
	e.web, err = New(h, st, eng, Hooks{
		SaveConfig: func(c *config.Config) error {
			if err := c.Validate(); err != nil {
				return err
			}
			_ = settings.Save(st, c)
			return h.Set(c)
		},
		AcceptCert: func(id, fp string) error {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.accepted = append(e.accepted, id)
			return nil
		},
		ResetPin: func(id string) error {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.resets = append(e.resets, id)
			return st.SetPin(id, "")
		},
		SendTest: func(context.Context) error { return nil },
		Delivery: func() notify.Delivery {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.delivery
		},
		Notice: func(title, summary string) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.notices = append(e.notices, title+": "+summary)
		},
	}, "test", log)
	if err != nil {
		t.Fatal(err)
	}
	e.srv = httptest.NewServer(e.web)
	t.Cleanup(e.srv.Close)
	e.jar, _ = cookiejar.New(nil)
	e.http = &http.Client{Jar: e.jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return e
}

func (e *env) get(path string) (*http.Response, string) {
	e.t.Helper()
	resp, err := e.http.Get(e.srv.URL + path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func (e *env) csrf(page string) string {
	e.t.Helper()
	_, body := e.get(page)
	m := csrfRe.FindStringSubmatch(body)
	if m == nil {
		e.t.Fatalf("no csrf token on %s", page)
	}
	return m[1]
}

func (e *env) post(path string, form url.Values) *http.Response {
	e.t.Helper()
	resp, err := e.http.PostForm(e.srv.URL+path, form)
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

const twoServers = `servers: [{name: AKKA, url: "https://a:1", api_key: secret-key-akka}, {name: SOL, url: "https://b:1"}]
notifications: {discord_webhook: "https://discord.com/api/webhooks/1/secret-hook"}
api: {token: swt_test_token}
`

func TestSetupFlow(t *testing.T) {
	e := newEnv(t, twoServers)
	resp, _ := e.get("/")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/setup" {
		t.Fatalf("expected redirect to setup, got %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	tok := e.web.SetupToken()
	csrf := e.csrf("/setup")
	if r := e.post("/setup", url.Values{"csrf": {csrf}, "token": {"wrong"}, "password": {"long enough pw"}, "password2": {"long enough pw"}}); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token accepted: %d", r.StatusCode)
	}
	if r := e.post("/setup", url.Values{"csrf": {csrf}, "token": {tok}, "password": {"short"}, "password2": {"short"}}); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("short password accepted: %d", r.StatusCode)
	}
	if r := e.post("/setup", url.Values{"csrf": {csrf}, "token": {tok}, "password": {"long enough pw"}, "password2": {"long enough pw"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("setup failed: %d", r.StatusCode)
	}
	if e.web.SetupToken() != "" {
		t.Fatal("setup token must be cleared")
	}
	resp, body := e.get("/settings")
	if resp.StatusCode != 200 || !strings.Contains(body, "Servers") {
		t.Fatalf("settings after setup: %d", resp.StatusCode)
	}
	for _, secret := range []string{"secret-key-akka", "secret-hook", "swt_test_token"} {
		if strings.Contains(body, secret) {
			t.Fatalf("settings page shows secret %q", secret)
		}
	}
}

func adminEnv(t *testing.T, yaml string) *env {
	e := newEnv(t, yaml)
	h, _ := HashPassword("correct horse battery")
	_ = e.st.SetSetting(KeyAdminHash, h)
	csrf := e.csrf("/login")
	if r := e.post("/login", url.Values{"csrf": {csrf}, "password": {"correct horse battery"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("login failed: %d", r.StatusCode)
	}
	return e
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

func TestLoginRateLimit(t *testing.T) {
	e := newEnv(t, twoServers)
	h, _ := HashPassword("correct horse battery")
	_ = e.st.SetSetting(KeyAdminHash, h)
	csrf := e.csrf("/login")
	for i := 0; i < 5; i++ {
		if r := e.post("/login", url.Values{"csrf": {csrf}, "password": {"nope"}}); r.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d: %d", i, r.StatusCode)
		}
	}
	if r := e.post("/login", url.Values{"csrf": {csrf}, "password": {"correct horse battery"}}); r.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected rate limit, got %d", r.StatusCode)
	}
}

func TestViewerModes(t *testing.T) {
	e := newEnv(t, twoServers+"access: {viewer: open}\n")
	h, _ := HashPassword("correct horse battery")
	_ = e.st.SetSetting(KeyAdminHash, h)
	if resp, body := e.get("/"); resp.StatusCode != 200 || !strings.Contains(body, "AKKA") || !strings.Contains(body, "Log in") {
		t.Fatalf("open viewing: %d", resp.StatusCode)
	}
	if resp, _ := e.get("/settings"); resp.StatusCode == 200 {
		t.Fatal("anonymous viewer reached settings")
	}
	resp, _ := e.get("/frag/problems")
	if resp.StatusCode != 200 {
		t.Fatalf("fragment: %d", resp.StatusCode)
	}

	e2 := newEnv(t, twoServers)
	_ = e2.st.SetSetting(KeyAdminHash, h)
	if resp, _ := e2.get("/grid"); resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login") {
		t.Fatalf("admin-only: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp, _ := e2.get("/frag/grid"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("fragment without login: %d", resp.StatusCode)
	}

	e3 := newEnv(t, twoServers+"access: {mode: proxy}\n")
	if resp, _ := e3.get("/settings"); resp.StatusCode != 200 {
		t.Fatalf("proxy mode: %d", resp.StatusCode)
	}
}

func TestAPIStatus(t *testing.T) {
	e := newEnv(t, twoServers)
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/v1/status", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	req.Header.Set("Authorization", "Bearer wrong")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", resp.StatusCode)
	}
	req.Header.Set("Authorization", "Bearer swt_test_token")
	resp, _ = http.DefaultClient.Do(req)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	body := string(b)
	if resp.StatusCode != 200 || !strings.Contains(body, `"check": "H1"`) || !strings.Contains(body, "0 of 2 servers up") {
		t.Fatalf("status: %d %s", resp.StatusCode, body)
	}
	for _, secret := range []string{"secret-key-akka", "secret-hook", "swt_test_token"} {
		if strings.Contains(body, secret) {
			t.Fatalf("API leaks %q", secret)
		}
	}
}

// /api/v1/ha gives Home Assistant per-server sensors: running, overall
// status, data moved in 24 h and 7 days, current rate.
func TestAPIForHomeAssistant(t *testing.T) {
	e := newEnv(t, `servers: [{name: AKKA, url: "https://a:1"}, {name: SOL, url: "https://b:1"}, {name: SIRIUS, url: "https://c:1", disabled: true}]
api: {token: swt_test_token}
`)
	// Like the collector manager, the hub only has the enabled servers.
	e.hub.SetServers([]*model.Server{{ID: "akka", Name: "AKKA"}, {ID: "sol", Name: "SOL", Status: model.StatusDown}})
	now := time.Now()
	e.hub.Update("akka", func(s *model.Server) {
		s.Status, s.Loaded = model.StatusUp, true
		s.Folders = map[string]*model.Folder{
			"a": {ID: "a", State: "idle"}, "b": {ID: "b", State: "syncing", NeedItems: 4}, "c": {ID: "c", Paused: true},
		}
		s.Rate = model.Rate{In: 12_500_000, Out: 250_000, At: now}
	})
	e.eng.Evaluate() // the engine's view follows the hub within a second
	_, _ = e.st.RecordTraffic("akka", "s", 0, 0, now.Add(-6*24*time.Hour), 7*24*time.Hour)
	_, _ = e.st.RecordTraffic("akka", "s", 4_000_000_000, 1_000_000_000, now.Add(-3*24*time.Hour), 7*24*time.Hour) // inside the week only
	_, _ = e.st.RecordTraffic("akka", "s", 5_500_000_000, 1_250_000_000, now.Add(-time.Hour), 7*24*time.Hour)

	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/v1/ha", nil)
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("without a token: %d", resp.StatusCode)
	}
	req.Header.Set("Authorization", "Bearer swt_test_token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("with the token: %v %v", err, resp.StatusCode)
	}
	var got struct {
		Servers map[string]map[string]any `json:"servers"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&got)
	resp.Body.Close()
	akka, sol := got.Servers["akka"], got.Servers["sol"]
	want := map[string]any{
		"name": "AKKA", "running": true, "status": "syncing", "folders": 3.0, "folders_syncing": 1.0, "folders_paused": 1.0,
		"received_24h_gb": 1.5, "sent_24h_gb": 0.25, "moved_24h_gb": 1.75,
		"received_7d_gb": 5.5, "sent_7d_gb": 1.25, "moved_7d_gb": 6.75,
		"download_mb_s": 12.5, "upload_mb_s": 0.25,
	}
	for k, v := range want {
		if akka[k] != v {
			t.Errorf("akka %s = %v, want %v", k, akka[k], v)
		}
	}
	if sol["running"] != false || sol["status"] != "unknown" || sol["download_mb_s"] != 0.0 {
		t.Errorf("sol (down): %v", sol)
	}
	// A disabled server is listed too, so Home Assistant's templates find it.
	if sirius := got.Servers["sirius"]; sirius["running"] != false || sirius["status"] != "disabled" || sirius["name"] != "SIRIUS" {
		t.Errorf("sirius (disabled): %v", sirius)
	}
}

// While notifications fail, every page says so; viewers don't see the
// error text.
func TestNotificationFailureBanner(t *testing.T) {
	e := adminEnv(t, twoServers+"access: {viewer: open}\n")
	const banner = "Notifications fail"
	if _, body := e.get("/"); strings.Contains(body, banner) {
		t.Fatal("banner while notifications work")
	}
	e.setDelivery(notify.Delivery{Failing: true, Since: time.Now(), Error: "discord: HTTP 404: Unknown Webhook"})
	for _, p := range []string{"/", "/grid", "/problems", "/settings", "/frag/alert"} {
		if _, body := e.get(p); !strings.Contains(body, banner) || !strings.Contains(body, "Unknown Webhook") {
			t.Fatalf("%s: no banner with the error for the admin", p)
		}
	}
	anon, _ := http.Get(e.srv.URL + "/")
	b, _ := io.ReadAll(anon.Body)
	anon.Body.Close()
	if !strings.Contains(string(b), banner) || strings.Contains(string(b), "Unknown Webhook") {
		t.Fatal("open viewer: want the banner without the error text")
	}
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/v1/status", nil)
	req.Header.Set("Authorization", "Bearer swt_test_token")
	resp, _ := http.DefaultClient.Do(req)
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"failing": true`) {
		t.Fatalf("API: %s", b)
	}
	e.setDelivery(notify.Delivery{})
	if _, body := e.get("/frag/alert"); strings.Contains(body, banner) || !strings.Contains(body, `id="sw-alert"`) {
		t.Fatalf("alert fragment after recovery: %s", body)
	}
}

func TestSecurityHeadersAndAssets(t *testing.T) {
	e := newEnv(t, twoServers)
	resp, _ := e.get("/login")
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe") || resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("headers: %v", resp.Header)
	}
	if pp := resp.Header.Get("Permissions-Policy"); !strings.Contains(pp, "camera=()") || strings.Contains(pp, "interest-cohort") {
		t.Fatalf("Permissions-Policy: %q", pp)
	}
	for _, a := range []string{"/assets/htmx.min.js", "/assets/htmx-ext-sse.js", "/assets/app.css", "/assets/app.js", "/assets/syncwatch-icon.svg", "/assets/syncwatch-icon-32.png", "/assets/syncwatch-icon-256.png", "/favicon.ico"} {
		if r, _ := e.get(a); r.StatusCode != 200 {
			t.Fatalf("%s: %d", a, r.StatusCode)
		}
	}
	if r, _ := e.get("/healthz"); r.StatusCode != 200 {
		t.Fatal("healthz")
	}
}

func TestSettingsForms(t *testing.T) {
	e := adminEnv(t, twoServers)
	csrf := e.csrf("/settings")
	// Add a server; a new server needs an API key.
	if r := e.post("/settings/servers", url.Values{"csrf": {csrf}, "name": {"SIRIUS"}, "url": {"https://c:8384"}}); !strings.Contains(r.Header.Get("Location"), "#servers") {
		t.Fatal("redirect")
	}
	if _, ok := e.cfg.Get().ServerByKey("sirius"); ok {
		t.Fatal("server without API key was added")
	}
	e.post("/settings/servers", url.Values{"csrf": {csrf}, "name": {"SIRIUS"}, "url": {"https://c:8384"}, "api_key": {"k3"}})
	if _, ok := e.cfg.Get().ServerByKey("sirius"); !ok {
		t.Fatal("server not added")
	}
	if v, _, _ := e.st.ServerKey("sirius"); v != "k3" {
		t.Fatal("API key not stored")
	}
	// Structure rules: invalid YAML is rejected, valid is applied.
	r := e.post("/settings/structure", url.Values{"csrf": {csrf}, "yaml": {"structure: {project_pattern: '('}"}})
	if loc := r.Header.Get("Location"); !strings.Contains(loc, "#structure") {
		t.Fatal(loc)
	}
	if e.cfg.Get().Structure.ProjectPattern == "(" {
		t.Fatal("invalid pattern saved")
	}
	e.post("/settings/structure", url.Values{"csrf": {csrf}, "yaml": {"structure: {project_pattern: '^LIVE-'}\nshares: {add: {AKKA: [/x/AKKA-LIVE-5COLLECT]}}"}})
	if e.cfg.Get().Structure.ProjectPattern != "^LIVE-" || len(e.cfg.Get().Shares.Add["AKKA"]) != 1 {
		t.Fatalf("structure not saved: %+v", e.cfg.Get().Structure)
	}
	// Checks: disable S7, make H7 urgent.
	form := url.Values{"csrf": {csrf}}
	for _, c := range config.Catalog {
		if c.ID != "S7" {
			form.Set("enabled_"+c.ID, "on")
		}
		if c.Urgent || c.ID == "H7" {
			form.Set("urgent_"+c.ID, "on")
		}
		form.Set("debounce_"+c.ID, config.Duration(c.Debounce).String())
	}
	for _, k := range []string{"failing_files", "stuck_sync", "paused", "pc_offline", "conflict_window", "inactive_project", "old_device", "dead_share"} {
		form.Set(k, "2d")
	}
	e.post("/settings/checks", form)
	if e.cfg.Get().CheckEnabled("S7") || !e.cfg.Get().Urgent("H7") || e.cfg.Get().Thresholds.PCOffline.D() != 48*time.Hour {
		t.Fatalf("checks not saved: %+v", e.cfg.Get().Checks)
	}
	// Export never contains secrets; import round-trips.
	_, exp := e.get("/settings/export")
	if strings.Contains(exp, "k3") || strings.Contains(exp, "secret") || !strings.Contains(exp, "SIRIUS") {
		t.Fatalf("export: %s", exp)
	}
	r = e.post("/settings/import", url.Values{"csrf": {csrf}, "yaml": {exp}})
	if !strings.Contains(r.Header.Get("Location"), "#import") {
		t.Fatal("import redirect")
	}
	if v, _, _ := e.st.ServerKey("sirius"); v != "k3" {
		t.Fatal("import without secrets must keep existing secrets")
	}
	// API token: generated once, shown once.
	resp, err := e.http.PostForm(e.srv.URL+"/settings/api-token", url.Values{"csrf": {csrf}, "op": {"generate"}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	tok, _ := e.st.Secret(store.SecretAPIToken)
	if !strings.HasPrefix(tok, "swt_") || !strings.Contains(string(b), tok) {
		t.Fatal("token not shown after generation")
	}
	if _, body := e.get("/settings"); strings.Contains(body, tok) {
		t.Fatal("token shown again")
	}
}

func TestPasswordHash(t *testing.T) {
	h, err := HashPassword("pässwörd with spaces")
	if err != nil || !strings.HasPrefix(h, "$argon2id$") {
		t.Fatal(h, err)
	}
	if !CheckPassword("pässwörd with spaces", h) || CheckPassword("password", h) || CheckPassword("x", "garbage") {
		t.Fatal("check")
	}
}

// Flash messages survive non-ASCII text and long errors.
func TestFlashCookie(t *testing.T) {
	for _, msg := range []string{"Saved; now: 3, \"quoted\"\nand… 100% åäö", strings.Repeat("Fel på servern – ", 400)} {
		enc := flashEncode(msg)
		if len(enc) > maxFlash {
			t.Fatalf("encoded flash is %d bytes", len(enc))
		}
		for i := 0; i < len(enc); i++ {
			if c := enc[i]; c < 0x21 || c > 0x7e || c == ';' || c == ',' || c == '"' || c == '\\' {
				t.Fatalf("byte %q isn't allowed in a cookie value", c)
			}
		}
		dec := flashDecode(enc)
		if !utf8.ValidString(dec) {
			t.Fatal("decoded flash isn't valid UTF-8")
		}
		if len(msg) < 100 && dec != msg {
			t.Fatalf("round trip: %q", dec)
		}
		if len(msg) > 1000 && (!strings.HasSuffix(dec, "…") || !strings.HasPrefix(msg, strings.TrimSuffix(dec, "…"))) {
			t.Fatalf("long message not cut cleanly: …%q", dec[len(dec)-20:])
		}
	}
	e := adminEnv(t, twoServers+"timezone: Europe/Stockholm\n")
	e.web.now = func() time.Time { return time.Date(2026, 10, 4, 22, 30, 0, 0, time.UTC) }
	if resp, _ := e.get("/settings/export"); !strings.Contains(resp.Header.Get("Content-Disposition"), "syncwatch-2026-10-05.yaml") {
		t.Fatalf("export file name: %q", resp.Header.Get("Content-Disposition"))
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"/grid": "/grid", "/problems?severity=error&server=akka": "/problems?severity=error&server=akka", "/problems#f-x": "/problems#f-x",
		"//evil.com": "/", "https://evil.com": "/", "/\\evil": "/", "": "/", "grid": "/",
		// Browsers strip tab, CR and LF, which would turn these into //evil.com.
		"/\t/evil.com": "/", "/\n/evil.com": "/", "/\r/evil.com": "/", "/a\\b": "/", "/\x7f": "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

// A crafted login link must not send the admin to another site.
func TestLoginRedirectStaysOnSite(t *testing.T) {
	e := newEnv(t, twoServers)
	h, _ := HashPassword("correct horse battery")
	_ = e.st.SetSetting(KeyAdminHash, h)
	_, body := e.get("/login?next=" + url.QueryEscape("/\t/evil.example"))
	if strings.Contains(body, "evil.example") {
		t.Fatal("login form carries the foreign next value")
	}
	csrf := e.csrf("/login")
	r := e.post("/login", url.Values{"csrf": {csrf}, "password": {"correct horse battery"}, "next": {"/\t/evil.example"}})
	if r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/" {
		t.Fatalf("login redirect: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
}

// The setup token is logged at WARN. When the log level hides warnings it
// is printed to stderr as well, since the first start can't finish without it.
func TestSetupTokenPrintedWhenLogHidesWarnings(t *testing.T) {
	e := newEnv(t, twoServers)
	var logBuf, errBuf bytes.Buffer
	e.web.log = slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelError}))
	e.web.LogSetupToken(&errBuf)
	tok := e.web.SetupToken()
	if tok == "" || !strings.Contains(errBuf.String(), tok) || logBuf.Len() != 0 {
		t.Fatalf("log level error: stderr %q, log %q", errBuf.String(), logBuf.String())
	}
	errBuf.Reset()
	e.web.log = slog.New(slog.NewTextHandler(&logBuf, nil))
	e.web.LogSetupToken(&errBuf)
	if !strings.Contains(logBuf.String(), tok) || errBuf.Len() != 0 {
		t.Fatalf("log level info: stderr %q, log %q", errBuf.String(), logBuf.String())
	}
	// Nothing once an admin password exists, or in proxy mode.
	logBuf.Reset()
	h, _ := HashPassword("correct horse battery")
	_ = e.st.SetSetting(KeyAdminHash, h)
	e.web.LogSetupToken(&errBuf)
	e2 := newEnv(t, twoServers+"access: {mode: proxy}\n")
	e2.web.log = e.web.log
	e2.web.LogSetupToken(&errBuf)
	if logBuf.Len() != 0 || errBuf.Len() != 0 {
		t.Fatalf("admin set or proxy mode: stderr %q, log %q", errBuf.String(), logBuf.String())
	}
}

// A form posted after the session expired goes to the login page without a
// next to the POST-only URL (a GET there would be 405); pages keep their
// next. A visit to /login with a session goes straight to next.
func TestLoginNextOnlyForPages(t *testing.T) {
	e := newEnv(t, twoServers)
	h, _ := HashPassword("correct horse battery")
	_ = e.st.SetSetting(KeyAdminHash, h)
	csrf := e.csrf("/login")
	if r := e.post("/settings/servers", url.Values{"csrf": {csrf}, "name": {"X"}}); r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/login" {
		t.Fatalf("anonymous POST: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	if r, _ := e.get("/problems?severity=error"); r.Header.Get("Location") != "/login?next=/problems%3Fseverity=error" {
		t.Fatalf("anonymous GET: %q", r.Header.Get("Location"))
	}
	if r := e.post("/login", url.Values{"csrf": {csrf}, "password": {"correct horse battery"}, "next": {"/grid"}}); r.Header.Get("Location") != "/grid" {
		t.Fatalf("login: %q", r.Header.Get("Location"))
	}
	if r, _ := e.get("/login?next=/problems"); r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/problems" {
		t.Fatalf("/login with a session: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	if r, _ := e.get("/login"); r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/" {
		t.Fatalf("/login with a session and no next: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
}

// A viewer session from before viewing was switched off gives no role. The
// login page shows the form to it; sending it on to a page would send it
// back to /login, round and round.
func TestLoginPageWithStaleViewerSession(t *testing.T) {
	e := newEnv(t, twoServers+"access: {viewer: password}\n")
	ah, _ := HashPassword("correct horse battery")
	vh, _ := HashPassword("viewer password 1")
	_ = e.st.SetSetting(KeyAdminHash, ah)
	_ = e.st.SetSetting(KeyViewerHash, vh)
	csrf := e.csrf("/login")
	if r := e.post("/login", url.Values{"csrf": {csrf}, "password": {"viewer password 1"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("viewer login: %d", r.StatusCode)
	}
	c := e.cfg.Get().Clone()
	c.Access.Viewer = config.ViewerNone
	if err := e.cfg.Set(c); err != nil {
		t.Fatal(err)
	}
	if r, _ := e.get("/"); r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/login?next=/" {
		t.Fatalf("page with a stale viewer session: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	if r, body := e.get("/login?next=/"); r.StatusCode != http.StatusOK || !strings.Contains(body, `name="password"`) {
		t.Fatalf("/login with a stale viewer session: %d, want the login form", r.StatusCode)
	}
	// The admin password still gets in.
	if r := e.post("/login", url.Values{"csrf": {csrf}, "password": {"correct horse battery"}, "next": {"/"}}); r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/" {
		t.Fatalf("admin login: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	if r, _ := e.get("/settings"); r.StatusCode != http.StatusOK {
		t.Fatalf("/settings after the admin login: %d", r.StatusCode)
	}
}

// The grid, the overview tile, /api/v1/status and /api/v1/ha count folders
// by the same state function: a watcher error is a warning everywhere and a
// scanning folder is never "syncing".
func TestFolderStateAgreesEverywhere(t *testing.T) {
	e := adminEnv(t, twoServers)
	e.hub.Update("akka", func(s *model.Server) {
		s.Status, s.Loaded, s.MyID = model.StatusUp, true, "AAAAAAA-BBBBBBB"
		s.Folders = map[string]*model.Folder{
			"f1": {ID: "f1", Label: "idle-folder", State: "idle"},
			"f2": {ID: "f2", Label: "sync-folder", State: "syncing", NeedItems: 4},
			"f3": {ID: "f3", Label: "scan-folder", State: "scanning"},
			"f4": {ID: "f4", Label: "err-folder", State: "error", Error: "folder marker missing"},
			"f5": {ID: "f5", Label: "watch-folder", State: "idle", WatchError: "inotify limit"},
			"f6": {ID: "f6", Label: "paused-folder", Paused: true},
		}
	})
	e.eng.Evaluate()
	_, ov := e.get("/frag/overview")
	for _, want := range []string{"✔ 1 idle", "⟳ 1 syncing", "⟳ 1 scanning", "✖ 1 error", "⚠ 1 file watcher error", "⏸ 1 paused"} {
		if !strings.Contains(ov, want) {
			t.Errorf("overview tile lacks %q", want)
		}
	}
	_, grid := e.get("/frag/grid")
	if !strings.Contains(grid, `c-warn" title="file watcher error: inotify limit"`) || !strings.Contains(grid, "file watcher error</span>") {
		t.Error("grid: the watcher error is not shown as a warning cell")
	}
	if n := strings.Count(grid, `cell c-err" title=`); n != 2 { // one per view (wide table and narrow list)
		t.Errorf("grid: %d error cells, want 2 (the folder error only)", n)
	}
	if !strings.Contains(grid, `c-sync" title="Scanning"`) {
		t.Error("grid: the scanning folder is not shown as scanning")
	}

	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/v1/status", nil)
	req.Header.Set("Authorization", "Bearer swt_test_token")
	resp, _ := http.DefaultClient.Do(req)
	var st struct {
		Servers []struct {
			ID      string         `json:"id"`
			Folders map[string]int `json:"folders"`
		} `json:"servers"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	wantCounts := map[string]int{"total": 6, "idle": 1, "syncing": 1, "scanning": 1, "error": 1, "watch_error": 1, "paused": 1}
	for _, sv := range st.Servers {
		if sv.ID != "akka" {
			continue
		}
		for k, v := range wantCounts {
			if sv.Folders[k] != v {
				t.Errorf("status API folders.%s = %d, want %d", k, sv.Folders[k], v)
			}
		}
	}

	req, _ = http.NewRequest(http.MethodGet, e.srv.URL+"/api/v1/ha", nil)
	req.Header.Set("Authorization", "Bearer swt_test_token")
	resp, _ = http.DefaultClient.Do(req)
	var ha struct {
		Servers map[string]map[string]any `json:"servers"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&ha)
	resp.Body.Close()
	akka := ha.Servers["akka"]
	for k, v := range map[string]any{"status": "error", "folders": 6.0, "folders_syncing": 1.0, "folders_scanning": 1.0, "folders_error": 1.0, "folders_watch_error": 1.0, "folders_paused": 1.0} {
		if akka[k] != v {
			t.Errorf("HA %s = %v, want %v", k, akka[k], v)
		}
	}
	// With the folder error gone, the watcher error is the server's status.
	e.hub.Update("akka", func(s *model.Server) { delete(s.Folders, "f4") })
	e.eng.Evaluate()
	resp, _ = http.DefaultClient.Do(req)
	_ = json.NewDecoder(resp.Body).Decode(&ha)
	resp.Body.Close()
	if ha.Servers["akka"]["status"] != "watch error" {
		t.Errorf("HA status with a watcher error = %v, want \"watch error\"", ha.Servers["akka"]["status"])
	}
}

// Severity words are the same everywhere in the dashboard: badges, the
// filter and the summary line say error, warning and suggestion. Counts
// get their noun in the right number.
func TestSeverityWordsAndPlurals(t *testing.T) {
	e := adminEnv(t, twoServers) // two unreachable servers: two H1 errors
	e.hub.Update("akka", func(s *model.Server) {
		s.Status, s.Loaded, s.MyID = model.StatusUp, true, "AAAAAAA-BBBBBBB"
		s.Folders = map[string]*model.Folder{"f1": {ID: "f1", Label: "only-folder", State: "idle"}}
	})
	e.eng.Evaluate()
	_, body := e.get("/problems")
	for _, want := range []string{"🔴 ERROR</span>", `<option value="warn" >WARNING</option>`, `<option value="info" >INFO</option>`,
		"🔴 ERROR 1</span>", "🟠 WARNING 0</span>", "🔵 INFO 0</span>"} {
		if !strings.Contains(body, want) {
			t.Errorf("problems page lacks %q", want)
		}
	}
	for _, old := range []string{">WARN<", ">warn<", ">info<", "1 errors", "suggestion"} {
		if strings.Contains(body, old) {
			t.Errorf("problems page still says %q", old)
		}
	}
	if _, grid := e.get("/grid"); !strings.Contains(grid, "1 folder</span>") || strings.Contains(grid, "1 folders") {
		t.Error("grid: a group with one folder says folders")
	}
	for n, want := range map[int]string{0: "0 errors", 1: "1 error", 2: "2 errors"} {
		if got := plural(n, "error", "errors"); got != want {
			t.Errorf("plural(%d) = %q, want %q", n, got, want)
		}
	}
	if model.Error.Label() != "ERROR" || model.Warn.Label() != "WARNING" || model.Info.Label() != "INFO" {
		t.Error("severity labels")
	}
	if model.ParseSeverity("suggestion") != model.Info || model.ParseSeverity("warning") != model.Warn {
		t.Error("ParseSeverity does not accept the dashboard words")
	}
}

// "Snooze until date" covers the whole chosen day in the configured zone,
// today included, and takes a reason like ignore does.
func TestSnoozeUntilDate(t *testing.T) {
	e := adminEnv(t, twoServers+"timezone: Pacific/Auckland\n")
	auckland := e.cfg.Get().Location()
	// 10:00 UTC is 23:00 in Auckland (NZDT): still 2026-10-10 there.
	now := time.Date(2026, 10, 10, 10, 0, 0, 0, time.UTC)
	e.web.now = func() time.Time { return now }
	id := e.eng.View().Open[0].ID
	csrf := e.csrf("/problems")
	msg, isErr := flash(e.post("/problems/"+id+"/snooze", url.Values{"csrf": {csrf}, "for": {"date"}, "until": {"2026-10-10"}, "reason": {"waiting for the disk"}}))
	if isErr {
		t.Fatalf("snoozing until today refused: %s", msg)
	}
	z, ok := e.eng.View().Snoozes[id]
	if want := time.Date(2026, 10, 11, 0, 0, 0, 0, auckland); !ok || !z.Until.Equal(want) || z.Reason != "waiting for the disk" {
		t.Fatalf("snooze = %+v, want until %v with the reason", z, want)
	}
	if msg != "The finding is snoozed until 2026-10-11 00:00." {
		t.Errorf("flash %q", msg)
	}
	if msg, isErr := flash(e.post("/problems/"+id+"/snooze", url.Values{"csrf": {csrf}, "for": {"date"}, "until": {"2026-10-09"}})); !isErr {
		t.Errorf("yesterday accepted: %q", msg)
	}
	_, body := e.get("/problems")
	if !strings.Contains(body, `name="until" min="2026-10-10"`) {
		t.Error("the date field does not start at today")
	}
	i := strings.Index(body, "/snooze\"")
	if j := strings.Index(body[i:], "/ignore\""); i < 0 || j < 0 || !strings.Contains(body[i:i+j], `name="reason"`) {
		t.Error("the snooze form has no reason field")
	}
}

// "Resolved this week" counts every resolution of the last 7 days and
// lists the latest 30, newest first.
func TestResolvedThisWeekCountAndOrder(t *testing.T) {
	e := adminEnv(t, twoServers)
	now := time.Now()
	e.web.now = func() time.Time { return now }
	// Opened in order 00..34 three weeks ago; 00 resolved most recently.
	for i := 0; i < 35; i++ {
		r := &store.Record{Finding: model.Finding{ID: fmt.Sprintf("h%02d", i), Check: "H7", Severity: model.Warn, Message: fmt.Sprintf("resolved-%02d", i)},
			OpenedAt: now.Add(-21*24*time.Hour + time.Duration(i)*time.Hour)}
		if err := e.st.HistoryOpen(r); err != nil {
			t.Fatal(err)
		}
		if err := e.st.HistoryResolve(r.ID, now.Add(-time.Duration(i+1)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	_, body := e.get("/problems")
	if !strings.Contains(body, "35 resolved this week") || !strings.Contains(body, "Resolved this week (35") {
		t.Error("the summary does not show the real count")
	}
	first, second, dropped := strings.Index(body, "resolved-00"), strings.Index(body, "resolved-01"), strings.Index(body, "resolved-30")
	if first < 0 || second < first || dropped >= 0 {
		t.Errorf("list order: resolved-00 at %d, resolved-01 at %d, resolved-30 at %d (want newest first, oldest five dropped)", first, second, dropped)
	}
}

// Ignored and snoozed findings stay visible: a collapsed list on the
// problems page with a way back, and a count on the overview.
func TestSnoozedAndIgnoredStayVisible(t *testing.T) {
	e := adminEnv(t, twoServers)
	id := e.eng.View().Open[0].ID
	csrf := e.csrf("/problems")
	e.post("/problems/"+id+"/ignore", url.Values{"csrf": {csrf}, "reason": {"known and harmless"}})
	_, body := e.get("/problems")
	for _, want := range []string{"Snoozed and ignored (1)", "ignored: known and harmless", "/problems/" + id + "/unsnooze", "Stop ignoring", `id="f-` + id + `"`} {
		if !strings.Contains(body, want) {
			t.Errorf("problems page lacks %q", want)
		}
	}
	if _, ov := e.get("/"); !strings.Contains(ov, "1 snoozed or ignored") {
		t.Error("overview does not mention the ignored finding")
	}
	// With "Show snoozed and ignored" on, the rows are inline and the block is gone.
	_, body = e.get("/problems?snoozed=1")
	if strings.Contains(body, "Snoozed and ignored (") || !strings.Contains(body, "ignored: known and harmless") {
		t.Error("snoozed=1: want the ignored finding inline, no separate block")
	}
}

// A live update must not wipe an ignore reason or snooze date someone is
// typing: the shipped script cancels the swap while a form inside the
// updated section is in use. The behaviour itself was checked in a headless
// browser (see docs/changes/r0-k.md); this pins the asset.
func TestLiveUpdateGuardShipped(t *testing.T) {
	e := newEnv(t, twoServers)
	_, js := e.get("/assets/app.js")
	for _, want := range []string{`"htmx:beforeSwap"`, "shouldSwap = false", `htmx.trigger(t, "sse:update")`, `"focusout"`} {
		if !strings.Contains(js, want) {
			t.Errorf("app.js lacks %s", want)
		}
	}
	if _, css := e.get("/assets/app.css"); !strings.Contains(css, ".updates-paused") {
		t.Error("app.css has no style for a paused section")
	}
}

// The JSON status API and the Home Assistant endpoint count what the
// dashboard counts: the overview's open, urgent and snoozed numbers, and
// each tile's folder states.
func TestAPICountsMatchOverview(t *testing.T) {
	e := adminEnv(t, twoServers) // two unreachable servers: two H1 errors
	e.hub.Update("akka", func(s *model.Server) {
		s.Folders = map[string]*model.Folder{
			"f1": {ID: "f1", State: "idle"}, "f2": {ID: "f2", State: "syncing", NeedItems: 4},
			"f3": {ID: "f3", State: "idle", WatchError: "inotify limit"}, "f4": {ID: "f4", Paused: true},
		}
	})
	e.eng.Evaluate()
	csrf := e.csrf("/problems")
	e.post("/problems/"+e.eng.View().Open[0].ID+"/ignore", url.Values{"csrf": {csrf}, "reason": {"known"}})
	vm := e.web.overview(e.eng.View())
	if vm.OpenCount != 1 || vm.SnoozedCount != 1 {
		t.Fatalf("overview open=%d snoozed=%d, want 1 and 1", vm.OpenCount, vm.SnoozedCount)
	}
	tiles := map[string]serverTile{}
	for _, tile := range vm.Servers {
		tiles[tile.ID] = tile
	}

	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/api/v1/status", nil)
	req.Header.Set("Authorization", "Bearer swt_test_token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		Counts  map[string]int `json:"counts"`
		Servers []struct {
			ID      string         `json:"id"`
			Folders map[string]int `json:"folders"`
		} `json:"servers"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if open := st.Counts["error"] + st.Counts["warn"] + st.Counts["info"]; open != vm.OpenCount || st.Counts["urgent"] != vm.UrgentOpen || st.Counts["snoozed"] != vm.SnoozedCount {
		t.Errorf("status API counts %v; overview says open=%d urgent=%d snoozed=%d", st.Counts, vm.OpenCount, vm.UrgentOpen, vm.SnoozedCount)
	}
	for _, sv := range st.Servers {
		tile := tiles[sv.ID]
		for k, n := range map[string]int{"total": tile.Folders, "idle": tile.Idle, "syncing": tile.Syncing, "scanning": tile.Scanning, "error": tile.Errors, "watch_error": tile.WatchErrors, "paused": tile.Paused} {
			if sv.Folders[k] != n {
				t.Errorf("status API %s folders.%s = %d, the tile says %d", sv.ID, k, sv.Folders[k], n)
			}
		}
	}

	req, _ = http.NewRequest(http.MethodGet, e.srv.URL+"/api/v1/ha", nil)
	req.Header.Set("Authorization", "Bearer swt_test_token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var ha struct {
		Servers map[string]map[string]any `json:"servers"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&ha)
	resp.Body.Close()
	for id, tile := range tiles {
		for k, n := range map[string]int{"folders": tile.Folders, "folders_syncing": tile.Syncing, "folders_scanning": tile.Scanning, "folders_error": tile.Errors, "folders_watch_error": tile.WatchErrors, "folders_paused": tile.Paused} {
			if ha.Servers[id][k] != float64(n) {
				t.Errorf("HA %s %s = %v, the tile says %d", id, k, ha.Servers[id][k], n)
			}
		}
	}
	if tile := tiles["akka"]; tile.Folders != 4 || tile.Idle != 1 || tile.Syncing != 1 || tile.WatchErrors != 1 || tile.Paused != 1 {
		t.Errorf("akka tile: %+v", tile)
	}
}
