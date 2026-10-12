package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/stclient"
	"github.com/ivarstudios/syncwatch/internal/store"
)

// browseLog records the directories listed through /rest/system/browse and
// lets a test fail listings of chosen directories.
type browseLog struct {
	mu     sync.Mutex
	listed []string
	fail   map[string]bool     // directories (trailing separator included) answered with 500
	extra  map[string][]string // listings not in the fixture, by directory with trailing separator
}

func (b *browseLog) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rest/system/browse" {
			cur := r.URL.Query().Get("current")
			b.mu.Lock()
			b.listed = append(b.listed, cur)
			fail := b.fail[cur]
			extra, ok := b.extra[cur]
			b.mu.Unlock()
			if fail {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			if ok {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(extra)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (b *browseLog) reset() {
	b.mu.Lock()
	b.listed = nil
	b.mu.Unlock()
}

// count returns how many listings had the given suffix (a directory name).
func (b *browseLog) count(suffix string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, p := range b.listed {
		if strings.HasSuffix(strings.TrimRight(p, `\`), suffix) {
			n++
		}
	}
	return n
}

// startCollector builds a collector for the fixture server behind h (white
// box: no manager, no loop) and runs its first full check, so that
// scan() can be called directly with chosen arguments.
func startCollector(t *testing.T, h http.Handler, yamlTail string) (*Collector, *model.Hub, *config.Holder, func()) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	cfg, err := config.ParseYAML([]byte(rulesYAML + fmt.Sprintf("servers: [{name: AKKA, url: %q}]\n", srv.URL) + yamlTail))
	if err != nil {
		t.Fatal(err)
	}
	holder, _ := config.NewHolder(cfg)
	st, _ := store.OpenMemory(make([]byte, 32))
	hub := model.NewHub(nil)
	sc := cfg.Servers[0]
	hub.SetServers([]*model.Server{{ID: sc.ID, Name: sc.Name, URL: sc.URL}})
	c, err := newCollector(sc, "k", hub, st, holder, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := c.fullCheck(ctx); err != nil {
		t.Fatal(err)
	}
	c.succeeded()
	return c, hub, holder, func() { cancel(); srv.Close(); c.client.Close() }
}

func structureOf(hub *model.Hub) *model.Structure {
	var out *model.Structure
	hub.View("akka", func(s *model.Server) { out = s.Structure.Clone() })
	return out
}

func shareByName(t *testing.T, st *model.Structure, name string) *model.Share {
	t.Helper()
	for _, sh := range st.Shares {
		if sh.Name == name {
			return sh
		}
	}
	t.Fatalf("share %s missing in %+v", name, st.Shares)
	return nil
}

// A share whose listing fails keeps its previous directory list (marked
// stale by the error), so that the directory findings (S4–S7) don't resolve
// and come back around an outage or a slow listing.
func TestScanKeepsDirsWhenListingFails(t *testing.T) {
	fake := newFakeST(t)
	bl := &browseLog{fail: map[string]bool{}}
	c, hub, _, stop := startCollector(t, bl.wrap(fake), "")
	defer stop()
	ctx := context.Background()
	if err := c.scan(ctx, true); err != nil {
		t.Fatal(err)
	}
	first := structureOf(hub)
	src := shareByName(t, first, "AKKA-LIVE-1SOURCE")
	if len(src.Dirs) != 6 || src.Error != "" {
		t.Fatalf("first scan: %d dirs, error %q", len(src.Dirs), src.Error)
	}

	bl.mu.Lock()
	bl.fail[src.Path+`\`] = true
	bl.mu.Unlock()
	time.Sleep(10 * time.Millisecond) // a later ScannedAt
	if err := c.scan(ctx, false); err != nil {
		t.Fatal(err)
	}
	second := structureOf(hub)
	src2 := shareByName(t, second, "AKKA-LIVE-1SOURCE")
	if src2.Error == "" || !strings.Contains(src2.Error, "500") {
		t.Fatalf("listing error not recorded: %q", src2.Error)
	}
	if len(src2.Dirs) != 6 || !src2.ScannedAt.Equal(src.ScannedAt) {
		t.Fatalf("stale share: %d dirs (want 6 kept), scanned %v (want %v)", len(src2.Dirs), src2.ScannedAt, src.ScannedAt)
	}
	// The other share was listed as usual.
	if pf := shareByName(t, second, "AKKA-LIVE-2PROJECTFILES"); pf.Error != "" || len(pf.Dirs) != 2 || !pf.ScannedAt.After(src.ScannedAt) {
		t.Fatalf("healthy share: %+v", pf)
	}

	// Once the listing works again the share is fresh.
	bl.mu.Lock()
	bl.fail = map[string]bool{}
	bl.mu.Unlock()
	if err := c.scan(ctx, false); err != nil {
		t.Fatal(err)
	}
	if src3 := shareByName(t, structureOf(hub), "AKKA-LIVE-1SOURCE"); src3.Error != "" || len(src3.Dirs) != 6 || !src3.ScannedAt.After(src.ScannedAt) {
		t.Fatalf("recovered share: %+v", src3)
	}
}

const alphaRenders = `D:\tmp\swsim\st\akka\share\AKKA-LIVE-2PROJECTFILES\LIVE-ALPHA-2PROJECTFILES\renders`

// Nested entries that a scan doesn't find again (those from events inside
// synced folders, which regular scans don't walk) are kept while the
// directory exists, but only if they still count under the current rules.
func TestScanReappliesRulesToKeptEntries(t *testing.T) {
	fake := newFakeST(t)
	bl := &browseLog{extra: map[string][]string{alphaRenders + `\`: {alphaRenders + `\LIVE-NESTED-2PROJECTFILES`}}}
	c, hub, h, stop := startCollector(t, bl.wrap(fake), "")
	defer stop()
	ctx := context.Background()
	if err := c.scan(ctx, true); err != nil {
		t.Fatal(err)
	}
	base := h.Get()
	event := stclient.DiskChangeData{FolderID: "p-alpha", Type: "dir", Action: "modified", Path: `renders\LIVE-NESTED-2PROJECTFILES`}
	nested := func() int { return len(structureOf(hub).Nested) }

	// Unchanged rules: the entry survives the scan (one parent listing).
	c.diskChange(event, time.Now())
	if err := c.scan(ctx, false); err != nil {
		t.Fatal(err)
	}
	if nested() != 1 {
		t.Fatalf("entry not kept: %+v", structureOf(hub).Nested)
	}

	changes := map[string]func(c *config.Config){
		"ignore_dirs with the middle directory": func(c *config.Config) { c.Structure.IgnoreDirs = append(c.Structure.IgnoreDirs, "renders") },
		"a depth that no longer reaches it":     func(c *config.Config) { c.Structure.NestedScanDepth = 1 },
		"a pattern it doesn't match":            func(c *config.Config) { c.Structure.ProjectPattern = "^LIVE-[A-K]" },
		"its share excluded": func(c *config.Config) {
			c.Shares.Exclude = map[string][]string{"akka": {`D:\tmp\swsim\st\akka\share\AKKA-LIVE-2PROJECTFILES`}}
		},
	}
	for what, change := range changes {
		if err := h.Set(base.Clone()); err != nil {
			t.Fatal(err)
		}
		c.diskChange(event, time.Now())
		if nested() != 1 {
			t.Fatalf("%s: entry not added", what)
		}
		next := base.Clone()
		change(next)
		if err := h.Set(next); err != nil {
			t.Fatal(err)
		}
		if err := c.scan(ctx, false); err != nil {
			t.Fatal(err)
		}
		if nested() != 0 {
			t.Errorf("%s: stale entry kept: %+v", what, structureOf(hub).Nested)
		}
	}
}

// Polled mode has no events to cover synced project folders, so every scan
// walks them like a baseline does. Events mode leaves them to events between
// baselines.
func TestPolledScanWalksSyncedFolders(t *testing.T) {
	for _, mode := range []string{config.ModeEvents, config.ModePolled} {
		fake := newFakeST(t)
		bl := &browseLog{}
		c, _, _, stop := startCollector(t, bl.wrap(fake), "collector: {mode: "+mode+"}\n")
		if err := c.scan(context.Background(), false); err != nil {
			t.Fatal(err)
		}
		stop()
		synced := bl.count("LIVE-BETA-1SOURCE") + bl.count("LIVE-GAMMA-1SOURCE") + bl.count("LIVE-ALPHA-2PROJECTFILES")
		if n := bl.count("LIVE-NOTSYNCED-1SOURCE"); n != 1 {
			t.Errorf("%s: project folder not in Syncthing listed %d times, want 1", mode, n)
		}
		want := 0
		if mode == config.ModePolled {
			want = 3
		}
		if synced != want {
			t.Errorf("%s: synced project folders listed %d times on a regular scan, want %d", mode, synced, want)
		}
	}
}

// In events mode the loop sits in a long-poll for up to event_timeout, so a
// scan falling due meanwhile started up to that late. scanTimer starts it
// on time, and only while the server is up.
func TestScanTimerStartsDueScans(t *testing.T) {
	old := scanTimerEvery
	scanTimerEvery = 50 * time.Millisecond
	defer func() { scanTimerEvery = old }()
	fake := newFakeST(t)
	c, hub, _, stop := startCollector(t, fake, "collector: {structure_interval: 1m}\n")
	defer stop()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	due := func(in time.Duration) {
		c.scanMu.Lock()
		c.lastScan, c.lastBase = time.Now().Add(in-time.Minute), time.Now()
		c.scanMu.Unlock()
	}
	due(300 * time.Millisecond)
	c.maybeScan(ctx) // what the loop does before its long-poll: not due yet
	if st := structureOf(hub); st != nil {
		t.Fatal("scan started before it was due")
	}
	first := waitFor(t, hub, "the timer's scan", func(s *model.Server) bool { return s.Structure != nil })
	if first.Structure.ScanBaseline {
		t.Fatal("regular scan expected")
	}

	// Not while the server is down.
	hub.Update("akka", func(s *model.Server) { s.Status = model.StatusDown })
	due(0)
	time.Sleep(300 * time.Millisecond)
	if st := structureOf(hub); st.ScannedAt.After(first.Structure.ScannedAt) {
		t.Fatal("a scan ran while the server was down")
	}

	// The timer keeps going without the loop once the server is back.
	hub.Update("akka", func(s *model.Server) { s.Status = model.StatusUp })
	waitFor(t, hub, "the timer's second scan", func(s *model.Server) bool { return s.Structure.ScannedAt.After(first.Structure.ScannedAt) })
}

// Each session has its own scanTimer. Before, the timer ran with the first
// session only: after a reconnect, a scan that fell due waited for the end
// of the long-poll (up to event_timeout).
func TestScanTimerRunsAgainAfterReconnect(t *testing.T) {
	old := scanTimerEvery
	scanTimerEvery = 50 * time.Millisecond
	defer func() { scanTimerEvery = old }()
	fake, hub, _, m, stop := setup(t)
	defer stop()
	waitFor(t, hub, "the first scan", func(s *model.Server) bool { return !s.LastStructureScan.IsZero() })
	dropAndReconnect(t, fake, m, nil)

	// The loop waits in long-polls of 15 seconds from now on.
	fake.mu.Lock()
	fake.longPoll = 15 * time.Second
	fake.mu.Unlock()
	polls := fake.count("/rest/events")
	waitUntil(t, "a long-poll of 15 seconds", func() bool { return fake.count("/rest/events") > polls })
	time.Sleep(100 * time.Millisecond)

	m.mu.Lock()
	c := m.running["akka"].c
	m.mu.Unlock()
	var before time.Time
	hub.View("akka", func(s *model.Server) { before = s.LastStructureScan })
	c.scanMu.Lock()
	c.lastScan, c.lastBase = time.Now().Add(300*time.Millisecond-time.Hour), time.Now()
	c.scanMu.Unlock()
	deadline := time.Now().Add(slow(3 * time.Second))
	for time.Now().Before(deadline) {
		var last time.Time
		hub.View("akka", func(s *model.Server) { last = s.LastStructureScan })
		if last.After(before) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the due scan did not start during the long-poll: no scanTimer after the reconnect")
}

// A gap in the event IDs (Syncthing's 1000-event buffer overflowed, which
// per-file events do during any big sync) no longer chains full re-checks:
// the top-level project folders of the folders in the batch are re-scanned,
// and a gap-caused full re-check runs at most once per gapFullEvery, with one
// deferred re-check catching up.
func TestMissedEventsRescansFoldersAndLimitsFullChecks(t *testing.T) {
	defer func(a, b, c time.Duration) { gapFullEvery, gapScanAfter, gapScanMax = a, b, c }(gapFullEvery, gapScanAfter, gapScanMax)
	gapFullEvery, gapScanAfter, gapScanMax = 400*time.Millisecond, 50*time.Millisecond, time.Second
	alpha := `D:\tmp\swsim\st\akka\share\AKKA-LIVE-2PROJECTFILES\LIVE-ALPHA-2PROJECTFILES`
	fake := newFakeST(t)
	bl := &browseLog{fail: map[string]bool{}, extra: map[string][]string{alpha + `\`: {alphaRenders}, alphaRenders + `\`: {alphaRenders + `\LIVE-NESTED-2PROJECTFILES`}}}
	c, hub, _, stop := startCollector(t, bl.wrap(fake), "")
	defer stop()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// A regular scan in events mode leaves synced project folders to events.
	if err := c.scan(ctx, false); err != nil {
		t.Fatal(err)
	}
	if n := len(structureOf(hub).Nested); n != 0 {
		t.Fatalf("%d nested entries after a regular scan", n)
	}
	nested := func() int { return len(structureOf(hub).Nested) }
	batch := []stclient.Event{{ID: 1010, Type: "LocalChangeDetected", Data: json.RawMessage(`{"folder":"p-alpha","folderID":"p-alpha","type":"file","action":"modified","path":"x.blend"}`)}}

	// The first gap asks for a full re-check at once and re-scans the folder's
	// top-level project folder: the nested folder whose event was lost appears.
	bl.reset()
	c.missedEvents(ctx, 5, 1010, batch)
	if len(c.fullNow) != 1 {
		t.Fatal("first gap: no full re-check requested")
	}
	<-c.fullNow
	waitFor(t, hub, "the re-scan after the gap", func(s *model.Server) bool { return len(s.Structure.Nested) == 1 })
	if bl.count("LIVE-BETA-1SOURCE")+bl.count("LIVE-GAMMA-1SOURCE") != 0 || bl.count("LIVE-ALPHA-2PROJECTFILES") != 1 {
		t.Fatalf("re-scan listed %v; want only the affected project folder", bl.listed)
	}

	// A second gap soon after: no full re-check now, one deferred catches up.
	c.missedEvents(ctx, 1010, 2020, batch)
	if len(c.fullNow) != 0 {
		t.Fatal("a second gap within gapFullEvery requested a full re-check at once")
	}
	waitUntil(t, "the deferred full re-check", func() bool { return len(c.fullNow) == 1 })
	<-c.fullNow

	// The re-scan drops an entry whose directory went away during the gap...
	bl.mu.Lock()
	bl.extra[alphaRenders+`\`] = []string{}
	bl.mu.Unlock()
	c.missedEvents(ctx, 2020, 3030, batch)
	waitFor(t, hub, "the entry removed by the re-scan", func(s *model.Server) bool { return len(s.Structure.Nested) == 0 })

	// ...but not when a listing of the walk failed.
	bl.mu.Lock()
	bl.extra[alphaRenders+`\`] = []string{alphaRenders + `\LIVE-NESTED-2PROJECTFILES`}
	bl.mu.Unlock()
	c.missedEvents(ctx, 3030, 4040, batch)
	waitFor(t, hub, "the entry back", func(s *model.Server) bool { return len(s.Structure.Nested) == 1 })
	bl.mu.Lock()
	bl.fail[alphaRenders+`\`] = true
	bl.mu.Unlock()
	before := bl.count("LIVE-ALPHA-2PROJECTFILES")
	c.missedEvents(ctx, 4040, 5050, batch)
	waitUntil(t, "a re-scan with a failing listing", func() bool { return bl.count("LIVE-ALPHA-2PROJECTFILES") > before })
	time.Sleep(100 * time.Millisecond)
	if nested() != 1 {
		t.Fatal("a failed listing removed the nested entry")
	}
}
