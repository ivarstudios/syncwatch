package engine

import (
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
)

// Tests of the pre-publication review (pp-rules).

// data-5 (a): LAPTOP-DANI always uses a relay, and an administrator pauses
// it on A and B (a holiday). It has no connection because of the pause: no
// H12 with the causes "The relay is off or unreachable, or the device is
// off". H6 reports the pause after thresholds.paused (3 days).
func TestPausedRelayDeviceGetsNoH12(t *testing.T) {
	r := newRig(t)
	r.withConfig(t, `
servers: [{name: A, url: "https://a:1"}, {name: B, url: "https://b:1"}]
devices: [{id: `+laptopID+`, relay: true}]
`)
	paused := r.now
	for _, id := range []string{"a", "b"} {
		r.hub.Update(id, func(s *model.Server) {
			// Syncthing closes the connection at the pause.
			s.Devices[laptopID] = &model.Device{ID: laptopID, Name: "LAPTOP-DANI", Paused: true, LastSeen: paused}
			s.Connections[laptopID] = &model.Connection{Connected: false, Paused: true}
		})
	}
	for _, d := range []time.Duration{0, 31 * time.Minute, 24 * time.Hour, 2*24*time.Hour + 30*time.Minute} {
		if h12 := find(r.at(d), "H12", "LAPTOP-DANI"); h12 != nil {
			t.Fatalf("+%v: H12 for a device that is paused on every server: %q", r.now.Sub(paused), h12.Message)
		}
	}
	v := r.at(time.Hour) // 3 days and 1 minute
	if find(v, "H6", "LAPTOP-DANI is paused on A") == nil || find(v, "H6", "LAPTOP-DANI is paused on B") == nil || find(v, "H12", "LAPTOP-DANI") != nil {
		t.Fatalf("after thresholds.paused: want H6 on A and B and no H12, got %d findings", len(v.Open))
	}
	// B resumes the device, and the device does not connect: H12.
	r.hub.Update("b", func(s *model.Server) { s.Devices[laptopID].Paused, s.Connections[laptopID].Paused = false, false })
	if find(r.at(time.Minute), "H12", "LAPTOP-DANI") == nil {
		t.Fatal("no H12 for a device that B does not pause and that is not connected")
	}
}

// data-5 (b): after a start of syncwatch, the servers finish their first
// full check one after the other. Until a server finished one, its devices
// are not known, and it can have a device connected. H7, C2 and H12 of a
// device wait for each server, as they wait for a server that is down.
func TestAbsenceWaitsForEveryServerAtStart(t *testing.T) {
	const (
		bjorn = "WSBJORN-AAAAAAA-BBBBBBB-CCCCCCC-DDDDDDD-EEEEEEE-FFFFFFF-GGGGGGG"
		gone  = "WSGONEE-AAAAAAA-BBBBBBB-CCCCCCC-DDDDDDD-EEEEEEE-FFFFFFF-GGGGGGG"
	)
	r := newRig(t)
	r.withConfig(t, `
servers: [{name: A, url: "https://a:1"}, {name: B, url: "https://b:1"}]
devices: [{id: `+laptopID+`, relay: true}]
`)
	start := r.now
	// load is the end of the first full check of a server, as the collector
	// does it: one update with the data, then the status up. The laptop
	// (relay) and WS-BJORN sync only with B. WS-GONE is offline for 8 days.
	load := func(id, myID string) {
		r.hub.Update(id, func(s *model.Server) {
			s.MyID, s.Loaded = myID, true
			for dev, name := range map[string]string{laptopID: "LAPTOP-DANI", bjorn: "WS-BJORN"} {
				s.Devices[dev] = &model.Device{ID: dev, Name: name, LastSeen: start.Add(-8 * 24 * time.Hour)}
				if id == "b" {
					s.Devices[dev].LastSeen = r.now
					s.Connections[dev] = &model.Connection{Connected: true, Type: "tcp-client", StartedAt: start.Add(-3 * time.Hour)}
					if dev == laptopID {
						s.Connections[dev].Type = "relay-client"
					}
				}
			}
			s.Devices[gone] = &model.Device{ID: gone, Name: "WS-GONE", LastSeen: start.Add(-8 * 24 * time.Hour)}
		})
		r.hub.Update(id, func(s *model.Server) { s.Status, s.LastContact = model.StatusUp, r.now })
	}
	restart := func() {
		r.hub = model.NewHub(func() time.Time { return r.now })
		r.hub.SetServers([]*model.Server{{ID: "a", Name: "A", MyID: "A-ID", PathSep: "/"}, {ID: "b", Name: "B", MyID: "B-ID", PathSep: "/"}})
		r.restart(t)
	}
	absent := func(v *View, label string) {
		t.Helper()
		for _, c := range []struct{ check, name string }{{"H12", "LAPTOP-DANI"}, {"H7", "LAPTOP-DANI"}, {"H7", "WS-BJORN"}, {"C2", "WS-BJORN"}} {
			if f := find(v, c.check, c.name); f != nil {
				t.Errorf("%s: %s %q, stale=%v", label, c.check, f.Message, f.Stale)
			}
		}
	}

	// A start: A finishes its full check first, B 20 seconds later.
	restart()
	load("a", "A-ID")
	v := r.at(time.Second)
	absent(v, "A loaded, B not")
	if find(v, "H7", "WS-GONE") != nil {
		t.Error("H7 of WS-GONE opened before B finished its full check")
	}
	r.now = r.now.Add(19 * time.Second)
	load("b", "B-ID")
	v = r.at(time.Second)
	absent(v, "both loaded")
	if g := find(v, "H7", "WS-GONE"); g == nil || g.Stale {
		t.Fatalf("H7 of WS-GONE should open when both servers finished a full check: %+v", g)
	}
	hist, err := r.st.HistoryBetween(start, r.now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hist {
		if h.Subject != "WS-GONE" {
			t.Errorf("history entry %s %q (opened %s, resolved %s)", h.Check, h.Message, h.OpenedAt.Format("15:04:05"), h.ResolvedAt.Format("15:04:05"))
		}
	}

	// The next start: B cannot be reached (the VPN is down), and it never
	// finishes a full check. The open H7 of WS-GONE stays as it is (stale).
	r.at(time.Hour)
	restart()
	load("a", "A-ID")
	r.hub.Update("b", func(s *model.Server) { s.Status, s.DownSince, s.LastError = model.StatusDown, r.now, "timeout" })
	for _, d := range []time.Duration{time.Second, time.Hour} {
		v = r.at(d)
		absent(v, "B down since the start")
		if g := find(v, "H7", "WS-GONE"); g == nil || !g.Stale {
			t.Fatalf("H7 of WS-GONE should stay open and stale while B is down: %+v", g)
		}
	}
	load("b", "B-ID")
	v = r.at(time.Second)
	absent(v, "B back")
	if g := find(v, "H7", "WS-GONE"); g == nil || g.Stale {
		t.Fatalf("H7 of WS-GONE should be open and not stale when B is back: %+v", g)
	}
}
