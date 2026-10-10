// Package web serves the dashboard (server-rendered HTML with htmx and
// server-sent events), the settings page and the JSON status API.
package web

import (
	"bytes"
	"context"
	"crypto/subtle"
	"embed"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/engine"
	"github.com/ivarstudios/syncwatch/internal/notify"
	"github.com/ivarstudios/syncwatch/internal/store"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed assets
var assetFS embed.FS

// Hooks are actions the web layer asks the application to perform.
type Hooks struct {
	SaveConfig      func(*config.Config) error
	AcceptCert      func(serverID, fingerprint string) error
	ResetPin        func(serverID string) error
	SendTest        func(ctx context.Context) error
	Notice          func(title, summary string)        // announce a security-relevant settings change
	NoticeLater     func() func(title, summary string) // capture today's channels, announce to them later
	ChannelsChanged func()                             // notification channels were replaced; retry now
	Delivery        func() notify.Delivery             // whether notifications reach their channels
}

// Server is the HTTP handler.
type Server struct {
	cfg     *config.Holder
	st      *store.Store
	eng     *engine.Engine
	hooks   Hooks
	log     *slog.Logger
	version string
	now     func() time.Time

	tmpl    *template.Template
	limiter *limiter
	mux     *http.ServeMux
	hosts   []string // names answered besides addresses, localhost and public_url's host

	setupMu    sync.Mutex
	setupToken string
}

// New creates the web server.
func New(cfg *config.Holder, st *store.Store, eng *engine.Engine, hooks Hooks, version string, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{cfg: cfg, st: st, eng: eng, hooks: hooks, log: log, version: version, now: time.Now, limiter: newLimiter()}
	t, err := template.New("").Funcs(s.funcs()).ParseFS(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}
	s.tmpl = t
	s.routes()
	return s, nil
}

// SetupToken returns the one-time setup token, creating it when no admin
// password exists yet. It returns "" once an admin password is set.
func (s *Server) SetupToken() string {
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	if h, _ := s.st.Setting(KeyAdminHash); h != "" {
		s.setupToken = ""
		return ""
	}
	if s.setupToken == "" {
		s.setupToken = randomToken(18)
	}
	return s.setupToken
}

// LogSetupToken logs the setup token while no admin password is set in
// built-in mode. When the log level hides warnings the line goes to stderr
// as well: the first start can't be finished without it.
func (s *Server) LogSetupToken(stderr io.Writer) {
	tok := s.SetupToken()
	if tok == "" || s.cfg.Get().Access.Mode != config.AccessBuiltin {
		return
	}
	const msg = "no admin password is set yet: open the dashboard and enter this one-time setup token"
	s.log.Warn(msg, "setup_token", tok)
	if !s.log.Enabled(context.Background(), slog.LevelWarn) {
		fmt.Fprintf(stderr, "syncwatch: %s: %s\n", msg, tok)
	}
}

func (s *Server) adminConfigured() bool {
	h, _ := s.st.Setting(KeyAdminHash)
	return h != ""
}

// AllowHosts adds names the dashboard answers to: the host of the listen
// address and extra names (SYNCWATCH_TLS_HOSTS). Addresses, localhost and
// the host of public_url are always answered. Call it before serving.
func (s *Server) AllowHosts(listen string, extra ...string) {
	if h, _, err := net.SplitHostPort(listen); err == nil && h != "" {
		s.hosts = append(s.hosts, h)
	}
	for _, h := range extra {
		if h = strings.TrimSpace(h); h != "" {
			s.hosts = append(s.hosts, h)
		}
	}
}

// hostAllowed reports whether the Host header names this server. A page on
// any other name (DNS rebinding) is refused: it would be same-origin with
// its own name, so the Origin check alone can't keep it out.
func (s *Server) hostAllowed(r *http.Request) bool {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.Trim(host, "[]"), ".")
	if host == "" {
		return false
	}
	if _, err := netip.ParseAddr(host); err == nil || strings.EqualFold(host, "localhost") {
		return true
	}
	if u, err := url.Parse(s.cfg.Get().PublicURL); err == nil && strings.EqualFold(u.Hostname(), host) {
		return true
	}
	for _, h := range s.hosts {
		if strings.EqualFold(strings.TrimSuffix(h, "."), host) {
			return true
		}
	}
	return false
}

