package rules

import (
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/model"
)

// S8: one urgent WARNING for each deleted folder, with when syncwatch found
// it deleted and its last size. The folders that went with their whole
// share give one finding for the share, not one for each folder. The
// findings come from the deleted folders that syncwatch keeps, thus also
// for a server whose first full check after a start is not complete.
func TestDeletedFolders(t *testing.T) {
	c := ivarConfig(t)
	detected := now.Add(-2 * time.Hour)
	measured := now.Add(-26 * time.Hour)
	akka := server("akka", "AKKA", akkaID)
	akka.Loaded, akka.Status = false, model.StatusUnknown
	src, renders := "/share/X/AKKA-LIVE-1SOURCE", "/share/X/AKKA-LIVE-3RENDERS"
	akka.Deleted = map[string]*model.DeletedDir{
		src + "/LIVE-GONE-1SOURCE": {Path: src + "/LIVE-GONE-1SOURCE", Name: "LIVE-GONE-1SOURCE", Share: src, ShareName: "AKKA-LIVE-1SOURCE", DetectedAt: detected,
			Size: &model.DirSize{Bytes: 120_400_000_000, Files: 3412, MeasuredAt: measured}},
		renders + "/LIVE-A": {Path: renders + "/LIVE-A", Name: "LIVE-A", Share: renders, ShareName: "AKKA-LIVE-3RENDERS", DetectedAt: detected, All: true},
		renders + "/LIVE-B": {Path: renders + "/LIVE-B", Name: "LIVE-B", Share: renders, ShareName: "AKKA-LIVE-3RENDERS", DetectedAt: detected, All: true,
			Size: &model.DirSize{Bytes: 2_000_000, Files: 1, Complete: true, MeasuredAt: measured}},
	}
	fs := run(t, c, akka)

	one := mustFind(t, fs, "S8", "AKKA-LIVE-1SOURCE/LIVE-GONE-1SOURCE on AKKA")
	if one.ID != model.FindingID("S8", "akka", "deleted:"+src+"/LIVE-GONE-1SOURCE") || one.Severity != model.Warn || !one.Urgent || !one.Since.Equal(detected) ||
		one.Category != model.CatStructure {
		t.Errorf("finding of one folder: %+v", one)
	}
	if !strings.Contains(one.Message, "no other device has a copy") || !strings.Contains(one.Message, "recycle bin") {
		t.Errorf("message %q", one.Message)
	}
	if one.DetailValue("Path") != src+"/LIVE-GONE-1SOURCE" || one.DetailValue("Last size") != "≥ 120 GB in 3412 files" ||
		one.DetailValue("Detected") != detected.Format(time.RFC3339) || one.DetailValue("Size measured") != measured.Format(time.RFC3339) {
		t.Errorf("details %+v", one.Details)
	}

	share := mustFind(t, fs, "S8", "AKKA-LIVE-3RENDERS on AKKA")
	if share.ID != model.FindingID("S8", "akka", "share-empty:"+renders) || !strings.Contains(share.Message, "2 project folders outside Syncthing went in one scan") ||
		!strings.Contains(share.Message, "mounted in the Syncthing container") {
		t.Errorf("finding of the share: %+v", share)
	}
	var listed []string
	for _, d := range share.Details {
		if d.Key == "Deleted folder" {
			listed = append(listed, d.Value)
		}
	}
	if strings.Join(listed, ", ") != "LIVE-A, LIVE-B (2.0 MB in 1 file)" {
		t.Errorf("folders of the share: %q", listed)
	}
	n := 0
	for _, f := range fs {
		if f.Check == "S8" {
			n++
		}
	}
	if n != 2 {
		t.Errorf("%d S8 findings, want 2:\n%s", n, dump(fs))
	}

	// Off in Settings → Checks: no finding.
	off := false
	c.Checks["S8"] = config.Check{Enabled: &off}
	if f := find(run(t, c, akka), "S8", ""); f != nil {
		t.Errorf("S8 while off: %+v", f)
	}
}
