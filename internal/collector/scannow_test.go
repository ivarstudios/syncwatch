package collector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
)

// "Scan now" starts a structure scan of a server for an
// administrator, through the path of the timed scan. One scan of a server
// runs at a time, an administrator starts at most 1 scan of a server in
// manualEvery, and the end of the scan is the time of the last scan.

// waitDone waits for the end of a scan.
func waitDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	if done == nil {
		t.Fatal("no scan to wait for")
	}
	select {
	case <-done:
	case <-time.After(slow(10 * time.Second)):
		t.Fatal("the scan did not end")
	}
}

func TestScanNowStartsAScanAtOnce(t *testing.T) {
	old := manualEvery
	manualEvery = 300 * time.Millisecond
	defer func() { manualEvery = old }()
	_, hub, _, m, stop := setup(t)
	defer stop()
	// The scan at the start; the next timed scan is due 1 hour later.
	first := waitFor(t, hub, "the first scan", func(s *model.Server) bool { return s.Structure != nil && !s.LastStructureScan.IsZero() })

	res := m.ScanNow("akka")
	if res.State != ScanStarted || !res.Started.After(first.Structure.ScannedAt) {
		t.Fatalf("Scan now: %+v, the first scan started %v", res, first.Structure.ScannedAt)
	}
	waitDone(t, res.Done)
	sv := waitFor(t, hub, "the scan of Scan now", func(s *model.Server) bool { return s.Structure.ScannedAt.After(first.Structure.ScannedAt) })
	if sv.Structure.ScanBaseline {
		t.Fatal("a regular scan expected: no baseline scan is due")
	}
	if sv.LastStructureScan.Before(sv.Structure.ScannedAt) || !sv.LastStructureScan.After(first.LastStructureScan) {
		t.Fatalf("last scan %v, the scan started %v", sv.LastStructureScan, sv.Structure.ScannedAt)
	}

	// A second press within manualEvery: no scan, the start of the last one.
	again := m.ScanNow("akka")
	if again.State != ScanTooSoon || !again.Started.Equal(res.Started) || !again.Next.Equal(res.Started.Add(manualEvery)) || again.Done != nil {
		t.Fatalf("second press: %+v, want too soon with the start %v", again, res.Started)
	}
	time.Sleep(manualEvery)
	if third := m.ScanNow("akka"); third.State != ScanStarted {
		t.Fatalf("after %v: %+v", manualEvery, third)
	} else {
		waitDone(t, third.Done)
	}

	// A server without a running collector is not up.
	if r := m.ScanNow("sol"); r.State != ScanNotUp || r.Done != nil {
		t.Fatalf("unknown server: %+v", r)
	}
}

// A scan that an administrator starts is the scan of the timer: a baseline
// scan when one is due, and the timed scans count from its start.
func TestScanNowRunsTheScanOfTheTimer(t *testing.T) {
	old := manualEvery
	manualEvery = 0
	defer func() { manualEvery = old }()
	c, hub, _, stop := startCollector(t, newFakeST(t), "")
	defer stop()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.scanMu.Lock()
	c.sessCtx = ctx // the session, without its timer
	c.lastScan, c.lastBase = time.Now(), time.Now().Add(-8*24*time.Hour)
	c.scanMu.Unlock()

	res := c.ScanNow()
	if res.State != ScanStarted {
		t.Fatalf("Scan now: %+v", res)
	}
	waitDone(t, res.Done)
	if st := structureOf(hub); st == nil || !st.ScanBaseline {
		t.Fatalf("a baseline scan is due: %+v", st)
	}
	c.scanMu.Lock()
	last, base := c.lastScan, c.lastBase
	c.scanMu.Unlock()
	if !last.Equal(res.Started) || !base.Equal(res.Started) {
		t.Fatalf("the timed scans count from %v (baseline %v), want %v", last, base, res.Started)
	}

	res = c.ScanNow()
	waitDone(t, res.Done)
	if st := structureOf(hub); st.ScanBaseline {
		t.Fatal("no baseline scan is due now: a regular scan expected")
	}
}

