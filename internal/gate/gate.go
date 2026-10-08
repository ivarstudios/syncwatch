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
	"strings"
	"sync"
	"time"

	"github.com/ivarstudios/syncwatch/internal/stclient"
)

// Paths whose answers are cleaned of secrets before they are passed on.
const (
	pathEvents        = "/rest/events"
	pathConfigFolders = "/rest/config/folders"
	pathHealth        = "/rest/noauth/health"
)

// maxBody caps an answer that has to be read to be cleaned.
const maxBody = 64 << 20

// Options configure a Gate.
type Options struct {
	Syncthing string          // Syncthing's GUI/API base URL, e.g. https://127.0.0.1:8384
	APIKey    string          // Syncthing's API key
	Token     string          // what syncwatch sends as X-API-Key
	Pin       string          // pinned SHA-256 of Syncthing's certificate, "" = trust on first use
	OnPin     func(fp string) // called when Syncthing's certificate is trusted on first use
	Log       *slog.Logger
}

// Gate is the HTTP handler.
type Gate struct {
	o        Options
	base     *url.URL
	loopback bool
	hc       *http.Client
	allowed  map[string]bool

	mu  sync.Mutex
	pin string
}

// New creates a gate. It does not contact Syncthing.
func New(o Options) (*Gate, error) {
	u, err := url.Parse(strings.TrimRight(o.Syncthing, "/"))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, fmt.Errorf("syncthing URL must be http(s)://host:port")
	}
	if o.APIKey == "" {
		return nil, errors.New("the gate needs Syncthing's API key")
	}
	if len(o.Token) < 16 {
		return nil, errors.New("the gate token must be at least 16 characters")
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	ip := net.ParseIP(u.Hostname())
	g := &Gate{o: o, base: u, pin: o.Pin, allowed: map[string]bool{},
		loopback: u.Hostname() == "localhost" || (ip != nil && ip.IsLoopback())}
	for _, p := range stclient.Allowlist() {
		g.allowed[p] = true
	}
	g.hc = &http.Client{
		Transport: &http.Transport{
			Proxy: nil,
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, //nolint:gosec // checked in verify
				VerifyConnection:   g.verify,
				MinVersion:         tls.VersionTLS12,
			},
			TLSHandshakeTimeout: 10 * time.Second,
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     60 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return g, nil
}

// verify checks Syncthing's certificate: anything on loopback (the gate
// runs next to Syncthing), a certificate that chains to a trusted CA, or the
// pinned self-signed one (trusted on first use).
func (g *Gate) verify(cs tls.ConnectionState) error {
	if g.loopback {
		return nil
	}
	if len(cs.PeerCertificates) == 0 {
		return errors.New("Syncthing presented no certificate")
	}
	leaf := cs.PeerCertificates[0]
	inter := x509.NewCertPool()
	for _, c := range cs.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: g.base.Hostname(), Intermediates: inter}); err == nil {
		return nil
	}
	sum := sha256.Sum256(leaf.Raw)
	fp := hex.EncodeToString(sum[:])
	g.mu.Lock()
	defer g.mu.Unlock()
	switch g.pin {
	case "":
		g.pin = fp
		if g.o.OnPin != nil {
			g.o.OnPin(fp)
		}
		return nil
	case fp:
		return nil
	}
	return fmt.Errorf("Syncthing's certificate changed (pinned %s, got %s); if that is expected, delete the gate's syncthing.pin and restart it",
		stclient.ShortFP(g.pin), stclient.ShortFP(fp))
}

func (g *Gate) deny(w http.ResponseWriter, r *http.Request, code int, why string) {
	g.o.Log.Warn("gate refused a request", "why", why, "method", r.Method, "path", r.URL.Path, "ip", clientIP(r))
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
	if r.Method != http.MethodGet {
		g.deny(w, r, http.StatusMethodNotAllowed, "only GET requests pass")
		return
	}
	// Exact paths only: no prefixes, no dot segments, no encoded slashes.
	if !g.allowed[r.URL.Path] || (r.URL.RawPath != "" && r.URL.RawPath != r.URL.Path) {
		g.deny(w, r, http.StatusNotFound, "endpoint not allowed")
		return
	}
	if r.URL.Path != pathHealth {
		tok := r.Header.Get("X-API-Key")
		if tok == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(g.o.Token)) != 1 {
			g.deny(w, r, http.StatusForbidden, "wrong or missing token")
			return
		}
	}

	up := *g.base
	up.Path = strings.TrimRight(g.base.Path, "/") + r.URL.Path
	up.RawPath, up.RawQuery = "", r.URL.RawQuery
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, up.String(), nil)
	if err != nil {
		http.Error(w, "syncwatch gate: bad request", http.StatusBadRequest)
		return
	}
	if r.URL.Path != pathHealth {
		req.Header.Set("X-API-Key", g.o.APIKey)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := g.hc.Do(req)
	if err != nil {
		if r.Context().Err() == nil {
			g.o.Log.Warn("reaching Syncthing failed", "path", r.URL.Path, "err", err)
		}
		http.Error(w, "syncwatch gate: Syncthing can't be reached: "+cleanErr(err), http.StatusBadGateway)
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
		http.Error(w, "syncwatch gate: reading Syncthing's answer failed", http.StatusBadGateway)
		return
	}
	out, err := clean(body)
	if err != nil {
		// An answer that can't be cleaned isn't passed on.
		g.o.Log.Warn("refusing an answer the gate can't check", "path", r.URL.Path, "err", err)
		http.Error(w, "syncwatch gate: unexpected answer from Syncthing", http.StatusBadGateway)
		return
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
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

// cleanErr keeps an upstream error short and free of the request URL.
func cleanErr(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	s := err.Error()
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func clientIP(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}
