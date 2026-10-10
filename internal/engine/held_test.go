package engine

import (
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
)

// pc sets what server A reports about the PC for folder LIVE-F: connected
// or not, its remote state and its completion.
func (r *rig) pc(connected bool, state string, percent float64) {
	r.hub.Update("a", func(s *model.Server) {
		f := s.Folders["f"]
		if f == nil {
			f = &model.Folder{ID: "f", Label: "LIVE-F", Path: "/s/X/LIVE-F", State: "idle", SharedWith: []string{"PC-ID"}, Completion: map[string]*model.Completion{}}
			s.Folders["f"] = f
		}
		var need int64
		if percent < 100 {
			need = 18
		}
		f.Completion["PC-ID"] = &model.Completion{Percent: percent, NeedItems: need, RemoteState: state}
		s.Devices["PC-ID"] = &model.Device{ID: "PC-ID", Name: "PC", LastSeen: r.now}
		s.Connections["PC-ID"] = &model.Connection{Connected: connected}
	})
}

// resolvedH5 counts the resolved H5 entries in the history.
func resolvedH5(t *testing.T, r *rig) int {
	t.Helper()
	hist, err := r.st.HistoryBetween(r.now.Add(-30*24*time.Hour), r.now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, h := range hist {
		if h.Check == "H5" && !h.ResolvedAt.IsZero() {
			n++
		}
	}
	return n
}

// H5 for a device stays open (stale) while the device is away, also for
// longer than stuck_sync, and is judged again from live data when it is back.
func TestStuckDeviceIsHeldWhileAway(t *testing.T) {
	r := newRig(t) // stuck_sync: 24h
	r.pc(true, "valid", 99.99)
	r.at(0)
	h5 := find(r.at(25*time.Hour), "H5", "LIVE-F on PC")
	if h5 == nil || h5.Stale {
		t.Fatalf("H5 should open after 24 hours stuck: %+v", h5)
	}
	// The PC is switched off; a full check reads "unknown" for it.
	r.pc(false, "unknown", 99.99)
	got := find(r.at(time.Second), "H5", "LIVE-F on PC")
	if got == nil || got.ID != h5.ID || !got.Stale {
		t.Fatalf("H5 must stay open and stale while the PC is off: %+v", got)
	}
	got = find(r.at(30*time.Hour), "H5", "LIVE-F on PC")
	if got == nil || !got.Stale || !got.OpenedAt.Equal(h5.OpenedAt) {
		t.Fatalf("H5 must stay open and stale past stuck_sync: %+v", got)
	}
	if n := resolvedH5(t, r); n != 0 {
		t.Fatalf("the disconnect wrote %d resolved history entries", n)
	}
	// Back, still stuck: the same finding, live again.
	r.pc(true, "valid", 99.99)
	got = find(r.at(time.Minute), "H5", "LIVE-F on PC")
	if got == nil || got.Stale || !got.OpenedAt.Equal(h5.OpenedAt) {
		t.Fatalf("H5 should continue as the same live finding: %+v", got)
	}
	// Synced: it resolves.
	r.pc(true, "valid", 100)
	if find(r.at(time.Minute), "H5", "LIVE-F on PC") != nil {
		t.Fatal("H5 should resolve at 100 percent")
	}
	if n := resolvedH5(t, r); n != 1 {
		t.Fatalf("expected 1 resolved history entry, got %d", n)
	}
}

// Only connected time counts: a PC that cannot take a file and is on 8
// hours a day gets H5 after 3 days, and the nights do not count.
func TestStuckDeviceCountsConnectedTime(t *testing.T) {
	r := newRig(t) // stuck_sync: 24h
	day := func(hours time.Duration) {
		r.pc(true, "valid", 99.5)
		r.at(0)
		r.at(hours)
		r.pc(false, "unknown", 99.5)
		r.at(0)
		for i := 0; i < 16; i++ {
			r.at(time.Hour)
		}
	}
	day(8 * time.Hour)
	day(8 * time.Hour)
	if v := r.at(0); find(v, "H5", "LIVE-F on PC") != nil {
		t.Fatal("H5 opened after 16 hours of connected time")
	}
	r.pc(true, "valid", 99.5)
	r.at(0)
	if find(r.at(7*time.Hour), "H5", "LIVE-F on PC") != nil {
		t.Fatal("H5 opened after 23 hours of connected time")
	}
	if find(r.at(time.Hour+time.Minute), "H5", "LIVE-F on PC") == nil {
		t.Fatal("H5 should open after more than 24 hours of connected time")
	}
}

// A PC switched off in the middle of a sync does not get H5 when it is
// back after the weekend and catches up in some minutes.
func TestStuckDeviceOffForTheWeekendCatchesUp(t *testing.T) {
	r := newRig(t)
	r.pc(true, "valid", 99)
	r.at(0)
	r.at(time.Hour)
	r.pc(false, "unknown", 99)
	r.at(0)
	if find(r.at(60*time.Hour), "H5", "LIVE-F on PC") != nil {
		t.Fatal("H5 opened while the PC was off")
	}
	r.pc(true, "valid", 99)
	if find(r.at(time.Second), "H5", "LIVE-F on PC") != nil {
		t.Fatal("H5 opened when the PC came back: the time off counted as stuck")
	}
	r.pc(true, "valid", 100)
	if find(r.at(5*time.Minute), "H5", "LIVE-F on PC") != nil {
		t.Fatal("H5 after the PC caught up")
	}
	if obs, _ := r.st.Observations(); len(obs) != 0 {
		t.Fatalf("the observation should end when the PC is synced: %v", obs)
	}
}

// A restart of the server, or of syncwatch, while the PC is still away
// keeps an open H5 as it is.
func TestStuckDeviceHeldThroughRestarts(t *testing.T) {
	r := newRig(t)
	r.pc(true, "valid", 99)
	r.at(0)
	h5 := find(r.at(25*time.Hour), "H5", "LIVE-F on PC")
	if h5 == nil {
		t.Fatal("H5 missing")
	}
	r.pc(false, "unknown", 99)
	r.at(time.Second)
	// The server stops and starts again; the PC is not connected yet.
	r.hub.Update("a", func(s *model.Server) { s.Status, s.DownSince = model.StatusDown, r.now })
	if got := find(r.at(time.Minute), "H5", "LIVE-F on PC"); got == nil || !got.Stale {
		t.Fatalf("H5 must stay open and stale while the server is down: %+v", got)
	}
	r.hub.Update("a", func(s *model.Server) { s.Status = model.StatusUp })
	if got := find(r.at(time.Minute), "H5", "LIVE-F on PC"); got == nil || !got.Stale || !got.OpenedAt.Equal(h5.OpenedAt) {
		t.Fatalf("H5 must stay held after the server restart: %+v", got)
	}
	// syncwatch restarts: the last connected view is lost, and the PC is
	// still away. What Syncthing reports for it now keeps the finding.
	r.freshHub(t)
	r.at(time.Second)
	r.hub.Update("a", func(s *model.Server) { up(s, "A-ID") })
	r.pc(false, "unknown", 99)
	if got := find(r.at(time.Minute), "H5", "LIVE-F on PC"); got == nil || !got.Stale || !got.OpenedAt.Equal(h5.OpenedAt) {
		t.Fatalf("H5 must stay held after a restart of syncwatch: %+v", got)
	}
	r.pc(true, "valid", 99)
	if got := find(r.at(time.Minute), "H5", "LIVE-F on PC"); got == nil || got.Stale || !got.OpenedAt.Equal(h5.OpenedAt) {
		t.Fatalf("H5 should continue as the same live finding: %+v", got)
	}
	if n := resolvedH5(t, r); n != 0 {
		t.Fatalf("the restarts wrote %d resolved history entries", n)
	}
}
