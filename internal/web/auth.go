package web

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/store"
)

// Roles.
const (
	RoleNone   = ""
	RoleViewer = "viewer"
	RoleAdmin  = "admin"
)

// Settings keys for credentials.
const (
	KeyAdminHash  = "auth.admin_hash"
	KeyViewerHash = "auth.viewer_hash"
)

const (
	sessionCookie = "sw_session"
	csrfCookie    = "sw_csrf"
	// hostPrefix starts the cookie names over HTTPS. A browser keeps a
	// __Host- cookie only with Secure, Path / and no Domain: a page on
	// another host name (a subdomain) or on plain HTTP cannot set or replace
	// it.
	hostPrefix = "__Host-"
)

// cookieName is the name of a cookie for a request: with the __Host- prefix
// over HTTPS (requestScheme), the plain name over HTTP.
func cookieName(https bool, name string) string {
	if https {
		return hostPrefix + name
	}
	return name
}

// argon2id parameters (OWASP minimum: 19 MiB, 2 iterations, 1 thread).
const (
	argonTime    = 2
	argonMemory  = 19 * 1024
	argonThreads = 1
	argonKeyLen  = 32
)

// hashSlots bounds concurrent argon2 computations. Each needs 19 MiB, so a
// burst of login attempts must not be able to exhaust the host's memory.
var hashSlots = make(chan struct{}, 2)

func argonKey(pw, salt []byte, t, m uint32, p uint8, n uint32) []byte {
	hashSlots <- struct{}{}
	defer func() { <-hashSlots }()
	return argon2.IDKey(pw, salt, t, m, p, n)
}

// HashPassword returns an argon2id PHC string.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argonKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads, b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

