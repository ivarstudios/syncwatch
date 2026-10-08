package store

import (
	"testing"
	"time"
)

func TestTraffic(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 10, 4, 10, 0, 0, 0, time.UTC)
	rec := func(start string, in, out int64, at time.Time) TrafficSample {
		t.Helper()
		sm, err := s.RecordTraffic("akka", start, in, out, at)
		if err != nil {
			t.Fatal(err)
		}
		return sm
	}
	if sm := rec("s1", 1000, 500, t0); sm != (TrafficSample{}) {
		t.Fatalf("first reading must be a baseline: %+v", sm)
	}
	if sm := rec("s1", 61000, 2500, t0.Add(time.Minute)); sm.In != 60000 || sm.Out != 2000 || sm.Over != time.Minute {
		t.Fatalf("second reading: %+v", sm)
	}
	// Syncthing restarted: the counters start from zero again.
	if sm := rec("s2", 300, 100, t0.Add(2*time.Minute)); sm.In != 300 || sm.Out != 100 {
		t.Fatalf("after a restart: %+v", sm)
	}
	// A counter going down means a restart too, even with the same start time.
	if sm := rec("s2", 50, 150, t0.Add(3*time.Minute)); sm.In != 50 || sm.Out != 150 {
		t.Fatalf("counter went down: %+v", sm)
	}
	// Nothing new: no row, but the reading moves on.
	if sm := rec("s2", 50, 150, t0.Add(4*time.Minute)); sm.In != 0 || sm.Out != 0 || sm.Over != time.Minute {
		t.Fatalf("idle minute: %+v", sm)
	}
	_, _ = s.RecordTraffic("sol", "x", 0, 0, t0)
	_, _ = s.RecordTraffic("sol", "x", 7, 9, t0.Add(time.Hour))

	all, err := s.TrafficSince(t0)
	if err != nil {
		t.Fatal(err)
	}
	if got := all["akka"]; got.In != 60000+300+50 || got.Out != 2000+100+150 {
		t.Fatalf("akka since the start: %+v", got)
	}
	if got := all["sol"]; got.In != 7 || got.Out != 9 {
		t.Fatalf("sol: %+v", got)
	}
	recent, _ := s.TrafficSince(t0.Add(150 * time.Second))
	if got := recent["akka"]; got.In != 50 || got.Out != 150 {
		t.Fatalf("akka in the last minutes: %+v", got)
	}
	if err := s.PruneTraffic(t0.Add(150 * time.Second)); err != nil {
		t.Fatal(err)
	}
	all, _ = s.TrafficSince(t0)
	if got := all["akka"]; got.In != 50 {
		t.Fatalf("after pruning: %+v", got)
	}
}
