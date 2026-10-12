package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
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
	conflicts  map[string]int // overrides the fixture's conflict counters

	listeners   map[string]stclient.ListenerStatus // connectionServiceStatus in system/status; nil: none
	need        map[string]int64                   // need items by "folder/device"; overrides the fixture's completion
	needNames   []string                           // what /rest/db/remoteneed lists
	needDeleted []string                           // what /rest/db/remoteneed lists as deleted, in the order of the names with needNames
	needStatus  int                                // when not 0, /rest/db/remoteneed answers this status with needBody
	needBody    string
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

// pushAll adds several events at once, so one long-poll returns them as a batch.
func (f *fakeST) pushAll(types ...string) {
	f.mu.Lock()
	for _, typ := range types {
		f.nextID++
		f.events = append(f.events, stclient.Event{ID: f.nextID, Type: typ, Time: time.Now(), Data: []byte(`{}`)})
	}
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
		if f.listeners != nil {
			st["connectionServiceStatus"] = f.listeners
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
		f.mu.Lock()
		need, needed := f.need[q.Get("folder")+"/"+q.Get("device")]
		f.mu.Unlock()
		for _, c := range all {
			if c.Folder == q.Get("folder") && c.Device == q.Get("device") {
				if needed {
					c.NeedItems, c.NeedBytes, c.Completion.Completion = need, need*1000, 50
				}
				_ = json.NewEncoder(w).Encode(c.Completion)
				return
			}
		}
		_, _ = w.Write([]byte(`{"completion":100,"remoteState":"unknown"}`))
	case "/rest/db/remoteneed":
		f.mu.Lock()
		names, deleted, status, body := f.needNames, f.needDeleted, f.needStatus, f.needBody
		f.mu.Unlock()
		if status != 0 {
			http.Error(w, body, status)
			return
		}
		if q.Get("page") != "1" || q.Get("perpage") != "100" || q.Get("folder") == "" || q.Get("device") == "" {
			f.t.Errorf("remoteneed query %q", r.URL.RawQuery)
		}
		// Like Syncthing 2.1.5: the files and the deletions that the device
		// did not do yet, in the order of the names, cut to the page.
		rn := stclient.RemoteNeed{Page: 1, PerPage: 100, Files: []stclient.NeedFile{}}
		for _, n := range names {
			rn.Files = append(rn.Files, stclient.NeedFile{Name: n, Size: 10})
		}
		for _, n := range deleted {
			rn.Files = append(rn.Files, stclient.NeedFile{Name: n, Deleted: true})
		}
		sort.Slice(rn.Files, func(i, j int) bool { return rn.Files[i].Name < rn.Files[j].Name })
		rn.Files = rn.Files[:min(len(rn.Files), 100)]
		_ = json.NewEncoder(w).Encode(rn)
	case "/rest/folder/errors":
		_, _ = w.Write([]byte(`{"errors":[{"path":"bad.txt","error":"permission denied"}]}`))
	case "/metrics":
		var c map[string]int
		_ = json.Unmarshal(f.fixture("conflicts"), &c)
		f.mu.Lock()
		for k, v := range f.conflicts {
			c[k] = v
		}
		f.mu.Unlock()
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
	stop := runManager(m)
	return fake, hub, h, m, func() { stop(); srv.Close() }
}

// runManager runs m until the returned stop function is called, which waits
// for Run (and so for every collector goroutine) to return: a test's cleanup
// must not restore the backoff variables under a collector still using them.
func runManager(m *Manager) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.Run(ctx)
	}()
	return func() { cancel(); <-done }
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
	// A wake-up that arrives while the failed session is still winding down
	// is drained as stale (see Run), so keep waking until the reconnect.
	waitUntil(t, "the collector to reconnect", func() bool {
		r.c.Wake()
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
	if sv.Status != model.StatusDown || !strings.Contains(sv.LastError, "type the API key again") {
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

// startManager runs a manager for one server, AKKA at srv, with extra YAML
// (e.g. a collector section) and returns its hub and store. prep, if set,
// prepares the store before the collector starts. stop ends the manager;
// call it before closing srv.
func startManager(t *testing.T, srv *httptest.Server, extra string, log *slog.Logger, prep func(*store.Store)) (hub *model.Hub, m *Manager, st *store.Store, stop func()) {
	t.Helper()
	cfg, err := config.ParseYAML([]byte(fmt.Sprintf("servers: [{name: AKKA, url: %q}]\n", srv.URL) + extra + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	h, _ := config.NewHolder(cfg)
	st, _ = store.OpenMemory(make([]byte, 32))
	_ = st.SetServerKey("akka", "k", srv.URL)
	if prep != nil {
		prep(st)
	}
	hub = model.NewHub(nil)
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	m = NewManager(hub, st, h, log)
	return hub, m, st, runManager(m)
}

// An HTTP error on one folder's db/status or db/completion is that folder's
// problem: the folder shows it, the other folders are read and the server
// stays up. Only 401/403 (a bad key) and transport errors concern the server.
func TestPerFolderErrorStaysWithTheFolder(t *testing.T) {
	fake := newFakeST(t)
	var mu sync.Mutex
	keyErr := false
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		mu.Lock()
		ke := keyErr
		mu.Unlock()
		switch {
		case r.URL.Path == "/rest/db/status" && q.Get("folder") == "p-beta":
			if ke {
				http.Error(w, "Not Authorized", http.StatusUnauthorized)
			} else {
				http.Error(w, "database is broken", http.StatusInternalServerError)
			}
			return
		case r.URL.Path == "/rest/db/completion" && q.Get("folder") == "p-alpha" && strings.HasPrefix(q.Get("device"), "SXOSWHO"):
			http.Error(w, "bad device", http.StatusBadRequest)
			return
		case r.URL.Path == "/rest/db/completion" && q.Get("folder") == "p-gamma":
			// Paused after the config was read: the normal 404, not a problem.
			http.Error(w, "folder is paused", http.StatusNotFound)
			return
		}
		fake.ServeHTTP(w, r)
	}))
	defer srv.Close()
	hub, m, _, stop := startManager(t, srv, "collector: {event_timeout: 5s, down_after_failures: 1}", nil, nil)
	defer stop()
	sv := waitFor(t, hub, "full check", func(s *model.Server) bool { return s.Loaded })
	if sv.Status != model.StatusUp || sv.LastError != "" {
		t.Fatalf("server: %s %q", sv.Status, sv.LastError)
	}
	beta := sv.Folders["p-beta"]
	if beta.State != "unknown" || !strings.Contains(beta.Error, "500") || beta.FolderSync() != model.SyncError {
		t.Fatalf("p-beta: state %q, error %q", beta.State, beta.Error)
	}
	alpha := sv.Folders["p-alpha"]
	if alpha.State != "idle" || !strings.Contains(alpha.Error, "400") || !strings.Contains(alpha.Error, "SXOSWHO") || len(alpha.Completion) != 1 {
		t.Fatalf("p-alpha: state %q, error %q, %d completions", alpha.State, alpha.Error, len(alpha.Completion))
	}
	gamma := sv.Folders["p-gamma"]
	if gamma.State != "idle" || gamma.Error != "" || len(gamma.Completion) != 0 {
		t.Fatalf("p-gamma: state %q, error %q, %d completions", gamma.State, gamma.Error, len(gamma.Completion))
	}

	// 401 on a per-folder request is about the key, so about the server.
	mu.Lock()
	keyErr = true
	mu.Unlock()
	m.RescanNow()
	sv = waitFor(t, hub, "down", func(s *model.Server) bool { return s.Status == model.StatusDown })
	if !strings.Contains(sv.LastError, "401") {
		t.Fatalf("LastError %q", sv.LastError)
	}
}

// An API key that can't be decrypted (the secret key changed, secret.key
// was lost) marks the server down with a message, so H1 says why, and is
// tried again regularly: once it can be read, monitoring resumes.
func TestUnreadableAPIKeyMarksServerDownAndRetries(t *testing.T) {
	old := keyRetry
	keyRetry = 200 * time.Millisecond
	defer func() { keyRetry = old }()
	fake := newFakeST(t)
	srv := httptest.NewTLSServer(fake)
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "syncwatch.db")
	first, err := store.Open(path, []byte(strings.Repeat("1", 32)))
	if err != nil {
		t.Fatal(err)
	}
	_ = first.SetServerKey("akka", "k", srv.URL)
	first.Close()
	st, err := store.Open(path, []byte(strings.Repeat("2", 32))) // another secret key
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := config.ParseYAML([]byte(fmt.Sprintf("servers: [{name: AKKA, url: %q}]\n", srv.URL)))
	h, _ := config.NewHolder(cfg)
	hub := model.NewHub(nil)
	m := NewManager(hub, st, h, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		<-done
		st.Close()
	}()
	sv := waitFor(t, hub, "down", func(s *model.Server) bool { return s.Status == model.StatusDown })
	if !strings.Contains(sv.LastError, "cannot read the API key") {
		t.Fatalf("LastError %q", sv.LastError)
	}
	// The key is entered again (now encrypted with the new secret key): the
	// next retry picks it up without a settings save.
	_ = st.SetServerKey("akka", "k", srv.URL)
	waitFor(t, hub, "full check", func(s *model.Server) bool { return s.Loaded && s.Status == model.StatusUp })
}

// In polled mode the loop sleeps until the next poll or scan is due, never
// less than a second, and a scan that is still running when the next one
// is due is left to the next poll rather than re-checked in a busy loop.
func TestPollWait(t *testing.T) {
	cfg, err := config.ParseYAML([]byte("collector: {mode: polled, poll_interval: 15m, structure_interval: 1h}\n"))
	if err != nil {
		t.Fatal(err)
	}
	h, _ := config.NewHolder(cfg)
	c := &Collector{cfg: h}
	c.lastFull, c.lastScan = time.Now(), time.Now()
	if w := c.pollWait(h.Get().Collector); w < 14*time.Minute || w > 15*time.Minute {
		t.Fatalf("nothing due: wait %v, want about 15 m", w)
	}
	c.lastScan = time.Now().Add(-50 * time.Minute)
	if w := c.pollWait(h.Get().Collector); w < 9*time.Minute || w > 10*time.Minute {
		t.Fatalf("scan due in 10 m: wait %v", w)
	}
	c.lastScan = time.Now().Add(-2 * time.Hour) // a scan is overdue
	if w := c.pollWait(h.Get().Collector); w != time.Second {
		t.Fatalf("scan overdue: wait %v, want 1 s", w)
	}
	c.scanning = true // the overdue scan is the one still running
	if w := c.pollWait(h.Get().Collector); w < 14*time.Minute || w > 15*time.Minute {
		t.Fatalf("scan running: wait %v, want the next poll", w)
	}
	c.lastFull = time.Now().Add(-time.Hour)
	if w := c.pollWait(h.Get().Collector); w != time.Second {
		t.Fatalf("poll overdue: wait %v, want 1 s", w)
	}

	// The loop itself: with the running scan 2 h old (interval 1 h) it used
	// to spin at full speed; now it sleeps.
	c.lastFull = time.Now()
	c.hub, c.log = model.NewHub(nil), slog.New(slog.NewTextHandler(io.Discard, nil))
	c.wake, c.fullNow = make(chan struct{}, 1), make(chan struct{}, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	_ = c.pollLoop(ctx)
	runtime.ReadMemStats(&after)
	if n := after.Mallocs - before.Mallocs; n > 1000 {
		t.Fatalf("%d allocations in 300 ms of pollLoop: it is spinning", n)
	}
}

// lockedBuf collects log output from the collector goroutines.
type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) count(s string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.b.String(), s)
}

