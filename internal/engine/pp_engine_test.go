package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
)

// Tests of S1 inside a share root and of the absence findings at the start.

// S2 lists the Syncthing folders inside a share root in
// place of their S1 findings. While S2 is ignored or snoozed, or not urgent
// while S1 is urgent, each folder inside has its own S1 again (as in older
// versions): a folder that Syncthing syncs 2 times always has a finding that a
// person sees, with the urgency of S1. Also a folder that somebody adds
// later. When S2 reports the folders again, their S1 findings close without
// a resolved message: the fault continues.
func TestShareRootHiddenGivesS1(t *testing.T) {
	const root = "/share/AKKA-LIVE-4FINALS"
	folder := func(r *rig, id, label, path string) {
		r.hub.Update("a", func(s *model.Server) {
			s.Folders[id] = &model.Folder{ID: id, Label: label, Path: path, State: "idle", Completion: map[string]*model.Completion{}}
		})
	}
	visibleUrgent := func(t *testing.T, r *rig, v *View, part string) *View {
		t.Helper()
		s1 := find(v, "S1", part)
		if s1 == nil {
			t.Fatalf("no S1 %q", part)
		}
		if _, hidden := v.Snoozed(s1.ID, r.now); hidden || !s1.Urgent || s1.Stale {
			t.Fatalf("S1 %q: hidden=%v urgent=%v stale=%v", part, hidden, s1.Urgent, s1.Stale)
		}
		r.eng.MarkPinged([]string{s1.ID}, r.now) // the urgent message went out
		return v
	}
	for _, mode := range []string{"ignore", "snooze", "not urgent"} {
		t.Run(mode, func(t *testing.T) {
			r := newRig(t)
			if mode == "not urgent" {
				r.withConfig(t, `
servers: [{name: A, url: "https://a:1"}, {name: B, url: "https://b:1"}]
checks: {S2: {urgent: false}}
`)
			}
			folder(r, "root", "AKKA-LIVE-4FINALS", root)
			folder(r, "fjord", "LIVE-FJORD-4FINALS", root+"/LIVE-FJORD-4FINALS")
			v := r.at(0)
			s2 := find(v, "S2", "AKKA-LIVE-4FINALS on A")
			if s2 == nil || s2.Urgent != (mode != "not urgent") {
				t.Fatalf("S2: %+v", s2)
			}
			switch mode {
			case "ignore":
				if err := r.eng.Ignore(s2.ID, "The share is synced as a whole on purpose", "admin"); err != nil {
					t.Fatal(err)
				}
			case "snooze":
				if err := r.eng.Snooze(s2.ID, r.now.Add(7*24*time.Hour), "VPN work", "admin"); err != nil {
					t.Fatal(err)
				}
			}
			visibleUrgent(t, r, r.at(time.Minute), "LIVE-FJORD-4FINALS inside AKKA-LIVE-4FINALS on A")
			// Somebody adds PROJ-B inside the share root as its own folder.
			folder(r, "projb", "PROJ-B", root+"/PROJ-B")
			visibleUrgent(t, r, r.at(time.Minute), "PROJ-B inside AKKA-LIVE-4FINALS on A")

			// S2 reports the folders again.
			switch mode {
			case "ignore":
				if err := r.eng.Unsnooze(s2.ID); err != nil {
					t.Fatal(err)
				}
			case "snooze":
				r.now = r.now.Add(7 * 24 * time.Hour) // the snooze ends
			case "not urgent":
				c := r.cfg.Get().Clone()
				delete(c.Checks, "S2")
				if err := r.cfg.Set(c); err != nil {
					t.Fatal(err)
				}
			}
			v = r.at(time.Minute)
			if s1 := find(v, "S1", "inside AKKA-LIVE-4FINALS"); s1 != nil {
				t.Fatalf("S1 still open: %s", s1.Message)
			}
			if s2 = find(v, "S2", "AKKA-LIVE-4FINALS on A"); s2 == nil || !strings.Contains(s2.Message, "It contains 2 other Syncthing folders") {
				t.Fatalf("S2: %+v", s2)
			}
			if pend, _ := r.st.PendingResolved(); len(pend) != 0 {
				t.Fatalf("a resolved message is due for %d findings, first %q: the folders are still inside", len(pend), pend[0].Message)
			}
			hist, _ := r.st.HistoryBetween(r.now.Add(-30*24*time.Hour), r.now.Add(time.Second))
			for _, h := range hist {
				if h.Check == "S1" && (h.ResolvedAt.IsZero() || !strings.HasSuffix(h.Message, "(listed in S2 AKKA-LIVE-4FINALS on A)")) {
					t.Errorf("history of S1: %q resolved %v", h.Message, h.ResolvedAt)
				}
			}
		})
	}
}

// LAPTOP-DANI always uses a relay, and an administrator pauses
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

// After a start of syncwatch, the servers finish their first
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
