package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/model"
)

// withProjects gives the rig a project pattern, so S3–S7 are evaluated.
func (r *rig) withProjects(t *testing.T) {
	t.Helper()
	c, _ := config.ParseYAML([]byte(`
servers: [{name: A, url: "https://a:1"}, {name: B, url: "https://b:1"}]
structure: {project_pattern: '^LIVE-'}
`))
	r.cfg, _ = config.NewHolder(c)
	r.restart(t)
}

// freshHub simulates a process restart: a new hub whose servers start as
// "unknown", like the collector manager publishes them.
func (r *rig) freshHub(t *testing.T) {
	t.Helper()
	r.hub = model.NewHub(func() time.Time { return r.now })
	r.hub.SetServers([]*model.Server{{ID: "a", Name: "A", PathSep: "/"}, {ID: "b", Name: "B", PathSep: "/"}})
	r.restart(t)
}

func up(s *model.Server, myID string) {
	s.Status, s.Loaded, s.MyID = model.StatusUp, true, myID
}

// scanned is a finished structure scan with one share.
func scanned(at time.Time, share *model.Share, nested ...*model.NestedDir) *model.Structure {
	st := &model.Structure{ScannedAt: at, Shares: []*model.Share{share}, Nested: map[string]*model.NestedDir{}}
	for _, n := range nested {
		st.Nested[n.Path] = n
	}
	return st
}

// A restart makes every server "unknown" until its first session. That
// says nothing about the server, so its findings, H1/H3 included, must
// wait instead of resolving (and sending "Resolved") on the first
// evaluation.
func TestUnknownStatusFreezesReachabilityFindings(t *testing.T) {
	for _, tc := range []struct {
		check  string
		status model.ServerStatus
	}{{"H1", model.StatusDown}, {"H3", model.StatusCertChanged}} {
		t.Run(tc.check, func(t *testing.T) {
			r := newRig(t)
			r.hub.Update("b", func(s *model.Server) {
				s.Status, s.DownSince, s.LastError = tc.status, r.now, "refused"
				s.CertChange = &model.CertChange{Old: "aa", New: "bb", SeenAt: r.now}
				s.Folders["p"] = &model.Folder{ID: "p", Label: "LIVE-P", Path: "/s/X/LIVE-P", Paused: true, State: "paused", Completion: map[string]*model.Completion{}}
			})
			r.at(0)
			v := r.at(2 * time.Hour) // paused: 1h, but B is already stale: no H6
			rec := find(v, tc.check, "B")
			if rec == nil || find(v, "H6", "LIVE-P") != nil {
				t.Fatalf("expected only %s, got %d findings", tc.check, len(v.Open))
			}
			r.eng.MarkPinged([]string{rec.ID}, r.now)

			r.freshHub(t)
			v = r.at(time.Second)
			got := find(v, tc.check, "B")
			if got == nil || !got.Stale {
				t.Fatalf("%s must stay open and stale while B is unknown: %+v", tc.check, got)
			}
			if pend, _ := r.st.PendingResolved(); len(pend) != 0 {
				t.Fatal("a 'resolved' message is due after the restart")
			}
			// B is found in the same state: the record carries on as before.
			r.hub.Update("b", func(s *model.Server) {
				s.Status, s.DownSince, s.LastError = tc.status, r.now, "refused"
				s.CertChange = &model.CertChange{Old: "aa", New: "bb", SeenAt: r.now}
			})
			v = r.at(time.Minute)
			got = find(v, tc.check, "B")
			if got == nil || got.Stale || got.PingedAt.IsZero() || !got.OpenedAt.Equal(rec.OpenedAt) {
				t.Fatalf("%s should continue as the same finding: %+v", tc.check, got)
			}
			// B is back: now it resolves.
			r.hub.Update("b", func(s *model.Server) { up(s, "B-ID"); s.CertChange = nil })
			if v = r.at(time.Minute); find(v, tc.check, "B") != nil {
				t.Fatalf("%s should resolve once B is up", tc.check)
			}
			hist, _ := r.st.HistoryBetween(r.now.Add(-24*time.Hour), r.now.Add(time.Hour))
			if len(hist) != 1 || hist[0].ResolvedAt.IsZero() {
				t.Fatalf("expected one closed history entry, got %+v", hist)
			}
		})
	}
}