// shrinkBackoff makes the session backoff fast for a test: step, then up
// to 6 steps for ordinary failures and 20 steps after a certificate change.
func shrinkBackoff(t *testing.T, step time.Duration) {
	t.Helper()
	oldStep, oldMax, oldCert := backoffStep, backoffMax, certBackoffMax
	backoffStep, backoffMax, certBackoffMax = step, 6*step, 20*step
	t.Cleanup(func() { backoffStep, backoffMax, certBackoffMax = oldStep, oldMax, oldCert })
}

func TestBackoff(t *testing.T) {
	c := &Collector{}
	for i, want := range []time.Duration{10, 10, 20, 30, 40, 50, 60, 60, 60} {
		c.failures = i
		if got := c.backoff(errors.New("x")); got != want*time.Second {
			t.Errorf("failures %d: backoff %v, want %v", i, got, want*time.Second)
		}
	}
	certErr := &stclient.CertChangedError{Server: "AKKA", Old: "aa", New: "bb"}
	for i, want := range map[int]time.Duration{0: 10, 1: 10, 2: 20, 6: 60, 7: 70, 29: 290, 30: 300, 31: 300, 100: 300} {
		c.certFails = i
		if got := c.backoff(certErr); got != want*time.Second {
			t.Errorf("certificate failures %d: backoff %v, want %v", i, got, want*time.Second)
		}
	}
}

