package app

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// monitor is a fake outside monitor service that records the calls.
type monitor struct {
	srv *httptest.Server

	mu     sync.Mutex
	calls  []time.Time
	paths  []string
	status int
	delay  time.Duration
}

func newMonitor(t *testing.T) *monitor {
	m := &monitor{status: http.StatusOK}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.calls = append(m.calls, time.Now())
		m.paths = append(m.paths, r.Method+" "+r.URL.RequestURI())
		status, delay := m.status, m.delay
		m.mu.Unlock()
		time.Sleep(delay)
		if status == http.StatusFound {
			http.Redirect(w, r, "/elsewhere", status)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte("OK"))
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *monitor) set(status int, delay time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status, m.delay = status, delay
}

func (m *monitor) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

func (m *monitor) times() []time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]time.Time(nil), m.calls...)
}

func (m *monitor) requests() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.paths...)
}

// waitFor waits until cond is true, at most 5 seconds.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out: %s", what)
}

// readyState is a fake web.Server.Ready.
type readyState struct {
	mu      sync.Mutex
	reasons []string
}

func (r *readyState) set(reasons ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reasons = reasons
}

func (r *readyState) ready(context.Context) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reasons
}

// lockedLog collects the log lines of the goroutines.
type lockedLog struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// The path and the query of a dead-man URL hold the secret of the monitor
// service.
const deadmanSecret = "/ping/9f3c-secret-uuid?key=secret-query"

// testDeadman makes a pusher for the monitor, lets set change it and runs
// it until the test ends. It returns the pusher and its log.
func testDeadman(t *testing.T, m *monitor, ready func(context.Context) []string, set func(*deadman)) (*deadman, *lockedLog) {
	t.Helper()
	logs := &lockedLog{}
	d, err := newDeadman(m.srv.URL+deadmanSecret, ready, "test", slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	set(d)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	t.Cleanup(func() { cancel(); <-done })
	go func() { defer close(done); d.run(ctx) }()
	return d, logs
}

// wakeFor sends evaluations to the pusher for dur.
func wakeFor(d *deadman, dur time.Duration) {
	for end := time.Now().Add(dur); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		d.Wake()
	}
}

// noSecrets fails when a log has the path or the query of the URL.
func noSecrets(t *testing.T, logs string) {
	t.Helper()
	for _, s := range []string{"secret-uuid", "secret-query", "/ping"} {
		if strings.Contains(logs, s) {
			t.Fatalf("the log has %q:\n%s", s, logs)
		}
	}
}

// The pusher calls the dead-man URL after evaluations, at most every 30
// seconds (here: every 300 ms), with GET and the complete URL.
func TestDeadmanCallsAfterEvaluations(t *testing.T) {
	m := newMonitor(t)
	d, logs := testDeadman(t, m, (&readyState{}).ready, func(d *deadman) { d.every = 300 * time.Millisecond })
	time.Sleep(400 * time.Millisecond)
	if n := m.count(); n != 0 {
		t.Fatalf("%d calls without an evaluation", n)
	}
	for range 20 { // a burst of evaluations: 1 call
		d.Wake()
		time.Sleep(5 * time.Millisecond)
	}
	waitFor(t, "the first call", func() bool { return m.count() == 1 })
	d.Wake() // too early: it waits for the interval
	waitFor(t, "the second call", func() bool { return m.count() == 2 })
	time.Sleep(450 * time.Millisecond) // no evaluation: no call
	if n := m.count(); n != 2 {
		t.Fatalf("%d calls, want 2", n)
	}
	if ts := m.times(); ts[1].Sub(ts[0]) < 250*time.Millisecond {
		t.Fatalf("2 calls %v apart, the interval is 300 ms", ts[1].Sub(ts[0]))
	}
	if r := m.requests(); r[0] != "GET "+deadmanSecret {
		t.Fatalf("the call: %q", r[0])
	}
	if strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("warnings for calls that worked:\n%s", logs)
	}
}

// While syncwatch is not healthy (/healthz 503) the pusher does not call
// the URL: the monitor service then gives its alarm. One log line for each
// change.
func TestDeadmanNotWhileUnhealthy(t *testing.T) {
	m := newMonitor(t)
	r := &readyState{}
	r.set("notifications fail from 2026-10-11 21:04")
	d, logs := testDeadman(t, m, r.ready, func(d *deadman) { d.every = 100 * time.Millisecond })
	wakeFor(d, 500*time.Millisecond)
	if n := m.count(); n != 0 {
		t.Fatalf("%d calls while syncwatch is not healthy", n)
	}
	if got := strings.Count(logs.String(), `msg="dead-man URL not called: syncwatch is not healthy" reasons="notifications fail from 2026-10-11 21:04"`); got != 1 {
		t.Fatalf("%d log lines for the unhealthy state:\n%s", got, logs)
	}
	r.set()
	wakeFor(d, 300*time.Millisecond)
	waitFor(t, "a call when syncwatch is healthy again", func() bool { return m.count() > 0 })
	if !strings.Contains(logs.String(), `msg="dead-man URL called again: syncwatch is healthy"`) {
		t.Fatalf("no log line for the healthy state:\n%s", logs)
	}
}

