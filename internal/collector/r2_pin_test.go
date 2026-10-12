package collector

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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

// W-035: the manager's OnPin hears of each certificate that the collector
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
