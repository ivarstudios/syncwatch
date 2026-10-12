package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/collector"
	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/store"
)

// A heartbeat 10 minutes before the start gives 1 gap from the heartbeat to
// the start and one INFO line. A heartbeat 30 seconds before (a restart),
// and no heartbeat (the first start), give none.
func TestGapAfterStop(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Stockholm")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 10, 11, 19, 24, 0, 0, time.UTC)
	for _, c := range []struct {
		name  string
		alive time.Time
		gap   bool
	}{
		{"first start", time.Time{}, false},
		{"restart", start.Add(-30 * time.Second), false},
		{"2 minutes", start.Add(-2 * time.Minute), false},
		{"stop of 10 minutes", start.Add(-10 * time.Minute), true},
	} {
		st, err := store.OpenMemory(make([]byte, 32))
		if err != nil {
			t.Fatal(err)
		}
		if !c.alive.IsZero() {
			if err := st.SetAliveAt(c.alive); err != nil {
				t.Fatal(err)
			}
		}
		var logs bytes.Buffer
		if err := noteGap(st, start, loc, slog.New(slog.NewTextHandler(&logs, nil))); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		gaps, err := st.Gaps(time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		if !c.gap {
			if len(gaps) != 0 || logs.Len() != 0 {
				t.Errorf("%s: gaps %+v, log %q", c.name, gaps, logs.String())
			}
			continue
		}
		if len(gaps) != 1 || !gaps[0].From.Equal(c.alive) || !gaps[0].To.Equal(start) || gaps[0].Reported {
			t.Errorf("%s: gaps %+v", c.name, gaps)
		}
		want := `level=INFO msg="syncwatch did not run" from=2026-10-11T21:14:00.000+02:00 to=2026-10-11T21:24:00.000+02:00 duration=10m0s`
		if !strings.Contains(logs.String(), want) {
			t.Errorf("%s: log %q, want %q", c.name, logs.String(), want)
		}
		st.Close()
	}
}

// aliveAndGaps returns the heartbeat and the gaps in the store of dir.
func aliveAndGaps(t *testing.T, dir string) (time.Time, []store.Gap) {
	t.Helper()
	st, err := OpenStore(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	alive, err := st.AliveAt()
	if err != nil {
		t.Fatal(err)
	}
	gaps, err := st.Gaps(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return alive, gaps
}

// runUntilListening runs Run until it listens. stop ends the run and waits
// for Run.
func runUntilListening(t *testing.T, o Options) (addr string, stop func()) {
	t.Helper()
	ready := make(chan string, 1)
	o.Listen, o.OnListen = "127.0.0.1:0", func(a string) { ready <- a }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, o) }()
	select {
	case addr = <-ready:
	case err := <-done:
		cancel()
		t.Fatalf("Run returned: %v", err)
	case <-time.After(30 * time.Second):
		cancel()
		t.Fatal("no listener")
	}
	return addr, func() {
		t.Helper()
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("Run did not return")
		}
	}
}