// CheckPassword verifies a password against an argon2id PHC string.
func CheckPassword(pw, encoded string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	b64 := base64.RawStdEncoding
	salt, err1 := b64.DecodeString(parts[4])
	want, err2 := b64.DecodeString(parts[5])
	if err1 != nil || err2 != nil || len(want) == 0 {
		return false
	}
	if m > 256*1024 || t > 16 || p == 0 || len(want) > 64 {
		return false // not a hash this program wrote; refuse to burn memory on it
	}
	got := argonKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// MinPasswordLength for admin and viewer passwords.
const MinPasswordLength = 10

func checkNewPassword(pw string) error {
	if len([]rune(pw)) < MinPasswordLength {
		return fmt.Errorf("The password must have at least %d characters.", MinPasswordLength)
	}
	return nil
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(t string) string {
	sum := sha256.Sum256([]byte(t))
	return hex.EncodeToString(sum[:])
}

// requestInfo is what auth decided about a request.
type requestInfo struct {
	Role    string
	Session *store.Session
	CSRF    string // the CSRF token that a form of this request must send
	HTTPS   bool   // the browser uses HTTPS (requestScheme)
}

func roleAtLeast(have, need string) bool {
	rank := map[string]int{RoleNone: 0, RoleViewer: 1, RoleAdmin: 2}
	return rank[have] >= rank[need]
}

// resolve works out the caller's role. Over HTTPS it reads only the cookies
// with the __Host- prefix. A session has its own CSRF token: a CSRF cookie
// that another page set does not replace it.
func (s *Server) resolve(r *http.Request) requestInfo {
	cfg := s.cfg.Get()
	info := requestInfo{HTTPS: s.requestScheme(r) == "https"}
	if c, err := r.Cookie(cookieName(info.HTTPS, csrfCookie)); err == nil {
		info.CSRF = c.Value
	}
	if cfg.Access.Mode == config.AccessProxy {
		info.Role = RoleAdmin
		return info
	}
	if c, err := r.Cookie(cookieName(info.HTTPS, sessionCookie)); err == nil && c.Value != "" {
		if z, err := s.st.Session(hashToken(c.Value), s.now()); err == nil && z != nil {
			info.Role, info.Session = z.Role, z
			// A session from before the CSRF token of the session (older
			// version) uses the cookie, as before.
			if z.CSRF != "" {
				info.CSRF = z.CSRF
			}
			if z.Role == RoleViewer && cfg.Access.Viewer == config.ViewerNone {
				info.Role = RoleNone
			}
			return info
		}
	}
	if cfg.Access.Viewer == config.ViewerOpen {
		info.Role = RoleViewer
	}
	return info
}

// requestScheme returns the scheme that the browser used: "https" for a
// request over TLS, or from a trusted reverse proxy that reports https in
// X-Forwarded-Proto; else "http". The header of another client does not
// count: it could make a plain HTTP dashboard set Secure cookies that the
// browser then does not send back, and use the HTTPS rules.
func (s *Server) requestScheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	vals := r.Header.Values("X-Forwarded-Proto")
	if len(vals) == 0 || !s.fromTrustedProxy(r) {
		return "http"
	}
	// The last value is the value of the trusted proxy: a proxy that adds
	// its value keeps the value that the client sent before it.
	last := vals[len(vals)-1]
	if i := strings.LastIndexByte(last, ','); i >= 0 {
		last = last[i+1:]
	}
	if strings.EqualFold(strings.TrimSpace(last), "https") {
		return "https"
	}
	return "http"
}

// fromTrustedProxy reports whether the request comes from an address of the
// trusted reverse proxies (TrustProxies).
func (s *Server) fromTrustedProxy(r *http.Request) bool {
	if len(s.trusted) == 0 {
		return false
	}
	a, err := netip.ParseAddr(clientIP(r))
	if err != nil {
		return false
	}
	a = a.Unmap().WithZone("")
	for _, p := range s.trusted {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

func (s *Server) secureCookies(r *http.Request) bool { return s.requestScheme(r) == "https" }

// TrustProxies sets the addresses of the reverse proxies whose
// X-Forwarded-Proto header syncwatch believes (SYNCWATCH_TRUSTED_PROXIES).
// Call it before serving.
func (s *Server) TrustProxies(p []netip.Prefix) {
	s.trusted = nil
	for _, x := range p {
		if a := x.Addr(); a.Is4In6() && x.Bits() >= 96 {
			x = netip.PrefixFrom(a.Unmap(), x.Bits()-96)
		}
		s.trusted = append(s.trusted, x.Masked())
	}
}

// setCSRFCookie sets the CSRF cookie: a cookie of the browser session, which
// a script of the page cannot read.
func setCSRFCookie(w http.ResponseWriter, https bool, tok string) {
	http.SetCookie(w, &http.Cookie{
		Name: cookieName(https, csrfCookie), Value: tok, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: https,
	})
}

// ensureCSRF sets the double-submit CSRF cookie if missing and returns its value.
func (s *Server) ensureCSRF(w http.ResponseWriter, r *http.Request, info *requestInfo) string {
	if info.CSRF != "" {
		return info.CSRF
	}
	info.CSRF = randomToken(24)
	setCSRFCookie(w, info.HTTPS, info.CSRF)
	return info.CSRF
}

// errCrossOrigin refuses a form from another page.
var errCrossOrigin = errors.New("cross-origin request refused")

// checkCSRF validates a state-changing request: the form field or header must
// match the CSRF token of the session (or, without one, the cookie), and
// Origin (when sent) must be this dashboard.
func checkCSRF(r *http.Request, info requestInfo) error {
	if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r.Host, info.HTTPS) {
		return errCrossOrigin
	}
	tok := r.Header.Get("X-CSRF-Token")
	if tok == "" {
		tok = r.FormValue("csrf")
	}
	if info.CSRF == "" || tok == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(info.CSRF)) != 1 {
		return errors.New("CSRF token missing or not valid: reload the page and try again")
	}
	return nil
}

// sameOrigin reports whether the Origin of a form is this dashboard: the
// host and port of the request, and the scheme https when the browser uses
// HTTPS. "null" (a sandboxed page, a file, a redirect from another site) is
// not this dashboard. Behind a reverse proxy that is not trusted syncwatch
// sees http: then an https Origin is correct too.
func sameOrigin(origin, host string, https bool) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Opaque != "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	switch u.Scheme {
	case "https":
	case "http":
		if https {
			return false
		}
	default:
		return false
	}
	return u.Host != "" && strings.EqualFold(u.Host, host)
}

// startSession logs a browser in: a new session with a new CSRF token, in
// the session and in the CSRF cookie. A CSRF token from before the login is
// not valid after it.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, role string) error {
	tok, csrf := randomToken(32), randomToken(24)
	now := s.now()
	ttl := s.cfg.Get().Access.SessionTTL.D()
	if err := s.st.CreateSession(store.Session{TokenHash: hashToken(tok), Role: role, CSRF: csrf, CreatedAt: now, ExpiresAt: now.Add(ttl)}); err != nil {
		return err
	}
	https := s.secureCookies(r)
	http.SetCookie(w, &http.Cookie{
		Name: cookieName(https, sessionCookie), Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: https, Expires: now.Add(ttl), MaxAge: int(ttl.Seconds()),
	})
	setCSRFCookie(w, https, csrf)
	if https {
		// The cookies of an older version over HTTPS have no prefix:
		// syncwatch does not read them any more.
		clearCookies(w, false, true, sessionCookie, csrfCookie)
	}
	return nil
}

// endSession logs a browser out. It clears the session and CSRF cookies in
// both forms, with and without the __Host- prefix.
func (s *Server) endSession(w http.ResponseWriter, r *http.Request) {
	https := s.secureCookies(r)
	for _, name := range []string{sessionCookie, hostPrefix + sessionCookie} {
		if c, err := r.Cookie(name); err == nil && c.Value != "" {
			_ = s.st.DeleteSession(hashToken(c.Value))
		}
	}
	clearCookies(w, false, https, sessionCookie, csrfCookie)
	if https {
		// A browser accepts a __Host- cookie, also an empty one, only over
		// HTTPS.
		clearCookies(w, true, true, sessionCookie, csrfCookie)
	}
}

