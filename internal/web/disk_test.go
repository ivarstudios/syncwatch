package web

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/stclient"
)

const (
	akkaNew   = "/s/AKKA-LIVE-2PROJECTFILES/LIVE-NEW-2PROJECTFILES"
	solAlpha  = "/z/SOL-LIVE-2PROJECTFILES/LIVE-ALPHA-2PROJECTFILES"
	akkaAlpha = "/s/AKKA-LIVE-2PROJECTFILES/LIVE-ALPHA-2PROJECTFILES"
)

// diskEnv: AKKA syncs LIVE-ALPHA-2PROJECTFILES and has LIVE-NEW on its disk,
// not in Syncthing (1.3 TB). SOL has LIVE-ALPHA on its disk, not in
// Syncthing, and its gate measures it now (412 GB from the night before).
func diskEnv(t *testing.T, e *env) {
	t.Helper()
	measured := time.Now().Add(-6 * time.Hour)
	e.hub.Update("akka", func(s *model.Server) {
		s.Status, s.Loaded, s.MyID = model.StatusUp, true, "AKKAAAA-AKKAAAA-AKKAAAA-AKKAAAA-AKKAAAA-AKKAAAA-AKKAAAA-AKKAAAA"
		s.Folders = map[string]*model.Folder{"alpha": {ID: "alpha", Label: "LIVE-ALPHA-2PROJECTFILES", Path: akkaAlpha, State: "idle"}}
		s.Structure = &model.Structure{Shares: []*model.Share{{Path: "/s/AKKA-LIVE-2PROJECTFILES", Name: "AKKA-LIVE-2PROJECTFILES", Dirs: []*model.Dir{
			{Name: "LIVE-ALPHA-2PROJECTFILES", Path: akkaAlpha, FolderIDs: []string{"alpha"}},
			{Name: "LIVE-NEW-2PROJECTFILES", Path: akkaNew},
			{Name: "_archive", Path: "/s/AKKA-LIVE-2PROJECTFILES/_archive"},
		}}}, Sizes: map[string]*model.DirSize{akkaNew: {Bytes: 1_270_000_000_000, Files: 41_000, Complete: true, MeasuredAt: measured, Took: 3 * time.Minute}}}
	})
	e.hub.Update("sol", func(s *model.Server) {
		s.Status, s.Loaded, s.MyID = model.StatusUp, true, "SOLSOLS-SOLSOLS-SOLSOLS-SOLSOLS-SOLSOLS-SOLSOLS-SOLSOLS-SOLSOLS"
		s.Structure = &model.Structure{Shares: []*model.Share{{Path: "/z/SOL-LIVE-2PROJECTFILES", Name: "SOL-LIVE-2PROJECTFILES", Dirs: []*model.Dir{
			{Name: "LIVE-ALPHA-2PROJECTFILES", Path: solAlpha},
		}}}, Sizes: map[string]*model.DirSize{solAlpha: {Bytes: 412_400_000_000, Files: 900, Complete: true, MeasuredAt: measured, State: stclient.SizeMeasuring}}}
	})
	e.eng.Evaluate()
}

const diskRules = "structure: {project_pattern: '^LIVE-', ignore_dirs: ['_*']}\n"

// rowOf returns the table row of the Folders page with the folder label.
func rowOf(t *testing.T, page, label string) string {
	t.Helper()
	for _, row := range strings.Split(page, "<tr ")[1:] {
		row, _, _ = strings.Cut(row, "</tr>")
		if strings.Contains(row, ">"+label+"</th>") {
			return row
		}
	}
	t.Fatalf("no row %s", label)
	return ""
}