// A start that fails before it listens (a TLS file that is missing, an
// address in use, a SYNCWATCH_ADMIN_PASSWORD that is too short) did not run:
// it records no gap and does not move the heartbeat. Docker starts such a
// container again and again, at most 1 minute apart, thus a failed start that
// moved the heartbeat cut the gap. The start that works records the gap from
// the last heartbeat of the syncwatch that ran.
func TestFailedStartKeepsTheHeartbeat(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	lastBeat := time.Now().Add(-3 * time.Hour).Truncate(time.Millisecond)
	if err := st.SetAliveAt(lastBeat); err != nil {
		t.Fatal(err)
	}
	st.Close()
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	missing := filepath.Join(dir, "certs", "nas.pem")
	for _, c := range []struct {
		name string
		o    Options
	}{
		{"TLS file missing", Options{Listen: "127.0.0.1:0", TLSCert: missing, TLSKey: missing}},
		{"address in use", Options{Listen: busy.Addr().String()}},
		{"SYNCWATCH_ADMIN_PASSWORD too short", Options{Listen: "127.0.0.1:0", AdminPassword: "short"}},
	} {
		c.o.DataDir, c.o.Log = dir, slog.New(slog.NewTextHandler(io.Discard, nil))
		if err := Run(context.Background(), c.o); err == nil {
			t.Fatalf("%s: the start did not fail", c.name)
		}
		if alive, gaps := aliveAndGaps(t, dir); !alive.Equal(lastBeat) || len(gaps) != 0 {
			t.Errorf("%s: heartbeat %v, want %v; gaps %+v", c.name, alive, lastBeat, gaps)
		}
	}

	started := time.Now()
	_, stop := runUntilListening(t, Options{DataDir: dir, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	stop()
	if _, gaps := aliveAndGaps(t, dir); len(gaps) != 1 || !gaps[0].From.Equal(lastBeat) || gaps[0].To.Before(started.Truncate(time.Millisecond)) {
		t.Fatalf("gaps %+v, want 1 from %v to the start that works (%v)", gaps, lastBeat, started)
	}
}

// fakeDiscord records the titles of the messages that reach it.
type fakeDiscord struct {
	srv *httptest.Server

	mu     sync.Mutex
	titles []string
}

func newFakeDiscord(t *testing.T) *fakeDiscord {
	f := &fakeDiscord{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Content string `json:"content"`
			Embeds  []struct {
				Title       string `json:"title"`
				Description string `json:"description"`
			} `json:"embeds"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &p)
		f.mu.Lock()
		for _, e := range p.Embeds {
			f.titles = append(f.titles, e.Title+"\n"+e.Description)
		}
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDiscord) has(s string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, x := range f.titles {
		if strings.Contains(x, s) {
			return true
		}
	}
	return false
}

// Run wires the parts of wave 3: the gap and the heartbeat (also at a clean
// stop), the dead-man URL (called after the first evaluation, the log names
// the host only), the health rules of /healthz, the trusted proxies, and the
// settings notice of a certificate that the collector pins (OnPin).
func TestRunWiring(t *testing.T) {
	config.AllowAnyWebhook = true
	t.Cleanup(func() { config.AllowAnyWebhook = false })
	discord := newFakeDiscord(t)
	mon := newMonitor(t)
	dir := t.TempDir()
	boot := filepath.Join(dir, "syncwatch.yaml")
	yaml := "access: {viewer: open}\nservers:\n  - name: SOL\n    url: https://127.0.0.1:1\nnotifications:\n  discord_webhook: " + discord.srv.URL + "/api/webhooks/1/x\n"
	if err := os.WriteFile(boot, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	// syncwatch stopped 10 minutes ago.
	st, err := OpenStore(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	stoppedAt := time.Now().Add(-10 * time.Minute)
	if err := st.SetAliveAt(stoppedAt); err != nil {
		t.Fatal(err)
	}
	st.Close()

	logs := &lockedLog{}
	addr := make(chan string, 1)
	mgr := make(chan *collector.Manager, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{DataDir: dir, Listen: "127.0.0.1:0", ConfigFile: boot, Log: slog.New(slog.NewTextHandler(logs, nil)),
			TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}, NotifyLimit: 15 * time.Minute, DeadmanURL: mon.srv.URL + deadmanSecret,
			OnListen: func(a string) { addr <- a }, onManager: func(m *collector.Manager) { mgr <- m }})
	}()
	var a string
	select {
	case a = <-addr:
	case err := <-done:
		t.Fatalf("Run returned: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("no listener")
	}
	resp, err := http.Get("http://" + a + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz: %d", resp.StatusCode)
	}
	waitFor(t, "a call of the dead-man URL after the first evaluation", func() bool { return mon.count() > 0 })
	for _, want := range []string{
		`level=INFO msg="syncwatch did not run" from=`,
		`msg="syncwatch started"`,
		`trusted_proxies=10.0.0.0/8 health_notify_limit=15m deadman=` + mon.srv.URL + "\n",
	} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("no %q in the log:\n%s", want, logs)
		}
	}
	noSecrets(t, logs.String())

	// The collector pins a certificate at the first contact: a settings
	// notice goes to Discord.
	(<-mgr).OnPin("sol", strings.Repeat("ab", 32), false)
	waitFor(t, "the settings notice of the pinned certificate", func() bool {
		return discord.has("Server SOL: certificate pinned\nsyncwatch pinned the certificate AB:AB:AB:AB:AB:AB:AB:AB of https://127.0.0.1:1 at the first contact.")
	})

	time.Sleep(50 * time.Millisecond)
	stopAt := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return")
	}
	st, err = OpenStore(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	alive, err := st.AliveAt()
	if err != nil || alive.Before(stopAt.Truncate(time.Millisecond)) {
		t.Fatalf("the heartbeat of the stop: %v %v, stopped at %v", alive, err, stopAt)
	}
	gaps, err := st.Gaps(time.Time{})
	if err != nil || len(gaps) != 1 || gaps[0].From.UnixMilli() != stoppedAt.UnixMilli() {
		t.Fatalf("gaps %+v %v", gaps, err)
	}
}