// A failed call gives at most one WARN line each hour (here: each 600 ms),
// with the host and never the path or the query of the URL. After a call
// that works, one INFO line.
func TestDeadmanFailuresLogged(t *testing.T) {
	m := newMonitor(t)
	m.set(http.StatusInternalServerError, 0)
	d, logs := testDeadman(t, m, (&readyState{}).ready, func(d *deadman) {
		d.every, d.warnEvery = 50*time.Millisecond, 600*time.Millisecond
	})
	host := strings.TrimPrefix(m.srv.URL, "http://")
	wakeFor(d, 400*time.Millisecond)
	if got := strings.Count(logs.String(), "level=WARN"); got != 1 || !strings.Contains(logs.String(), `level=WARN msg="cannot call the dead-man URL" host=`+host+` err="HTTP 500" failures=1`) {
		t.Fatalf("after failures for 400 ms: %d WARN lines:\n%s", got, logs)
	}
	if calls := m.count(); calls < 3 {
		t.Fatalf("only %d calls in 400 ms", calls)
	}
	wakeFor(d, 400*time.Millisecond)
	if got := strings.Count(logs.String(), "level=WARN"); got != 2 {
		t.Fatalf("after failures for 800 ms: %d WARN lines, want 2:\n%s", got, logs)
	}
	m.set(http.StatusOK, 0)
	wakeFor(d, 200*time.Millisecond)
	waitFor(t, "an INFO line for the call that works", func() bool {
		return strings.Contains(logs.String(), `level=INFO msg="the dead-man URL answers again" host=`+host)
	})
	noSecrets(t, logs.String())
}

// syncwatch does not follow a redirect (it goes to an address that nobody
// configured) and does not wait more than the timeout. The errors name the
// host only.
func TestDeadmanRedirectAndTimeout(t *testing.T) {
	m := newMonitor(t)
	m.set(http.StatusFound, 0)
	d, logs := testDeadman(t, m, (&readyState{}).ready, func(d *deadman) { d.every = 50 * time.Millisecond })
	d.Wake()
	waitFor(t, "a WARN line for the redirect", func() bool {
		return strings.Contains(logs.String(), `err="HTTP 302: syncwatch does not follow a redirect"`)
	})
	for _, r := range m.requests() {
		if strings.Contains(r, "elsewhere") {
			t.Fatalf("the redirect was followed: %s", r)
		}
	}
	noSecrets(t, logs.String())

	m2 := newMonitor(t)
	m2.set(http.StatusOK, 400*time.Millisecond)
	d2, logs2 := testDeadman(t, m2, (&readyState{}).ready, func(d *deadman) { d.client.Timeout = 100 * time.Millisecond })
	start := time.Now()
	d2.Wake()
	waitFor(t, "a WARN line for the timeout", func() bool { return strings.Contains(logs2.String(), "Client.Timeout exceeded") })
	if took := time.Since(start); took > 350*time.Millisecond {
		t.Fatalf("the call took %v with a timeout of 100 ms", took)
	}
	noSecrets(t, logs2.String())
}

// At the stop the database ping of Ready fails because its context ends:
// that is no unhealthy state, and the pusher logs nothing.
func TestDeadmanQuietAtStop(t *testing.T) {
	m := newMonitor(t)
	logs := &lockedLog{}
	ready := func(ctx context.Context) []string {
		if ctx.Err() != nil {
			return []string{"database not available"}
		}
		return nil
	}
	d, err := newDeadman(m.srv.URL+deadmanSecret, ready, "test", slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d.try(ctx)
	if logs.String() != "" || m.count() != 0 {
		t.Fatalf("a try at the stop: %d calls, log:\n%s", m.count(), logs)
	}
}

// SYNCWATCH_DEADMAN_URL is an http or https URL with a host and without a
// user name or password. The error does not repeat the value.
func TestParseDeadmanURL(t *testing.T) {
	for _, ok := range []string{"https://hc-ping.invalid/0b7c-uuid", "http://kuma.lan.invalid:3001/api/push/abc?status=up&msg=OK", " https://hc-ping.invalid/x "} {
		if _, err := ParseDeadmanURL(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"hc-ping.invalid/secret-1", "ftp://hc-ping.invalid/secret-2", "https:///secret-3", "https://user:secret-4@hc-ping.invalid/x",
		"mailto:secret-5@hc-ping.invalid", "https://hc ping.invalid/secret-6", "//hc-ping.invalid/secret-7", "https://:443/secret-8"} {
		_, err := ParseDeadmanURL(bad)
		if err == nil {
			t.Errorf("%q accepted", bad)
			continue
		}
		if strings.Contains(err.Error(), "secret") {
			t.Errorf("%q: the error repeats the value: %v", bad, err)
		}
	}
}