// One scan of a server runs at a time: a press while a scan runs gets that
// scan, and the end of the scan is the time of the last scan.
func TestScanNowOneScanAtATime(t *testing.T) {
	old := manualEvery
	manualEvery = 0
	defer func() { manualEvery = old }()
	fake := newFakeST(t)
	var hold atomic.Bool
	release := make(chan struct{})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rest/system/browse" && hold.Load() {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		fake.ServeHTTP(w, r)
	}))
	defer srv.Close()
	hub, m, _, stop := startManager(t, srv, rulesYAML+"collector: {event_timeout: 5s}", nil, nil)
	defer stop()
	waitFor(t, hub, "the first scan", func(s *model.Server) bool { return !s.LastStructureScan.IsZero() })

	hold.Store(true)
	first := m.ScanNow("akka")
	if first.State != ScanStarted {
		t.Fatalf("Scan now: %+v", first)
	}
	second := m.ScanNow("akka")
	if second.State != ScanRunning || !second.Started.Equal(first.Started) || second.Done != first.Done {
		t.Fatalf("a press while the scan runs: %+v, want the scan of %v", second, first.Started)
	}
	time.Sleep(100 * time.Millisecond)
	released := time.Now()
	hold.Store(false)
	close(release)
	waitDone(t, first.Done)
	var last time.Time
	hub.View("akka", func(s *model.Server) { last = s.LastStructureScan })
	if last.Before(released) {
		t.Fatalf("the time of the last scan %v is before the end of the scan (after %v)", last, released)
	}
}

