package engine

import (
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
)

// H2 opens at once for a link that dropped some time ago, but a link that
// could form only now (a server is reachable again, or Syncthing started)
// gets some time: Syncthing needs some seconds to connect again.
func TestLinkGraceAfterStart(t *testing.T) {
	r := newRig(t)
	r.hub.Update("a", func(s *model.Server) {
		s.Devices["B-ID"] = &model.Device{ID: "B-ID", Name: "B", LastSeen: r.now.Add(-time.Hour)}
	})
	r.hub.Update("b", func(s *model.Server) {
		s.Devices["A-ID"] = &model.Device{ID: "A-ID", Name: "A", LastSeen: r.now.Add(-time.Hour)}
	})
	if find(r.at(0), "H2", "A ↔ B") == nil {
		t.Fatal("H2 should open at once for a link that dropped an hour ago")
	}
	link := func(connected bool) {
		r.hub.Update("a", func(s *model.Server) { s.Connections["B-ID"] = &model.Connection{Connected: connected} })
	}
	link(true)
	if find(r.at(time.Minute), "H2", "A ↔ B") != nil {
		t.Fatal("H2 should resolve")
	}
	// B stops (H1); the link drops while B is down.
	r.hub.Update("b", func(s *model.Server) { s.Status, s.DownSince, s.LastError = model.StatusDown, r.now, "timeout" })
	r.at(time.Second)
	link(false)
	r.at(10 * time.Minute)
	// B is reachable again; Syncthing connects the servers some seconds later.
	r.hub.Update("b", func(s *model.Server) { s.Status = model.StatusUp })
	if v := r.at(time.Second); find(v, "H2", "A ↔ B") != nil || find(v, "H1", "B") != nil {
		t.Fatal("H2 opened in the evaluation in which H1 resolved")
	}
	link(true)
	if find(r.at(5*time.Second), "H2", "A ↔ B") != nil {
		t.Fatal("H2 for a link that formed again")
	}
	// Syncthing on A starts again and the link does not form.
	link(false)
	r.hub.Update("a", func(s *model.Server) { s.StartTime = r.now })
	if find(r.at(time.Minute), "H2", "A ↔ B") != nil {
		t.Fatal("H2 opened in the first minute after a start of Syncthing")
	}
	if find(r.at(70*time.Second), "H2", "A ↔ B") == nil {
		t.Fatal("H2 should open after the grace")
	}
}
