package gate

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/stclient"
)

const (
	realKey = "syncthing-real-api-key-0123456789"
	token   = "gate-token-for-syncwatch-0123456789"
)

// fakeSyncthing answers the allowlisted endpoints and records what it got.
type fakeSyncthing struct {
	mu   sync.Mutex
	reqs []*http.Request
}

func (f *fakeSyncthing) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.reqs = append(f.reqs, r.Clone(context.Background()))
	f.mu.Unlock()
	if r.URL.Path != "/rest/noauth/health" && r.Header.Get("X-API-Key") != realKey {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/rest/noauth/health":
		_, _ = w.Write([]byte(`{"status":"OK"}`))
	case "/rest/system/status":
		_, _ = w.Write([]byte(`{"myID":"AAAA","startTime":"2026-10-04T10:00:00Z","pathSeparator":"/"}`))
	case "/rest/events":
		_, _ = w.Write([]byte(`[{"id":1,"type":"StateChanged","time":"2026-10-04T10:00:00Z","data":{"folder":"f","from":"idle","to":"scanning"}},
			{"id":2,"type":"ConfigSaved","time":"2026-10-04T10:00:01Z","data":{"gui":{"apiKey":"` + realKey + `","password":"$2a$hash"}}}]`))
	case "/rest/config/folders":
		_, _ = w.Write([]byte(`[{"id":"f","label":"LIVE-F","path":"/s/LIVE-F","rescanIntervalS":3600,
			"devices":[{"deviceID":"AAAA","encryptionPassword":""},{"deviceID":"BBBB","encryptionPassword":"untrusted-secret"}]}]`))
	case "/rest/system/shutdown", "/rest/config":
		t := "must never be reached"
		_, _ = w.Write([]byte(`{"error":"` + t + `"}`))
	default:
		_, _ = w.Write([]byte(`{}`))
	}
}

func (f *fakeSyncthing) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func (f *fakeSyncthing) last() *http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs[len(f.reqs)-1]
}

func setup(t *testing.T) (*fakeSyncthing, *Gate, *httptest.Server) {
	t.Helper()
	st := &fakeSyncthing{}
	up := httptest.NewTLSServer(st)
	t.Cleanup(up.Close)
	g, err := New(Options{Syncthing: up.URL, APIKey: realKey, Token: token, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	gs := httptest.NewTLSServer(g)
	t.Cleanup(gs.Close)
	return st, g, gs
}

func get(t *testing.T, gs *httptest.Server, method, path, tok string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, gs.URL+path, nil)
	if tok != "" {
		req.Header.Set("X-API-Key", tok)
	}
	resp, err := gs.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestGateOnlyPassesAllowlistedGETs(t *testing.T) {
	st, _, gs := setup(t)
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{http.MethodPost, "/rest/system/status", http.StatusMethodNotAllowed},
		{http.MethodPut, "/rest/config/folders", http.StatusMethodNotAllowed},
		{http.MethodDelete, "/rest/config/folders", http.StatusMethodNotAllowed},
		{http.MethodGet, "/rest/config", http.StatusNotFound},
		{http.MethodGet, "/rest/system/shutdown", http.StatusNotFound},
		{http.MethodGet, "/rest/system/config", http.StatusNotFound},
		{http.MethodGet, "/rest/events/../config", http.StatusNotFound},
		{http.MethodGet, "/rest/config/folders/f", http.StatusNotFound},
		{http.MethodGet, "/rest%2Fconfig", http.StatusNotFound},
		{http.MethodGet, "/", http.StatusNotFound},
	} {
		if code, _ := get(t, gs, c.method, c.path, token); code != c.want {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, code, c.want)
		}
	}
	if n := st.count(); n != 0 {
		t.Fatalf("%d refused requests reached Syncthing", n)
	}
}

func TestGateToken(t *testing.T) {
	st, _, gs := setup(t)
	for _, tok := range []string{"", "wrong", realKey} {
		if code, _ := get(t, gs, http.MethodGet, "/rest/system/status", tok); code != http.StatusForbidden {
			t.Errorf("token %q: %d", tok, code)
		}
	}
	if st.count() != 0 {
		t.Fatal("a request without the gate token reached Syncthing")
	}
	code, body := get(t, gs, http.MethodGet, "/rest/system/status", token)
	if code != 200 || !strings.Contains(body, "AAAA") {
		t.Fatalf("with the token: %d %s", code, body)
	}
	if k := st.last().Header.Get("X-API-Key"); k != realKey {
		t.Fatalf("Syncthing got key %q", k)
	}
	// Health needs no token and gets no key.
	if code, _ := get(t, gs, http.MethodGet, "/rest/noauth/health", ""); code != 200 || st.last().Header.Get("X-API-Key") != "" {
		t.Fatalf("health: %d, key sent %q", code, st.last().Header.Get("X-API-Key"))
	}
	// The gate's own health answer, for the container healthcheck.
	if code, _ := get(t, gs, http.MethodGet, "/healthz", ""); code != 200 {
		t.Fatalf("gate healthz: %d", code)
	}
}

