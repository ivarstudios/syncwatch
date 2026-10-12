package collector

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/stclient"
	"github.com/ivarstudios/syncwatch/internal/store"
)

// pinCalls records the calls of Manager.OnPin.
func pinCalls(m *Manager) chan string {
	ch := make(chan string, 4)
	m.OnPin = func(id, fp string, renewal bool) { ch <- fmt.Sprintf("%s %s renewal=%v", id, fp, renewal) }
	return ch
}

func wantPin(t *testing.T, ch chan string, want string) {
	t.Helper()
	select {
	case got := <-ch:
		if got != want {
			t.Fatalf("OnPin %q, want %q", got, want)
		}
	case <-time.After(slow(10 * time.Second)):
		t.Fatalf("no OnPin call, want %q", want)
	}
}

// The manager's OnPin hears of each certificate that the collector
// pins without an administrator: at the first contact (renewal false) and
// for a CA-signed certificate that replaces the pinned one (renewal true).
// A pinned certificate that does not change gives no call.
func TestOnPinFirstUseAndRenewal(t *testing.T) {
	fake := newFakeST(t)
	srv := httptest.NewTLSServer(fake)
	defer srv.Close()
	fp := stclient.Fingerprint(srv.Certificate())
	run := func(pin string) (*model.Hub, *store.Store, chan string, func()) {
		cfg, _ := config.ParseYAML([]byte(fmt.Sprintf("servers: [{name: AKKA, url: %q}]\ncollector: {event_timeout: 5s}\n", srv.URL)))
		h, _ := config.NewHolder(cfg)
		st, _ := store.OpenMemory(make([]byte, 32))
		_ = st.SetServerKey("akka", "k", srv.URL)
		if pin != "" {
			_ = st.SetPin("akka", pin)
		}
		hub := model.NewHub(nil)
		m := NewManager(hub, st, h, slog.New(slog.NewTextHandler(io.Discard, nil)))
		ch := pinCalls(m)
		return hub, st, ch, runManager(m)
	}

	// The first contact.
	hub, st, ch, stop := run("")
	wantPin(t, ch, "akka "+fp+" renewal=false")
	waitFor(t, hub, "full check", func(s *model.Server) bool { return s.Loaded })
	if pin, _ := st.Pin("akka"); pin != fp {
		t.Fatalf("stored pin %q", pin)
	}
	stop()

	// The pinned certificate again: no call.
	hub, _, ch, stop = run(fp)
	waitFor(t, hub, "full check", func(s *model.Server) bool { return s.Loaded })
	select {
	case got := <-ch:
		t.Fatalf("OnPin %q for the pinned certificate", got)
	case <-time.After(300 * time.Millisecond):
	}
	stop()

	// A certificate from a certificate authority replaces an older pin.
	old := rootCAs
	rootCAs = srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	defer func() { rootCAs = old }()
	hub, st, ch, stop = run(strings.Repeat("de", 32))
	defer stop()
	wantPin(t, ch, "akka "+fp+" renewal=true")
	sv := waitFor(t, hub, "full check", func(s *model.Server) bool { return s.Loaded })
	if pin, _ := st.Pin("akka"); pin != fp || !sv.Cert.CASigned {
		t.Fatalf("stored pin %q, CA-signed %v", pin, sv.Cert.CASigned)
	}
}

// 2 front ends with their own CA-signed certificate for the same host name
// (DNS round robin, a load balancer) give the certificates A, B, A, B ... at
// each new connection, and the collector pins each one. Only the first pin
// of each certificate gives a call: A (renewal false), then B (renewal
// true), not one for each connection.
func TestOnPinAlternatingCASigned(t *testing.T) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(der)
	var leaves [2]tls.Certificate
	var fps [2]string
	for i := range leaves {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(int64(i + 2)), Subject: pkix.Name{CommonName: fmt.Sprintf("front end %c", 'A'+i)},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
			IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
			KeyUsage:    x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		leaves[i] = tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
		leaf, _ := x509.ParseCertificate(der)
		fps[i] = stclient.Fingerprint(leaf)
	}
	old := rootCAs
	rootCAs = x509.NewCertPool()
	rootCAs.AddCert(ca)
	defer func() { rootCAs = old }()

	// A new connection for each request, with the other certificate.
	var handshakes atomic.Int32
	srv := httptest.NewUnstartedServer(newFakeST(t))
	srv.Config.SetKeepAlivesEnabled(false)
	srv.TLS = &tls.Config{GetConfigForClient: func(*tls.ClientHelloInfo) (*tls.Config, error) {
		n := handshakes.Add(1)
		return &tls.Config{Certificates: []tls.Certificate{leaves[(n-1)%2]}, NextProtos: []string{"http/1.1"}, SessionTicketsDisabled: true}, nil
	}}
	srv.StartTLS()
	defer srv.Close()

	cfg, _ := config.ParseYAML([]byte(fmt.Sprintf("servers: [{name: AKKA, url: %q}]\ncollector: {event_timeout: 5s}\n", srv.URL)))
	h, _ := config.NewHolder(cfg)
	st, _ := store.OpenMemory(make([]byte, 32))
	_ = st.SetServerKey("akka", "k", srv.URL)
	hub := model.NewHub(nil)
	m := NewManager(hub, st, h, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ch := make(chan string, 100)
	m.OnPin = func(id, fp string, renewal bool) { ch <- fmt.Sprintf("%s %s renewal=%v", id, fp, renewal) }
	stop := runManager(m)
	defer stop()
	waitFor(t, hub, "full check", func(s *model.Server) bool { return s.Loaded })
	if n := handshakes.Load(); n < 4 {
		t.Fatalf("%d handshakes in the full check, want A, B, A, B at least", n)
	}
	time.Sleep(300 * time.Millisecond) // the calls run in their own goroutines
	stop()
	close(ch)
	var calls []string
	for c := range ch {
		calls = append(calls, c)
	}
	want := []string{"akka " + fps[0] + " renewal=false", "akka " + fps[1] + " renewal=true"}
	if fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Fatalf("%d handshakes gave %d OnPin calls %q, want %q", handshakes.Load(), len(calls), calls, want)
	}
}

// pinNews: a certificate is news when the collector did not pin it in the
// last pinQuiet.
func TestPinNews(t *testing.T) {
	pinned := map[string]time.Time{}
	t0 := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	for i, c := range []struct {
		fp   string
		at   time.Duration
		want bool
	}{
		{"a", 0, true},
		{"b", time.Minute, true},
		{"a", 2 * time.Minute, false},
		{"b", 3 * time.Minute, false},
		{"a", 2*time.Minute + pinQuiet - time.Second, false}, // the last pin counts
		{"b", 3*time.Minute + pinQuiet, true},
		{"c", 3*time.Minute + pinQuiet, true},
	} {
		if got := pinNews(pinned, c.fp, t0.Add(c.at)); got != c.want {
			t.Fatalf("%d: pinNews(%s, +%v) = %v, want %v", i, c.fp, c.at, got, c.want)
		}
	}
	if len(pinned) != 3 {
		t.Fatalf("pinned %v", pinned)
	}
}
