package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
// the host only), the health rules of /healthz and the trusted proxies.
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{DataDir: dir, Listen: "127.0.0.1:0", ConfigFile: boot, Log: slog.New(slog.NewTextHandler(logs, nil)),
			TrustedProxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}, NotifyLimit: 15 * time.Minute, DeadmanURL: mon.srv.URL + deadmanSecret,
			OnListen: func(a string) { addr <- a }})
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
