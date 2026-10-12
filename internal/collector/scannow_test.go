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

// W-056: "Scan now" starts a structure scan of a server for an
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