// After a restart the first full check makes a server "up" before the
// first structure scan has filled Structure. S3 and the directory checks
// have nothing to go on until then and must not resolve.
func TestStructureFindingsWaitForFirstScan(t *testing.T) {
	r := newRig(t)
	r.withProjects(t)
	share := &model.Share{Path: "/s/X", Name: "X", Dirs: []*model.Dir{{Name: "junk", Path: "/s/X/junk"}, {Name: "LIVE-F", Path: "/s/X/LIVE-F", FolderIDs: []string{"f"}}}}
	nested := &model.NestedDir{Path: "/s/X/LIVE-F/sub/LIVE-N", Parent: "/s/X/LIVE-F", Share: "/s/X", Source: "scan", FoundAt: r.now}
	r.hub.Update("a", func(s *model.Server) {
		s.Folders["f"] = &model.Folder{ID: "f", Label: "LIVE-F", Path: "/s/X/LIVE-F", State: "idle", Completion: map[string]*model.Completion{}}
		s.Structure = scanned(r.now, share, nested)
	})
	v := r.at(0)
	s3, s4 := find(v, "S3", "LIVE-N"), find(v, "S4", "junk")
	if s3 == nil || s4 == nil {
		t.Fatalf("expected S3 and S4, got %d findings", len(v.Open))
	}
	r.eng.MarkPinged([]string{s3.ID}, r.now)

	r.freshHub(t)
	r.at(time.Second) // A unknown: frozen
	r.hub.Update("a", func(s *model.Server) {
		up(s, "A-ID")
		s.Folders["f"] = &model.Folder{ID: "f", Label: "LIVE-F", Path: "/s/X/LIVE-F", State: "idle", Completion: map[string]*model.Completion{}}
	})
	v = r.at(time.Second)
	if g := find(v, "S3", "LIVE-N"); g == nil || !g.Stale {
		t.Fatalf("S3 must stay open and stale before the first scan: %+v", g)
	}
	if g := find(v, "S4", "junk"); g == nil || !g.Stale {
		t.Fatalf("S4 must stay open and stale before the first scan: %+v", g)
	}
	if pend, _ := r.st.PendingResolved(); len(pend) != 0 {
		t.Fatal("a 'resolved' message is due before the first scan")
	}
	// The scan finishes: the nested folder is gone, the bad name is not.
	r.hub.Update("a", func(s *model.Server) { s.Structure = scanned(r.now, share) })
	v = r.at(time.Minute)
	if find(v, "S3", "LIVE-N") != nil {
		t.Fatal("S3 should resolve after a scan that no longer finds it")
	}
	if g := find(v, "S4", "junk"); g == nil || g.Stale || !g.OpenedAt.Equal(s4.OpenedAt) {
		t.Fatalf("S4 should continue as the same finding: %+v", g)
	}
}

// A share whose listing failed has no directories in the scan result. The
// findings under it must wait for a listing that works, not resolve and
// re-open with every transient browse error.
func TestFailedShareListingFreezesItsFindings(t *testing.T) {
	r := newRig(t)
	r.withProjects(t)
	x := &model.Share{Path: "/s/X", Name: "X", Dirs: []*model.Dir{{Name: "junk", Path: "/s/X/junk"}}}
	y := &model.Share{Path: "/s/Y", Name: "Y", Dirs: []*model.Dir{{Name: "stuff", Path: "/s/Y/stuff"}}}
	structure := func(at time.Time, shares ...*model.Share) *model.Structure {
		return &model.Structure{ScannedAt: at, Shares: shares, Nested: map[string]*model.NestedDir{}}
	}
	r.hub.Update("a", func(s *model.Server) { s.Structure = structure(r.now, x, y) })
	v := r.at(0)
	if find(v, "S4", "junk") == nil || find(v, "S4", "stuff") == nil {
		t.Fatalf("expected S4 for both shares, got %d findings", len(v.Open))
	}
	// X times out on the next scan; Y lists fine and "stuff" was renamed.
	xFailed := &model.Share{Path: "/s/X", Name: "X", Error: "timed out"}
	yEmpty := &model.Share{Path: "/s/Y", Name: "Y"}
	r.hub.Update("a", func(s *model.Server) { s.Structure = structure(r.now.Add(time.Hour), xFailed, yEmpty) })
	v = r.at(time.Hour)
	if g := find(v, "S4", "junk"); g == nil || !g.Stale {
		t.Fatalf("the finding under the failed share must stay open and stale: %+v", g)
	}
	if find(v, "S4", "stuff") != nil {
		t.Fatal("the finding under the listed share should resolve")
	}
	// The next scan lists X again, and "junk" is gone.
	r.hub.Update("a", func(s *model.Server) {
		s.Structure = structure(r.now.Add(time.Hour), &model.Share{Path: "/s/X", Name: "X"}, yEmpty)
	})
	if v = r.at(time.Hour); len(v.Open) != 0 {
		t.Fatalf("expected everything resolved, got %+v", v.Open[0])
	}
}

