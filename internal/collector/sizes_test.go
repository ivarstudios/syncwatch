package collector

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/stclient"
)

// sizeGate answers /syncwatch/size as a gate does, records the requests,
// and passes everything else to the fake Syncthing. With none it answers
// 404, as Syncthing without a gate does.
type sizeGate struct {
	mu   sync.Mutex
	none bool
	reqs []sizeReq
}

type sizeReq struct {
	path    string
	measure bool
}

var measuredAt = time.Date(2026, 10, 14, 2, 30, 0, 0, time.UTC)

func (g *sizeGate) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/syncwatch/size" {
			next.ServeHTTP(w, r)
			return
		}
		q := r.URL.Query()
		g.mu.Lock()
		none := g.none
		g.reqs = append(g.reqs, sizeReq{q.Get("path"), q.Get("measure") == "1"})
		g.mu.Unlock()
		if none {
			http.NotFound(w, r)
			return
		}
		res := stclient.DirSize{Path: q.Get("path"), Bytes: int64(len(q.Get("path"))) * 1000, Files: 7, Complete: true, MeasuredAt: measuredAt, TookMS: 1200}
		if q.Get("measure") == "1" {
			res.State = stclient.SizeQueued
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(res)
	})
}

// take returns the requests since the last call.
func (g *sizeGate) take() []sizeReq {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := g.reqs
	g.reqs = nil
	return out
}

func sizesOf(c *Collector) (map[string]*model.DirSize, bool) {
	var sizes map[string]*model.DirSize
	none := false
	c.hub.View(c.id, func(s *model.Server) {
		if s.Structure != nil {
			sizes, none = s.Structure.Clone().Sizes, s.Structure.NoSizes
		}
	})
	return sizes, none
}

// startSizes is a scanned collector behind a size gate, with the time zone
// UTC and the nightly measurement at 02:00.
func startSizes(t *testing.T) (*Collector, *sizeGate, []string, context.Context) {
	t.Helper()
	g := &sizeGate{}
	c, _, h, stop := startCollector(t, g.wrap(newFakeST(t)), "timezone: UTC\ncollector: {size_time: '02:00'}\n")
	t.Cleanup(stop)
	ctx := context.Background()
	if err := c.scan(ctx, true); err != nil {
		t.Fatal(err)
	}
	dirs := unsyncedDirs(structureOf(c.hub), h.Structure())
	if len(dirs) == 0 {
		t.Fatal("the fixture has no project folder outside Syncthing")
	}
	g.take()
	return c, g, dirs, ctx
}

// By day, syncwatch reads the sizes that the gate has, without a new
// measurement, and reads them again only after an hour.
func TestSizesReadByDay(t *testing.T) {
	c, g, dirs, ctx := startSizes(t)
	noon := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	c.syncSizes(ctx, noon)
	reqs := g.take()
	if len(reqs) != len(dirs) {
		t.Fatalf("%d requests for %d folders: %+v", len(reqs), len(dirs), reqs)
	}
	for _, r := range reqs {
		if r.measure {
			t.Errorf("a measurement by day: %+v", r)
		}
	}
	sizes, none := sizesOf(c)
	if none || len(sizes) != len(dirs) {
		t.Fatalf("sizes %v, none %v", sizes, none)
	}
	for _, p := range dirs {
		if s := sizes[p]; s == nil || s.Bytes != int64(len(p))*1000 || !s.MeasuredAt.Equal(measuredAt) || s.Took != 1200*time.Millisecond || !s.Complete {
			t.Errorf("%s: %+v", p, s)
		}
	}
	c.syncSizes(ctx, noon.Add(30*time.Minute))
	if reqs := g.take(); len(reqs) != 0 {
		t.Errorf("read again within the hour: %+v", reqs)
	}
	c.syncSizes(ctx, noon.Add(61*time.Minute))
	if reqs := g.take(); len(reqs) != len(dirs) {
		t.Errorf("not read again after an hour: %+v", reqs)
	}
}

