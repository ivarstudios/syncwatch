package harness

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Syncwatch is a running syncwatch process.
type Syncwatch struct {
	Bin      string
	DataDir  string
	Addr     string // host:port
	Token    string // API token
	Password string // admin password
	cmd      *exec.Cmd
	done     chan struct{}
	logPath  string
	hc       *http.Client
	scheme   string // "" is http
}

// IVARRules is the structure section used by the simulation (the IVAR example rules).
const IVARRules = `structure:
  project_pattern: '^(ME-)?LIVE-[A-Za-z0-9]'
  share_type_from_path: '-(?P<me>ME-)?LIVE-(?P<type>[1-5][A-Z]+)$'
  type_tokens: [1SOURCE, 2PROJECTFILES, 3INTERMEDIATE, 4FINALS, 5COLLECT]
  placement:
    - when_name_matches: '^ME-LIVE-'
      share_must_have: { me: true }
    - when_name_matches: '^LIVE-'
      share_must_have: { me: false }
  ignore_dirs: ['.*', '@*', '#recycle']
  nested_scan_depth: 3
`

// WriteConfig writes a bootstrap YAML for the environment with short test
// timings. extra is appended verbatim.
func WriteConfig(path string, env *Env, webhook, role, token string, extra string) error {
	var b strings.Builder
	b.WriteString("public_url: http://dashboard.test\ntimezone: Europe/Stockholm\nservers:\n")
	for _, inst := range env.Order {
		if !inst.Server {
			continue
		}
		fmt.Fprintf(&b, "  - name: %s\n    url: %s\n    api_key: %s\n", inst.Name, inst.URL(), inst.APIKey)
	}
	fmt.Fprintf(&b, `collector:
  mode: events
  event_timeout: 20s
  full_check_interval: 5m
  structure_interval: 1m
  request_timeout: 10s
  down_after_failures: 2
checks:
  H1: { debounce: 15s }
  H2: { debounce: 30s }
  H3: { debounce: 5s }
  H4: { debounce: 15s }
  S1: { debounce: 10s }
  S2: { debounce: 10s }
  S3: { debounce: 10s }
thresholds:
  paused: 20s
  pc_offline: 40s
notifications:
  discord_webhook: %s
  discord_role_id: "%s"
  quiet_hours: "off"
  reminder_time: "off"
  weekly_day: "off"
api:
  token: %s
`, webhook, role, token)
	b.WriteString(IVARRules)
	b.WriteString(extra)
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

// StartSyncwatch runs the binary with a data directory containing syncwatch.yaml.
func StartSyncwatch(bin, dataDir, addr, password, token string) (*Syncwatch, error) {
	jar, _ := cookiejar.New(nil)
	s := &Syncwatch{Bin: bin, DataDir: dataDir, Addr: addr, Token: token, Password: password,
		logPath: filepath.Join(dataDir, "syncwatch.log"),
		hc:      &http.Client{Jar: jar, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	return s, s.Start()
}

// Start launches the process.
func (s *Syncwatch) Start() error {
	logf, err := os.OpenFile(s.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	cmd := exec.Command(s.Bin, "serve", "--data", s.DataDir, "--listen", s.Addr, "--log-level", "debug")
	// The fake Discord isn't on discord.com.
	cmd.Env = append(os.Environ(), "SYNCWATCH_ADMIN_PASSWORD="+s.Password, "SYNCWATCH_ALLOW_ANY_WEBHOOK=true")
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		return err
	}
	s.cmd = cmd
	s.done = make(chan struct{})
	go func() {
		_ = cmd.Wait()
		logf.Close()
		close(s.done)
	}()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := s.hc.Get(s.URL() + "/healthz"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return nil
			}
		}
		select {
		case <-s.done:
			return fmt.Errorf("syncwatch exited; see %s", s.logPath)
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("syncwatch did not start")
}

// Stop kills the process.
func (s *Syncwatch) Stop() {
	if s.cmd == nil {
		return
	}
	_ = s.cmd.Process.Kill()
	<-s.done
	s.cmd = nil
}

// URL is the dashboard base URL.
func (s *Syncwatch) URL() string {
	if s.scheme != "" {
		return s.scheme + "://" + s.Addr
	}
	return "http://" + s.Addr
}

// LogPath is the process log.
func (s *Syncwatch) LogPath() string { return s.logPath }

// Status is the subset of /api/v1/status the tests use.
type Status struct {
	Health  string `json:"health"`
	Servers []struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Status  string `json:"status"`
		Version string `json:"version"`
		Folders struct {
			Total int `json:"total"`
		} `json:"folders"`
	} `json:"servers"`
	Devices []struct {
		Name      string `json:"name"`
		Server    bool   `json:"server"`
		Connected bool   `json:"connected"`
	} `json:"devices"`
	Findings []Finding      `json:"findings"`
	Counts   map[string]int `json:"counts"`
}

// Finding is one finding from the status API.
type Finding struct {
	ID       string   `json:"id"`
	Check    string   `json:"check"`
	Checks   []string `json:"checks"`
	Severity string   `json:"severity"`
	Server   string   `json:"server"`
	Subject  string   `json:"subject"`
	Message  string   `json:"message"`
	Urgent   bool     `json:"urgent"`
	Stale    bool     `json:"stale"`
	Snoozed  bool     `json:"snoozed"`
	Details  []struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	} `json:"details"`
}

// Detail returns a detail value by key.
func (f Finding) Detail(key string) string {
	for _, d := range f.Details {
		if d.Key == key {
			return d.Value
		}
	}
	return ""
}

// Has reports whether the finding includes a check.
func (f Finding) Has(check string) bool {
	for _, c := range f.Checks {
		if c == check {
			return true
		}
	}
	return f.Check == check
}

// Status fetches /api/v1/status with the bearer token.
func (s *Syncwatch) Status() (*Status, error) {
	req, _ := http.NewRequest(http.MethodGet, s.URL()+"/api/v1/status", nil)
	req.Header.Set("Authorization", "Bearer "+s.Token)
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status: HTTP %d %s", resp.StatusCode, b)
	}
	var st Status
	return &st, json.NewDecoder(resp.Body).Decode(&st)
}

