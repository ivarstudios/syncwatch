package engine

import (
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

// A cross-server finding is judged on whichever related servers are
// reachable: one unreachable NAS must not blind the PC checks on the other.
func TestCrossFindingsFreezeOnlyWhenEveryServerIsStale(t *testing.T) {
	r := newRig(t)
	old := r.now.Add(-10 * 24 * time.Hour)
	for _, id := range []string{"a", "b"} {
		r.hub.Update(id, func(s *model.Server) { s.Devices["PC-ID"] = &model.Device{ID: "PC-ID", Name: "PC", LastSeen: old} })
	}
	r.hub.Update("b", func(s *model.Server) { s.Status, s.DownSince = model.StatusDown, r.now })
	v := r.at(time.Hour)
	h7 := find(v, "H7", "PC")
	if h7 == nil || h7.Stale {
		t.Fatalf("H7 should open while A still sees the PC offline: %+v", h7)
	}
	// With both servers down nothing can be said: it freezes.
	r.hub.Update("a", func(s *model.Server) { s.Status, s.DownSince = model.StatusDown, r.now })
	if h7 = find(r.at(time.Minute), "H7", "PC"); h7 == nil || !h7.Stale {
		t.Fatalf("H7 should be frozen with every server down: %+v", h7)
	}
	// A is back and the PC has connected to it: it resolves.
	r.hub.Update("a", func(s *model.Server) {
		s.Status = model.StatusUp
		s.Connections["PC-ID"] = &model.Connection{Connected: true, StartedAt: r.now}
	})
	if find(r.at(time.Minute), "H7", "PC") != nil {
		t.Fatal("H7 should resolve once a reachable server sees the PC")
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
