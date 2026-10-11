package engine

import (
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
)

// MarkRepeated records the time of a repeat of the urgent message on an
// open finding, keeps it across a restart and a new message text, and
// skips a finding that is not open.
func TestMarkRepeated(t *testing.T) {
	r := newRig(t)
	r.hub.Update("b", func(s *model.Server) {
		s.Status, s.DownSince, s.LastError = model.StatusDown, r.now, "connection refused"
	})
	h1 := find(r.at(0), "H1", "B")
	if h1 == nil {
		t.Fatal("H1 missing")
	}
	pinged := r.now
	r.eng.MarkPinged([]string{h1.ID}, pinged)
	repeated := r.now.Add(2 * time.Hour)
	r.eng.MarkRepeated([]string{h1.ID, "no-such-finding"}, repeated)
	if got := find(r.eng.View(), "H1", "B"); got == nil || !got.RepeatedAt.Equal(repeated) || !got.PingedAt.Equal(pinged) {
		t.Fatalf("after the mark: %+v", got)
	}
	// Another error text changes the message: the times stay.
	r.hub.Update("b", func(s *model.Server) { s.LastError = "timed out" })
	r.at(time.Minute)
	r.restart(t)
	got := find(r.at(time.Minute), "H1", "B")
	if got == nil || !got.RepeatedAt.Equal(repeated) || !got.PingedAt.Equal(pinged) {
		t.Fatalf("after a new message and a restart: %+v", got)
	}

	// Resolved: a mark changes nothing.
	r.hub.Update("b", func(s *model.Server) { s.Status, s.LastError = model.StatusUp, "" })
	if find(r.at(time.Minute), "H1", "B") != nil {
		t.Fatal("H1 still open")
	}
	r.eng.MarkRepeated([]string{h1.ID}, repeated.Add(time.Hour))
	rec, err := r.st.Record(h1.ID)
	if err != nil || rec == nil || rec.Open() || !rec.RepeatedAt.Equal(repeated) {
		t.Fatalf("resolved record: %+v %v", rec, err)
	}
}
