package engine

import (
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
)

// A finding that flaps back before its "resolved" message went out carries
// on as the same, announced problem: it keeps PingedAt and also the time of
// its last repeat. Without RepeatedAt, the notifier
// counts the repeat interval from the old urgent message and repeats the
// urgent message again at once after the re-open.
func TestReopenKeepsTheTimeOfTheLastRepeat(t *testing.T) {
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
	repeated := r.now.Add(3 * time.Hour)
	r.eng.MarkRepeated([]string{h1.ID}, repeated)

	// B is back for a moment: H1 resolves, its "resolved" message is not
	// sent yet.
	r.hub.Update("b", func(s *model.Server) { s.Status, s.LastError = model.StatusUp, "" })
	if find(r.at(3*time.Hour+time.Minute), "H1", "B") != nil {
		t.Fatal("H1 should resolve")
	}
	pend, _ := r.st.PendingResolved()
	if len(pend) != 1 {
		t.Fatalf("pending resolved: %d", len(pend))
	}
	// B goes down again before that message goes out.
	r.hub.Update("b", func(s *model.Server) {
		s.Status, s.DownSince, s.LastError = model.StatusDown, r.now, "connection refused"
	})
	again := find(r.at(time.Second), "H1", "B")
	if again == nil || !again.PingedAt.Equal(pinged) {
		t.Fatalf("H1 should be open again with the old PingedAt: %+v", again)
	}
	if !again.RepeatedAt.Equal(repeated) {
		t.Fatalf("the re-opened H1 lost the time of its last repeat: RepeatedAt %v, want %v", again.RepeatedAt, repeated)
	}
	rec, err := r.st.Record(h1.ID)
	if err != nil || rec == nil || !rec.Open() || !rec.RepeatedAt.Equal(repeated) {
		t.Fatalf("the stored record must keep RepeatedAt: %+v %v", rec, err)
	}
	// The "resolved" message of the first occurrence goes out after the
	// re-open: the new occurrence is unannounced, without the old times.
	r.eng.MarkResolvedNotified([]string{pend[0].ID}, r.now)
	if got := find(r.eng.View(), "H1", "B"); got == nil || !got.PingedAt.IsZero() || !got.RepeatedAt.IsZero() {
		t.Fatalf("the new occurrence must be unannounced: %+v", got)
	}
}