func (s *Server) routes() {
	m := http.NewServeMux()
	sub, _ := fs.Sub(assetFS, "assets")
	assets := http.StripPrefix("/assets/", http.FileServer(http.FS(sub)))
	m.Handle("GET /assets/", cacheFor(assets, 24*time.Hour))
	// Browsers and bookmark tools that don't read the <link> tags ask for this.
	m.Handle("GET /favicon.ico", cacheFor(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, sub, "syncwatch-icon.ico")
	}), 24*time.Hour))
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.st.Ping(r.Context()); err != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})

	m.HandleFunc("GET /login", s.page(RoleNone, s.handleLoginPage))
	m.HandleFunc("POST /login", s.page(RoleNone, s.handleLogin))
	m.HandleFunc("POST /logout", s.action(RoleNone, s.handleLogout))
	m.HandleFunc("GET /setup", s.page(RoleNone, s.handleSetupPage))
	m.HandleFunc("POST /setup", s.page(RoleNone, s.handleSetup))

	m.HandleFunc("GET /{$}", s.page(RoleViewer, s.handleOverview))
	m.HandleFunc("GET /grid", s.page(RoleViewer, s.handleGrid))
	m.HandleFunc("GET /problems", s.page(RoleViewer, s.handleProblems))
	m.HandleFunc("GET /frag/overview", s.page(RoleViewer, s.fragOverview))
	m.HandleFunc("GET /frag/grid", s.page(RoleViewer, s.fragGrid))
	m.HandleFunc("GET /frag/problems", s.page(RoleViewer, s.fragProblems))
	m.HandleFunc("GET /events", s.page(RoleViewer, s.handleSSE))
	m.HandleFunc("GET /frag/alert", s.page(RoleViewer, s.fragAlert))

	m.HandleFunc("POST /problems/{id}/snooze", s.action(RoleAdmin, s.handleSnooze))
	m.HandleFunc("POST /problems/{id}/ignore", s.action(RoleAdmin, s.handleIgnore))
	m.HandleFunc("POST /problems/{id}/unsnooze", s.action(RoleAdmin, s.handleUnsnooze))

	m.HandleFunc("GET /settings", s.page(RoleAdmin, s.handleSettings))
	m.HandleFunc("POST /settings/servers", s.action(RoleAdmin, s.handleServerSave))
	m.HandleFunc("POST /settings/servers/{id}/delete", s.action(RoleAdmin, s.handleServerDelete))
	m.HandleFunc("POST /settings/servers/{id}/accept-cert", s.action(RoleAdmin, s.handleAcceptCert))
	m.HandleFunc("POST /settings/notifications", s.action(RoleAdmin, s.handleNotifySave))
	m.HandleFunc("POST /settings/notifications/test", s.action(RoleAdmin, s.handleNotifyTest))
	m.HandleFunc("POST /settings/checks", s.action(RoleAdmin, s.handleChecksSave))
	m.HandleFunc("POST /settings/structure", s.action(RoleAdmin, s.handleStructureSave))
	m.HandleFunc("POST /settings/collector", s.action(RoleAdmin, s.handleCollectorSave))
	m.HandleFunc("POST /settings/access", s.action(RoleAdmin, s.handleAccessSave))
	m.HandleFunc("POST /settings/password", s.action(RoleAdmin, s.handlePasswordSave))
	m.HandleFunc("POST /settings/api-token", s.action(RoleAdmin, s.handleAPIToken))
	m.HandleFunc("GET /settings/export", s.page(RoleAdmin, s.handleExport))
	m.HandleFunc("POST /settings/import", s.action(RoleAdmin, s.handleImport))

	m.HandleFunc("GET /api/v1/status", s.handleAPIStatus)
	m.HandleFunc("GET /api/v1/ha", s.handleAPIHA)
	s.mux = m
}

