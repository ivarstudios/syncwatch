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
)

const (
	akkaGone     = "/s/AKKA-LIVE-2PROJECTFILES/LIVE-GONE-2PROJECTFILES"
	solNew       = "/z/SOL-LIVE-2PROJECTFILES/LIVE-NEW-2PROJECTFILES"
	akkaOldAlpha = "/s/AKKA-LIVE-2PROJECTFILES/LIVE-ALPHA-2PROJECTFILES-old/LIVE-ALPHA-2PROJECTFILES"
)

// deletedEnv is diskEnv with deleted folders (S8): LIVE-GONE on AKKA (it
// was 120 GB), in a row of its own; LIVE-NEW on SOL, in the row of the
// folder that AKKA has on its disk; and a LIVE-ALPHA on AKKA in another
// place, whose name has the row of the Syncthing folder of AKKA.
func deletedEnv(t *testing.T, e *env) {
	t.Helper()
	diskEnv(t, e)
	found := time.Now().Add(-90 * time.Minute)
	e.hub.Update("akka", func(s *model.Server) {
		s.Deleted = map[string]*model.DeletedDir{
			akkaGone: {Path: akkaGone, Name: "LIVE-GONE-2PROJECTFILES", Share: "/s/AKKA-LIVE-2PROJECTFILES", ShareName: "AKKA-LIVE-2PROJECTFILES", DetectedAt: found,
				Size: &model.DirSize{Bytes: 120_400_000_000, Files: 3412, Complete: true, MeasuredAt: found.Add(-20 * time.Hour)}},
			akkaOldAlpha: {Path: akkaOldAlpha, Name: "LIVE-ALPHA-2PROJECTFILES", Share: "/s/AKKA-LIVE-2PROJECTFILES", ShareName: "AKKA-LIVE-2PROJECTFILES", DetectedAt: found},
		}
	})
	e.hub.Update("sol", func(s *model.Server) {
		s.Deleted = map[string]*model.DeletedDir{
			solNew: {Path: solNew, Name: "LIVE-NEW-2PROJECTFILES", Share: "/z/SOL-LIVE-2PROJECTFILES", ShareName: "SOL-LIVE-2PROJECTFILES", DetectedAt: found, All: true},
		}
	})
	e.eng.Evaluate()
}

// A deleted folder is a yellow box with ✕ in the column of its server. The
// row and its group show also with "Only folders with faults", and the
// toggle of the folders not in Syncthing does not hide it. Only an
// administrator gets ✕, and ✕ asks before it removes the folder.
func TestGridShowsDeletedFolders(t *testing.T) {
	e := adminEnv(t, twoServers+diskRules)
	deletedEnv(t, e)
	_, page := e.get("/grid")

	gone := rowOf(t, page, "LIVE-GONE-2PROJECTFILES")
	if !strings.Contains(gone, `class="frow has-problem deleted"`) || strings.Contains(gone, "disk-only") {
		t.Errorf("the row of the deleted folder:\n%s", gone)
	}
	if !strings.Contains(gone, `<span class="cell c-deleted" title="Deleted on AKKA in AKKA-LIVE-2PROJECTFILES: found `) ||
		!strings.Contains(gone, "It was not in Syncthing: no other device has a copy. Last size 120 GB in 3412 files, measured ") ||
		!strings.Contains(gone, "restore the folder from the recycle bin or a backup of the server. Else select ✕ to remove it from syncwatch.") ||
		!strings.Contains(gone, `⚠</span> deleted</span>`) {
		t.Errorf("the yellow box:\n%s", gone)
	}
	form := regexp.MustCompile(`<form method="post" action="/grid/forget" class="measure" data-confirm="([^"]*)">.*?</form>`)
	f := form.FindAllStringSubmatch(gone, -1)
	if len(f) != 1 || !strings.Contains(f[0][0], `name="server" value="akka"`) || !strings.Contains(f[0][0], `name="path" value="`+akkaGone+`"`) ||
		!strings.Contains(f[0][0], `aria-label="Remove the deleted folder LIVE-GONE-2PROJECTFILES on AKKA from syncwatch">✕</button>`) ||
		f[0][1] != "Do you really want to remove the deleted folder LIVE-GONE-2PROJECTFILES on AKKA from syncwatch? syncwatch then stops reporting it. Nothing changes on the server." {
		t.Errorf("✕ of the deleted folder: %q", f)
	}
	if strings.Contains(gone, "/grid/measure") {
		t.Error("⟳ next to a deleted folder")
	}

	// In the row of the folder that AKKA has on its disk: grey on AKKA,
	// yellow on SOL; the share went with all its directories.
	fresh := rowOf(t, page, "LIVE-NEW-2PROJECTFILES")
	if !strings.Contains(fresh, `class="frow has-problem deleted"`) || !strings.Contains(fresh, "▢</span> 1.3 TB</span>") ||
		!strings.Contains(fresh, `title="Deleted on SOL in SOL-LIVE-2PROJECTFILES: found `) || !strings.Contains(fresh, "make sure that the share is mounted in the Syncthing container") ||
		len(form.FindAllString(fresh, -1)) != 1 || strings.Count(fresh, "/grid/measure") != 1 {
		t.Errorf("the row with a grey and a yellow box:\n%s", fresh)
	}

	// The cell of AKKA in the row of LIVE-ALPHA has the Syncthing folder:
	// the deleted LIVE-ALPHA gets a row of its own.
	if n := strings.Count(page, ">LIVE-ALPHA-2PROJECTFILES</th>"); n != 2 {
		t.Errorf("%d rows LIVE-ALPHA-2PROJECTFILES, want 2", n)
	}
	if !strings.Contains(page, `name="path" value="`+akkaOldAlpha+`"`) {
		t.Error("no ✕ for the deleted LIVE-ALPHA in its other place")
	}

	if !strings.Contains(page, "⚠ 3 deleted</span>") || strings.Contains(page, "✔ no faults") {
		t.Error("the group heading does not count the deleted folders, or says no faults")
	}
	if !strings.Contains(page, `<span class="c-warn">⚠ 2 deleted</span></summary>`) {
		t.Error("the list of AKKA does not count its deleted folders")
	}
	if cells, slots := strings.Count(page, "<td"), strings.Count(page, `<span class="mslot">`); cells != slots {
		t.Errorf("%d cells, %d slots", cells, slots)
	}

	// The findings: one for each folder, one for the share of SOL.
	n := 0
	for _, r := range e.eng.View().Open {
		if r.Check == "S8" {
			n++
			if !r.Urgent {
				t.Errorf("S8 not urgent: %s", r.Subject)
			}
		}
	}
	if n != 3 {
		t.Errorf("%d S8 findings, want 3", n)
	}

	// A viewer sees the yellow box, not ✕.
	v := newEnv(t, twoServers+diskRules+"access: {viewer: open}\n")
	deletedEnv(t, v)
	_, vp := v.get("/grid")
	if !strings.Contains(vp, `⚠</span> deleted</span>`) || strings.Contains(vp, "/grid/forget") || strings.Contains(vp, "select ✕") {
		t.Error("a viewer gets ✕, or no yellow box")
	}
}