// A cross-server finding about what the servers see (H8, X1) is judged on
// whichever related servers are reachable: one unreachable NAS must not
// blind these checks on the other. The absence of a PC (H7, C2) is
// different: the unreachable NAS can have the PC connected, so these wait
// while any server that has the PC configured is unreachable.
func TestCrossFindingsFreezeOnlyWhenEveryServerIsStale(t *testing.T) {
	r := newRig(t) // pc_offline: 7 days, old_device: 90 days
	for _, id := range []string{"a", "b"} {
		r.hub.Update(id, func(s *model.Server) {
			s.Version = "v2.1.5"
			s.Devices["PC-ID"] = &model.Device{ID: "PC-ID", Name: "MORTIS", LastSeen: r.now.Add(-10 * 24 * time.Hour)}
			s.Devices["NEW-ID"] = &model.Device{ID: "NEW-ID", Name: "LATEPC", LastSeen: r.now.Add(-5 * 24 * time.Hour)}
			s.Devices["OLD-ID"] = &model.Device{ID: "OLD-ID", Name: "OLDPC", LastSeen: r.now.Add(-100 * 24 * time.Hour)}
			s.Devices["V-ID"] = &model.Device{ID: "V-ID", Name: "VENUS", LastSeen: r.now}
			s.Folders["x-"+id] = &model.Folder{ID: "x-" + id, Label: "LIVE-SAME", Path: "/s/X/LIVE-SAME", State: "idle", Completion: map[string]*model.Completion{}}
		})
	}
	r.hub.Update("a", func(s *model.Server) {
		s.Connections["V-ID"] = &model.Connection{Connected: true, ClientVersion: "v2.0.9", StartedAt: r.now}
	})
	live := func(v *View, check, part string, stale bool) {
		t.Helper()
		if g := find(v, check, part); g == nil || g.Stale != stale {
			t.Fatalf("%s %s: want open with stale=%v, got %+v", check, part, stale, g)
		}
	}
	v := r.at(0)
	live(v, "H7", "MORTIS", false)
	live(v, "C2", "OLDPC", false)
	live(v, "H8", "VENUS", false)
	live(v, "X1", "LIVE-SAME", false)
	// B goes down. A still sees VENUS and the folders: H8 and X1 carry on.
	// B can have the PCs connected: H7 and C2 stay as they are, and no new
	// H7 opens for LATEPC, which is offline for 8 days now.
	r.hub.Update("b", func(s *model.Server) { s.Status, s.DownSince = model.StatusDown, r.now })
	v = r.at(3 * 24 * time.Hour)
	live(v, "H8", "VENUS", false)
	live(v, "X1", "LIVE-SAME", false)
	live(v, "H7", "MORTIS", true)
	live(v, "C2", "OLDPC", true)
	if g := find(v, "H7", "LATEPC"); g != nil {
		t.Fatalf("H7 opened while a server of the PC is unreachable: %+v", g)
	}
	// With both servers down nothing can be said: everything freezes.
	r.hub.Update("a", func(s *model.Server) { s.Status, s.DownSince = model.StatusDown, r.now })
	v = r.at(time.Minute)
	live(v, "H8", "VENUS", true)
	live(v, "X1", "LIVE-SAME", true)
	// A is back and the PC has connected to it. While B is down, H7 waits.
	r.hub.Update("a", func(s *model.Server) {
		s.Status = model.StatusUp
		s.Connections["PC-ID"] = &model.Connection{Connected: true, StartedAt: r.now}
	})
	v = r.at(time.Minute)
	live(v, "H8", "VENUS", false)
	live(v, "H7", "MORTIS", true)
	// B is back: H7 resolves for the PC that A sees, and opens for LATEPC.
	r.hub.Update("b", func(s *model.Server) { s.Status = model.StatusUp })
	v = r.at(time.Minute)
	if find(v, "H7", "MORTIS") != nil {
		t.Fatal("H7 should resolve once a reachable server sees the PC and no server is stale")
	}
	live(v, "H7", "LATEPC", false)
	live(v, "C2", "OLDPC", false)
}