// A server whose certificate changed used to be retried every 10 s
// forever, with a WARN line each time (about 8,600 a day). The retries now
// back off like other failures, further, and the warning is repeated at
// most once an hour; the attempts in between log at DEBUG.
func TestCertChangedBacksOffAndWarnsOnce(t *testing.T) {
	shrinkBackoff(t, 50*time.Millisecond)
	fake := newFakeST(t)
	srv := httptest.NewTLSServer(fake)
	defer srv.Close()
	var logs lockedBuf
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	hub, _, _, stop := startManager(t, srv, "", log, func(st *store.Store) {
		_ = st.SetPin("akka", strings.Repeat("ab", 32)) // a pin that doesn't match
	})
	defer stop()
	sv := waitFor(t, hub, "cert-changed", func(s *model.Server) bool { return s.Status == model.StatusCertChanged })
	if sv.CertChange == nil {
		t.Fatal("no CertChange recorded")
	}
	time.Sleep(time.Second)
	const line = "certificate changed, syncwatch does not connect until an administrator accepts it"
	n, warns := logs.count(line), logs.count("level=WARN msg=\""+line)
	t.Logf("%d attempts in about 1 s, %d at WARN", n, warns)
	// Attempts at 0, 50, 150, 300, 500, 750 ms with the growing backoff;
	// every 50 ms (about 20) with the old fixed one.
	if n < 3 || n > 10 {
		t.Fatalf("%d attempts in 1 s; want about 6", n)
	}
	if warns != 1 {
		t.Fatalf("%d WARN lines; want 1", warns)
	}
	if fake.count("/rest/system/status") != 0 {
		t.Fatal("a request reached the server despite the changed certificate")
	}
}

