package web

import (
	"context"
	"encoding/json"
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
	if resp.StatusCode != 200 || !strings.Contains(body, `"check": "H1"`) || !strings.Contains(body, "0/2 servers OK") {
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
	_, _ = e.st.RecordTraffic("akka", "s", 0, 0, now.Add(-6*24*time.Hour))
	_, _ = e.st.RecordTraffic("akka", "s", 4_000_000_000, 1_000_000_000, now.Add(-3*24*time.Hour)) // inside the week only
	_, _ = e.st.RecordTraffic("akka", "s", 5_500_000_000, 1_250_000_000, now.Add(-time.Hour))

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
	const banner = "Notifications are failing"
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