// Find returns the first open finding with a check whose subject or message contains part.
func (st *Status) Find(check, part string) *Finding {
	for i := range st.Findings {
		f := &st.Findings[i]
		if f.Has(check) && strings.Contains(f.Subject+" "+f.Message, part) {
			return f
		}
	}
	return nil
}

// WaitStatus polls the status API until pred is true.
func (s *Syncwatch) WaitStatus(ctx context.Context, timeout time.Duration, pred func(*Status) bool) (*Status, error) {
	deadline := time.Now().Add(timeout)
	var last *Status
	var lastErr error
	for time.Now().Before(deadline) {
		st, err := s.Status()
		if err == nil {
			last = st
			if pred(st) {
				return st, nil
			}
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return last, ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if last == nil {
		return nil, fmt.Errorf("timed out: %v", lastErr)
	}
	return last, fmt.Errorf("timed out waiting for condition")
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

// Login logs in as admin with the cookie jar.
func (s *Syncwatch) Login() error {
	page, err := s.Get("/login")
	if err != nil {
		return err
	}
	m := csrfRe.FindStringSubmatch(page)
	if m == nil {
		return fmt.Errorf("no CSRF token on the login page")
	}
	resp, err := s.hc.PostForm(s.URL()+"/login", url.Values{"csrf": {m[1]}, "password": {s.Password}, "next": {"/"}})
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		return fmt.Errorf("login: HTTP %d", resp.StatusCode)
	}
	return nil
}

// Get fetches a page with the session cookie and returns its body.
func (s *Syncwatch) Get(path string) (string, error) {
	resp, err := s.hc.Get(s.URL() + path)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return string(b), fmt.Errorf("GET %s: HTTP %d", path, resp.StatusCode)
	}
	return string(b), nil
}

// PostForm submits a form with the CSRF token from page, returning the redirect location.
func (s *Syncwatch) PostForm(page, path string, form url.Values) (string, error) {
	body, err := s.Get(page)
	if err != nil {
		return "", err
	}
	m := csrfRe.FindStringSubmatch(body)
	if m == nil {
		return "", fmt.Errorf("no CSRF token on %s", page)
	}
	form.Set("csrf", m[1])
	resp, err := s.hc.PostForm(s.URL()+path, form)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther && resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("POST %s: HTTP %d %s", path, resp.StatusCode, b)
	}
	return resp.Header.Get("Location"), nil
}

// Client exposes the HTTP client (with the session cookie).
func (s *Syncwatch) Client() *http.Client { return s.hc }

// StartSyncwatchRemote returns a client for a syncwatch that is already
// running elsewhere (e.g. in a container). An "https://" address is reached
// over HTTPS, accepting the image's self-signed certificate.
func StartSyncwatchRemote(addr, password, token string) (*Syncwatch, error) {
	jar, _ := cookiejar.New(nil)
	s := &Syncwatch{Addr: addr, Token: token, Password: password,
		hc: &http.Client{Jar: jar, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if rest, ok := strings.CutPrefix(addr, "https://"); ok {
		s.Addr, s.scheme = rest, "https"
		s.hc.Transport = &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}} //nolint:gosec // test stack's self-signed certificate
	}
	return s, nil
}
