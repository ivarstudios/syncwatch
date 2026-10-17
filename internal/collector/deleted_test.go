package collector

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/stclient"
)

const (
	shareRoot   = `D:\tmp\swsim\st\akka\share`
	srcShare    = shareRoot + `\AKKA-LIVE-1SOURCE`
	notSynced   = srcShare + `\LIVE-NOTSYNCED-1SOURCE`
	workshop    = srcShare + `\ME-LIVE-WORKSHOP`
	kyrgyzSrc   = srcShare + `\LIVE-KYRGYZ-2PROJECTFILES-BLENDER`
	betaSynced  = srcShare + `\LIVE-BETA-1SOURCE`
	gammaSynced = srcShare + `\LIVE-GAMMA-1SOURCE`
)

func deletedOf(c *Collector) map[string]*model.DeletedDir {
	var out map[string]*model.DeletedDir
	c.hub.View(c.id, func(s *model.Server) { out = s.Clone().Deleted })
	return out
}

// startDeleted is a scanned collector whose share listings a test can
// change (bl.extra), behind a size gate. Its sizes are read once.
func startDeleted(t *testing.T) (*Collector, *browseLog, *config.Holder, context.Context) {
	t.Helper()
	g := &sizeGate{}
	bl := &browseLog{fail: map[string]bool{}, extra: map[string][]string{}}
	c, _, h, stop := startCollector(t, g.wrap(bl.wrap(newFakeST(t))), "timezone: UTC\n")
	t.Cleanup(stop)
	ctx := context.Background()
	if err := c.scan(ctx, true); err != nil {
		t.Fatal(err)
	}
	c.syncSizes(ctx, time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC))
	if d := deletedOf(c); len(d) != 0 {
		t.Fatalf("deleted folders after the first scan: %+v", d)
	}
	return c, bl, h, ctx
}

