// Package gate is a read-only proxy in front of one Syncthing instance.
// It holds Syncthing's API key, which can change anything, and gives
// syncwatch a token that only reaches the GET endpoints syncwatch's client
// is allowed to use. Secrets in Syncthing's answers (the GUI credentials in
// ConfigSaved events, passwords for untrusted devices in the folder
// configuration) are removed before they leave, so whoever holds the token,
// or syncwatch's /data, can read Syncthing's state but not change it.
package gate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ivarstudios/syncwatch/internal/stclient"
)

// Paths the gate treats specially: cleaned of secrets, restricted in what
// they may ask for, or read by the gate itself.
const (
	pathEvents        = "/rest/events"
	pathConfigFolders = "/rest/config/folders"
	pathHealth        = "/rest/noauth/health"
	pathSystemStatus  = "/rest/system/status"
	pathBrowse        = "/rest/system/browse"
	pathDBStatus      = "/rest/db/status"
	pathCompletion    = "/rest/db/completion"
	pathFolderErrors  = "/rest/folder/errors"
)

// maxBody caps an answer that has to be read to be cleaned.
const maxBody = 64 << 20

// Limits on what a request may ask for, and how long the gate keeps
// Syncthing's folder list.
const (
	maxEventTimeout = 600              // seconds; the collector's longest long-poll
	rootsMaxAge     = 10 * time.Minute // Syncthing's folder list is read again after this
	rootsMissAge    = time.Minute      // or sooner, when a directory isn't under it (a folder may just have been added)
	rootsRetry      = 30 * time.Second // wait after a failed read of the folder list
	logInterval     = time.Minute      // one log line per client, reason and minute about refused requests
	maxRefusals     = 4096             // refusal counters kept (client × reason)
)

// Timeouts toward Syncthing. Event long-polls go through their own
// transport without a header timeout; their deadline comes from the
// request's own timeout (eventsWait).
const (
	dialTimeout   = 10 * time.Second
	headerTimeout = 60 * time.Second // ResponseHeaderTimeout for everything but events
	maxEventsWait = 11 * time.Minute // the most an events long-poll may take
)

// eventsExtra is how much longer than its own timeout an events long-poll
// may take. A variable so tests can shorten it.
var eventsExtra = 30 * time.Second

// queryParams lists, per endpoint, the parameters syncwatch's client sends.
// An endpoint that isn't listed takes no query at all.
var queryParams = map[string][]string{
	pathBrowse:       {"current"},
	pathEvents:       {"since", "timeout", "events"},
	pathDBStatus:     {"folder"},
	pathCompletion:   {"folder", "device"},
	pathFolderErrors: {"folder", "page", "perpage"},
}

// eventMask is the collector's event subscription, the only one the token
// may ask for.
var eventMask = func() map[string]bool {
	m := map[string]bool{}
	for _, e := range stclient.EventMask {
		m[e] = true
	}
	return m
}()

// Options configure a Gate.
type Options struct {
	Syncthing   string          // Syncthing's GUI/API base URL, e.g. https://127.0.0.1:8384
	APIKey      string          // Syncthing's API key
	Token       string          // what syncwatch sends as X-API-Key
	Pin         string          // pinned SHA-256 of Syncthing's certificate, "" = trust on first use
	OnPin       func(fp string) // called with the fingerprint to store whenever the pin is set or replaced
	RootCAs     *x509.CertPool  // nil means the system pool
	BrowseRoots []string        // directories the token may list besides the parents of Syncthing's folders
	Log         *slog.Logger
}

// Gate is the HTTP handler.
type Gate struct {
	o        Options
	base     *url.URL
	loopback bool
	hc       *http.Client // Syncthing
	hcLong   *http.Client // Syncthing, for event long-polls
	allowed  map[string]bool
	now      func() time.Time

	mu  sync.Mutex
	pin string

	// The directories a browse request may list, from Syncthing's folder
	// list, and when it was read.
	rmu     sync.Mutex
	roots   []browsePath
	windows bool // Syncthing's path separator is \
	rootsAt time.Time
	tryAt   time.Time
	tryErr  error

	lmu      sync.Mutex
	refusals map[string]*refusal // per client and reason
}

