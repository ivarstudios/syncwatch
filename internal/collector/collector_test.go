package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/rules"
	"github.com/ivarstudios/syncwatch/internal/stclient"
	"github.com/ivarstudios/syncwatch/internal/store"
)

// fakeST serves fixtures recorded with "syncwatch probe --record" and lets the
// test push events.
type fakeST struct {
	t        *testing.T
	dir      string
	mu       sync.Mutex
	events   []stclient.Event
	nextID   int64
	notify   chan struct{}
	requests map[string]int
	paused   map[string]bool

	dropEvents int    // close this many event long-polls without an answer
	dropped    int    // event long-polls closed so far
	startTime  string // overrides the fixture's startTime (a "restart")
	inBytes    int64  // traffic totals since "start"
	outBytes   int64
}

func newFakeST(t *testing.T) *fakeST {
	return &fakeST{t: t, dir: filepath.Join("..", "..", "test", "fixtures", "sim-akka"), notify: make(chan struct{}, 1), requests: map[string]int{}, paused: map[string]bool{}}
}

func (f *fakeST) fixture(name string) []byte {
	b, err := os.ReadFile(filepath.Join(f.dir, name+".json"))
	if err != nil {
		f.t.Fatal(err)
	}
	return b
}

func (f *fakeST) push(typ string, data any) {
	b, _ := json.Marshal(data)
	f.mu.Lock()
	f.nextID++
	f.events = append(f.events, stclient.Event{ID: f.nextID, Type: typ, Time: time.Now(), Data: b})
	f.mu.Unlock()
	select {
	case f.notify <- struct{}{}:
	default:
	}
}

