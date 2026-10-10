package stclient

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
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
)

// testCA is a throwaway CA that signs leaf certificates for 127.0.0.1.
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

// leaf makes a server certificate for 127.0.0.1, signed by the CA or by itself.
func (ca *testCA) leaf(t *testing.T, cn string, selfSigned bool) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:    x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	parent, signer := ca.cert, ca.key
	if selfSigned {
		parent, signer = tmpl, key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func fpOf(c tls.Certificate) string {
	cert, _ := x509.ParseCertificate(c.Certificate[0])
	return Fingerprint(cert)
}

// switchingServer is a fake Syncthing whose certificate can be swapped between
// connections. It counts the requests that reach it (none do when the
// handshake fails).
func switchingServer(t *testing.T, first tls.Certificate) (*httptest.Server, func(tls.Certificate), *atomic.Int32) {
	t.Helper()
	var reached atomic.Int32
	var current atomic.Pointer[tls.Certificate]
	current.Store(&first)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached.Add(1)
		_, _ = w.Write([]byte(`{"myID":"X","startTime":"2026-01-01T00:00:00Z","pathSeparator":"/"}`))
	}))
	srv.TLS = &tls.Config{GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
		return &tls.Config{Certificates: []tls.Certificate{*current.Load()}, NextProtos: []string{"http/1.1"}}, nil
	}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv, func(c tls.Certificate) { current.Store(&c) }, &reached
}

type pinLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *pinLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *pinLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// A server first seen with a CA-signed certificate is pinned like any other,
// so a self-signed certificate that turns up later at the same address is
// refused during the handshake: no request, and so no API key, reaches it.
func TestCASignedThenSelfSignedRefused(t *testing.T) {
	ca := newTestCA(t)
	caLeaf, selfLeaf := ca.leaf(t, "syncthing", false), ca.leaf(t, "mitm", true)
	srv, swap, reached := switchingServer(t, caLeaf)
	var pinned []string
	c, err := New(Options{Name: "S", URL: srv.URL, APIKey: "secret", RootCAs: ca.pool, OnPin: func(fp string) { pinned = append(pinned, fp) }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := c.SystemStatus(ctx); err != nil {
		t.Fatal(err)
	}
	if !c.LastCert().CASigned || c.Pin() != fpOf(caLeaf) || len(pinned) != 1 || pinned[0] != fpOf(caLeaf) {
		t.Fatalf("CA-signed certificate not pinned: CASigned %v, pin %q, OnPin %v", c.LastCert().CASigned, c.Pin(), pinned)
	}

	swap(selfLeaf)
	c.Close()
	_, err = c.SystemStatus(ctx)
	ce, ok := IsCertChanged(err)
	if !ok {
		t.Fatalf("self-signed certificate after a CA-signed one: want CertChangedError, got %v", err)
	}
	if ce.Old != fpOf(caLeaf) || ce.New != fpOf(selfLeaf) {
		t.Fatalf("CertChangedError %+v", ce)
	}
	if n := reached.Load(); n != 1 {
		t.Fatalf("%d requests reached the server; the second must fail in the handshake", n)
	}
	if c.Pin() != fpOf(caLeaf) || len(pinned) != 1 {
		t.Fatalf("pin replaced by the refused certificate: %q, OnPin %v", c.Pin(), pinned)
	}
	if lc := c.LastCert(); lc.Fingerprint != fpOf(selfLeaf) || lc.CASigned {
		t.Fatalf("LastCert should show the refused certificate: %+v", lc)
	}
}

// A pinned certificate may change to one that chains to a trusted CA for the
// host (a renewal): the connection goes ahead, the pin follows, a line is
// logged. The renewed pin then refuses a self-signed certificate as before.
func TestCARenewalUpdatesPin(t *testing.T) {
	ca := newTestCA(t)
	leaf1, leaf2, selfLeaf := ca.leaf(t, "syncthing", false), ca.leaf(t, "syncthing", false), ca.leaf(t, "mitm", true)
	srv, swap, reached := switchingServer(t, leaf1)
	var logs pinLog
	var pinned []string
	c, err := New(Options{Name: "S", URL: srv.URL, APIKey: "secret", RootCAs: ca.pool,
		Log: slog.New(slog.NewTextHandler(&logs, nil)), OnPin: func(fp string) { pinned = append(pinned, fp) }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := c.SystemStatus(ctx); err != nil {
		t.Fatal(err)
	}
	swap(leaf2)
	c.Close()
	if _, err := c.SystemStatus(ctx); err != nil {
		t.Fatalf("renewed CA-signed certificate refused: %v", err)
	}
	if c.Pin() != fpOf(leaf2) || len(pinned) != 2 || pinned[1] != fpOf(leaf2) {
		t.Fatalf("pin not updated on renewal: %q, OnPin %v", c.Pin(), pinned)
	}
	if !strings.Contains(logs.String(), "certificate renewed") || !strings.Contains(logs.String(), "old="+ShortFP(fpOf(leaf1))) {
		t.Fatalf("renewal not logged: %s", logs.String())
	}
	if reached.Load() != 2 {
		t.Fatalf("%d requests reached the server, want 2", reached.Load())
	}

	swap(selfLeaf)
	c.Close()
	_, err = c.SystemStatus(ctx)
	if ce, ok := IsCertChanged(err); !ok || ce.Old != fpOf(leaf2) {
		t.Fatalf("self-signed certificate after the renewal: want CertChangedError with the renewed pin, got %v", err)
	}
	if reached.Load() != 2 {
		t.Fatal("a request reached the server with the refused certificate")
	}
}

// Going from a pinned self-signed certificate to a CA-signed one for the host
// is an upgrade and is accepted the same way.
func TestSelfSignedToCASignedAccepted(t *testing.T) {
	ca := newTestCA(t)
	selfLeaf, caLeaf := ca.leaf(t, "syncthing", true), ca.leaf(t, "syncthing", false)
	srv, swap, _ := switchingServer(t, selfLeaf)
	c, err := New(Options{Name: "S", URL: srv.URL, APIKey: "k", RootCAs: ca.pool, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := c.SystemStatus(ctx); err != nil {
		t.Fatal(err)
	}
	if c.Pin() != fpOf(selfLeaf) || c.LastCert().CASigned {
		t.Fatalf("self-signed first use: pin %q CASigned %v", c.Pin(), c.LastCert().CASigned)
	}
	swap(caLeaf)
	c.Close()
	if _, err := c.SystemStatus(ctx); err != nil {
		t.Fatalf("CA-signed certificate after a self-signed pin refused: %v", err)
	}
	if c.Pin() != fpOf(caLeaf) || !c.LastCert().CASigned {
		t.Fatalf("after the upgrade: pin %q CASigned %v", c.Pin(), c.LastCert().CASigned)
	}
}

// A self-signed certificate that matches the pin is accepted without any
// callback or log line.
func TestPinnedSelfSignedUnchanged(t *testing.T) {
	srv, _, _ := fakeSyncthing(t)
	fp := Fingerprint(srv.Certificate())
	for _, pin := range []string{fp} {
		var logs pinLog
		c, err := New(Options{Name: "t", URL: srv.URL, APIKey: "k", Pin: pin, Log: slog.New(slog.NewTextHandler(&logs, nil)),
			OnPin: func(string) { t.Errorf("pin %q: OnPin called for an unchanged certificate", pin) }})
		if err != nil {
			t.Fatalf("pin %q: %v", pin, err)
		}
		if err := c.Health(context.Background()); err != nil {
			t.Fatalf("pin %q: %v", pin, err)
		}
		if c.Pin() != fp {
			t.Fatalf("pin %q: client pin %q, want %q", pin, c.Pin(), fp)
		}
		if logs.String() != "" {
			t.Fatalf("pin %q: unexpected log: %s", pin, logs.String())
		}
	}
}