// The Folders page lists the project folders on the disk of a server that
// are not in Syncthing: a grey box with the size in the column of the
// server, in the row of the Syncthing folder with the same name or in a row
// of their own. They are no folders of Syncthing and no faults.
func TestGridShowsFoldersNotInSyncthing(t *testing.T) {
	e := adminEnv(t, twoServers+diskRules)
	diskEnv(t, e)
	_, page := e.get("/grid")

	alpha := rowOf(t, page, "LIVE-ALPHA-2PROJECTFILES")
	if strings.Contains(alpha, "disk-only") || !strings.Contains(alpha, `<span class="cell c-disk" title="On the disk of SOL in SOL-LIVE-2PROJECTFILES, not in Syncthing. 412 GB in 900 files`) ||
		!strings.Contains(alpha, "⟳</span> 412 GB</span>") || !strings.Contains(alpha, "The gate measures it now.") {
		t.Errorf("the SOL cell of the synced folder:\n%s", alpha)
	}
	fresh := rowOf(t, page, "LIVE-NEW-2PROJECTFILES")
	if !strings.Contains(fresh, `class="frow disk-only"`) || !strings.Contains(fresh, "▢</span> 1.3 TB</span>") ||
		!strings.Contains(fresh, `title="Not in Syncthing · on the disk of AKKA"`) {
		t.Errorf("the folder that is only on the disk:\n%s", fresh)
	}
	if strings.Contains(page, "_archive") {
		t.Error("an ignored directory is on the page")
	}
	if !strings.Contains(page, "1 folder</span>") || !strings.Contains(page, "▢ 2 not in Syncthing</span>") || !strings.Contains(page, "✔ no faults") {
		t.Errorf("the group heading counts the folders of Syncthing only, and the folders not in Syncthing apart")
	}
	if !strings.Contains(page, `id="grid-disk" checked`) {
		t.Error("no toggle for the folders not in Syncthing")
	}

	// The Measure button of a row asks for each of its folders on a disk.
	form := regexp.MustCompile(`<form method="post" action="/grid/measure" class="measure">.*?</form>`)
	f := form.FindString(alpha)
	if f == "" || !strings.Contains(f, `name="server" value="sol"`) || !strings.Contains(f, `name="path" value="`+solAlpha+`"`) ||
		strings.Contains(f, akkaAlpha) || !strings.Contains(f, `name="back" value="/grid#r-`) {
		t.Errorf("Measure form of the synced row: %s", f)
	}
	if f := form.FindString(fresh); !strings.Contains(f, `name="server" value="akka"`) || !strings.Contains(f, `name="path" value="`+akkaNew+`"`) {
		t.Errorf("Measure form of the row on the disk: %s", f)
	}

	// A viewer sees the folders and their sizes, not the button.
	v := newEnv(t, twoServers+diskRules+"access: {viewer: open}\n")
	diskEnv(t, v)
	if _, vp := v.get("/grid"); !strings.Contains(vp, "1.3 TB") || strings.Contains(vp, "/grid/measure") {
		t.Error("a viewer gets the Measure button, or no sizes")
	}
}

// A server without a gate that measures sizes still shows its folders on
// the disk, without a size and without the button.
func TestGridFoldersNotInSyncthingWithoutGate(t *testing.T) {
	e := adminEnv(t, twoServers+diskRules)
	diskEnv(t, e)
	e.hub.Update("akka", func(s *model.Server) { s.Structure.Sizes, s.Structure.NoSizes = nil, true })
	e.eng.Evaluate()
	_, page := e.get("/grid")
	fresh := rowOf(t, page, "LIVE-NEW-2PROJECTFILES")
	if !strings.Contains(fresh, "▢</span> not in Syncthing</span>") || !strings.Contains(fresh, "The server has no syncwatch gate that measures sizes.") ||
		strings.Contains(fresh, "/grid/measure") {
		t.Errorf("without a gate:\n%s", fresh)
	}
}

