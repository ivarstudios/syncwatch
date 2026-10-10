package gate

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/stclient"
)

// testCA signs leaf certificates for 127.0.0.1.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pool *x509.CertPool
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &testCA{cert: cert, key: key, pool: pool}
}

func (ca *testCA) leaf(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "syncthing"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:    x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func fpOf(c tls.Certificate) string {
	cert, _ := x509.ParseCertificate(c.Certificate[0])
	return stclient.Fingerprint(cert)
}

type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// Off loopback, a CA-signed Syncthing certificate is pinned too. A
// self-signed one that turns up later is refused before any request is
// sent; a renewed CA-signed one is accepted and takes over the pin.
func TestGatePinsCASignedSyncthing(t *testing.T) {
	ca := newTestCA(t)
	leaf1, leaf2, self := ca.leaf(t), ca.leaf(t), selfSigned(t)
	var current atomic.Pointer[tls.Certificate]
	current.Store(&leaf1)
	st := &fakeSyncthing{}
	up := httptest.NewUnstartedServer(st)
	up.TLS = &tls.Config{GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
		return &tls.Config{Certificates: []tls.Certificate{*current.Load()}, NextProtos: []string{"http/1.1"}}, nil
	}}
	up.StartTLS()
	defer up.Close()

	var logs lockedLog
	var pinned []string
	g, err := New(Options{Syncthing: up.URL, APIKey: realKey, Token: token, RootCAs: ca.pool,
		Log: slog.New(slog.NewTextHandler(&logs, nil)), OnPin: func(fp string) { pinned = append(pinned, fp) }})
	if err != nil {
		t.Fatal(err)
	}
	g.loopback = false
	gs := httptest.NewTLSServer(g)
	defer gs.Close()

	if code, _ := get(t, gs, http.MethodGet, "/rest/system/status", token); code != 200 {
		t.Fatalf("CA-signed first use: %d", code)
	}
	if len(pinned) != 1 || pinned[0] != fpOf(leaf1) || !strings.Contains(logs.String(), "pinned Syncthing's CA-signed certificate") {
		t.Fatalf("CA-signed certificate not pinned: OnPin %v, log %s", pinned, logs.String())
	}

	current.Store(&self)
	g.hc.CloseIdleConnections()
	code, body := get(t, gs, http.MethodGet, "/rest/system/status", token)
	if code != http.StatusBadGateway || !strings.Contains(body, "certificate changed") {
		t.Fatalf("self-signed certificate after a CA-signed one: %d %s", code, body)
	}
	if st.count() != 1 {
		t.Fatalf("%d requests reached Syncthing; the second must fail in the handshake", st.count())
	}
	if len(pinned) != 1 {
		t.Fatalf("the refused certificate was pinned: %v", pinned)
	}

	current.Store(&leaf2)
	g.hc.CloseIdleConnections()
	if code, body := get(t, gs, http.MethodGet, "/rest/system/status", token); code != 200 {
		t.Fatalf("renewed CA-signed certificate: %d %s", code, body)
	}
	if len(pinned) != 2 || pinned[1] != fpOf(leaf2) || !strings.Contains(logs.String(), "Syncthing's certificate renewed") {
		t.Fatalf("renewal not pinned: OnPin %v, log %s", pinned, logs.String())
	}

	// The renewed pin refuses a self-signed certificate like the first one did.
	current.Store(&self)
	g.hc.CloseIdleConnections()
	if code, _ := get(t, gs, http.MethodGet, "/rest/system/status", token); code != http.StatusBadGateway {
		t.Fatalf("self-signed certificate after the renewal: %d", code)
	}
	if st.count() != 2 {
		t.Fatalf("%d requests reached Syncthing, want 2", st.count())
	}
}

// On loopback nothing is checked or pinned, as before.
func TestGateTrustsLoopback(t *testing.T) {
	st, g, gs := setup(t)
	g.o.OnPin = func(fp string) { t.Errorf("pinned %s on loopback", fp) }
	if code, _ := get(t, gs, http.MethodGet, "/rest/system/status", token); code != 200 || st.count() != 1 {
		t.Fatalf("loopback: %d, %d requests", code, st.count())
	}
}
