package engine

import (
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
)

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