// A wake-up left over from while the session was running (FullCheckNow is
// called from inside the event loop) must not skip the backoff after the
// next failure: two quick failures would mark the server down at once.
func TestStaleWakeDoesNotSkipBackoff(t *testing.T) {
	shrinkBackoff(t, 300*time.Millisecond)
	var mu sync.Mutex
	requests := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		http.Error(w, "Not Authorized", http.StatusUnauthorized) // fails without retries
	}))
	defer srv.Close()
	cfg, _ := config.ParseYAML([]byte(fmt.Sprintf("servers: [{name: AKKA, url: %q}]\n", srv.URL)))
	h, _ := config.NewHolder(cfg)
	st, _ := store.OpenMemory(make([]byte, 32))
	hub := model.NewHub(nil)
	hub.SetServers([]*model.Server{{ID: "akka", Name: "AKKA"}})
	c, err := newCollector(h.Get().Servers[0], "k", hub, st, h, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	c.Wake() // left over from before the session
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	defer func() { cancel(); <-done }() // before the cleanup restores the backoff
	go func() {
		defer close(done)
		c.Run(ctx)
	}()
	time.Sleep(150 * time.Millisecond) // the first session has failed; the backoff is 300 ms
	mu.Lock()
	n := requests
	mu.Unlock()
	if n != 1 {
		t.Fatalf("%d requests within 150 ms of a failure; want 1 (the stale wake-up skipped the backoff)", n)
	}
	waitUntil(t, "the retry after the backoff", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return requests >= 2
	})
}