// Measure: only an administrator, with the CSRF token, for enabled servers.
// It returns to the row with a message.
func TestMeasureButton(t *testing.T) {
	e := adminEnv(t, twoServers+diskRules)
	diskEnv(t, e)
	csrf := e.csrf("/settings")
	resp := e.post("/grid/measure", url.Values{"csrf": {csrf}, "back": {"/grid#r-abc"}, "server": {"sol"}, "path": {solAlpha}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/grid#r-abc" {
		t.Fatalf("answer %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if msg, isErr := flash(resp); isErr || msg != "The gate of SOL measures LIVE-ALPHA-2PROJECTFILES now. The size shows on this page when the measurement is complete." {
		t.Errorf("message %q (error %v)", msg, isErr)
	}
	e.mu.Lock()
	got := strings.Join(e.measured, ",")
	e.measured = nil
	e.measureErr = errors.New("the server is not up")
	e.mu.Unlock()
	if got != "sol|"+solAlpha {
		t.Errorf("measured %q", got)
	}

	resp = e.post("/grid/measure", url.Values{"csrf": {csrf}, "server": {"sol"}, "path": {solAlpha}})
	if msg, isErr := flash(resp); !isErr || !strings.Contains(msg, "SOL: the server is not up") || resp.Header.Get("Location") != "/grid" {
		t.Errorf("error message %q (error %v) to %q", msg, isErr, resp.Header.Get("Location"))
	}
	resp = e.post("/grid/measure", url.Values{"csrf": {csrf}, "server": {"nope"}, "path": {solAlpha}, "back": {"/settings"}})
	if msg, isErr := flash(resp); !isErr || !strings.Contains(msg, "not in Settings → Servers") || resp.Header.Get("Location") != "/grid" {
		t.Errorf("unknown server: %q (error %v) to %q", msg, isErr, resp.Header.Get("Location"))
	}
	if resp := e.post("/grid/measure", url.Values{"server": {"sol"}, "path": {solAlpha}}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("without the CSRF token: %d", resp.StatusCode)
	}
	if resp := e.post("/grid/measure", url.Values{"csrf": {csrf}, "server": {"sol", "akka"}, "path": {solAlpha}}); resp.StatusCode != http.StatusSeeOther {
		t.Errorf("incomplete request: %d", resp.StatusCode)
	} else if msg, isErr := flash(resp); !isErr || !strings.Contains(msg, "not complete") {
		t.Errorf("incomplete request: %q", msg)
	}

	v := newEnv(t, twoServers+diskRules+"access: {viewer: open}\n")
	if resp := v.post("/grid/measure", url.Values{"server": {"sol"}, "path": {solAlpha}}); resp.StatusCode == http.StatusSeeOther {
		t.Error("a viewer can start a measurement")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.measured) != 0 {
		t.Errorf("a viewer measured %v", v.measured)
	}
}

// Settings → Collector: the nightly time of the size measurement, "off",
// empty for the default, and a refused value that keeps what was typed.
func TestSettingsSizeTime(t *testing.T) {
	e := adminEnv(t, twoServers)
	csrf := e.csrf("/settings")
	col := url.Values{"csrf": {csrf}, "mode": {"events"}, "event_timeout": {"50s"}, "full_check_interval": {"4h"}, "poll_interval": {"15m"},
		"structure_interval": {"1h"}, "baseline_interval": {"7d"}, "request_timeout": {"30s"}, "concurrency": {"3"}, "down_after_failures": {"2"}}
	for in, want := range map[string]string{"3:15": "3:15", "Off": "off", "": "02:00"} {
		col.Set("size_time", in)
		if msg, isErr := flash(e.post("/settings/collector", col)); isErr || e.cfg.Get().Collector.SizeTime != want {
			t.Errorf("size_time %q: %q, stored %q", in, msg, e.cfg.Get().Collector.SizeTime)
		}
	}
	col.Set("size_time", "25:00")
	resp, body := e.postPage("/settings/collector", col)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Measure folder sizes each night at (collector.size_time)") ||
		!strings.Contains(body, `name="size_time" value="25:00" placeholder="02:00" aria-invalid="true"`) || e.cfg.Get().Collector.SizeTime != "02:00" {
		t.Errorf("size_time 25:00: %d, stored %q", resp.StatusCode, e.cfg.Get().Collector.SizeTime)
	}
	// A form without the input (a script of an older version) keeps the value.
	col.Del("size_time")
	if _, isErr := flash(e.post("/settings/collector", col)); isErr || e.cfg.Get().Collector.SizeTime != "02:00" {
		t.Errorf("without the input: stored %q", e.cfg.Get().Collector.SizeTime)
	}
}