// ServeHTTP adds security headers to every response.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=()")
	if !s.hostAllowed(r) {
		s.log.Info("request for an unknown host name refused", "host", r.Host, "ip", clientIP(r))
		http.Error(w, fmt.Sprintf("syncwatch doesn't answer to the host name %q: open it by IP address and set public_url under Settings → Notifications, or set SYNCWATCH_TLS_HOSTS to the names you use", r.Host), http.StatusMisdirectedRequest)
		return
	}
	s.mux.ServeHTTP(w, r)
}

func cacheFor(h http.Handler, d time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(d.Seconds())))
		h.ServeHTTP(w, r)
	})
}

type handler func(w http.ResponseWriter, r *http.Request, info requestInfo)

// page wraps a GET (or login/setup POST) handler with role checks.
func (s *Server) page(need string, h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Login and setup parse a form before anyone is logged in.
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		info := s.resolve(r)
		s.ensureCSRF(w, r, &info)
		if !roleAtLeast(info.Role, need) {
			s.deny(w, r, info)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		h(w, r, info)
	}
}

// action wraps a state-changing POST with role and CSRF checks.
func (s *Server) action(need string, h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
		info := s.resolve(r)
		if !roleAtLeast(info.Role, need) {
			s.deny(w, r, info)
			return
		}
		if err := checkCSRF(r, info); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		h(w, r, info)
	}
}

