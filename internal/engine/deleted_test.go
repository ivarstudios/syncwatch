package engine

import (
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
)

// S8 comes from the deleted folders that syncwatch keeps itself: it stays
// open while its server is down or not contacted after a restart, and it
// resolves when an administrator removes the folder, also then.
func TestDeletedFolderFindingIgnoresServerState(t *testing.T) {
	r := newRig(t)
	gone := &model.DeletedDir{Path: "/s/A-LIVE-1SOURCE/LIVE-GONE", Name: "LIVE-GONE", Share: "/s/A-LIVE-1SOURCE", ShareName: "A-LIVE-1SOURCE", DetectedAt: r.now}
	r.hub.Update("a", func(s *model.Server) { s.Deleted = map[string]*model.DeletedDir{gone.Path: gone.Clone()} })
	if find(r.at(0), "S8", "LIVE-GONE") == nil {
		t.Fatal("no S8")
	}
	r.hub.Update("a", func(s *model.Server) { s.Status, s.Loaded = model.StatusDown, false })
	rec := find(r.at(time.Hour), "S8", "LIVE-GONE")
	if rec == nil || rec.Stale {
		t.Fatalf("S8 while the server is down: %+v", rec)
	}
	r.hub.Update("a", func(s *model.Server) { s.Deleted = nil })
	if rec := find(r.at(time.Minute), "S8", "LIVE-GONE"); rec != nil {
		t.Fatalf("S8 open after the removal while the server is down: %+v", rec)
	}
}