// A structure scan runs in the background with the session's context: when
// the session fails (the server went away) the scan's listings are
// cancelled instead of running on, each to its own timeout.
func TestScanStopsWhenSessionEnds(t *testing.T) {
	fake := newFakeST(t)
	var mu sync.Mutex
	hung, finished := 0, 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rest/system/browse" {
			mu.Lock()
			hung++
			mu.Unlock()
			<-r.Context().Done() // the listing never answers
			mu.Lock()
			finished++
			mu.Unlock()
			return
		}
		fake.ServeHTTP(w, r)
	}))
	defer srv.Close()
	hub, _, _, stop := startManager(t, srv, rulesYAML+"collector: {event_timeout: 5s, down_after_failures: 1}", nil, nil)
	defer stop()
	waitFor(t, hub, "full check", func(s *model.Server) bool { return s.Loaded })
	waitUntil(t, "the scan to start listing", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return hung > 0
	})
	// Cut the event long-poll: the session fails and the server is marked down.
	fake.mu.Lock()
	fake.dropEvents = 1
	fake.mu.Unlock()
	waitFor(t, hub, "down", func(s *model.Server) bool { return s.Status == model.StatusDown })
	deadline := time.Now().Add(slow(3 * time.Second))
	for time.Now().Before(deadline) {
		mu.Lock()
		done := finished == hung
		mu.Unlock()
		if done {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("%d of %d hung listings still running 3 s after the session ended", hung-finished, hung)
}

// Error texts reach the dashboard, the API and H1 messages: HTTP errors as
// they are, the usual network failures as a phrase, nothing like ": EOF".
func TestShortErr(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{&stclient.StatusError{Endpoint: "/rest/system/status", Code: 500, Body: "boom"}, "/rest/system/status: HTTP 500 boom"},
		{fmt.Errorf("db/status x: %w", &stclient.StatusError{Endpoint: "/rest/db/status", Code: 401}), "/rest/db/status: HTTP 401 (check the API key)"},
		{fmt.Errorf("wrapped: %w", context.DeadlineExceeded), "timeout"},
		{errors.New(": EOF"), "connection closed early"},
		{errors.New("/rest/events: reading response: unexpected EOF"), "connection closed early"},
		{errors.New(": dial tcp 127.0.0.1:1: connect: connection refused"), "connection refused"},
		{errors.New(": dial tcp 127.0.0.1:1: connectex: No connection could be made because the target machine actively refused it."), "connection refused"},
		{errors.New(": dial tcp 10.0.0.9:8384: connect: no route to host"), "no route to host"},
		{errors.New(": dial tcp 10.0.0.9:8384: i/o timeout"), "timeout"},
		{errors.New(": net/http: TLS handshake timeout"), "timeout"},
		{errors.New(": read tcp 10.0.0.1:5->10.0.0.9:8384: read: connection reset by peer"), "connection reset"},
		{errors.New(": read tcp 10.0.0.1:5->10.0.0.9:8384: wsarecv: An existing connection was forcibly closed by the remote host."), "connection reset"},
		{errors.New(": dial tcp: lookup nas.invalid on 127.0.0.11:53: no such host"), "host name not found"},
		{errors.New(": something else entirely"), "something else entirely"},
		{errors.New(strings.Repeat("x", 300)), strings.Repeat("x", 200)},
	} {
		if got := shortErr(tc.err); got != tc.want {
			t.Errorf("shortErr(%q) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// A server that closes the connection without an answer used to show the
// error ": EOF" on the tile and in the H1 message.
func TestLastErrorIsReadable(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close()
			}
		}
	}))
	defer srv.Close()
	hub, _, _, stop := startManager(t, srv, "collector: {down_after_failures: 1}", nil, nil)
	defer stop()
	sv := waitFor(t, hub, "down", func(s *model.Server) bool { return s.Status == model.StatusDown })
	if sv.LastError != "connection closed early" {
		t.Fatalf("LastError %q", sv.LastError)
	}
}