// clearCookies tells the browser to delete cookies, with or without the
// __Host- prefix.
func clearCookies(w http.ResponseWriter, prefixed, secure bool, names ...string) {
	for _, name := range names {
		http.SetCookie(w, &http.Cookie{Name: cookieName(prefixed, name), Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
			SameSite: http.SameSiteLaxMode, Secure: secure})
	}
}

// limiter rate-limits failed password attempts. Failures from one client IP
// are capped hard. Failures from everywhere together only slow attempts down,
// so nobody can lock the admin out by failing logins from a few addresses.
// Logins count by client address; a password that an administrator session
// types to confirm a change counts by session (see confirmKey), so failed
// logins at a shared address do not block the confirmations.
type limiter struct {
	mu       sync.Mutex
	perIP    map[string]*attempts
	global   []time.Time
	window   time.Duration
	maxIP    int
	slowAll  int           // failures from everywhere before attempts are slowed down
	step     time.Duration // extra delay per failure above slowAll
	maxDelay time.Duration
}

// attempts is what the limiter holds against one client address: attempts
// still being checked, and failures by the role whose password was tried.
type attempts struct {
	inflight int
	failed   map[string][]time.Time
}

func newLimiter() *limiter {
	return &limiter{perIP: map[string]*attempts{}, window: 10 * time.Minute, maxIP: 5, slowAll: 30,
		step: 200 * time.Millisecond, maxDelay: 5 * time.Second}
}

func prune(ts []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(ts) && ts[i].Before(cutoff) {
		i++
	}
	return ts[i:]
}

// check reserves an attempt from ip. It reports whether the attempt may be
// made now, and how long to wait before checking the password. The attempt
// counts against ip until fail or success ends it, so attempts sent at the
// same moment can't all pass before the first failure is recorded.
func (l *limiter) check(ip string, now time.Time) (ok bool, delay time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := now.Add(-l.window)
	l.global = prune(l.global, cut)
	a := l.attempts(ip)
	for role, ts := range a.failed {
		if ts = prune(ts, cut); len(ts) == 0 {
			delete(a.failed, role)
			continue
		}
		a.failed[role] = ts
		if len(ts)+a.inflight >= l.maxIP {
			return false, 0
		}
	}
	if a.inflight >= l.maxIP {
		return false, 0
	}
	a.inflight++
	if over := len(l.global) - l.slowAll; over >= 0 {
		delay = min(time.Duration(over+1)*l.step, l.maxDelay)
	}
	return true, delay
}

func (l *limiter) attempts(ip string) *attempts {
	a := l.perIP[ip]
	if a == nil {
		a = &attempts{failed: map[string][]time.Time{}}
		l.perIP[ip] = a
	}
	return a
}

// end finishes an attempt from ip, forgetting the address once nothing is
// held against it.
func (l *limiter) end(ip string, edit func(*attempts)) {
	a := l.attempts(ip)
	if a.inflight > 0 {
		a.inflight--
	}
	edit(a)
	if a.inflight == 0 && len(a.failed) == 0 {
		delete(l.perIP, ip)
	}
}

// fail ends an attempt that failed, counting it against the roles whose
// password was tried. It reports true when failures from everywhere have
// just reached the point where attempts are slowed down.
func (l *limiter) fail(ip string, now time.Time, roles ...string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.end(ip, func(a *attempts) {
		for _, role := range roles {
			a.failed[role] = append(a.failed[role], now)
		}
	})
	l.global = append(l.global, now)
	// Only the count up to the longest delay matters.
	if limit := l.slowAll + int(l.maxDelay/l.step) + 1; len(l.global) > limit {
		l.global = l.global[len(l.global)-limit:]
	}
	if len(l.perIP) > 10000 {
		l.perIP = map[string]*attempts{}
	}
	return len(l.global) == l.slowAll
}

// retryAfter returns how long check keeps refusing ip: until enough of its
// failures are older than the window.
func (l *limiter) retryAfter(ip string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.perIP[ip]
	if a == nil {
		return 0
	}
	cut := now.Add(-l.window)
	var d time.Duration
	for _, ts := range a.failed {
		ts = prune(ts, cut)
		// The failure at i must expire for len(ts)+inflight to drop below maxIP.
		if i := len(ts) + a.inflight - l.maxIP; i >= 0 && i < len(ts) {
			d = max(d, ts[i].Add(l.window).Sub(now))
		}
	}
	return d
}

// wait sleeps for d unless the request goes away first.
func wait(r *http.Request, d time.Duration) {
	if d <= 0 {
		return
	}
	select {
	case <-r.Context().Done():
	case <-time.After(d):
	}
}

// success ends an attempt that passed and forgets the failed guesses at that
// role's password from ip. Guesses at the other role's password stay, so
// knowing the viewer password buys no extra guesses at the admin password.
func (l *limiter) success(ip, role string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.end(ip, func(a *attempts) { delete(a.failed, role) })
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
