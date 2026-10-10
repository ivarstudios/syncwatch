package notify

import (
	"testing"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/model"
)

// Switching a check to urgent must not ping every old finding of it at
// once: the debounce counts from the moment the finding became urgent.
func TestDebounceCountsFromBecomingUrgent(t *testing.T) {
	r := newRig(t, local(2026, 10, 6, 10, 0), "AKKA", "SOL")
	r.hub.Update("akka", func(s *model.Server) {
		s.PendingDevices = []model.PendingDevice{{DeviceID: "NEW-ID", Name: "newpc", Time: r.now}}
	})
	r.at(r.now)
	r.at(local(2026, 10, 6, 12, 0))
	if m := r.sender.take(); len(m) != 0 {
		t.Fatalf("H9 is not urgent, got %s", m[0].Title)
	}
	c := r.n.cfg.Get().Clone()
	yes := true
	c.Checks = map[string]config.Check{"H9": {Urgent: &yes}}
	if err := r.n.cfg.Set(c); err != nil {
		t.Fatal(err)
	}
	r.at(local(2026, 10, 6, 12, 1))
	if m := r.sender.take(); len(m) != 0 {
		t.Fatalf("pinged within the debounce of becoming urgent: %s", m[0].Title)
	}
	r.at(local(2026, 10, 6, 12, 6))
	m := r.sender.take()
	if len(m) != 1 || m[0].Kind != KindUrgent {
		t.Fatalf("want one urgent message after the debounce, got %d", len(m))
	}
}