// Bytes moved while there were no readings (the server unreachable, or
// syncwatch down) used to be booked at the first reading afterwards, so the
// day and week sums included older traffic. After more than three missed
// samples the counters are only re-baselined.
func TestTrafficGapIsNotBooked(t *testing.T) {
	old := trafficEvery
	trafficEvery = 200 * time.Millisecond
	defer func() { trafficEvery = old }()
	shrinkBackoff(t, 100*time.Millisecond)
	fake := newFakeST(t)
	fake.inBytes, fake.outBytes = 5_000_000, 1_000_000
	var mu sync.Mutex
	away := false
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		a := away
		mu.Unlock()
		if a {
			http.Error(w, "Not Authorized", http.StatusUnauthorized)
			return
		}
		fake.ServeHTTP(w, r)
	}))
	defer srv.Close()
	hub, _, st, stop := startManager(t, srv, "collector: {event_timeout: 5s, down_after_failures: 1}", nil, nil)
	defer stop()
	waitFor(t, hub, "loaded", func(s *model.Server) bool { return s.Loaded })
	time.Sleep(500 * time.Millisecond) // a baseline reading
	mu.Lock()
	away = true
	mu.Unlock()
	fake.push("StateChanged", map[string]string{"folder": "p-alpha", "to": "idle"}) // ends the open long-poll
	waitFor(t, hub, "down", func(s *model.Server) bool { return s.Status == model.StatusDown })
	fake.mu.Lock()
	fake.inBytes += 30_000_000
	fake.outBytes += 3_000_000
	fake.mu.Unlock()
	time.Sleep(time.Second) // more than three sampling intervals without a reading
	mu.Lock()
	away = false
	mu.Unlock()
	waitFor(t, hub, "up again", func(s *model.Server) bool { return s.Status == model.StatusUp })
	time.Sleep(500 * time.Millisecond) // the reading that re-baselines
	fake.mu.Lock()
	fake.inBytes += 1_000_000
	fake.outBytes += 100_000
	fake.mu.Unlock()
	waitFor(t, hub, "a transfer rate", func(s *model.Server) bool { return s.Rate.In > 0 })
	tr, err := st.TrafficSince(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got := tr["akka"]; got.In != 1_000_000 || got.Out != 100_000 {
		t.Fatalf("recorded traffic: %+v; the bytes moved while away must not count", got)
	}
}

// Several ConfigSaved or Pending*Changed events in one batch are one config
// re-read and one pending re-read, after the batch, not one per event
// (while no long-poll is open). A failing pending re-read is logged, once
// per session.
func TestEventBatchRefreshesOnce(t *testing.T) {
	fake := newFakeST(t)
	var mu sync.Mutex
	pendingFail := false
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		pf := pendingFail
		mu.Unlock()
		if pf && r.URL.Path == "/rest/cluster/pending/devices" {
			fake.mu.Lock()
			fake.requests[r.URL.Path]++
			fake.mu.Unlock()
			http.Error(w, "nope", http.StatusBadRequest)
			return
		}
		fake.ServeHTTP(w, r)
	}))
	defer srv.Close()
	var logs lockedBuf
	log := slog.New(slog.NewTextHandler(&logs, nil))
	hub, _, _, stop := startManager(t, srv, "collector: {event_timeout: 5s}", log, nil)
	defer stop()
	waitFor(t, hub, "full check", func(s *model.Server) bool { return s.Loaded })
	waitUntil(t, "the event loop", func() bool { return fake.count("/rest/events") >= 2 })
	folders, pending := fake.count("/rest/config/folders"), fake.count("/rest/cluster/pending/devices")
	fake.pushAll("ConfigSaved", "PendingDevicesChanged", "ConfigSaved", "PendingFoldersChanged", "ConfigSaved")
	waitUntil(t, "the re-reads", func() bool {
		return fake.count("/rest/config/folders") > folders && fake.count("/rest/cluster/pending/devices") > pending
	})
	time.Sleep(300 * time.Millisecond)
	if n := fake.count("/rest/config/folders") - folders; n != 1 {
		t.Fatalf("%d config re-reads for a batch of 3 ConfigSaved; want 1", n)
	}
	if n := fake.count("/rest/cluster/pending/devices") - pending; n != 1 {
		t.Fatalf("%d pending re-reads for a batch of 2 pending events; want 1", n)
	}

	mu.Lock()
	pendingFail = true
	mu.Unlock()
	const line = "cannot read the pending devices and folders again"
	for i := 0; i < 2; i++ {
		n := fake.count("/rest/cluster/pending/devices")
		fake.push("PendingDevicesChanged", map[string]any{})
		waitUntil(t, "the pending re-read", func() bool { return fake.count("/rest/cluster/pending/devices") > n })
	}
	time.Sleep(200 * time.Millisecond)
	if n := logs.count("level=WARN msg=\"" + line); n != 1 {
		t.Fatalf("%d WARN lines for two failed pending re-reads in one session; want 1", n)
	}
}