// refusal counts one client's refused requests for one reason since the
// last log line about them.
type refusal struct {
	n  int
	at time.Time // when they were last logged
}

// New creates a gate. It does not contact Syncthing.
func New(o Options) (*Gate, error) {
	u, err := url.Parse(strings.TrimRight(o.Syncthing, "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("the Syncthing API URL must be https://address:port or http://address:port")
	}
	if o.APIKey == "" {
		return nil, errors.New("the gate needs the Syncthing API key")
	}
	if len(o.Token) < 16 {
		return nil, errors.New("the gate token must be at least 16 characters")
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	ip := net.ParseIP(u.Hostname())
	g := &Gate{o: o, base: u, pin: o.Pin, allowed: map[string]bool{}, now: time.Now, refusals: map[string]*refusal{},
		loopback: u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())}
	for _, p := range stclient.Allowlist() {
		g.allowed[p] = true
	}
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	g.hc = &http.Client{Transport: g.transport(headerTimeout), CheckRedirect: noRedirect}
	g.hcLong = &http.Client{Transport: g.transport(0), CheckRedirect: noRedirect}
	return g, nil
}

// transport connects to Syncthing: no proxy, a dial timeout, and a
// response header timeout unless 0 (event long-polls).
func (g *Gate) transport(headerTimeout time.Duration) *http.Transport {
	return &http.Transport{
		Proxy:       nil,
		DialContext: (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // checked in verify
			VerifyConnection:   g.verify,
			MinVersion:         tls.VersionTLS12,
		},
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: headerTimeout,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       60 * time.Second,
	}
}

// verify checks Syncthing's certificate: anything on loopback (the gate
// runs next to Syncthing). Elsewhere the certificate is pinned on first use,
// CA-signed or not, and may only change to one that chains to a trusted CA
// for the host name (a renewal); any other change refuses the connection.
func (g *Gate) verify(cs tls.ConnectionState) error {
	if g.loopback {
		return nil
	}
	if len(cs.PeerCertificates) == 0 {
		return errors.New("Syncthing sent no certificate")
	}
	leaf := cs.PeerCertificates[0]
	inter := x509.NewCertPool()
	for _, c := range cs.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	_, caErr := leaf.Verify(x509.VerifyOptions{DNSName: g.base.Hostname(), Roots: g.o.RootCAs, Intermediates: inter})
	sum := sha256.Sum256(leaf.Raw)
	fp := hex.EncodeToString(sum[:])
	g.mu.Lock()
	defer g.mu.Unlock()
	log := g.o.Log.With("sha256", stclient.ShortFP(fp))
	switch {
	case g.pin == fp:
		return nil
	case g.pin == "":
		if caErr == nil {
			log.Info("CA-signed Syncthing certificate pinned")
		} else {
			log.Info("self-signed Syncthing certificate accepted and pinned on first use")
		}
	case caErr == nil:
		log.Info("Syncthing certificate replaced, new certificate pinned", "old", stclient.ShortFP(g.pin))
	default:
		return fmt.Errorf("%w (pinned %s, new %s). If the new certificate is correct, delete syncthing.pin in the data directory of the gate. Then restart the gate.",
			errCertChanged, stclient.ShortFP(g.pin), stclient.ShortFP(fp))
	}
	g.pin = fp
	if g.o.OnPin != nil {
		g.o.OnPin(fp)
	}
	return nil
}

// deny refuses a request. why is one of a few fixed reasons, detail says
// more when there is more to say. Refusals are logged once a minute per
// client and reason, with a count, so a scanner can't fill the log.
func (g *Gate) deny(w http.ResponseWriter, r *http.Request, code int, why, detail string) {
	ip, now := clientIP(r), g.now()
	g.lmu.Lock()
	if len(g.refusals) >= maxRefusals {
		for k, rf := range g.refusals {
			if now.Sub(rf.at) >= logInterval {
				delete(g.refusals, k)
			}
		}
		if len(g.refusals) >= maxRefusals {
			clear(g.refusals)
		}
	}
	rf := g.refusals[ip+" "+why]
	if rf == nil {
		rf = &refusal{}
		g.refusals[ip+" "+why] = rf
	}
	rf.n++
	n := 0
	if rf.at.IsZero() || now.Sub(rf.at) >= logInterval {
		n, rf.n, rf.at = rf.n, 0, now
	}
	g.lmu.Unlock()
	if n > 0 {
		g.o.Log.Warn("gate refused a request", "why", why, "detail", detail, "method", r.Method, "path", r.URL.Path, "ip", ip, "count", n)
	}
	if detail != "" {
		why += ": " + detail
	}
	http.Error(w, "syncwatch gate: "+why, code)
}

// ServeHTTP passes allowlisted GET requests with the right token on to
// Syncthing, with Syncthing's own key.
func (g *Gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path == "/healthz" && r.Method == http.MethodGet {
		_, _ = w.Write([]byte("ok\n")) // the gate itself, for the container healthcheck
		return
	}
	// The token first, for everything (Syncthing's noauth health too): a
	// client without it learns nothing, not even which paths exist.
	tok := r.Header.Get("X-API-Key")
	if tok == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(g.o.Token)) != 1 {
		g.deny(w, r, http.StatusForbidden, "gate token missing or not correct", "")
		return
	}
	if r.Method != http.MethodGet {
		g.deny(w, r, http.StatusMethodNotAllowed, "only GET requests are accepted", "")
		return
	}
	// Exact paths only: no prefixes, no dot segments, no encoded slashes.
	if !g.allowed[r.URL.Path] || (r.URL.RawPath != "" && r.URL.RawPath != r.URL.Path) {
		g.deny(w, r, http.StatusNotFound, "endpoint not permitted", "")
		return
	}
	// Only the parameters syncwatch's client sends, and only what it asks for.
	q, err := checkQuery(r.URL.Path, r.URL.RawQuery)
	if err != nil {
		g.deny(w, r, http.StatusBadRequest, "query not permitted", err.Error())
		return
	}
	if r.URL.Path == pathBrowse {
		dir, ok, err := g.checkBrowse(q.Get("current"))
		if err != nil {
			http.Error(w, upstreamError(err, "cannot read the folder list of Syncthing"), http.StatusBadGateway)
			return
		}
		if !ok {
			g.deny(w, r, http.StatusForbidden, "directory not in a permitted browse root", "")
			return
		}
		q.Set("current", dir)
	}
	hc, ctx := g.hc, r.Context()
	if r.URL.Path == pathEvents {
		// A long-poll may take as long as it asked for, plus a margin.
		hc = g.hcLong
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, eventsWait(q.Get("timeout")))
		defer cancel()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.upstream(r.URL.Path, q.Encode()), nil)
	if err != nil {
		http.Error(w, "syncwatch gate: incorrect request", http.StatusBadRequest)
		return
	}
	if r.URL.Path != pathHealth {
		req.Header.Set("X-API-Key", g.o.APIKey)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		if r.Context().Err() == nil {
			g.o.Log.Warn("request to Syncthing failed", "path", r.URL.Path, "err", err)
		}
		http.Error(w, upstreamError(err, "Syncthing unreachable"), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	clean := cleaner(r.URL.Path)
	if clean == nil || resp.StatusCode/100 != 2 {
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, io.LimitReader(resp.Body, maxBody))
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		http.Error(w, "syncwatch gate: cannot read the answer of Syncthing", http.StatusBadGateway)
		return
	}
	out, err := clean(body)
	if err != nil {
		// An answer that can't be cleaned isn't passed on.
		g.o.Log.Warn("cannot check the answer from Syncthing, answer refused", "path", r.URL.Path, "err", err)
		http.Error(w, "syncwatch gate: unknown answer format from Syncthing", http.StatusBadGateway)
		return
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
}

// upstream is the URL of path at Syncthing.
func (g *Gate) upstream(path, rawQuery string) string {
	up := *g.base
	up.Path = strings.TrimRight(g.base.Path, "/") + path
	up.RawPath, up.RawQuery = "", rawQuery
	return up.String()
}

// checkQuery parses a request's query and refuses what syncwatch's client
// never sends: parameters the endpoint doesn't take, repeated ones, event
// long-polls over 10 minutes, event masks other than the collector's own.
// What it returns is what is passed on.
func checkQuery(path, raw string) (url.Values, error) {
	q, err := url.ParseQuery(raw)
	if err != nil {
		return nil, errors.New("the query is not valid")
	}
	for k, vs := range q {
		if !slices.Contains(queryParams[path], k) {
			return nil, fmt.Errorf("the gate does not permit the parameter %.40q", k)
		}
		if len(vs) != 1 {
			return nil, fmt.Errorf("the parameter %q appears 2 times", k)
		}
	}
	switch path {
	case pathBrowse:
		if cur := q.Get("current"); cur == "" {
			return nil, errors.New("the parameter current is necessary")
		} else if !strings.HasSuffix(cur, "/") && !strings.HasSuffix(cur, `\`) {
			return nil, errors.New("the parameter current must end with a separator")
		}
	case pathEvents:
		if v := q.Get("since"); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err != nil || n < 0 {
				return nil, errors.New("the parameter since must be 0 or a positive integer")
			}
		}
		if v := q.Get("timeout"); v != "" {
			if n, err := strconv.Atoi(v); err != nil || n < 0 || n > maxEventTimeout {
				return nil, fmt.Errorf("the parameter timeout must be between 0 and %d seconds", maxEventTimeout)
			}
		}
		if v := q.Get("events"); v != "" && !sameMask(strings.Split(v, ",")) {
			return nil, errors.New("the parameter events must be the event mask of the collector")
		}
	case pathFolderErrors:
		for _, k := range []string{"page", "perpage"} {
			if v := q.Get(k); v != "" {
				if n, err := strconv.Atoi(v); err != nil || n < 1 {
					return nil, fmt.Errorf("the parameter %s must be a positive integer", k)
				}
			}
		}
	}
	return q, nil
}

// eventsWait is how long an events long-poll may take: its own timeout
// (Syncthing's default when it has none) plus a margin, at most 11 minutes.
func eventsWait(timeout string) time.Duration {
	n := 60
	if timeout != "" {
		n, _ = strconv.Atoi(timeout) // checked by checkQuery
	}
	if d := time.Duration(n)*time.Second + eventsExtra; d < maxEventsWait {
		return d
	}
	return maxEventsWait
}

// sameMask reports whether names are exactly the collector's event mask,
// in any order. Syncthing keeps a buffered subscription per distinct mask
// for as long as it runs, so the token mustn't get to invent masks.
func sameMask(names []string) bool {
	seen := map[string]bool{}
	for _, n := range names {
		if !eventMask[n] {
			return false
		}
		seen[n] = true
	}
	return len(seen) == len(eventMask)
}

// browsePath is a directory path the way Syncthing's host will read it:
// its root ("/" or, on Windows, "C:/" or "//" for a UNC path; "" when
// relative) and its elements, with empty and dot elements resolved.
type browsePath struct {
	root  string
	elems []string
	dir   bool // ended with a separator: lists that directory (without one, Syncthing lists names starting with the last element)
}

// parseBrowsePath cleans p as Syncthing's host resolves it. When
// Syncthing's separator is \ both separators count; otherwise only / does
// and \ is an ordinary character. Case is kept; compare with sameName.
func parseBrowsePath(p string, windows bool) browsePath {
	if windows {
		p = strings.ReplaceAll(p, `\`, "/")
	}
	bp := browsePath{dir: strings.HasSuffix(p, "/")}
	switch {
	case windows && len(p) >= 3 && p[1] == ':' && p[2] == '/' && isLetter(p[0]):
		bp.root, p = p[:3], p[3:]
	case windows && strings.HasPrefix(p, "//"):
		bp.root = "//"
	case strings.HasPrefix(p, "/"):
		bp.root = "/"
	}
	for _, e := range strings.Split(p, "/") {
		switch e {
		case "", ".":
		case "..":
			if n := len(bp.elems); n > 0 && bp.elems[n-1] != ".." {
				bp.elems = bp.elems[:n-1]
			} else if bp.root == "" {
				bp.elems = append(bp.elems, "..")
			}
		default:
			bp.elems = append(bp.elems, e)
		}
	}
	return bp
}

func isLetter(c byte) bool { return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' }

// String renders the path with Syncthing's separator, with a trailing one
// when it names a directory.
func (bp browsePath) String(windows bool) string {
	s := bp.root + strings.Join(bp.elems, "/")
	if bp.dir && len(bp.elems) > 0 {
		s += "/"
	}
	if windows {
		s = strings.ReplaceAll(s, "/", `\`)
	}
	return s
}

// empty reports a path with nothing in it; Syncthing lists its roots for it.
func (bp browsePath) empty() bool { return bp.root == "" && len(bp.elems) == 0 }

// under reports whether bp is root or lies inside it.
func (bp browsePath) under(root browsePath, windows bool) bool {
	if len(bp.elems) < len(root.elems) || !sameName(bp.root, root.root, windows) {
		return false
	}
	for i, e := range root.elems {
		if !sameName(bp.elems[i], e, windows) {
			return false
		}
	}
	return true
}

func sameName(a, b string, windows bool) bool {
	if windows {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// checkBrowse cleans a browse request's directory and reports whether the
// token may list it: it has to lie under the parent of one of Syncthing's
// folders, or under an extra root. The cleaned path is what is passed on,
// so what was checked is what Syncthing lists. err means Syncthing's folder
// list isn't known yet.
func (g *Gate) checkBrowse(current string) (dir string, ok bool, err error) {
	g.rmu.Lock()
	defer g.rmu.Unlock()
	if err := g.refreshRootsLocked(rootsMaxAge); err != nil && g.rootsAt.IsZero() {
		return "", false, err
	}
	p := parseBrowsePath(current, g.windows)
	if !p.dir || p.empty() {
		return "", false, nil
	}
	if !g.underRootLocked(p) {
		// A folder may just have been added: look again, at most once a minute.
		if g.refreshRootsLocked(rootsMissAge) != nil || !g.underRootLocked(p) {
			return "", false, nil
		}
	}
	return p.String(g.windows), true, nil
}

func (g *Gate) underRootLocked(p browsePath) bool {
	for _, r := range g.roots {
		if p.under(r, g.windows) {
			return true
		}
	}
	return false
}

// refreshRootsLocked reads Syncthing's folder list when the one it holds
// is older than maxAge. A failed read isn't repeated for a while, and the
// old list stays in use.
func (g *Gate) refreshRootsLocked(maxAge time.Duration) error {
	now := g.now()
	if !g.rootsAt.IsZero() && now.Sub(g.rootsAt) < maxAge {
		return nil
	}
	if !g.tryAt.IsZero() && now.Sub(g.tryAt) < rootsRetry {
		return g.tryErr
	}
	g.tryAt = now
	roots, windows, err := g.fetchRoots()
	if g.tryErr = err; err != nil {
		g.o.Log.Warn("cannot read the folder list of Syncthing, browse requests need it", "err", cleanErr(err))
		return err
	}
	g.roots, g.windows, g.rootsAt = roots, windows, now
	return nil
}

// fetchRoots reads Syncthing's path separator and folder list and returns
// the directories the token may list: each folder's parent (a drive root
// counts, / doesn't: syncwatch never lists it) and the extra roots.
func (g *Gate) fetchRoots() (roots []browsePath, windows bool, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), headerTimeout)
	defer cancel()
	var st stclient.SystemStatus
	if err := g.getJSON(ctx, pathSystemStatus, &st); err != nil {
		return nil, false, err
	}
	var folders []stclient.FolderConfig
	if err := g.getJSON(ctx, pathConfigFolders, &folders); err != nil {
		return nil, false, err
	}
	windows = st.PathSeparator == `\`
	for _, f := range folders {
		p := parseBrowsePath(f.Path, windows)
		if n := len(p.elems); n > 0 {
			p.elems = p.elems[:n-1]
		}
		if p.empty() || (p.root == "/" && len(p.elems) == 0) {
			continue
		}
		roots = append(roots, p)
	}
	for _, r := range g.o.BrowseRoots {
		if p := parseBrowsePath(r, windows); !p.empty() {
			roots = append(roots, p)
		}
	}
	return roots, windows, nil
}

// getJSON is the gate's own request to Syncthing.
func (g *Gate) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.upstream(path, ""), nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", g.o.APIKey)
	req.Header.Set("Accept", "application/json")
	resp, err := g.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d", path, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(out)
}

// cleaner returns the function that removes secrets from an endpoint's
// answer, or nil when it carries none.
func cleaner(path string) func([]byte) ([]byte, error) {
	switch path {
	case pathEvents:
		return cleanEvents
	case pathConfigFolders:
		return cleanFolders
	}
	return nil
}

// cleanEvents drops the payload of ConfigSaved events: it is Syncthing's
// whole configuration, including the GUI API key and password hash.
func cleanEvents(body []byte) ([]byte, error) {
	var evs []map[string]json.RawMessage
	if err := json.Unmarshal(body, &evs); err != nil {
		return nil, err
	}
	for _, e := range evs {
		var typ string
		_ = json.Unmarshal(e["type"], &typ)
		if typ == "ConfigSaved" {
			delete(e, "data")
		}
	}
	return json.Marshal(evs)
}

// cleanFolders drops the passwords for untrusted (encrypted) devices from
// the folder configuration.
func cleanFolders(body []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var folders []map[string]any
	if err := dec.Decode(&folders); err != nil {
		return nil, err
	}
	for _, f := range folders {
		devs, _ := f["devices"].([]any)
		for _, d := range devs {
			if m, ok := d.(map[string]any); ok {
				delete(m, "encryptionPassword")
			}
		}
	}
	return json.Marshal(folders)
}

// errCertChanged marks the verify error for a changed Syncthing certificate.
var errCertChanged = errors.New("the certificate of Syncthing changed")

// upstreamError is the body of a 502 for an error reaching Syncthing.
// syncwatch's client keeps 200 bytes of it, so it stays under 150, and it
// says what to do in the one case where the operator must act.
func upstreamError(err error, what string) string {
	if errors.Is(err, errCertChanged) || strings.Contains(err.Error(), errCertChanged.Error()) {
		return "syncwatch gate: the certificate of Syncthing changed. Delete syncthing.pin in the data directory of the gate. Then restart the gate"
	}
	return "syncwatch gate: " + what + ": " + cleanErr(err)
}

// maxErrText is how much of an upstream error a 502 body carries.
const maxErrText = 90

// cleanErr keeps an upstream error short and free of the request URL.
func cleanErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	s := err.Error()
	if len(s) > maxErrText {
		s = s[:maxErrText]
	}
	return s
}

func clientIP(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}