func TestGateRemovesSecrets(t *testing.T) {
	st, _, gs := setup(t)
	code, body := get(t, gs, http.MethodGet, "/rest/events?since=0&timeout=1&events=StateChanged,ConfigSaved", token)
	if code != 200 || strings.Contains(body, realKey) || strings.Contains(body, "hash") {
		t.Fatalf("events: %d %s", code, body)
	}
	var evs []map[string]any
	if err := json.Unmarshal([]byte(body), &evs); err != nil || len(evs) != 2 || evs[0]["data"] == nil || evs[1]["data"] != nil {
		t.Fatalf("events after cleaning: %v %s", err, body)
	}
	if q := st.last().URL.RawQuery; q != "since=0&timeout=1&events=StateChanged,ConfigSaved" && q != "since=0&timeout=1&events=StateChanged%2CConfigSaved" {
		t.Fatalf("query not passed on: %q", q)
	}
	code, body = get(t, gs, http.MethodGet, "/rest/config/folders", token)
	if code != 200 || strings.Contains(body, "untrusted-secret") || strings.Contains(body, "encryptionPassword") {
		t.Fatalf("folders: %d %s", code, body)
	}
	if !strings.Contains(body, `"rescanIntervalS":3600`) || !strings.Contains(body, `"deviceID":"BBBB"`) {
		t.Fatalf("folders lost other fields: %s", body)
	}
}

// syncwatch's own client works through the gate with the gate token.
func TestClientThroughGate(t *testing.T) {
	_, _, gs := setup(t)
	cl, err := stclient.New(stclient.Options{Name: "NAS", URL: gs.URL, APIKey: token, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if st, err := cl.SystemStatus(ctx); err != nil || st.MyID != "AAAA" {
		t.Fatalf("status: %+v %v", st, err)
	}
	fs, err := cl.ConfigFolders(ctx)
	if err != nil || len(fs) != 1 || len(fs[0].Devices) != 2 {
		t.Fatalf("folders: %+v %v", fs, err)
	}
	evs, err := cl.Events(ctx, 0, time.Second, stclient.EventMask)
	if err != nil || len(evs) != 2 {
		t.Fatalf("events: %v %v", evs, err)
	}
	if err := cl.Health(ctx); err != nil {
		t.Fatal(err)
	}
	if cl.LastCert().Fingerprint == "" {
		t.Fatal("the client didn't see the gate's certificate")
	}
}

// Off loopback, Syncthing's certificate is pinned on first use and a
// changed one is refused.
func TestGatePinsSyncthing(t *testing.T) {
	_, g, gs := setup(t)
	var pinned string
	g.loopback = false
	g.o.OnPin = func(fp string) { pinned = fp }
	if code, _ := get(t, gs, http.MethodGet, "/rest/system/status", token); code != 200 || pinned == "" {
		t.Fatalf("first use: %d, pinned %q", code, pinned)
	}
	// httptest servers share one certificate; this one gets its own.
	other := httptest.NewUnstartedServer(&fakeSyncthing{})
	other.TLS = &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}}
	other.StartTLS()
	defer other.Close()
	g.base.Host = strings.TrimPrefix(other.URL, "https://")
	g.hc.CloseIdleConnections()
	code, body := get(t, gs, http.MethodGet, "/rest/system/status", token)
	if code != http.StatusBadGateway || !strings.Contains(body, "certificate changed") {
		t.Fatalf("changed certificate: %d %s", code, body)
	}
}

func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "syncthing"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestNewChecksOptions(t *testing.T) {
	for _, o := range []Options{
		{Syncthing: "ftp://x", APIKey: "k", Token: token},
		{Syncthing: "https://127.0.0.1:8384", Token: token},
		{Syncthing: "https://127.0.0.1:8384", APIKey: "k", Token: "short"},
	} {
		if _, err := New(o); err == nil {
			t.Errorf("accepted %+v", o)
		}
	}
}
