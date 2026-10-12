package store

import (
	"testing"
	"time"
)

func TestAliveAt(t *testing.T) {
	s := testStore(t)
	if at, err := s.AliveAt(); err != nil || !at.IsZero() {
		t.Fatalf("never written: %v %v", at, err)
	}
	t0 := time.Date(2026, 10, 11, 21, 4, 5, 123_000_000, time.UTC)
	if err := s.SetAliveAt(t0); err != nil {
		t.Fatal(err)
	}
	if at, err := s.AliveAt(); err != nil || !at.Equal(t0) {
		t.Fatalf("got %v %v, want %v", at, err, t0)
	}
}

// Gaps are listed oldest first, from a time on, and marked as reported up to
// a time. The list keeps the gaps of 30 days, at most 50.
func TestGaps(t *testing.T) {
	s := testStore(t)
	if g, err := s.Gaps(time.Time{}); err != nil || len(g) != 0 {
		t.Fatalf("no gaps: %+v %v", g, err)
	}
	t0 := time.Date(2026, 10, 11, 21, 4, 0, 0, time.UTC)
	recent := Gap{From: t0, To: t0.Add(20 * time.Minute)}
	older := Gap{From: t0.Add(-48 * time.Hour), To: t0.Add(-47 * time.Hour)}
	for _, g := range []Gap{recent, older} {
		if err := s.AddGap(g); err != nil {
			t.Fatal(err)
		}
	}
	same := func(got, want []Gap) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if !got[i].From.Equal(want[i].From) || !got[i].To.Equal(want[i].To) || got[i].Reported != want[i].Reported {
				return false
			}
		}
		return true
	}
	if got, _ := s.Gaps(time.Time{}); !same(got, []Gap{older, recent}) {
		t.Fatalf("all gaps: %+v", got)
	}
	if got, _ := s.Gaps(t0.Add(20 * time.Minute)); !same(got, []Gap{recent}) {
		t.Fatalf("gaps that ended at or after the end of the recent one: %+v", got)
	}
	if err := s.MarkGapsReported(t0.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	older.Reported = true
	if got, _ := s.Gaps(time.Time{}); !same(got, []Gap{older, recent}) {
		t.Fatalf("after the first mark: %+v", got)
	}
	if err := s.MarkGapsReported(recent.To); err != nil {
		t.Fatal(err)
	}
	recent.Reported = true
	if got, _ := s.Gaps(time.Time{}); !same(got, []Gap{older, recent}) {
		t.Fatalf("after the second mark: %+v", got)
	}

	// A gap that ended more than 30 days before the newest gap goes.
	if err := s.AddGap(Gap{From: t0.Add(-31*24*time.Hour - time.Hour), To: t0.Add(-31 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Gaps(time.Time{}); !same(got, []Gap{older, recent}) {
		t.Fatalf("a gap of 31 days before is kept: %+v", got)
	}
	later := Gap{From: t0.Add(31 * 24 * time.Hour), To: t0.Add(31*24*time.Hour + 10*time.Minute)}
	if err := s.AddGap(later); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Gaps(time.Time{}); !same(got, []Gap{later}) {
		t.Fatalf("after a gap 31 days later: %+v", got)
	}

	// At most 50 gaps: the oldest go.
	s2 := testStore(t)
	for i := 0; i < 55; i++ {
		from := t0.Add(time.Duration(i) * time.Hour)
		if err := s2.AddGap(Gap{From: from, To: from.Add(10 * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := s2.Gaps(time.Time{})
	if len(got) != 50 || !got[0].From.Equal(t0.Add(5*time.Hour)) || !got[49].From.Equal(t0.Add(54*time.Hour)) {
		t.Fatalf("%d gaps, first %v, last %v", len(got), got[0].From, got[len(got)-1].From)
	}
}

// A gap from the same heartbeat replaces the gap that was recorded before:
// its end was corrected (a clock that was behind at the start). A message
// did not name the corrected gap yet.
func TestGapFromTheSameHeartbeatReplaces(t *testing.T) {
	s := testStore(t)
	friday := time.Date(2026, 10, 9, 18, 0, 0, 0, time.UTC)
	older := Gap{From: friday.Add(-48 * time.Hour), To: friday.Add(-47 * time.Hour)}
	short := Gap{From: friday, To: friday.Add(3 * time.Minute)}
	for _, g := range []Gap{older, short} {
		if err := s.AddGap(g); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.MarkGapsReported(short.To); err != nil {
		t.Fatal(err)
	}
	full := Gap{From: friday, To: friday.Add(62 * time.Hour)}
	if err := s.AddGap(full); err != nil {
		t.Fatal(err)
	}
	got, err := s.Gaps(time.Time{})
	if err != nil || len(got) != 2 || !got[0].To.Equal(older.To) || !got[0].Reported ||
		!got[1].From.Equal(full.From) || !got[1].To.Equal(full.To) || got[1].Reported {
		t.Fatalf("gaps %+v %v", got, err)
	}
}
