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
)

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
		return fmt.Errorf("the password must be at least %d characters", MinPasswordLength)
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
	CSRF    string
}

func roleAtLeast(have, need string) bool {
	rank := map[string]int{RoleNone: 0, RoleViewer: 1, RoleAdmin: 2}
	return rank[have] >= rank[need]
}

// resolve works out the caller's role.
func (s *Server) resolve(r *http.Request) requestInfo {
	cfg := s.cfg.Get()
	info := requestInfo{}
	if c, err := r.Cookie(csrfCookie); err == nil {
		info.CSRF = c.Value
	}
	if cfg.Access.Mode == config.AccessProxy {
		info.Role = RoleAdmin
		return info
	}
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if z, err := s.st.Session(hashToken(c.Value), s.now()); err == nil && z != nil {
			info.Role, info.Session = z.Role, z
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

func (s *Server) secureCookies(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

// ensureCSRF sets the double-submit CSRF cookie if missing and returns its value.
func (s *Server) ensureCSRF(w http.ResponseWriter, r *http.Request, info *requestInfo) string {
	if info.CSRF != "" {
		return info.CSRF
	}
	info.CSRF = randomToken(24)
	http.SetCookie(w, &http.Cookie{
		Name: csrfCookie, Value: info.CSRF, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: s.secureCookies(r),
	})
	return info.CSRF
}

// checkCSRF validates a state-changing request: the form field or header must
// match the cookie, and Origin (when sent) must be this host.
func checkCSRF(r *http.Request, info requestInfo) error {
	if origin := r.Header.Get("Origin"); origin != "" && origin != "null" {
		host := r.Host
		if !strings.HasSuffix(origin, "://"+host) {
			return errors.New("cross-origin request refused")
		}
	}
	tok := r.Header.Get("X-CSRF-Token")
	if tok == "" {
		tok = r.FormValue("csrf")
	}
	if info.CSRF == "" || tok == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(info.CSRF)) != 1 {
		return errors.New("invalid or missing CSRF token; reload the page and try again")
	}
	return nil
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request, role string) error {
	tok := randomToken(32)
	now := s.now()
	ttl := s.cfg.Get().Access.SessionTTL.D()
	if err := s.st.CreateSession(store.Session{TokenHash: hashToken(tok), Role: role, CSRF: "", CreatedAt: now, ExpiresAt: now.Add(ttl)}); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: tok, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: s.secureCookies(r), Expires: now.Add(ttl), MaxAge: int(ttl.Seconds()),
	})
	return nil
}

func (s *Server) endSession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = s.st.DeleteSession(hashToken(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

// limiter rate-limits failed password attempts. Failures from one client IP
// are capped hard. Failures from everywhere together only slow attempts down,
// so nobody can lock the admin out by failing logins from a few addresses.
type limiter struct {
	mu       sync.Mutex
	perIP    map[string][]time.Time
	global   []time.Time
	window   time.Duration
	maxIP    int
	slowAll  int           // failures from everywhere before attempts are slowed down
	step     time.Duration // extra delay per failure above slowAll
	maxDelay time.Duration
}

func newLimiter() *limiter {
	return &limiter{perIP: map[string][]time.Time{}, window: 10 * time.Minute, maxIP: 5, slowAll: 30,
		step: 200 * time.Millisecond, maxDelay: 5 * time.Second}
}

func prune(ts []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(ts) && ts[i].Before(cutoff) {
		i++
	}
	return ts[i:]
}

// check reports whether another attempt from ip may be made now, and how
// long to wait before checking the password.
func (l *limiter) check(ip string, now time.Time) (ok bool, delay time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := now.Add(-l.window)
	l.perIP[ip] = prune(l.perIP[ip], cut)
	l.global = prune(l.global, cut)
	if len(l.perIP[ip]) >= l.maxIP {
		return false, 0
	}
	if over := len(l.global) - l.slowAll; over >= 0 {
		delay = min(time.Duration(over+1)*l.step, l.maxDelay)
	}
	return true, delay
}

// fail records a failed attempt. It reports true when failures from
// everywhere have just reached the point where attempts are slowed down.
func (l *limiter) fail(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.perIP[ip] = append(l.perIP[ip], now)
	l.global = append(l.global, now)
	// Only the count up to the longest delay matters.
	if limit := l.slowAll + int(l.maxDelay/l.step) + 1; len(l.global) > limit {
		l.global = l.global[len(l.global)-limit:]
	}
	if len(l.perIP) > 10000 {
		l.perIP = map[string][]time.Time{}
	}
	return len(l.global) == l.slowAll
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

func (l *limiter) success(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.perIP, ip)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