// list makes the listing of the share 1SOURCE the given directories.
func (b *browseLog) list(share string, dirs ...string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.extra[share+`\`] = append([]string{}, dirs...)
}

// A project folder outside Syncthing that a listing of its share no longer
// has is deleted: once, with the time of the listing and its last size, in
// the hub and in the store. A synced folder that goes is not (Syncthing
// reports it: H4), and a share that the scan cannot list changes nothing.
// When a listing has the folder again, it is back.
func TestScanFindsDeletedFolders(t *testing.T) {
	c, bl, _, ctx := startDeleted(t)

	// LIVE-NOTSYNCED and the synced LIVE-BETA go.
	bl.list(srcShare, gammaSynced, kyrgyzSrc, workshop, srcShare+`\random stuff`)
	before := time.Now()
	if err := c.scan(ctx, false); err != nil {
		t.Fatal(err)
	}
	del := deletedOf(c)
	d := del[notSynced]
	if len(del) != 1 || d == nil {
		t.Fatalf("deleted: %+v", del)
	}
	if d.Name != "LIVE-NOTSYNCED-1SOURCE" || d.Share != srcShare || d.ShareName != "AKKA-LIVE-1SOURCE" || d.All || d.DetectedAt.Before(before) ||
		d.Size == nil || d.Size.Bytes != int64(len(notSynced))*1000 || !d.Size.MeasuredAt.Equal(measuredAt) || d.Size.State != "" {
		t.Errorf("deleted folder: %+v, size %+v", d, d.Size)
	}
	stored, err := c.st.DeletedDirs("akka")
	if err != nil || len(stored) != 1 || stored[notSynced] == nil || !stored[notSynced].DetectedAt.Equal(d.DetectedAt.Truncate(time.Millisecond)) ||
		stored[notSynced].Size == nil || stored[notSynced].Size.Bytes != d.Size.Bytes {
		t.Fatalf("store: %+v, %v", stored, err)
	}

	// The next scan does not find it again, and a failed listing changes
	// nothing.
	if err := c.scan(ctx, false); err != nil {
		t.Fatal(err)
	}
	bl.mu.Lock()
	bl.fail[srcShare+`\`] = true
	bl.mu.Unlock()
	if err := c.scan(ctx, false); err != nil {
		t.Fatal(err)
	}
	if again := deletedOf(c); len(again) != 1 || !again[notSynced].DetectedAt.Equal(d.DetectedAt) {
		t.Errorf("after 2 more scans: %+v", again)
	}

	// Back, in other case: the server ignores case.
	bl.mu.Lock()
	bl.fail = map[string]bool{}
	bl.mu.Unlock()
	bl.list(srcShare, gammaSynced, kyrgyzSrc, strings.ToUpper(notSynced), workshop)
	if err := c.scan(ctx, false); err != nil {
		t.Fatal(err)
	}
	if back := deletedOf(c); len(back) != 0 {
		t.Errorf("the folder is back, but deleted: %+v", back)
	}
	if stored, err := c.st.DeletedDirs("akka"); err != nil || len(stored) != 0 {
		t.Errorf("store after the folder is back: %+v, %v", stored, err)
	}
}

// A share that lists no directory where it listed 2 or more marks its
// folders outside Syncthing with All: a share that is not mounted in the
// Syncthing container looks the same as a deletion of everything.
func TestScanEmptyShare(t *testing.T) {
	c, bl, _, ctx := startDeleted(t)
	bl.list(srcShare)
	// The share exists: its parent lists it.
	bl.list(shareRoot, srcShare, shareRoot+`\AKKA-LIVE-2PROJECTFILES`)
	if err := c.scan(ctx, false); err != nil {
		t.Fatal(err)
	}
	del := deletedOf(c)
	if len(del) != 3 || del[notSynced] == nil || del[workshop] == nil || del[kyrgyzSrc] == nil {
		t.Fatalf("deleted: %+v", del)
	}
	for p, d := range del {
		if !d.All {
			t.Errorf("%s without All", p)
		}
	}
	if del[betaSynced] != nil || del[gammaSynced] != nil {
		t.Error("a synced folder is marked deleted")
	}
}

// The listings are in the store: the first scan after a restart finds the
// folders that were deleted while syncwatch did not run. The manager reads
// the deleted folders into the hub before the first evaluation, also for a
// server that it cannot contact, and an administrator removes one.
func TestDeletedFoldersOverRestart(t *testing.T) {
	c, bl, h, ctx := startDeleted(t)
	bl.list(srcShare, betaSynced, gammaSynced, kyrgyzSrc, srcShare+`\random stuff`)

	// A new collector with a new hub and the same store: a restart.
	hub := model.NewHub(nil)
	sc := h.Get().Servers[0]
	hub.SetServers([]*model.Server{{ID: sc.ID, Name: sc.Name, URL: sc.URL}})
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	c2, err := newCollector(sc, "k", hub, c.st, h, log, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.client.Close()
	if err := c2.fullCheck(ctx); err != nil {
		t.Fatal(err)
	}
	c2.succeeded()
	if err := c2.scan(ctx, false); err != nil {
		t.Fatal(err)
	}
	del := deletedOf(c2)
	if len(del) != 2 || del[notSynced] == nil || del[workshop] == nil || del[notSynced].Size != nil {
		t.Fatalf("deleted after the restart: %+v", del)
	}

	// Another start: the manager reads them, before any contact (no API key
	// here, so no collector).
	hub2 := model.NewHub(nil)
	m := NewManager(hub2, c.st, h, log)
	m.ctx = ctx
	m.Sync()
	var loaded map[string]*model.DeletedDir
	hub2.View("akka", func(s *model.Server) { loaded = s.Clone().Deleted })
	if len(loaded) != 2 || loaded[workshop] == nil || loaded[workshop].Name != "ME-LIVE-WORKSHOP" || loaded[workshop].ShareName != "AKKA-LIVE-1SOURCE" ||
		!loaded[workshop].DetectedAt.Equal(del[workshop].DetectedAt.Truncate(time.Millisecond)) {
		t.Fatalf("the manager read %+v", loaded)
	}

	if err := m.ForgetDeleted("akka", workshop); err != nil {
		t.Fatal(err)
	}
	hub2.View("akka", func(s *model.Server) { loaded = s.Clone().Deleted })
	stored, _ := c.st.DeletedDirs("akka")
	if len(loaded) != 1 || loaded[workshop] != nil || len(stored) != 1 || stored[workshop] != nil {
		t.Errorf("after the removal: hub %+v, store %+v", loaded, stored)
	}
	if err := m.ForgetDeleted("akka", workshop); !errors.Is(err, ErrNotDeleted) {
		t.Errorf("a second removal: %v", err)
	}
	// It does not come back at the next scan: the listings do not have it.
	if err := c2.scan(ctx, false); err != nil {
		t.Fatal(err)
	}
	if d := deletedOf(c2); d[notSynced] == nil {
		t.Errorf("after the next scan: %+v", d)
	}
	if stored, _ := c.st.DeletedDirs("akka"); stored[workshop] != nil {
		t.Errorf("the removed folder is back in the store: %+v", stored)
	}

	// A removed server leaves nothing for a new server with the same ID.
	if err := c.st.ForgetListings("akka"); err != nil {
		t.Fatal(err)
	}
	if l, _ := c.st.ShareListings("akka"); len(l) != 0 {
		t.Errorf("listings of a removed server: %+v", l)
	}
	if d, _ := c.st.DeletedDirs("akka"); len(d) != 0 {
		t.Errorf("deleted folders of a removed server: %+v", d)
	}
}

// A measurement that failed (the directory does not exist) has no size:
// it keeps the last size, with the error, or shows no size. A size of 0
// bytes stays a size when the gate measured it.
func TestFailedMeasurementIsNoSize(t *testing.T) {
	at := time.Date(2026, 10, 15, 2, 5, 0, 0, time.UTC)
	failed := stclient.DirSize{Error: "the directory does not exist", MeasuredAt: at, TookMS: 1}
	old := &model.DirSize{Bytes: 5e9, Files: 12, Complete: true, MeasuredAt: measuredAt, Took: time.Minute}
	if ds := fromGate(failed, old); ds.Bytes != 5e9 || ds.Files != 12 || !ds.Complete || !ds.MeasuredAt.Equal(measuredAt) || ds.Took != time.Minute || ds.Error != failed.Error {
		t.Errorf("with a last size: %+v", ds)
	}
	if ds := fromGate(failed, nil); ds.Bytes != 0 || !ds.MeasuredAt.IsZero() || ds.Took != 0 || ds.Error != failed.Error {
		t.Errorf("without a last size: %+v", ds)
	}
	empty := stclient.DirSize{Complete: true, MeasuredAt: at}
	if ds := fromGate(empty, old); ds.Bytes != 0 || !ds.MeasuredAt.Equal(at) || !ds.Complete {
		t.Errorf("an empty folder: %+v", ds)
	}
	// The gate had too many measurements: the result has the last size.
	busy := stclient.DirSize{Bytes: 7e9, Files: 3, Complete: true, MeasuredAt: measuredAt, Error: "the gate has 2000 measurements in its queue: try again later"}
	if ds := fromGate(busy, nil); ds.Bytes != 7e9 || !ds.MeasuredAt.Equal(measuredAt) {
		t.Errorf("a busy gate: %+v", ds)
	}
}