func (f *fakeST) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		f.t.Errorf("non-GET request: %s %s", r.Method, r.URL.Path)
	}
	f.mu.Lock()
	f.requests[r.URL.Path]++
	f.mu.Unlock()
	q := r.URL.Query()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/rest/system/status":
		var st map[string]any
		_ = json.Unmarshal(f.fixture("system-status"), &st)
		f.mu.Lock()
		if f.startTime != "" {
			st["startTime"] = f.startTime
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(st)
	case "/rest/system/version":
		_, _ = w.Write(f.fixture("system-version"))
	case "/rest/config/folders":
		var fs []map[string]any
		_ = json.Unmarshal(f.fixture("config-folders"), &fs)
		f.mu.Lock()
		for _, x := range fs {
			if f.paused[x["id"].(string)] {
				x["paused"] = true
			}
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(fs)
	case "/rest/config/devices":
		_, _ = w.Write(f.fixture("config-devices"))
	case "/rest/system/connections":
		f.mu.Lock()
		in, out := f.inBytes, f.outBytes
		f.mu.Unlock()
		fmt.Fprintf(w, `{"connections": %s, "total": {"inBytesTotal": %d, "outBytesTotal": %d}}`, f.fixture("connections"), in, out)
	case "/rest/stats/device":
		_, _ = w.Write(f.fixture("stats-device"))
	case "/rest/stats/folder":
		_, _ = w.Write(f.fixture("stats-folder"))
	case "/rest/cluster/pending/devices", "/rest/cluster/pending/folders":
		_, _ = w.Write([]byte(`{}`))
	case "/rest/db/status":
		var all map[string]json.RawMessage
		_ = json.Unmarshal(f.fixture("db-status"), &all)
		_, _ = w.Write(all[q.Get("folder")])
	case "/rest/db/completion":
		f.mu.Lock()
		p := f.paused[q.Get("folder")]
		f.mu.Unlock()
		if p {
			http.Error(w, "folder is paused", http.StatusNotFound)
			return
		}
		var all []struct {
			Folder, Device string
			stclient.Completion
		}
		_ = json.Unmarshal(f.fixture("completion"), &all)
		for _, c := range all {
			if c.Folder == q.Get("folder") && c.Device == q.Get("device") {
				_ = json.NewEncoder(w).Encode(c.Completion)
				return
			}
		}
		_, _ = w.Write([]byte(`{"completion":100,"remoteState":"unknown"}`))
	case "/rest/folder/errors":
		_, _ = w.Write([]byte(`{"errors":[{"path":"bad.txt","error":"permission denied"}]}`))
	case "/metrics":
		var c map[string]int
		_ = json.Unmarshal(f.fixture("conflicts"), &c)
		for k, v := range c {
			fmt.Fprintf(w, "syncthing_model_folder_conflicts_total{folder=%q} %d\n", k, v)
		}
	case "/rest/system/browse":
		var all map[string][]string
		_ = json.Unmarshal(f.fixture("browse"), &all)
		out := all[strings.TrimRight(q.Get("current"), `\`)]
		if out == nil {
			out = []string{}
		}
		_ = json.NewEncoder(w).Encode(out)
	case "/rest/events":
		f.mu.Lock()
		drop := f.dropEvents > 0
		if drop {
			f.dropEvents--
			f.dropped++
		}
		f.mu.Unlock()
		if drop {
			// Like a VPN or firewall cutting a long-poll: the answer starts
			// but never completes, so the client can't simply retry it.
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("["))
			return
		}
		var since int64
		fmt.Sscan(q.Get("since"), &since)
		deadline := time.After(2 * time.Second)
		for {
			f.mu.Lock()
			var out []stclient.Event
			for _, e := range f.events {
				if e.ID > since {
					out = append(out, e)
				}
			}
			f.mu.Unlock()
			if len(out) > 0 {
				_ = json.NewEncoder(w).Encode(out)
				return
			}
			select {
			case <-f.notify:
			case <-deadline:
				_, _ = w.Write([]byte(`[]`))
				return
			case <-r.Context().Done():
				return
			}
		}
	default:
		f.t.Errorf("unexpected request %s", r.URL.Path)
		http.NotFound(w, r)
	}
}

const rulesYAML = `
structure:
  project_pattern: '^(ME-)?LIVE-[A-Za-z0-9]'
  share_type_from_path: '-(?P<me>ME-)?LIVE-(?P<type>[1-5][A-Z]+)$'
  type_tokens: [1SOURCE, 2PROJECTFILES]
`

func setup(t *testing.T) (*fakeST, *model.Hub, *config.Holder, *Manager, func()) {
	t.Helper()
	fake := newFakeST(t)
	srv := httptest.NewTLSServer(fake)
	cfg, err := config.ParseYAML([]byte(rulesYAML + fmt.Sprintf("servers: [{name: AKKA, url: %q}]\ncollector: {event_timeout: 5s}\n", srv.URL)))
	if err != nil {
		t.Fatal(err)
	}
	h, _ := config.NewHolder(cfg)
	st, _ := store.OpenMemory(make([]byte, 32))
	_ = st.SetServerKey("akka", "k", srv.URL)
	hub := model.NewHub(nil)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := NewManager(hub, st, h, log)
	ctx, cancel := context.WithCancel(context.Background())
	go m.Run(ctx)
	return fake, hub, h, m, func() { cancel(); srv.Close() }
}

// slow scales a test timeout by SYNCWATCH_TEST_SLOW (default 1), for runs
// under emulation or the race detector that are many times slower.
func slow(d time.Duration) time.Duration {
	if n, err := strconv.Atoi(os.Getenv("SYNCWATCH_TEST_SLOW")); err == nil && n > 1 {
		return d * time.Duration(n)
	}
	return d
}

func waitFor(t *testing.T, hub *model.Hub, what string, pred func(*model.Server) bool) *model.Server {
	t.Helper()
	deadline := time.Now().Add(slow(10 * time.Second))
	for time.Now().Before(deadline) {
		var got *model.Server
		hub.View("akka", func(s *model.Server) {
			if pred(s) {
				got = s.Clone()
			}
		})
		if got != nil {
			return got
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
	return nil
}

func TestCollectorFromFixtures(t *testing.T) {
	fake, hub, h, mgr, stop := setup(t)
	defer stop()

	sv := waitFor(t, hub, "full check", func(s *model.Server) bool { return s.Loaded && s.Status == model.StatusUp })
	if len(sv.Folders) != 3 || len(sv.Devices) != 5 || sv.PathSep != `\` || sv.Version != "v2.1.5" {
		t.Fatalf("state: %d folders, %d devices, sep %q", len(sv.Folders), len(sv.Devices), sv.PathSep)
	}
	if c := sv.Folders["p-alpha"].Completion; len(c) == 0 {
		t.Fatal("no completion data")
	}
	sv = waitFor(t, hub, "structure scan", func(s *model.Server) bool { return s.Structure != nil && len(s.Structure.Shares) == 2 })
	// The scan records what it cost the server: at least one listing per share.
	if n := sv.Structure.ScanListings; n < 2 || n != fake.count("/rest/system/browse") {
		t.Fatalf("scan listings %d, browse requests %d", n, fake.count("/rest/system/browse"))
	}

	// Events: a folder error, failing files, and a nested project folder.
	fake.push("StateChanged", map[string]any{"folder": "p-beta", "from": "idle", "to": "error", "error": "folder path missing"})
	fake.push("FolderErrors", map[string]any{"folder": "p-gamma", "errors": []map[string]string{{"path": "a.blend", "error": "locked"}}})
	fake.push("LocalChangeDetected", map[string]any{"folder": "p-alpha", "folderID": "p-alpha", "type": "dir", "action": "modified", "path": `renders\LIVE-NESTED-2PROJECTFILES`})
	fake.push("LocalChangeDetected", map[string]any{"folder": "p-alpha", "folderID": "p-alpha", "type": "dir", "action": "modified", "path": `renders\plain`})
	sv = waitFor(t, hub, "events applied", func(s *model.Server) bool {
		return s.Folders["p-beta"].State == "error" && len(s.Folders["p-gamma"].FileErrors) == 1 && s.Structure != nil && len(s.Structure.Nested) == 1
	})
	for _, n := range sv.Structure.Nested {
		if !strings.HasSuffix(n.Path, `LIVE-ALPHA-2PROJECTFILES\renders\LIVE-NESTED-2PROJECTFILES`) || n.Source != "event" {
			t.Fatalf("nested: %+v", n)
		}
	}
	fs := rules.Evaluate(rules.Input{Snapshot: hub.Snapshot(), Config: h.Get(), Structure: h.Structure(), Now: time.Now()})
	var checks []string
	for _, f := range fs {
		checks = append(checks, strings.Join(f.Checks, "+")+":"+f.Subject)
	}
	all := strings.Join(checks, "\n")
	for _, want := range []string{"H4:LIVE-BETA-1SOURCE on AKKA", "H4:LIVE-GAMMA-1SOURCE on AKKA", "S3:AKKA-LIVE-2PROJECTFILES/LIVE-ALPHA-2PROJECTFILES/renders/LIVE-NESTED-2PROJECTFILES on AKKA", "S4:AKKA-LIVE-1SOURCE/random stuff on AKKA"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in\n%s", want, all)
		}
	}

	// A deleted directory removes the nested finding.
	fake.push("LocalChangeDetected", map[string]any{"folder": "p-alpha", "type": "dir", "action": "deleted", "path": `renders`})
	waitFor(t, hub, "nested removed", func(s *model.Server) bool { return len(s.Structure.Nested) == 0 })

	// ConfigSaved re-reads only the config: no db/status for known folders.
	fake.mu.Lock()
	fake.paused["p-gamma"] = true
	dbBefore := fake.requests["/rest/db/status"]
	fake.mu.Unlock()
	fake.push("ConfigSaved", map[string]any{"version": 1})
	sv = waitFor(t, hub, "config refresh after ConfigSaved", func(s *model.Server) bool { return s.Folders["p-gamma"].Paused })
	if sv.Status != model.StatusUp || sv.Folders["p-beta"].State != "error" {
		t.Fatalf("state lost on config refresh: %s, p-beta %s", sv.Status, sv.Folders["p-beta"].State)
	}
	fake.mu.Lock()
	dbAfter := fake.requests["/rest/db/status"]
	fake.mu.Unlock()
	if dbAfter != dbBefore {
		t.Fatalf("ConfigSaved caused %d db/status calls", dbAfter-dbBefore)
	}
	// A full re-check with a paused folder (404 on completion) must not fail.
	var lastFull time.Time
	hub.View("akka", func(s *model.Server) { lastFull = s.LastFullCheck })
	mgr.RescanNow()
	sv = waitFor(t, hub, "full re-check", func(s *model.Server) bool { return s.LastFullCheck.After(lastFull) })
	if sv.Status != model.StatusUp || sv.LastError != "" {
		t.Fatalf("full re-check with a paused folder failed: %s %s", sv.Status, sv.LastError)
	}
}

func TestCollectorMarksServerDown(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	cfg, _ := config.ParseYAML([]byte(fmt.Sprintf("servers: [{name: AKKA, url: %q}]\ncollector: {down_after_failures: 1}\n", srv.URL)))
	h, _ := config.NewHolder(cfg)
	st, _ := store.OpenMemory(make([]byte, 32))
	_ = st.SetServerKey("akka", "k", srv.URL)
	hub := model.NewHub(nil)
	m := NewManager(hub, st, h, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	sv := waitFor(t, hub, "down", func(s *model.Server) bool { return s.Status == model.StatusDown })
	if !strings.Contains(sv.LastError, "500") || sv.DownSince.IsZero() {
		t.Fatalf("down state: %+v", sv)
	}
}

// A server that freezes during the full check must be reported as down after
// about one request's timeout and retries, not after every queued request
// has timed out in turn.
func TestFullCheckStopsAtFirstFailure(t *testing.T) {
	fake := newFakeST(t)
	var mu sync.Mutex
	hung := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rest/db/completion" {
			mu.Lock()
			hung++
			mu.Unlock()
			<-r.Context().Done()
			return
		}
		fake.ServeHTTP(w, r)
	}))
	defer srv.Close()
	cfg, _ := config.ParseYAML([]byte(fmt.Sprintf("servers: [{name: AKKA, url: %q}]\ncollector: {event_timeout: 5s, request_timeout: 1s, concurrency: 3, down_after_failures: 1}\n", srv.URL)))
	h, _ := config.NewHolder(cfg)
	st, _ := store.OpenMemory(make([]byte, 32))
	_ = st.SetServerKey("akka", "k", srv.URL)
	hub := model.NewHub(nil)
	m := NewManager(hub, st, h, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	go m.Run(ctx)
	// One request: 3 attempts of 1 s plus 1 s and 2 s back-off. The fixture
	// has 7 folder × device pairs, which used to take about 20 s.
	waitFor(t, hub, "down", func(s *model.Server) bool { return s.Status == model.StatusDown })
	took := time.Since(start)
	mu.Lock()
	n := hung
	mu.Unlock()
	if took > slow(10*time.Second) {
		t.Errorf("marked down after %v; want about 6 s", took.Round(time.Second))
	}
	if n > 9 {
		t.Errorf("%d db/completion requests before giving up; want at most concurrency × attempts = 9", n)
	}
}

// Traffic counters are read regularly: the bytes moved are stored for the
// day and week sums, and the current rate is kept on the server.
func TestTrafficSampling(t *testing.T) {
	old := trafficEvery
	trafficEvery = 200 * time.Millisecond
	defer func() { trafficEvery = old }()
	fake := newFakeST(t)
	fake.inBytes, fake.outBytes = 5_000_000, 1_000_000 // before syncwatch started watching
	srv := httptest.NewTLSServer(fake)
	defer srv.Close()
	cfg, _ := config.ParseYAML([]byte(fmt.Sprintf("servers: [{name: AKKA, url: %q}]\ncollector: {event_timeout: 5s}\n", srv.URL)))
	h, _ := config.NewHolder(cfg)
	st, _ := store.OpenMemory(make([]byte, 32))
	_ = st.SetServerKey("akka", "k", srv.URL)
	hub := model.NewHub(nil)
	m := NewManager(hub, st, h, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	waitFor(t, hub, "loaded", func(s *model.Server) bool { return s.Loaded })
	time.Sleep(500 * time.Millisecond) // a baseline reading
	fake.mu.Lock()
	fake.inBytes += 30_000_000
	fake.outBytes += 3_000_000
	fake.mu.Unlock()
	sv := waitFor(t, hub, "a transfer rate", func(s *model.Server) bool { return s.Rate.In > 0 })
	if sv.Rate.Out <= 0 || sv.Rate.In < sv.Rate.Out {
		t.Fatalf("rate: %+v", sv.Rate)
	}
	tr, err := st.TrafficSince(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// Only what moved while syncwatch watched counts, not the totals before.
	if got := tr["akka"]; got.In != 30_000_000 || got.Out != 3_000_000 {
		t.Fatalf("recorded traffic: %+v", got)
	}
	waitFor(t, hub, "the rate back to zero", func(s *model.Server) bool { return s.Rate.In == 0 && !s.Rate.At.IsZero() })
}

func TestMissingAPIKey(t *testing.T) {
	cfg, _ := config.ParseYAML([]byte("servers: [{name: AKKA, url: 'https://127.0.0.1:1'}]\n"))
	h, _ := config.NewHolder(cfg)
	st, _ := store.OpenMemory(make([]byte, 32))
	hub := model.NewHub(nil)
	m := NewManager(hub, st, h, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	sv := waitFor(t, hub, "no key", func(s *model.Server) bool { return s.LastError != "" })
	if sv.Status != model.StatusDown || !strings.Contains(sv.LastError, "API key") {
		t.Fatalf("%+v", sv)
	}
}

func TestPolledMode(t *testing.T) {
	fake := newFakeST(t)
	srv := httptest.NewTLSServer(fake)
	defer srv.Close()
	cfg, err := config.ParseYAML([]byte(rulesYAML + fmt.Sprintf("servers: [{name: AKKA, url: %q}]\ncollector: {mode: polled, poll_interval: 1m}\n", srv.URL)))
	if err != nil {
		t.Fatal(err)
	}
	h, _ := config.NewHolder(cfg)
	st, _ := store.OpenMemory(make([]byte, 32))
	_ = st.SetServerKey("akka", "k", srv.URL)
	hub := model.NewHub(nil)
	m := NewManager(hub, st, h, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	sv := waitFor(t, hub, "polled full check", func(s *model.Server) bool { return s.Loaded && s.Structure != nil })
	if sv.Mode != config.ModePolled {
		t.Fatalf("mode %q", sv.Mode)
	}
	time.Sleep(300 * time.Millisecond)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.requests["/rest/events"] != 0 {
		t.Fatal("polled mode must not use the event stream")
	}
}

func (f *fakeST) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[path]
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(slow(10 * time.Second))
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// dropAndReconnect closes the next event long-poll, runs between (while the
// collector is disconnected) and wakes the collector instead of waiting out
// its backoff. It returns once the collector has reconnected and is
// following events again.
func dropAndReconnect(t *testing.T, fake *fakeST, m *Manager, between func()) {
	t.Helper()
	fake.mu.Lock()
	fake.dropEvents = 1
	dropped := fake.dropped
	fake.mu.Unlock()
	waitUntil(t, "the event long-poll to be dropped", func() bool {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return fake.dropped > dropped
	})
	if between != nil {
		between()
	}
	status, events := fake.count("/rest/system/status"), fake.count("/rest/events")
	m.mu.Lock()
	r := m.running["akka"]
	m.mu.Unlock()
	r.c.Wake()
	waitUntil(t, "the collector to reconnect", func() bool {
		return fake.count("/rest/system/status") > status && fake.count("/rest/events") >= events+2
	})
}

func TestReconnectResumesEventsWithoutFullCheck(t *testing.T) {
	fake, hub, _, m, stop := setup(t)
	defer stop()
	waitFor(t, hub, "full check", func(s *model.Server) bool { return s.Loaded && s.Structure != nil })
	full := fake.count("/rest/db/status")

	// Dropped connections alone: Syncthing kept buffering events, so there is
	// nothing to re-read.
	for i := 0; i < 2; i++ {
		dropAndReconnect(t, fake, m, nil)
	}
	if got := fake.count("/rest/db/status"); got != full {
		t.Fatalf("a dropped connection triggered a full re-check (db/status %d -> %d)", full, got)
	}
	sv := waitFor(t, hub, "up", func(s *model.Server) bool { return s.Status == model.StatusUp })
	if sv.LastError != "" {
		t.Fatalf("server still shows an error: %q", sv.LastError)
	}

	// Events that happened while disconnected are applied on reconnect.
	dropAndReconnect(t, fake, m, func() {
		fake.push("FolderPaused", map[string]string{"id": "p-alpha", "label": "LIVE-ALPHA-2PROJECTFILES"})
	})
	waitFor(t, hub, "paused folder", func(s *model.Server) bool { return s.Folders["p-alpha"] != nil && s.Folders["p-alpha"].Paused })
	if got := fake.count("/rest/db/status"); got != full {
		t.Fatalf("events after a reconnect triggered a full re-check (db/status %d -> %d)", full, got)
	}

	// Missed events (a gap in the IDs) still mean a full re-check.
	dropAndReconnect(t, fake, m, func() {
		fake.mu.Lock()
		fake.nextID += 5
		fake.mu.Unlock()
		fake.push("StateChanged", map[string]string{"folder": "p-alpha", "to": "idle"})
	})
	waitUntil(t, "a full re-check after missed events", func() bool { return fake.count("/rest/db/status") > full })

	// So does a Syncthing restart.
	full = fake.count("/rest/db/status")
	dropAndReconnect(t, fake, m, func() {
		fake.mu.Lock()
		fake.startTime = "2026-10-02T09:00:00+02:00"
		fake.mu.Unlock()
	})
	waitUntil(t, "a full re-check after a restart", func() bool { return fake.count("/rest/db/status") > full })
}

func TestKeyOnlySentToItsURL(t *testing.T) {
	fake := newFakeST(t)
	var keys []string
	var mu sync.Mutex
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		keys = append(keys, r.Header.Get("X-API-Key"))
		mu.Unlock()
		fake.ServeHTTP(w, r)
	}))
	defer srv.Close()
	cfg, _ := config.ParseYAML([]byte(fmt.Sprintf("servers: [{name: AKKA, url: %q}]\n", srv.URL)))
	h, _ := config.NewHolder(cfg)
	st, _ := store.OpenMemory(make([]byte, 32))
	// The key was entered for another address; the server's URL was changed later.
	_ = st.SetServerKey("akka", "real-key", "https://192.0.2.10:8384")
	hub := model.NewHub(nil)
	m := NewManager(hub, st, h, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	sv := waitFor(t, hub, "refusal", func(s *model.Server) bool { return s.LastError != "" })
	if sv.Status != model.StatusDown || !strings.Contains(sv.LastError, "enter the API key again") {
		t.Fatalf("%+v", sv)
	}
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	n := len(keys)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("%d request(s) reached the new URL", n)
	}

	// Entering the key for the new URL lets the collector connect.
	_ = st.SetServerKey("akka", "real-key", srv.URL)
	m.Sync()
	waitFor(t, hub, "full check", func(s *model.Server) bool { return s.Loaded })
}

func TestResetPinTakesEffectImmediately(t *testing.T) {
	fake := newFakeST(t)
	srv := httptest.NewTLSServer(fake)
	defer srv.Close()
	cfg, _ := config.ParseYAML([]byte(fmt.Sprintf("servers: [{name: AKKA, url: %q}]\n", srv.URL)))
	h, _ := config.NewHolder(cfg)
	st, _ := store.OpenMemory(make([]byte, 32))
	_ = st.SetServerKey("akka", "k", srv.URL)
	_ = st.SetPin("akka", strings.Repeat("ab", 32))
	hub := model.NewHub(nil)
	m := NewManager(hub, st, h, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.Run(ctx)
	waitFor(t, hub, "certificate change", func(s *model.Server) bool { return s.Status == model.StatusCertChanged })
	if err := m.ResetPin("akka"); err != nil {
		t.Fatal(err)
	}
	// No restart and no other settings change: the running collector trusts
	// the next certificate on first use.
	waitFor(t, hub, "up", func(s *model.Server) bool { return s.Status == model.StatusUp })
	if pin, _ := st.Pin("akka"); pin == "" || pin == strings.Repeat("ab", 32) {
		t.Fatalf("new certificate not pinned: %q", pin)
	}
}
