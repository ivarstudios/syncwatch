package engine

import (
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/stclient"
)

// The dashboards refresh when what the Folders page shows changes: a size in
// a cell as the page writes it, a folder of a share, a size on the disk and
// its measurement. Not for each byte of a sync.
func TestVisibleFingerprintFollowsTheFoldersPage(t *testing.T) {
	snap := func(edit func(sv *model.Server)) *model.Snapshot {
		sv := &model.Server{ID: "akka", Name: "AKKA", Status: model.StatusUp, Loaded: true,
			Folders: map[string]*model.Folder{"f": {ID: "f", Label: "LIVE-F", State: "syncing", GlobalBytes: 400_000_000_000, NeedBytes: 280_000_000_000, NeedItems: 5}},
			Structure: &model.Structure{ScannedAt: time.Unix(1000, 0), Shares: []*model.Share{{Path: "/s/AKKA-LIVE-1SOURCE", Name: "AKKA-LIVE-1SOURCE",
				Dirs: []*model.Dir{{Name: "LIVE-F", FolderIDs: []string{"f"}}, {Name: "LIVE-NEW", Path: "/s/AKKA-LIVE-1SOURCE/LIVE-NEW"}}}}}}
		if edit != nil {
			edit(sv)
		}
		return &model.Snapshot{Servers: []*model.Server{sv}}
	}
	base := visibleFingerprint(snap(nil))
	for name, edit := range map[string]func(sv *model.Server){
		"a size arrives": func(sv *model.Server) {
			sv.Structure.Sizes = map[string]*model.DirSize{"/s/AKKA-LIVE-1SOURCE/LIVE-NEW": {Bytes: 1_300_000_000_000, MeasuredAt: time.Unix(2000, 0), Complete: true}}
		},
		"a measurement starts": func(sv *model.Server) {
			sv.Structure.Sizes = map[string]*model.DirSize{"/s/AKKA-LIVE-1SOURCE/LIVE-NEW": {State: stclient.SizeMeasuring}}
		},
		"no gate": func(sv *model.Server) { sv.Structure.NoSizes = true },
		"a new folder in a share": func(sv *model.Server) {
			sv.Structure.Shares[0].Dirs = append(sv.Structure.Shares[0].Dirs, &model.Dir{Name: "LIVE-OTHER"})
		},
		"the folder joins Syncthing": func(sv *model.Server) { sv.Structure.Shares[0].Dirs[1].FolderIDs = []string{"g"} },
		"the received size changes":  func(sv *model.Server) { sv.Folders["f"].NeedBytes = 200_000_000_000 },
		"the folder grows":           func(sv *model.Server) { sv.Folders["f"].GlobalBytes = 500_000_000_000 },
	} {
		if visibleFingerprint(snap(edit)) == base {
			t.Errorf("%s: the dashboards do not refresh", name)
		}
	}
	// 120.0 GB and 120.3 GB both show as "120 GB".
	if visibleFingerprint(snap(func(sv *model.Server) { sv.Folders["f"].NeedBytes = 279_700_000_000 })) != base {
		t.Error("a change of the bytes that the page does not show refreshes the dashboards")
	}
}
