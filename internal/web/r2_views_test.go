package web

import (
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
)

// r2vFullID matches a complete Syncthing device ID (8 groups of 7).
var r2vFullID = regexp.MustCompile(`[A-Z2-7]{7}(-[A-Z2-7]{7}){7}`)

const (
	r2vIDAkka    = "AKKAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA"
	r2vIDAnna    = "PCANNAA-BBBBBBB-CCCCCCC-DDDDDDD-EEEEEEE-FFFFFFF-GGGGGGG-HHHHHHH"
	r2vIDUnknown = "XUNKNWN-2222222-3333333-4444444-5555555-6666666-7777777-QQQQQQQ"
)

// r2vViewerGet fetches a page without a session: with open access, as a viewer.
func (e *env) r2vViewerGet(path string) string {
	e.t.Helper()
	resp, err := http.Get(e.srv.URL + path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		e.t.Fatalf("%s as a viewer: %d", path, resp.StatusCode)
	}
	return string(b)
}

// W-048 (c), C12: viewers (also with open access) see device IDs only in
// their short form, and no "Open in Syncthing" links. Administrators see
// everything as before.
func TestViewersSeeShortDeviceIDs(t *testing.T) {
	e := adminEnv(t, twoServers+"access: {viewer: open}\n")
	now := time.Now()
	e.hub.Update("akka", func(s *model.Server) {
		s.Status, s.Loaded, s.MyID, s.LastContact, s.GUIURL = model.StatusUp, true, r2vIDAkka, now, "https://akka.invalid:8384"
		s.Devices = map[string]*model.Device{r2vIDAkka: {ID: r2vIDAkka, Name: "AKKA"}, r2vIDAnna: {ID: r2vIDAnna, Name: "ANNA-PC"}}
		s.Connections = map[string]*model.Connection{r2vIDAnna: {Connected: true, StartedAt: now}}
		s.Folders = map[string]*model.Folder{"f1": {ID: "f1", Label: "projects", State: "idle", SharedWith: []string{r2vIDAnna},
			Completion: map[string]*model.Completion{r2vIDAnna: {Percent: 100, RemoteState: "valid"}}}}
		s.PendingDevices = []model.PendingDevice{{DeviceID: r2vIDUnknown, Address: "10.0.0.9:22000", Time: now}}
		s.PendingFolders = []model.PendingFolder{{FolderID: "f2", Label: "offered", OfferedBy: r2vIDAnna, Time: now}}
	})
	e.eng.Evaluate()

	// The administrator: the full IDs in the details and the column titles,
	// and the links to Syncthing.
	_, problems := e.get("/problems")
	for _, want := range []string{r2vIDUnknown, r2vIDAnna, "Open in Syncthing"} {
		if !strings.Contains(problems, want) {
			t.Errorf("administrator, /problems: no %q", want)
		}
	}
	if _, grid := e.get("/grid"); !strings.Contains(grid, `title="`+r2vIDAnna+`"`) || !strings.Contains(grid, `data-key="dev:`+r2vIDAnna+`"`) {
		t.Error("administrator, /grid: no full device ID in the column title")
	}
	if _, ov := e.get("/"); !strings.Contains(ov, `href="https://akka.invalid:8384"`) || !strings.Contains(ov, "Open in Syncthing") {
		t.Error("administrator, /: no link to Syncthing on the AKKA tile")
	}

	// A viewer: no complete device ID and no link to Syncthing on any page or
	// live update.
	for _, p := range []string{"/", "/problems", "/grid", "/frag/overview", "/frag/problems", "/frag/grid"} {
		body := e.r2vViewerGet(p)
		if m := r2vFullID.FindString(body); m != "" {
			t.Errorf("viewer, %s: full device ID %s", p, m)
		}
		if strings.Contains(body, "Open in Syncthing") || strings.Contains(body, "akka.invalid:8384") || strings.Contains(body, `href="https://b:1"`) {
			t.Errorf("viewer, %s: a link to Syncthing", p)
		}
	}
	vp := e.r2vViewerGet("/problems")
	for _, want := range []string{"<th>Device ID</th><td class=\"mono\">XUNKNWN…</td>", "<th>Offered by</th><td class=\"mono\">PCANNAA…</td>"} {
		if !strings.Contains(vp, want) {
			t.Errorf("viewer, /problems: no %q", want)
		}
	}
	if vg := e.r2vViewerGet("/grid"); !strings.Contains(vg, `title="PCANNAA…"`) || !strings.Contains(vg, `title="AKKAAAA…"`) {
		t.Error("viewer, /grid: the column titles do not show the short device IDs")
	}

	// The view models: only the administrator's form has the full values.
	vm := e.web.problems(e.eng.View(), problemFilter{}, RoleViewer)
	for _, r := range vm.Rows {
		if r.Link != "" {
			t.Errorf("viewer row %s has the link %q", r.Check, r.Link)
		}
	}
	if got := shortDeviceID(r2vIDAnna); got != "PCANNAA…" {
		t.Errorf("shortDeviceID = %q", got)
	}
	if got := shortDeviceID("PCANNAA"); got != "PCANNAA" {
		t.Errorf("shortDeviceID of a short value = %q, want it unchanged", got)
	}
}
