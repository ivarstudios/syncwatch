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
		!strings.Contains(fresh, `title="LIVE-NEW-2PROJECTFILES · not in Syncthing · on the disk of AKKA"`) {
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

	// ⟳ sits next to each grey box and measures that one folder on that one
	// server: never in the name column, and not while the gate measures it.
	form := regexp.MustCompile(`</span><span class="mslot"><form method="post" action="/grid/measure" class="measure">.*?</form></span></td>`)
	if strings.Contains(alpha, "/grid/measure") {
		t.Errorf("a ⟳ while the gate of SOL measures the folder:\n%s", alpha)
	}
	f := form.FindAllString(fresh, -1)
	if len(f) != 1 || !strings.Contains(f[0], `name="server" value="akka"`) || !strings.Contains(f[0], `name="path" value="`+akkaNew+`"`) ||
		!strings.Contains(f[0], `name="back" value="/grid#r-`) || !strings.Contains(f[0], `aria-label="Measure the size of LIVE-NEW-2PROJECTFILES on AKKA now"`) {
		t.Errorf("⟳ of the folder on the disk of AKKA: %q", f)
	}
	for _, row := range []string{alpha, fresh} {
		if th, _, _ := strings.Cut(row, "</th>"); strings.Contains(th, "/grid/measure") {
			t.Errorf("a ⟳ in the name column: %s", th)
		}
	}
	e.hub.Update("sol", func(s *model.Server) { s.Structure.Sizes[solAlpha].State = "" })
	e.eng.Evaluate()
	_, page = e.get("/grid")
	f = form.FindAllString(rowOf(t, page, "LIVE-ALPHA-2PROJECTFILES"), -1)
	if len(f) != 1 || !strings.Contains(f[0], `name="server" value="sol"`) || !strings.Contains(f[0], `name="path" value="`+solAlpha+`"`) || strings.Contains(f[0], akkaAlpha) {
		t.Errorf("⟳ of the folder on the disk of SOL, in the synced row: %q", f)
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
	if !strings.Contains(fresh, "▢</span> no gate</span><span class=\"mslot\"></span>") || !strings.Contains(fresh, "The server has no syncwatch gate that measures sizes.") ||
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
	for _, c := range [][2]string{{"3:15", "3:15"}, {"Off", "off"}, {"", "02:00"}} {
		in, want := c[0], c[1]
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

// Each cell shows a size: a synced folder its size, a folder that syncs the
// part that the server has, with the percentage in the tooltip. Each group
// has the same columns, so each device is in the same column on the whole
// page, and each cell has the slot for ⟳ so that the boxes stay in line.
func TestGridCellsShowSizesInLine(t *testing.T) {
	e := adminEnv(t, twoServers+diskRules)
	diskEnv(t, e)
	e.hub.Update("akka", func(s *model.Server) {
		s.Folders["alpha"].GlobalBytes = 412_400_000_000
		s.Folders["beta"] = &model.Folder{ID: "beta", Label: "LIVE-BETA-1SOURCE", Path: "/s/AKKA-LIVE-1SOURCE/LIVE-BETA-1SOURCE", State: "syncing",
			GlobalBytes: 400_000_000_000, NeedBytes: 280_000_000_000, NeedItems: 5}
	})
	e.eng.Evaluate()
	_, page := e.get("/grid")
	alpha := rowOf(t, page, "LIVE-ALPHA-2PROJECTFILES")
	if !strings.Contains(alpha, `<span class="cell c-ok" title="Up to date. 100%: 412 GB"><span aria-hidden="true">✔</span> 412 GB</span>`) {
		t.Errorf("synced folder:\n%s", alpha)
	}
	beta := rowOf(t, page, "LIVE-BETA-1SOURCE")
	if !strings.Contains(beta, `<span class="cell c-sync" title="syncing. 30%: 120 GB of 400 GB, 5 remaining items"><span aria-hidden="true">⟳</span> 120 GB</span>`) {
		t.Errorf("folder that syncs:\n%s", beta)
	}
	if got := progress(99.9, 412_400_000_000, 0); got != "99%: 412 GB" {
		t.Errorf("only deletions remain: %q", got)
	}

	// The same columns in each group, and a slot in each cell.
	colgroups := regexp.MustCompile(`<colgroup>.*?</colgroup>`).FindAllString(page, -1)
	if len(colgroups) < 2 {
		t.Fatalf("%d groups with a colgroup, want 2 or more", len(colgroups))
	}
	for _, cg := range colgroups {
		if cg != colgroups[0] || !strings.HasPrefix(cg, `<colgroup><col class="fcol"><col class="dcol">`) {
			t.Errorf("colgroup %q, want %q", cg, colgroups[0])
		}
	}
	if cells, slots := strings.Count(page, "<td"), strings.Count(page, `<span class="mslot">`); cells == 0 || cells != slots {
		t.Errorf("%d cells, %d slots for ⟳", cells, slots)
	}
}

// The rows and the groups of the Folders page are in alphabetical order,
// also with faults: a folder is where the eye looks for it. The faults show
// in the cells and on the Findings page.
func TestGridIsAlphabetical(t *testing.T) {
	e := adminEnv(t, twoServers+diskRules)
	diskEnv(t, e)
	e.hub.Update("akka", func(s *model.Server) {
		s.Folders["zulu"] = &model.Folder{ID: "zulu", Label: "LIVE-ZULU-2PROJECTFILES", Path: "/s/AKKA-LIVE-2PROJECTFILES/LIVE-ZULU-2PROJECTFILES", State: "idle", Paused: true}
		s.Folders["beta"] = &model.Folder{ID: "beta", Label: "LIVE-BETA-1SOURCE", Path: "/s/AKKA-LIVE-1SOURCE/LIVE-BETA-1SOURCE", State: "idle"}
	})
	e.eng.Evaluate()
	_, page := e.get("/grid")
	if !strings.Contains(rowOf(t, page, "LIVE-ZULU-2PROJECTFILES"), "has-problem") {
		t.Fatal("LIVE-ZULU-2PROJECTFILES has no fault")
	}
	wide, _, _ := strings.Cut(page, `<div class="grid-narrow">`)
	last := -1
	for _, label := range []string{"LIVE-BETA-1SOURCE", "LIVE-ALPHA-2PROJECTFILES", "LIVE-NEW-2PROJECTFILES", "LIVE-ZULU-2PROJECTFILES"} {
		i := strings.Index(wide, ">"+label+"</th>")
		if i <= last {
			t.Errorf("%s at %d, after %d: want the groups and their rows in alphabetical order", label, i, last)
		}
		last = i
	}
}

// The rows and the groups of the Folders page have ids. htmx gives a new
// element with the id of an old one its own classes again after a live
// update (the settle, after afterSwap): that dropped the classes of the
// filters, and each update showed all folders again. app.js filters again
// when the update is settled. The behaviour runs in the browser; this pins
// the shipped script.
func TestGridFilterSurvivesLiveUpdates(t *testing.T) {
	e := adminEnv(t, twoServers+diskRules)
	_, js := e.get("/assets/app.js")
	if !regexp.MustCompile(`"htmx:afterSettle", function \(\) \{\s*applyGridFilter\(\);\s*applySiteFilter\(\);`).MatchString(js) {
		t.Error("app.js does not filter again after the settle of a live update")
	}
}