// Only a server that is up in a running session can be scanned.
func TestScanNowNotUp(t *testing.T) {
	old := manualEvery
	manualEvery = 0
	defer func() { manualEvery = old }()
	c, hub, _, stop := startCollector(t, newFakeST(t), "")
	defer stop()
	if r := c.ScanNow(); r.State != ScanNotUp {
		t.Fatalf("before a session: %+v", r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.maybeScan(ctx) // the session's first scan
	waitFor(t, hub, "the first scan", func(s *model.Server) bool { return !s.LastStructureScan.IsZero() })
	waitUntil(t, "the end of the first scan", func() bool {
		c.scanMu.Lock()
		defer c.scanMu.Unlock()
		return !c.scanning
	})

	hub.Update("akka", func(s *model.Server) { s.Status = model.StatusDown })
	if r := c.ScanNow(); r.State != ScanNotUp {
		t.Fatalf("server down: %+v", r)
	}
	hub.Update("akka", func(s *model.Server) { s.Status = model.StatusUp })
	if r := c.ScanNow(); r.State != ScanStarted {
		t.Fatalf("server up again: %+v", r)
	} else {
		waitDone(t, r.Done)
	}
	cancel() // the session ends
	if r := c.ScanNow(); r.State != ScanNotUp {
		t.Fatalf("after the session: %+v", r)
	}
}

// A scan that cannot list a share keeps the last known state of the share
// (TestScanKeepsDirsWhenListingFails), and also its time: ListedAt stays the
// end of the last scan that listed the share. The last scan of the server
// moves only for a scan that listed at least 1 share, or for a server
// without shares.
func TestScanThatCannotListAShareKeepsItsTime(t *testing.T) {
	fake := newFakeST(t)
	bl := &browseLog{fail: map[string]bool{}}
	c, hub, h, stop := startCollector(t, bl.wrap(fake), "")
	defer stop()
	scan := func() *model.Structure {
		t.Helper()
		time.Sleep(10 * time.Millisecond) // a later end
		if err := c.scan(context.Background(), false); err != nil {
			t.Fatal(err)
		}
		return structureOf(hub)
	}
	last := func() (at time.Time) {
		hub.View("akka", func(s *model.Server) { at = s.LastStructureScan })
		return at
	}
	fail := func(paths ...string) {
		bl.mu.Lock()
		bl.fail = map[string]bool{}
		for _, p := range paths {
			bl.fail[p+`\`] = true
		}
		bl.mu.Unlock()
	}
	const src, pf = "AKKA-LIVE-1SOURCE", "AKKA-LIVE-2PROJECTFILES"

	first := scan()
	end1 := last()
	srcPath, pfPath := shareByName(t, first, src).Path, shareByName(t, first, pf).Path
	if end1.Before(first.ScannedAt) || !shareByName(t, first, src).ListedAt.Equal(end1) || !shareByName(t, first, pf).ListedAt.Equal(end1) {
		t.Fatalf("first scan: end %v (start %v), shares listed at %v and %v", end1, first.ScannedAt,
			shareByName(t, first, src).ListedAt, shareByName(t, first, pf).ListedAt)
	}

	// 1 of 2 shares cannot be listed: the server and the other share get
	// the end of this scan, the failed share keeps its time.
	fail(srcPath)
	second := scan()
	end2 := last()
	if s := shareByName(t, second, src); s.Error == "" || !s.ListedAt.Equal(end1) {
		t.Fatalf("the share that the scan cannot list: error %q, listed at %v, want %v", s.Error, s.ListedAt, end1)
	}
	if !end2.After(end1) || !shareByName(t, second, pf).ListedAt.Equal(end2) {
		t.Fatalf("1 share listed: the last scan %v (before %v), the share listed at %v", end2, end1, shareByName(t, second, pf).ListedAt)
	}

	// No share can be listed: the scan claims no check.
	fail(srcPath, pfPath)
	third := scan()
	if got := last(); !got.Equal(end2) {
		t.Fatalf("no share listed: the last scan moved from %v to %v", end2, got)
	}
	if s, p := shareByName(t, third, src), shareByName(t, third, pf); s.Error == "" || p.Error == "" || !s.ListedAt.Equal(end1) || !p.ListedAt.Equal(end2) {
		t.Fatalf("no share listed: %+v, %+v", s, p)
	}

	// The listings work again.
	fail()
	fourth := scan()
	end4 := last()
	if !end4.After(end2) || !shareByName(t, fourth, src).ListedAt.Equal(end4) || !shareByName(t, fourth, pf).ListedAt.Equal(end4) {
		t.Fatalf("listings again: the last scan %v, shares listed at %v and %v", end4, shareByName(t, fourth, src).ListedAt, shareByName(t, fourth, pf).ListedAt)
	}

	// A server without shares: each scan is complete.
	next := h.Get().Clone()
	next.Shares.Exclude = map[string][]string{"akka": {srcPath, pfPath}}
	if err := h.Set(next); err != nil {
		t.Fatal(err)
	}
	if none := scan(); len(none.Shares) != 0 || !last().After(end4) {
		t.Fatalf("no shares: %d shares, the last scan %v (before %v)", len(none.Shares), last(), end4)
	}
}

// A scan that stops before the end writes no result and does not count:
// End tells why it stopped, the timed scans count from the scan before it,
// and an administrator can start the next scan at once. Before, the next
// press got "too soon" for 30 seconds with a scan without a result, and the
// next timed scan came 1 interval after the stopped one.
func TestScanNowStoppedScanDoesNotCount(t *testing.T) {
	fake := newFakeST(t)
	var hold atomic.Bool
	release := make(chan struct{})
	srv := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rest/system/browse" && hold.Load() {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		fake.ServeHTTP(w, r)
	})
	defer close(release)
	c, hub, h, stop := startCollector(t, srv, "")
	defer stop()
	state := func() (lastScan, lastBase, manualAt time.Time) {
		c.scanMu.Lock()
		defer c.scanMu.Unlock()
		return c.lastScan, c.lastBase, c.manualAt
	}
	last := func() (at time.Time) {
		hub.View("akka", func(s *model.Server) { at = s.LastStructureScan })
		return at
	}
	running := func() (<-chan struct{}, *ScanEnd) {
		c.scanMu.Lock()
		defer c.scanMu.Unlock()
		return c.scanDone, c.scanEnd
	}
	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	c.maybeScan(ctx1) // the first scan of the session
	waitUntil(t, "the end of the first scan", func() bool { d, _ := running(); return d == nil && !last().IsZero() })
	first := last()
	scan0, base0, _ := state()

	// Scan now, and the session ends during the scan (a dropped long-poll,
	// the VPN).
	hold.Store(true)
	res := c.ScanNow()
	if res.State != ScanStarted || res.End == nil {
		t.Fatalf("Scan now: %+v", res)
	}
	time.Sleep(50 * time.Millisecond)
	cancel1()
	waitDone(t, res.Done)
	hold.Store(false)
	if res.End.Stopped != ScanStopSession {
		t.Fatalf("a scan whose session ended: stopped %q, want %q", res.End.Stopped, ScanStopSession)
	}
	if got := last(); !got.Equal(first) {
		t.Fatalf("a stopped scan moved the last scan from %v to %v", first, got)
	}
	if scan, base, manual := state(); !scan.Equal(scan0) || !base.Equal(base0) || !manual.IsZero() {
		t.Fatalf("a stopped scan counts: the timed scans from %v (want %v), baseline %v (want %v), the 30 seconds from %v (want none)",
			scan, scan0, base, base0, manual)
	}

	// The next session: no timed scan is due yet, and the administrator can
	// start a scan at once.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	c.maybeScan(ctx2)
	again := c.ScanNow()
	if again.State != ScanStarted {
		t.Fatalf("a press after the stopped scan: %+v, want a new scan", again)
	}
	waitDone(t, again.Done)
	if again.End.Stopped != "" || !last().After(first) {
		t.Fatalf("the new scan: stopped %q, the last scan %v (before %v)", again.End.Stopped, last(), first)
	}

	// A timed baseline scan that stops: the next regular scan stays due,
	// and the next session starts it at once. The baseline scans count
	// from the stopped one: the next session does not start it again.
	due := time.Now().Add(-2 * time.Hour)
	c.scanMu.Lock()
	c.lastScan, c.lastBase = due, time.Now().Add(-8*24*time.Hour)
	c.scanMu.Unlock()
	hold.Store(true)
	ctx3, cancel3 := context.WithCancel(context.Background())
	c.maybeScan(ctx3)
	timed, end := running()
	if timed == nil {
		t.Fatal("no timed scan started")
	}
	_, stoppedBase, _ := state()
	time.Sleep(50 * time.Millisecond)
	cancel3()
	waitDone(t, timed)
	hold.Store(false)
	if scan, base, _ := state(); end.Stopped != ScanStopSession || !scan.Equal(due) || !base.Equal(stoppedBase) || base.Before(due) {
		t.Fatalf("the stopped baseline scan: stopped %q, the regular scans count from %v (want %v), the baseline scans from %v (want %v)",
			end.Stopped, scan, due, base, stoppedBase)
	}
	before := last()
	ctx4, cancel4 := context.WithCancel(context.Background())
	defer cancel4()
	c.maybeScan(ctx4)
	waitUntil(t, "the due scan in the next session", func() bool { return last().After(before) })
	if structureOf(hub).ScanBaseline {
		t.Fatal("the next session started the stopped baseline scan again")
	}

	// A scan of a server whose first full check is not complete.
	waitUntil(t, "the end of the due scan", func() bool { d, _ := running(); return d == nil })
	hub.Update("akka", func(s *model.Server) { s.Loaded = false })
	c.scanMu.Lock()
	c.startScan(ctx4, h.Get().Collector)
	done, end := c.scanDone, c.scanEnd
	c.scanMu.Unlock()
	waitDone(t, done)
	if end.Stopped != ScanStopNotLoaded {
		t.Fatalf("a server that is not loaded: stopped %q, want %q", end.Stopped, ScanStopNotLoaded)
	}
}
