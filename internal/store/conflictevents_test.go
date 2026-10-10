package store

import (
	"testing"
	"time"
)

// Conflicts recorded from events count together with the increases of the
// conflict counters, in the same window, and leave the counters alone.
func TestAddConflicts(t *testing.T) {
	s := testStore(t)
	t0 := time.Now()
	_ = s.RecordConflicts("a", "s1", map[string]int64{"f": 5}, t0) // baseline
	if err := s.AddConflicts("a", "f", 1, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	_ = s.AddConflicts("a", "f", 2, t0.Add(2*time.Minute))
	_ = s.AddConflicts("a", "g", 0, t0.Add(2*time.Minute)) // nothing to record
	_ = s.AddConflicts("b", "f", 1, t0.Add(-time.Hour))    // before the window
	_ = s.RecordConflicts("a", "s1", map[string]int64{"f": 6}, t0.Add(3*time.Minute))
	got, err := s.ConflictsSince(t0.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if got["a"]["f"] != 4 || len(got["a"]) != 1 || len(got["b"]) != 0 {
		t.Fatalf("got %v, want a/f 4 (1 from the counter, 3 from events) and nothing else", got)
	}
	if err := s.PruneConflicts(t0.Add(90 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ConflictsSince(t0.Add(-2 * time.Hour)); got["a"]["f"] != 3 {
		t.Fatalf("after the prune: %v, want a/f 3", got)
	}
}