// Conflict counters (H10) are read at every session start, not only at
// full checks, and a counter that restarted with Syncthing counts in
// full. A restart that breaks no request is noticed by the next full check,
// which also follows the restarted event IDs from 0.
func TestConflictsAcrossReconnectsAndRestarts(t *testing.T) {
	fake, hub, _, m, stop := setup(t)
	defer stop()
	st := m.st
	waitFor(t, hub, "full check", func(s *model.Server) bool { return s.Loaded && s.Structure != nil })
	full := fake.count("/rest/db/status")
	since := func() int64 {
		t.Helper()
		c, err := st.ConflictsSince(time.Now().Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return c["akka"]["p-alpha"]
	}
	if n := since(); n != 0 {
		t.Fatalf("baseline reading counted %d conflicts", n)
	}
	// Some events first, so the collector's event ID is well above the
	// ones Syncthing hands out after a restart.
	for i := 0; i < 3; i++ {
		fake.push("StateChanged", map[string]string{"folder": "p-alpha", "to": "scanning"})
	}
	waitFor(t, hub, "events applied", func(s *model.Server) bool { return s.Folders["p-alpha"].State == "scanning" })

	// Two conflicts since the full check: read when the collector reconnects
	// (a resume, no full check).
	fake.mu.Lock()
	fake.conflicts = map[string]int{"p-alpha": 2}
	fake.mu.Unlock()
	dropAndReconnect(t, fake, m, nil)
	waitUntil(t, "the conflicts to be counted at session start", func() bool { return since() == 2 })
	if got := fake.count("/rest/db/status"); got != full {
		t.Fatalf("the reconnect ran a full re-check (db/status %d -> %d)", full, got)
	}

	// Syncthing restarts: the counter starts over, and its new value is new.
	dropAndReconnect(t, fake, m, func() {
		fake.mu.Lock()
		fake.startTime = "2026-10-02T09:00:00+02:00"
		fake.conflicts = map[string]int{"p-alpha": 1}
		fake.mu.Unlock()
	})
	waitUntil(t, "the conflicts after the restart to be counted", func() bool { return since() == 3 })

	// A restart that breaks no request: the next full check notices the new
	// startTime and follows the event IDs, which start over, from 0.
	fake.mu.Lock()
	fake.startTime = "2026-10-03T09:00:00+02:00"
	fake.conflicts = map[string]int{"p-alpha": 1}
	fake.events, fake.nextID = nil, 0
	fake.mu.Unlock()
	m.RescanNow()
	waitUntil(t, "the restart to be counted", func() bool { return since() == 4 })
	fake.push("FolderPaused", map[string]string{"id": "p-beta", "label": "LIVE-BETA-1SOURCE"}) // event ID 1
	waitFor(t, hub, "the event after the restart", func(s *model.Server) bool { return s.Folders["p-beta"] != nil && s.Folders["p-beta"].Paused })
}