// In the hour after size_time, syncwatch asks the gate to measure each
// folder once a night.
func TestSizesMeasuredEachNight(t *testing.T) {
	c, g, dirs, ctx := startSizes(t)
	night := time.Date(2026, 10, 15, 2, 10, 0, 0, time.UTC)
	c.syncSizes(ctx, night)
	reqs := g.take()
	if len(reqs) != len(dirs) {
		t.Fatalf("%d requests for %d folders", len(reqs), len(dirs))
	}
	for _, r := range reqs {
		if !r.measure {
			t.Errorf("no measurement at night: %+v", r)
		}
	}
	// The gate measures now: syncwatch reads the state again, without a new request.
	c.syncSizes(ctx, night.Add(time.Minute))
	for _, r := range g.take() {
		if r.measure {
			t.Errorf("measured twice in one night: %+v", r)
		}
	}
	if reqs := g.take(); len(reqs) != 0 {
		t.Errorf("unexpected: %+v", reqs)
	}
	c.syncSizes(ctx, night.Add(2*time.Hour)) // after the window
	for _, r := range g.take() {
		if r.measure {
			t.Errorf("a measurement after the window: %+v", r)
		}
	}
	c.syncSizes(ctx, night.Add(24*time.Hour)) // the next night
	n := 0
	for _, r := range g.take() {
		if r.measure {
			n++
		}
	}
	if n != len(dirs) {
		t.Errorf("the next night: %d measurements, want %d", n, len(dirs))
	}

	if k := nightKey(c.cfg.Get(), time.Date(2026, 10, 15, 1, 59, 0, 0, time.UTC)); k != "" {
		t.Errorf("before size_time: %q", k)
	}
	cfg := c.cfg.Get()
	cfg.Collector.SizeTime = "off"
	if k := nightKey(cfg, night); k != "" {
		t.Errorf("size_time off: %q", k)
	}
}

// A server without a gate (Syncthing answers 404) measures no sizes: one
// request in a session, then none.
func TestSizesWithoutGate(t *testing.T) {
	c, g, _, ctx := startSizes(t)
	g.mu.Lock()
	g.none = true
	g.mu.Unlock()
	now := time.Date(2026, 10, 15, 2, 10, 0, 0, time.UTC)
	c.syncSizes(ctx, now)
	if reqs := g.take(); len(reqs) != 1 {
		t.Fatalf("%d requests, want 1: %+v", len(reqs), reqs)
	}
	if sizes, none := sizesOf(c); !none || sizes != nil {
		t.Fatalf("sizes %v, none %v", sizes, none)
	}
	c.syncSizes(ctx, now.Add(2*time.Hour))
	if reqs := g.take(); len(reqs) != 0 {
		t.Errorf("asked again in the same session: %+v", reqs)
	}
}

// Measure on the Folders page: only for a project folder outside Syncthing
// of the server, and only while the server is up.
func TestMeasureNow(t *testing.T) {
	c, g, dirs, ctx := startSizes(t)
	if err := c.MeasureNow(dirs[0]); !errors.Is(err, ErrSizeNotUp) {
		t.Fatalf("without a session: %v", err)
	}
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c.scanMu.Lock()
	c.sessCtx = sctx
	c.scanMu.Unlock()
	if err := c.MeasureNow(dirs[0]); err != nil {
		t.Fatal(err)
	}
	if reqs := g.take(); len(reqs) != 1 || reqs[0] != (sizeReq{dirs[0], true}) {
		t.Fatalf("requests %+v", reqs)
	}
	if sizes, _ := sizesOf(c); sizes[dirs[0]] == nil || sizes[dirs[0]].State != stclient.SizeQueued {
		t.Errorf("state after Measure: %+v", sizes[dirs[0]])
	}
	if err := c.MeasureNow(`D:\elsewhere\LIVE-X`); !errors.Is(err, ErrSizeNotOnDisk) {
		t.Errorf("another directory: %v", err)
	}
	if reqs := g.take(); len(reqs) != 0 {
		t.Errorf("asked the gate for another directory: %+v", reqs)
	}
}