func (s *Server) deny(w http.ResponseWriter, r *http.Request, info requestInfo) {
	if r.Header.Get("HX-Request") == "true" || strings.HasPrefix(r.URL.Path, "/frag/") || r.URL.Path == "/events" {
		w.Header().Set("HX-Redirect", "/login")
		http.Error(w, "login required", http.StatusUnauthorized)
		return
	}
	if info.Role != RoleNone {
		s.render(w, r, http.StatusForbidden, "error", info, map[string]any{"Title": "Not allowed", "Message": "This page needs the admin password."})
		return
	}
	if !s.adminConfigured() && s.cfg.Get().Access.Mode == config.AccessBuiltin {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	to := "/login"
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		// Only a page can be returned to; a form has to be sent again.
		to += "?next=" + urlQueryEscape(r.URL.RequestURI())
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func urlQueryEscape(s string) string {
	return strings.NewReplacer("%", "%25", "&", "%26", "?", "%3F", "#", "%23", "+", "%2B", " ", "%20").Replace(s)
}

// pageData is passed to every full-page template.
type pageData struct {
	Title    string
	Nav      string
	Role     string
	LoggedIn bool
	CSRF     string
	Version  string
	Proxy    bool
	Flash    string
	FlashErr string
	Alert    *deliveryAlert
	Data     any
}

// deliveryAlert is the banner shown while notifications are failing.
type deliveryAlert struct {
	Since string
	Error string // only for admins
}

func (s *Server) alert(info requestInfo) *deliveryAlert {
	if info.Role == RoleNone || s.hooks.Delivery == nil {
		return nil
	}
	d := s.hooks.Delivery()
	if !d.Failing {
		return nil
	}
	a := &deliveryAlert{Since: s.fmtTime(d.Since)}
	if info.Role == RoleAdmin {
		a.Error = d.Error
	}
	return a
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name string, info requestInfo, data any) {
	pd := pageData{Role: info.Role, LoggedIn: info.Session != nil, CSRF: info.CSRF, Version: s.version, Proxy: s.cfg.Get().Access.Mode == config.AccessProxy, Alert: s.alert(info), Data: data}
	if m, ok := data.(map[string]any); ok {
		if t, ok := m["Title"].(string); ok {
			pd.Title = t
		}
	}
	if c, err := r.Cookie("sw_flash"); err == nil {
		pd.Flash = flashDecode(c.Value)
		http.SetCookie(w, &http.Cookie{Name: "sw_flash", Path: "/", MaxAge: -1})
	}
	if c, err := r.Cookie("sw_flash_err"); err == nil {
		pd.FlashErr = flashDecode(c.Value)
		http.SetCookie(w, &http.Cookie{Name: "sw_flash_err", Path: "/", MaxAge: -1})
	}
	pd.Nav = name
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, pd); err != nil {
		s.log.Error("rendering template", "template", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) fragment(w http.ResponseWriter, name string, info requestInfo, data any) {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, pageData{Role: info.Role, CSRF: info.CSRF, Alert: s.alert(info), Data: data}); err != nil {
		s.log.Error("rendering fragment", "template", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

// maxFlash caps an encoded flash message, well under browsers' 4 KB limit
// for a cookie with its name and attributes.
const maxFlash = 3000

// flashEncode percent-encodes a flash message for its cookie (cookie values
// must be ASCII), cut on a character boundary when it would be too long.
func flashEncode(s string) string {
	enc := url.QueryEscape(s)
	if len(enc) <= maxFlash {
		return enc
	}
	more := url.QueryEscape("…")
	var b strings.Builder
	for _, c := range s {
		e := url.QueryEscape(string(c))
		if b.Len()+len(e)+len(more) > maxFlash {
			break
		}
		b.WriteString(e)
	}
	return b.String() + more
}

func flashDecode(s string) string {
	d, err := url.QueryUnescape(s)
	if err != nil {
		return ""
	}
	return d
}

// redirect with a flash message shown on the next page.
func (s *Server) redirect(w http.ResponseWriter, r *http.Request, to, msg string, isErr bool) {
	name := "sw_flash"
	if isErr {
		name = "sw_flash_err"
	}
	if msg != "" {
		http.SetCookie(w, &http.Cookie{Name: name, Value: flashEncode(msg), Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 60})
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func (s *Server) funcs() template.FuncMap {
	return template.FuncMap{
		"join": strings.Join,
		"title": func(id string) string {
			return config.CatalogByID[id].Title
		},
		"lower": strings.ToLower,
		"eq2":   func(a, b any) bool { return fmt.Sprint(a) == fmt.Sprint(b) },
		"datefmt": func(t time.Time) string {
			if t.IsZero() {
				return ""
			}
			return t.In(s.cfg.Get().Location()).Format("2006-01-02")
		},
		"timefmt":      s.fmtTime,
		"severityName": func(sev any) string { return fmt.Sprint(sev) },
		"plus1":        func(i int) int { return i + 1 },
		"list":         func(s ...string) []string { return s },
	}
}

// Login, logout and first-run setup.

func (s *Server) handleLoginPage(w http.ResponseWriter, r *http.Request, info requestInfo) {
	if s.cfg.Get().Access.Mode == config.AccessProxy {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if !s.adminConfigured() {
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
		return
	}
	if info.Session != nil {
		// Already logged in, for example through a link from Discord.
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "login", info, map[string]any{"Title": "Log in", "Next": safeNext(r.URL.Query().Get("next"))})
}

// safeNext returns n if it is a path on this site, otherwise "/". Browsers
// drop tabs and newlines from URLs and read a backslash as a slash, so
// "/\t/evil.example" or "/\evil.example" would leave the site.
func safeNext(n string) string {
	if !strings.HasPrefix(n, "/") || strings.HasPrefix(n, "//") || strings.ContainsRune(n, '\\') {
		return "/"
	}
	for _, c := range n {
		if c < 0x20 || c == 0x7f {
			return "/"
		}
	}
	if u, err := url.Parse(n); err != nil || u.Scheme != "" || u.Host != "" || u.User != nil {
		return "/"
	}
	return n
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request, info requestInfo) {
	if err := checkCSRF(r, info); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	ip := clientIP(r)
	now := s.now()
	next := safeNext(r.FormValue("next"))
	ok, delay := s.limiter.check(ip, now)
	if !ok {
		s.log.Warn("login rate limit reached", "ip", ip)
		s.render(w, r, http.StatusTooManyRequests, "login", info, map[string]any{"Title": "Log in", "Next": next, "Error": "Too many failed attempts. Try again in a few minutes."})
		return
	}
	wait(r, delay)
	pw := r.FormValue("password")
	role := RoleNone
	tried := []string{RoleAdmin} // a wrong password was a guess at each password it was checked against
	if h, _ := s.st.Setting(KeyAdminHash); h != "" && CheckPassword(pw, h) {
		role = RoleAdmin
	} else if s.cfg.Get().Access.Viewer == config.ViewerPassword {
		tried = append(tried, RoleViewer)
		if h, _ := s.st.Setting(KeyViewerHash); h != "" && CheckPassword(pw, h) {
			role = RoleViewer
		}
	}
	if role == RoleNone {
		if s.limiter.fail(ip, now, tried...) {
			s.log.Warn("many failed logins from different addresses; logins are slowed down for a while")
		}
		s.log.Warn("failed login", "ip", ip)
		time.Sleep(300 * time.Millisecond)
		s.render(w, r, http.StatusUnauthorized, "login", info, map[string]any{"Title": "Log in", "Next": next, "Error": "Wrong password."})
		return
	}
	s.limiter.success(ip, role)
	if err := s.startSession(w, r, role); err != nil {
		http.Error(w, "could not start session", http.StatusInternalServerError)
		return
	}
	s.log.Info("login", "role", role, "ip", ip)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request, info requestInfo) {
	s.endSession(w, r)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func (s *Server) handleSetupPage(w http.ResponseWriter, r *http.Request, info requestInfo) {
	if s.adminConfigured() || s.cfg.Get().Access.Mode == config.AccessProxy {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.render(w, r, http.StatusOK, "setup", info, map[string]any{"Title": "Set up syncwatch"})
}

func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request, info requestInfo) {
	if err := checkCSRF(r, info); err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	if s.adminConfigured() {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	ip := clientIP(r)
	ok, delay := s.limiter.check(ip, s.now())
	if !ok {
		s.render(w, r, http.StatusTooManyRequests, "setup", info, map[string]any{"Title": "Set up syncwatch", "Error": "Too many failed attempts. Try again later."})
		return
	}
	wait(r, delay)
	want := s.SetupToken()
	got := strings.TrimSpace(r.FormValue("token"))
	if want == "" || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
		s.limiter.fail(ip, s.now(), RoleAdmin)
		s.render(w, r, http.StatusUnauthorized, "setup", info, map[string]any{"Title": "Set up syncwatch", "Error": "Wrong setup token. It is printed in the container log."})
		return
	}
	s.limiter.success(ip, RoleAdmin)
	pw := r.FormValue("password")
	if err := checkNewPassword(pw); err != nil {
		s.render(w, r, http.StatusBadRequest, "setup", info, map[string]any{"Title": "Set up syncwatch", "Error": err.Error(), "Token": got})
		return
	}
	if pw != r.FormValue("password2") {
		s.render(w, r, http.StatusBadRequest, "setup", info, map[string]any{"Title": "Set up syncwatch", "Error": "The passwords don't match.", "Token": got})
		return
	}
	hash, err := HashPassword(pw)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if err := s.st.SetSetting(KeyAdminHash, hash); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	s.SetupToken() // clears the token
	_ = s.startSession(w, r, RoleAdmin)
	s.log.Info("admin password set through first-run setup", "ip", ip)
	s.redirect(w, r, "/settings", "Admin password set. Add your Syncthing servers below.", false)
}

// Dashboard pages.

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request, info requestInfo) {
	v := s.eng.View()
	s.render(w, r, http.StatusOK, "overview", info, map[string]any{"Title": "Overview", "VM": s.overview(v)})
}

func (s *Server) fragOverview(w http.ResponseWriter, r *http.Request, info requestInfo) {
	s.fragment(w, "overview-body", info, map[string]any{"VM": s.overview(s.eng.View())})
}

func (s *Server) handleGrid(w http.ResponseWriter, r *http.Request, info requestInfo) {
	s.render(w, r, http.StatusOK, "grid", info, map[string]any{"Title": "Folders", "VM": s.grid(s.eng.View())})
}

func (s *Server) fragGrid(w http.ResponseWriter, r *http.Request, info requestInfo) {
	s.fragment(w, "grid-body", info, map[string]any{"VM": s.grid(s.eng.View())})
}

func (s *Server) handleProblems(w http.ResponseWriter, r *http.Request, info requestInfo) {
	f := parseFilter(r.URL.Query())
	s.render(w, r, http.StatusOK, "problems", info, map[string]any{"Title": "Problems", "VM": s.problems(s.eng.View(), f), "Admin": info.Role == RoleAdmin})
}

func (s *Server) fragProblems(w http.ResponseWriter, r *http.Request, info requestInfo) {
	f := parseFilter(r.URL.Query())
	s.fragment(w, "problems-body", info, map[string]any{"VM": s.problems(s.eng.View(), f), "Admin": info.Role == RoleAdmin})
}

// fragAlert refreshes the notification banner on open pages.
func (s *Server) fragAlert(w http.ResponseWriter, r *http.Request, info requestInfo) {
	s.fragment(w, "alert", info, nil)
}

// SSE: one "update" event whenever the visible state changes, at most every 3 s.
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request, info requestInfo) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch, cancel := s.eng.Subscribe()
	defer cancel()
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "retry: 5000\n\n")
	fl.Flush()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	var last time.Time
	pending := false
	var timer <-chan time.Time
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case <-ch:
			if time.Since(last) < 3*time.Second {
				if !pending {
					pending = true
					timer = time.After(3*time.Second - time.Since(last))
				}
				continue
			}
			last = time.Now()
			fmt.Fprint(w, "event: update\ndata: 1\n\n")
			fl.Flush()
		case <-timer:
			pending, timer = false, nil
			last = time.Now()
			fmt.Fprint(w, "event: update\ndata: 1\n\n")
			fl.Flush()
		}
	}
}

// Problem actions.

func (s *Server) handleSnooze(w http.ResponseWriter, r *http.Request, info requestInfo) {
	id := r.PathValue("id")
	var until time.Time
	now := s.now()
	switch r.FormValue("for") {
	case "1d":
		until = now.Add(24 * time.Hour)
	case "1w":
		until = now.Add(7 * 24 * time.Hour)
	case "date":
		d, err := time.ParseInLocation("2006-01-02", r.FormValue("until"), s.cfg.Get().Location())
		if err != nil || !d.After(now) {
			s.redirect(w, r, backTo(r), "Pick a date in the future.", true)
			return
		}
		until = d
	default:
		s.redirect(w, r, backTo(r), "Unknown snooze length.", true)
		return
	}
	reason := strings.TrimSpace(r.FormValue("reason"))
	if err := s.eng.Snooze(id, until, reason, info.Role); err != nil {
		s.redirect(w, r, backTo(r), err.Error(), true)
		return
	}
	detail := "Snoozed until " + s.fmtTime(until) + "."
	if reason != "" {
		detail += " Reason: " + reason
	}
	s.announceHidden(r, id, "snoozed", detail)
	s.redirect(w, r, backTo(r), "Snoozed until "+s.fmtTime(until)+".", false)
}

func (s *Server) handleIgnore(w http.ResponseWriter, r *http.Request, info requestInfo) {
	reason := strings.TrimSpace(r.FormValue("reason"))
	if reason == "" {
		s.redirect(w, r, backTo(r), "Give a reason for ignoring the problem.", true)
		return
	}
	if err := s.eng.Ignore(r.PathValue("id"), reason, info.Role); err != nil {
		s.redirect(w, r, backTo(r), err.Error(), true)
		return
	}
	s.announceHidden(r, r.PathValue("id"), "ignored", "Ignored: "+reason)
	s.redirect(w, r, backTo(r), "Problem ignored.", false)
}

// announceHidden tells the channels when an urgent problem is snoozed or
// ignored, since nobody is pinged about it from then on.
func (s *Server) announceHidden(r *http.Request, id, what, detail string) {
	for _, f := range s.eng.View().Open {
		if f.ID == id && f.Urgent {
			s.announce(r, "Urgent problem "+what, f.Check+" "+f.Subject+": "+f.Message+"\n"+detail)
			return
		}
	}
}

func (s *Server) handleUnsnooze(w http.ResponseWriter, r *http.Request, info requestInfo) {
	if err := s.eng.Unsnooze(r.PathValue("id")); err != nil {
		s.redirect(w, r, backTo(r), err.Error(), true)
		return
	}
	s.redirect(w, r, backTo(r), "Snooze removed.", false)
}

func backTo(r *http.Request) string {
	if b := r.FormValue("back"); b != "" {
		return safeNext(b)
	}
	return "/problems"
}