// ✕: only an administrator, with the CSRF token. It returns to the row
// with a message.
func TestForgetButton(t *testing.T) {
	e := adminEnv(t, twoServers+diskRules)
	deletedEnv(t, e)
	csrf := e.csrf("/settings")
	resp := e.post("/grid/forget", url.Values{"csrf": {csrf}, "back": {"/grid#r-abc"}, "server": {"akka"}, "path": {akkaGone}})
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/grid#r-abc" {
		t.Fatalf("answer %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if msg, isErr := flash(resp); isErr || msg != "The deleted folder LIVE-GONE-2PROJECTFILES on AKKA is removed from syncwatch." {
		t.Errorf("message %q (error %v)", msg, isErr)
	}
	e.mu.Lock()
	got := strings.Join(e.forgotten, ",")
	e.forgotten = nil
	e.forgetErr = errors.New("the folder is not marked as deleted now: reload the page")
	e.mu.Unlock()
	if got != "akka|"+akkaGone {
		t.Errorf("removed %q", got)
	}

	resp = e.post("/grid/forget", url.Values{"csrf": {csrf}, "server": {"akka"}, "path": {akkaGone}, "back": {"/settings"}})
	if msg, isErr := flash(resp); !isErr || msg != "syncwatch cannot remove LIVE-GONE-2PROJECTFILES on AKKA: the folder is not marked as deleted now: reload the page." ||
		resp.Header.Get("Location") != "/grid" {
		t.Errorf("error message %q (error %v) to %q", msg, isErr, resp.Header.Get("Location"))
	}
	if msg, isErr := flash(e.post("/grid/forget", url.Values{"csrf": {csrf}, "server": {"akka"}})); !isErr || !strings.Contains(msg, "not complete") {
		t.Errorf("without a path: %q", msg)
	}
	if resp := e.post("/grid/forget", url.Values{"server": {"akka"}, "path": {akkaGone}}); resp.StatusCode != http.StatusForbidden {
		t.Errorf("without the CSRF token: %d", resp.StatusCode)
	}

	v := newEnv(t, twoServers+diskRules+"access: {viewer: open}\n")
	if resp := v.post("/grid/forget", url.Values{"server": {"akka"}, "path": {akkaGone}}); resp.StatusCode == http.StatusSeeOther {
		t.Error("a viewer can remove a deleted folder")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if len(v.forgotten) != 0 {
		t.Errorf("a viewer removed %v", v.forgotten)
	}
}