// During a VPN outage the far NAS is unreachable, and the near NAS loses
// the connections to the far site's PCs. Those PCs still sync with the far
// NAS, so they are not offline: no H7, and no C2 later.
func TestPCOfflineWaitsForEveryServer(t *testing.T) {
	r := newRig(t)
	r.hub.Update("a", func(s *model.Server) {
		s.Devices["PC-ID"] = &model.Device{ID: "PC-ID", Name: "EDIT-02", LastSeen: r.now}
	})
	r.hub.Update("b", func(s *model.Server) {
		s.Devices["PC-ID"] = &model.Device{ID: "PC-ID", Name: "EDIT-02", LastSeen: r.now}
		s.Connections["PC-ID"] = &model.Connection{Connected: true, StartedAt: r.now}
	})
	r.at(0)
	r.hub.Update("b", func(s *model.Server) { s.Status, s.DownSince, s.LastError = model.StatusDown, r.now, "timeout" })
	r.at(0)
	for _, d := range []time.Duration{8 * 24 * time.Hour, 100 * 24 * time.Hour} {
		v := r.at(d)
		if g := find(v, "H7", "EDIT-02"); g != nil {
			t.Fatalf("H7 while the PC's other server is unreachable: %+v", g)
		}
		if g := find(v, "C2", "EDIT-02"); g != nil {
			t.Fatalf("C2 while the PC's other server is unreachable: %+v", g)
		}
	}
	// B is back and still has the PC connected.
	r.hub.Update("b", func(s *model.Server) { s.Status = model.StatusUp })
	if v := r.at(time.Minute); find(v, "H7", "EDIT-02") != nil || find(v, "C2", "EDIT-02") != nil {
		t.Fatal("H7 or C2 for a PC that B has connected")
	}
}

// Both the collector manager and the engine start as goroutines. An
// evaluation that runs before the manager has published the server set
// must not take the empty hub for "no servers, so no problems".
func TestEvaluateWaitsForServerSet(t *testing.T) {
	r := newRig(t)
	r.hub.Update("a", func(s *model.Server) {
		s.Folders["f"] = &model.Folder{ID: "f", Label: "LIVE-F", Path: "/s/X/LIVE-F", State: "error", Error: "boom", Completion: map[string]*model.Completion{}}
	})
	if find(r.at(0), "H4", "LIVE-F") == nil {
		t.Fatal("H4 missing")
	}
	// A process restart: a new hub that nobody has filled yet.
	r.hub = model.NewHub(func() time.Time { return r.now })
	r.restart(t)
	v := r.at(time.Second)
	if find(v, "H4", "LIVE-F") == nil {
		t.Fatal("an evaluation on an empty hub resolved the open finding")
	}
	// Once the servers are published the finding is evaluated as usual.
	r.hub.SetServers([]*model.Server{
		{ID: "a", Name: "A", Status: model.StatusUp, Loaded: true, MyID: "A-ID", PathSep: "/"},
		{ID: "b", Name: "B", Status: model.StatusUp, Loaded: true, MyID: "B-ID", PathSep: "/"},
	})
	if find(r.at(time.Second), "H4", "LIVE-F") != nil {
		t.Fatal("H4 should resolve once the server is seen healthy")
	}
}

