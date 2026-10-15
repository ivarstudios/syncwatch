package web

// The shared helpers of the web tests: the test server (env), requests,
// forms and log buffers. The test files of the package only use them.

import (
	"bytes"
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

	mu         sync.Mutex
	notices    []string        // titles and summaries sent through Hooks.Notice
	noticeAll  []notify.Notice // the same notices with their Security flag
	accepted   []string        // AcceptCert calls
	resets     []string        // ResetPin calls
	measured   []string        // MeasureSize calls: "server|path"
	measureErr error           // what MeasureSize answers
	delivery   notify.Delivery
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

// noticeValues returns the settings notices sent so far, with their Security
// flag. The summaries end with the client address line of announce.
func (e *env) noticeValues() []notify.Notice {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]notify.Notice(nil), e.noticeAll...)
}

// hasAPIToken reports whether an API token with the value tok works: the
// single API token of an older version (the secret api.token), or a row of
// api_tokens.
func (e *env) hasAPIToken(tok string) bool {
	e.t.Helper()
	if v, err := e.st.Secret(store.SecretAPIToken); err == nil && v != "" && v == tok {
		return true
	}
	t, err := e.st.APITokenByValue(tok)
	return err == nil && t != nil
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
		MeasureSize: func(id, path string) error {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.measured = append(e.measured, id+"|"+path)
			return e.measureErr
		},
		SendTest:        func(context.Context) error { return nil },
		SendNoticesTest: func(context.Context) error { return nil },
		Delivery: func() notify.Delivery {
			e.mu.Lock()
			defer e.mu.Unlock()
			return e.delivery
		},
		Notice: func(n notify.Notice) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.notices = append(e.notices, n.Title+": "+n.Summary)
			e.noticeAll = append(e.noticeAll, n)
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

// postPage sends a form and returns the answer with its body.
func (e *env) postPage(path string, form url.Values) (*http.Response, string) {
	e.t.Helper()
	resp, err := e.http.PostForm(e.srv.URL+path, form)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// section returns the part of a settings page from the section with the id
// to the next section.
func section(t *testing.T, page, id string) string {
	t.Helper()
	i := strings.Index(page, `<section class="card" id="`+id+`">`)
	if i < 0 {
		t.Fatalf("no section %s", id)
	}
	if j := strings.Index(page[i+1:], "<section "); j >= 0 {
		return page[i : i+1+j]
	}
	return page[i:]
}

// stranger logs in from a browser of its own at the address of the test
// client (another person behind the same VPN or reverse proxy).
func (e *env) stranger(password string) int {
	e.t.Helper()
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
	return resp.StatusCode
}

// lockedBuffer collects log lines that the server goroutines write.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// notices returns the notices sent so far.
func (e *env) noticeList() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.notices...)
}

// checksForm fills the checks form with the current configuration.
func checksForm(cfg *config.Config, csrf string) url.Values {
	form := url.Values{"csrf": {csrf}}
	for _, c := range config.Catalog {
		if cfg.CheckEnabled(c.ID) {
			form.Set("enabled_"+c.ID, "on")
		}
		if cfg.Urgent(c.ID) {
			form.Set("urgent_"+c.ID, "on")
		}
		form.Set("debounce_"+c.ID, config.Duration(cfg.Debounce(c.ID)).String())
	}
	th := cfg.Thresholds
	for k, v := range map[string]config.Duration{"failing_files": th.FailingFiles, "stuck_sync": th.StuckSync, "paused": th.Paused, "pc_offline": th.PCOffline,
		"conflict_window": th.ConflictWindow, "inactive_project": th.InactiveProject, "old_device": th.OldDevice, "dead_share": th.DeadShare} {
		form.Set(k, v.String())
	}
	return form
}

// notifyForm fills the notifications form with the current configuration.
func notifyForm(cfg *config.Config, csrf string) url.Values {
	n := cfg.Notify
	return url.Values{"csrf": {csrf}, "discord_role_id": {n.DiscordRoleID}, "quiet_hours": {n.QuietHours}, "reminder_time": {n.ReminderTime},
		"weekly_day": {n.WeeklyDay}, "weekly_time": {n.WeeklyTime}, "weekly_top": {"10"}, "timezone": {cfg.Timezone}, "public_url": {cfg.PublicURL}}
}

var revRe = regexp.MustCompile(`name="rev" value="([^"]+)"`)

// syncBuffer is a log destination that the server's goroutines can share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// apiGet sends an API request with the given Authorization header ("" for
// none) and Host ("" for the test server's), and decodes a JSON answer.
func (e *env) apiGet(path, auth, host string) (*http.Response, map[string]any) {
	e.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+path, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if host != "" {
		req.Host = host
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		e.t.Fatalf("%s: %d, not JSON: %q", path, resp.StatusCode, b)
	}
	return resp, out
}

// apiServers returns the servers[] entries of /api/v1/status by ID.
func apiServers(st map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	list, _ := st["servers"].([]any)
	for _, x := range list {
		sv, _ := x.(map[string]any)
		id, _ := sv["id"].(string)
		out[id] = sv
	}
	return out
}
