package engine

import (
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/store"
)

type rig struct {
	now time.Time
	hub *model.Hub
	st  *store.Store
	cfg *config.Holder
	eng *Engine
}

func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{now: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)}
	st, err := store.OpenMemory(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	r.st = st
	c, _ := config.ParseYAML([]byte(`
servers: [{name: A, url: "https://a:1"}, {name: B, url: "https://b:1"}]
thresholds: {paused: 1h}
`))
	r.cfg, _ = config.NewHolder(c)
	r.hub = model.NewHub(func() time.Time { return r.now })
	r.hub.SetServers([]*model.Server{
		{ID: "a", Name: "A", Status: model.StatusUp, Loaded: true, MyID: "A-ID", PathSep: "/"},
		{ID: "b", Name: "B", Status: model.StatusUp, Loaded: true, MyID: "B-ID", PathSep: "/"},
	})
	r.restart(t)
	return r
}

func (r *rig) restart(t *testing.T) {
	t.Helper()
	e, err := New(r.st, r.hub, r.cfg, func() time.Time { return r.now }, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	r.eng = e
}

func (r *rig) at(d time.Duration) *View {
	r.now = r.now.Add(d)
	r.eng.Evaluate()
	return r.eng.View()
}

func find(v *View, check, part string) *store.Record {
	for _, rec := range v.Open {
		if rec.Check == check && strings.Contains(rec.Subject+" "+rec.Message, part) {
			return rec
		}
	}
	return nil
}

// Failing files alone wait for thresholds.failing_files before H4 opens; a
// folder error doesn't.
func TestFailingFilesWaitBeforeH4(t *testing.T) {
	r := newRig(t)
	r.hub.Update("a", func(s *model.Server) {
		s.Folders["f"] = &model.Folder{ID: "f", Label: "LIVE-F", Path: "/s/X/LIVE-F", State: "idle", PullErrors: 2, Completion: map[string]*model.Completion{}}
	})
	if find(r.at(0), "H4", "LIVE-F") != nil {
		t.Fatal("H4 opened at once for failing files")
	}
	if find(r.at(50*time.Minute), "H4", "LIVE-F") != nil {
		t.Fatal("H4 opened before the hour was up")
	}
	if find(r.at(11*time.Minute), "H4", "LIVE-F") == nil {
		t.Fatal("H4 not open after an hour of failing files")
	}
	r.hub.Update("a", func(s *model.Server) {
		s.Folders["g"] = &model.Folder{ID: "g", Label: "LIVE-G", Path: "/s/X/LIVE-G", State: "error", Error: "folder marker missing", Completion: map[string]*model.Completion{}}
	})
	if find(r.at(time.Minute), "H4", "LIVE-G") == nil {
		t.Fatal("a folder error must open H4 at once")
	}
}

func TestFreezeWhileServerDown(t *testing.T) {
	r := newRig(t)
	r.hub.Update("a", func(s *model.Server) {
		s.Folders["f"] = &model.Folder{ID: "f", Label: "LIVE-F", Path: "/s/X/LIVE-F", State: "error", Error: "boom", Completion: map[string]*model.Completion{}}
		s.Devices["B-ID"] = &model.Device{ID: "B-ID", Name: "B"}
	})
	r.hub.Update("b", func(s *model.Server) { s.Devices["A-ID"] = &model.Device{ID: "A-ID", Name: "A"} })
	v := r.at(0)
	h4 := find(v, "H4", "LIVE-F")
	h2 := find(v, "H2", "A ↔ B")
	if h4 == nil || h2 == nil {
		t.Fatalf("expected H4 and H2, got %d findings", len(v.Open))
	}

	// A goes down: H1 opens, H4 (from A) and H2 (A↔B) freeze instead of resolving.
	r.hub.Update("a", func(s *model.Server) {
		s.Status, s.DownSince, s.LastError = model.StatusDown, r.now, "refused"
		s.Folders["f"].State, s.Folders["f"].Error = "idle", "" // stale data must not resolve it either
	})
	v = r.at(time.Minute)
	if find(v, "H1", "A") == nil {
		t.Fatal("H1 missing")
	}
	h4 = find(v, "H4", "LIVE-F")
	h2 = find(v, "H2", "A ↔ B")
	if h4 == nil || !h4.Stale || h2 == nil || !h2.Stale {
		t.Fatalf("expected frozen stale H4/H2: %+v %+v", h4, h2)
	}

	// A is back and healthy: H1, H4 and H2 resolve.
	r.hub.Update("a", func(s *model.Server) { s.Status = model.StatusUp })
	r.hub.Update("b", func(s *model.Server) { s.Connections["A-ID"] = &model.Connection{Connected: true} })
	v = r.at(time.Minute)
	if len(v.Open) != 0 {
		for _, rec := range v.Open {
			t.Logf("%s %s stale=%v", rec.Check, rec.Message, rec.Stale)
		}
		t.Fatal("expected everything resolved")
	}
	hist, _ := r.st.HistoryBetween(r.now.Add(-time.Hour), r.now.Add(time.Hour))
	if len(hist) != 3 {
		t.Fatalf("history: %d entries", len(hist))
	}
}

func TestNewFindingsFromStaleServerWait(t *testing.T) {
	r := newRig(t)
	r.hub.Update("a", func(s *model.Server) {
		s.Status = model.StatusDown
		s.Folders["f"] = &model.Folder{ID: "f", Label: "LIVE-F", Path: "/s/X/LIVE-F", State: "error", Completion: map[string]*model.Completion{}}
	})
	v := r.at(0)
	if find(v, "H4", "LIVE-F") != nil {
		t.Fatal("a new finding from an unreachable server's stale data must not open")
	}
}

func TestMinDurationAndRestart(t *testing.T) {
	r := newRig(t)
	r.hub.Update("a", func(s *model.Server) {
		s.Folders["p"] = &model.Folder{ID: "p", Label: "LIVE-P", Path: "/s/X/LIVE-P", Paused: true, State: "paused", Completion: map[string]*model.Completion{}}
	})
	if v := r.at(0); find(v, "H6", "LIVE-P") != nil {
		t.Fatal("H6 opened before its threshold")
	}
	r.at(30 * time.Minute)
	// The observation survives a monitor restart.
	r.restart(t)
	v := r.at(31 * time.Minute)
	rec := find(v, "H6", "LIVE-P")
	if rec == nil {
		t.Fatal("H6 should open after an hour paused, across a restart")
	}
	if got := r.now.Sub(rec.Since); got < time.Hour {
		t.Fatalf("since should be the first observation, got %v ago", got)
	}
	// Unpausing resolves it and clears the observation.
	r.hub.Update("a", func(s *model.Server) { s.Folders["p"].Paused = false; s.Folders["p"].State = "idle" })
	if v := r.at(time.Minute); find(v, "H6", "LIVE-P") != nil {
		t.Fatal("H6 should resolve")
	}
	r.hub.Update("a", func(s *model.Server) { s.Folders["p"].Paused = true })
	if v := r.at(time.Minute); find(v, "H6", "LIVE-P") != nil {
		t.Fatal("a new pause starts a new observation")
	}
}

func TestRecordsPersistAcrossRestart(t *testing.T) {
	r := newRig(t)
	r.hub.Update("b", func(s *model.Server) { s.Status, s.DownSince = model.StatusDown, r.now })
	v := r.at(0)
	h1 := find(v, "H1", "B")
	if h1 == nil {
		t.Fatal("H1 missing")
	}
	r.eng.MarkPinged([]string{h1.ID}, r.now)
	r.restart(t)
	v = r.at(time.Minute)
	got := find(v, "H1", "B")
	if got == nil || got.PingedAt.IsZero() || !got.OpenedAt.Equal(h1.OpenedAt) {
		t.Fatalf("record not restored: %+v", got)
	}
}