// Raising a threshold must not resolve an open finding whose condition
// still holds: the minimum duration gates the opening only.
func TestRaisedThresholdKeepsOpenFinding(t *testing.T) {
	r := newRig(t) // paused: 1h
	r.hub.Update("a", func(s *model.Server) {
		s.Folders["p"] = &model.Folder{ID: "p", Label: "LIVE-P", Path: "/s/X/LIVE-P", Paused: true, State: "paused", Completion: map[string]*model.Completion{}}
	})
	r.at(0)
	h6 := find(r.at(2*time.Hour), "H6", "LIVE-P")
	if h6 == nil {
		t.Fatal("H6 should be open")
	}
	c := r.cfg.Get().Clone()
	c.Thresholds.Paused = config.Duration(10 * time.Hour)
	if err := r.cfg.Set(c); err != nil {
		t.Fatal(err)
	}
	if g := find(r.at(time.Minute), "H6", "LIVE-P"); g == nil || !g.OpenedAt.Equal(h6.OpenedAt) {
		t.Fatalf("raising thresholds.paused changed the open H6: %+v", g)
	}
	// A new pause after that waits for the full new threshold.
	r.hub.Update("a", func(s *model.Server) { s.Folders["p"].Paused = false; s.Folders["p"].State = "idle" })
	r.at(time.Minute)
	r.hub.Update("a", func(s *model.Server) { s.Folders["p"].Paused = true })
	r.at(0) // the observation starts here
	if find(r.at(5*time.Hour), "H6", "LIVE-P") != nil {
		t.Fatal("a new pause opened before the raised threshold")
	}
	if find(r.at(6*time.Hour), "H6", "LIVE-P") == nil {
		t.Fatal("a new pause should open after the raised threshold")
	}
}

// A server that is removed or disabled in the settings leaves the hub.
// Its findings can't be judged again and nobody fixed anything, so they
// are closed quietly: no "resolved" message, and the history says why.
func TestRemovedServerClosesItsFindingsQuietly(t *testing.T) {
	r := newRig(t)
	old := r.now.Add(-10 * 24 * time.Hour)
	for _, id := range []string{"a", "b"} {
		r.hub.Update(id, func(s *model.Server) { s.Devices["PC-ID"] = &model.Device{ID: "PC-ID", Name: "PC", LastSeen: old} })
	}
	r.hub.Update("b", func(s *model.Server) {
		s.Folders["f"] = &model.Folder{ID: "f", Label: "LIVE-F", Path: "/s/X/LIVE-F", State: "error", Error: "boom", Completion: map[string]*model.Completion{}}
	})
	v := r.at(0)
	h4, h7 := find(v, "H4", "LIVE-F"), find(v, "H7", "PC")
	if h4 == nil || h7 == nil {
		t.Fatalf("expected H4 and H7, got %d findings", len(v.Open))
	}
	r.eng.MarkPinged([]string{h4.ID}, r.now)
	// B goes down (H4 freezes, H1 opens and is pinged), then the admin
	// removes B from the settings.
	r.hub.Update("b", func(s *model.Server) { s.Status, s.DownSince = model.StatusDown, r.now })
	v = r.at(time.Minute)
	h1 := find(v, "H1", "B")
	if h1 == nil || find(v, "H4", "LIVE-F") == nil {
		t.Fatalf("expected H1 and a frozen H4, got %d findings", len(v.Open))
	}
	r.eng.MarkPinged([]string{h1.ID}, r.now)
	r.hub.SetServers([]*model.Server{{ID: "a", Name: "A", PathSep: "/"}})
	v = r.at(time.Minute)
	if find(v, "H1", "B") != nil || find(v, "H4", "LIVE-F") != nil {
		t.Fatalf("B's findings should be closed, got %d findings", len(v.Open))
	}
	if g := find(v, "H7", "PC"); g == nil || len(g.Related) != 1 || g.Related[0] != "a" {
		t.Fatalf("H7 should carry on with the remaining server: %+v", g)
	}
	if pend, _ := r.st.PendingResolved(); len(pend) != 0 {
		t.Fatalf("no 'resolved' message is due for a removed server, got %d", len(pend))
	}
	hist, _ := r.st.HistoryBetween(r.now.Add(-time.Hour), r.now.Add(time.Hour))
	closed := 0
	for _, h := range hist {
		if h.Server == "b" {
			if h.ResolvedAt.IsZero() || !strings.Contains(h.Message, "server removed or disabled") {
				t.Fatalf("history entry not closed with a note: %+v", h)
			}
			closed++
		}
	}
	if closed != 2 {
		t.Fatalf("expected 2 closed entries for B, got %d", closed)
	}
	// Removing the last server that sees the PC closes H7 the same way.
	r.hub.SetServers(nil)
	if v = r.at(time.Minute); len(v.Open) != 0 {
		t.Fatalf("expected everything closed, got %+v", v.Open[0])
	}
	if pend, _ := r.st.PendingResolved(); len(pend) != 0 {
		t.Fatal("no 'resolved' message is due")
	}
	if obs, _ := r.st.Observations(); len(obs) != 0 {
		t.Fatalf("observations of removed servers should be dropped: %v", obs)
	}
}

// The notifier reads the pending "resolved" rows, sends, then marks them.
// A finding that re-opens in between inherits the old PingedAt (so a
// flap isn't announced twice). The mark must not then land on that new
// open record: the "resolved" went out, so the new occurrence has to be
// announced again and reported resolved when it really ends.
func TestResolvedMarkDoesNotTouchReopenedFinding(t *testing.T) {
	r := newRig(t)
	r.hub.Update("b", func(s *model.Server) { s.Status, s.DownSince, s.LastError = model.StatusDown, r.now, "refused" })
	h1 := find(r.at(0), "H1", "B")
	r.eng.MarkPinged([]string{h1.ID}, r.now)
	r.hub.Update("b", func(s *model.Server) { s.Status = model.StatusUp })
	r.at(time.Minute) // resolved
	pend, _ := r.st.PendingResolved()
	if len(pend) != 1 {
		t.Fatalf("pending resolved: %d", len(pend))
	}
	// While the "resolved" message is being sent, B goes down again.
	r.hub.Update("b", func(s *model.Server) { s.Status, s.DownSince = model.StatusDown, r.now })
	again := find(r.at(time.Second), "H1", "B")
	if again == nil || again.PingedAt.IsZero() {
		t.Fatalf("H1 should be open again, carrying the ping: %+v", again)
	}
	r.eng.MarkResolvedNotified([]string{pend[0].ID}, r.now)
	rec, _ := r.st.Record(h1.ID)
	if !rec.Open() || !rec.PingedAt.IsZero() || !rec.ResolvedNotifiedAt.IsZero() {
		t.Fatalf("the re-opened record must be open and unannounced: open=%v pinged=%v notified=%v", rec.Open(), !rec.PingedAt.IsZero(), !rec.ResolvedNotifiedAt.IsZero())
	}
	if got := find(r.eng.View(), "H1", "B"); got == nil || !got.PingedAt.IsZero() {
		t.Fatalf("the engine's copy must agree: %+v", got)
	}
	// Announced again, then really resolved: a "resolved" is due once more.
	r.eng.MarkPinged([]string{h1.ID}, r.now)
	r.hub.Update("b", func(s *model.Server) { s.Status = model.StatusUp })
	r.at(time.Minute)
	if pend, _ = r.st.PendingResolved(); len(pend) != 1 {
		t.Fatalf("pending resolved after the second round: %d", len(pend))
	}
	r.eng.MarkResolvedNotified([]string{pend[0].ID}, r.now)
	if pend, _ = r.st.PendingResolved(); len(pend) != 0 {
		t.Fatal("the mark on a resolved row must stick")
	}
}

// A finding that becomes urgent after it opened (the check switched to
// urgent, or a directory gaining an urgent reason) records when that
// happened, so the notifier's debounce counts from there instead of from
// an OpenedAt that may be days old.
func TestUrgentSinceFollowsUrgency(t *testing.T) {
	r := newRig(t)
	r.hub.Update("a", func(s *model.Server) {
		s.PendingDevices = []model.PendingDevice{{DeviceID: "NEW-ID", Name: "newpc", Time: r.now}}
	})
	h9 := find(r.at(0), "H9", "newpc")
	if h9 == nil || h9.Urgent || !h9.UrgentSince.IsZero() {
		t.Fatalf("H9 should open as not urgent: %+v", h9)
	}
	yes := true
	setUrgent := func(v *bool) {
		c := r.cfg.Get().Clone()
		c.Checks["H9"] = config.Check{Urgent: v}
		if err := r.cfg.Set(c); err != nil {
			t.Fatal(err)
		}
	}
	setUrgent(&yes)
	got := find(r.at(time.Hour), "H9", "newpc")
	if got == nil || !got.Urgent || !got.UrgentSince.Equal(r.now) || !got.OpenedAt.Equal(h9.OpenedAt) {
		t.Fatalf("UrgentSince should be set when the finding becomes urgent: %+v", got)
	}
	if !got.UrgentFrom().Equal(r.now) {
		t.Fatalf("the debounce should count from %v, got %v", r.now, got.UrgentFrom())
	}
	// It survives a restart and doesn't move on later evaluations.
	r.restart(t)
	if g := find(r.at(time.Minute), "H9", "newpc"); g == nil || !g.UrgentSince.Equal(got.UrgentSince) {
		t.Fatalf("UrgentSince not restored: %+v", g)
	}
	// Switching urgency off clears it; a finding opened urgent counts
	// from its opening.
	setUrgent(nil)
	if g := find(r.at(time.Minute), "H9", "newpc"); g == nil || g.Urgent || !g.UrgentSince.IsZero() {
		t.Fatalf("UrgentSince should be cleared: %+v", g)
	}
	r.hub.Update("b", func(s *model.Server) { s.Status, s.DownSince = model.StatusDown, r.now })
	if g := find(r.at(time.Minute), "H1", "B"); g == nil || !g.UrgentSince.Equal(g.OpenedAt) {
		t.Fatalf("an urgent finding counts from its opening: %+v", g)
	}
}

// Finding IDs are predictable, so a snooze or ignore of an ID that never
// showed up would mute a future problem (say a server's H1) without a
// trace. Only open or recently resolved findings can be hidden, and an
// ignore outlives its finding by 90 days at most.
func TestSnoozeOnlyKnownFindings(t *testing.T) {
	r := newRig(t)
	id := model.FindingID("H1", "b", "server")
	if err := r.eng.Ignore(id, "mute in advance", "admin"); err == nil {
		t.Fatal("ignoring a finding that never existed must be refused")
	}
	if err := r.eng.Snooze(id, r.now.Add(24*time.Hour), "", "admin"); err == nil {
		t.Fatal("snoozing a finding that never existed must be refused")
	}
	if _, ok := r.eng.View().Snoozed(id, r.now); ok {
		t.Fatal("nothing should be snoozed")
	}
	// B goes down: H1 opens and can be ignored.
	r.hub.Update("b", func(s *model.Server) { s.Status, s.DownSince = model.StatusDown, r.now })
	if find(r.at(0), "H1", "B") == nil {
		t.Fatal("H1 missing")
	}
	if err := r.eng.Ignore(id, "known outage", "admin"); err != nil {
		t.Fatal(err)
	}
	// An ignore on an open finding survives pruning.
	r.now = r.now.Add(100 * 24 * time.Hour)
	r.eng.Prune()
	if _, ok := r.eng.View().Snoozed(id, r.now); !ok {
		t.Fatal("the ignore of an open finding was pruned")
	}
	// B is back: H1 resolves, and it can still be hidden for a while
	// (a re-open stays hidden), but not forever.
	r.hub.Update("b", func(s *model.Server) { s.Status = model.StatusUp })
	if find(r.at(time.Minute), "H1", "B") != nil {
		t.Fatal("H1 should resolve")
	}
	if err := r.eng.Unsnooze(id); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(10 * 24 * time.Hour)
	if err := r.eng.Ignore(id, "still remembered", "admin"); err != nil {
		t.Fatal(err)
	}
	r.now = r.now.Add(100 * 24 * time.Hour)
	r.eng.Prune()
	if _, ok := r.eng.View().Snoozed(id, r.now); ok {
		t.Fatal("an ignore of a finding resolved 110 days ago should be pruned")
	}
	if err := r.eng.Ignore(id, "too late", "admin"); err == nil {
		t.Fatal("a finding resolved 110 days ago can't be ignored any more")
	}
}
