package rules

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/model"
)

// count gives the number of findings of a check.
func count(fs []model.Finding, check string) int {
	n := 0
	for _, f := range fs {
		if f.Check == check {
			n++
		}
	}
	return n
}

// rows gives the values of a detail key, in order.
func rows(f *model.Finding, key string) []string {
	var out []string
	for _, d := range f.Details {
		if d.Key == key {
			out = append(out, d.Value)
		}
	}
	return out
}

// W-046 (a): a share root that is a Syncthing folder is one finding. The
// Syncthing folders inside it are rows of S2, not one urgent S1 each. An S1
// between 2 folders that are not share roots stays.
func TestShareRootListsTheFoldersInside(t *testing.T) {
	c := ivarConfig(t)
	akka := server("akka", "AKKA", akkaID)
	sh := "/share/CACHEDEV1_DATA/AKKA-LIVE-4FINALS"
	akka.Folders["root"] = folder("root", "AKKA-LIVE-4FINALS", sh)
	akka.Folders["a"] = folder("a", "LIVE-AURORA-4FINALS", sh+"/LIVE-AURORA-4FINALS")
	akka.Folders["f"] = folder("f", "LIVE-FJORD-4FINALS", sh+"/LIVE-FJORD-4FINALS")
	akka.Folders["n"] = folder("n", "LIVE-NORRSKEN-4FINALS", sh+"/LIVE-NORRSKEN-4FINALS")
	akka.Folders["x"] = folder("x", "LIVE-X", "/share/CACHEDEV1_DATA/AKKA-LIVE-1SOURCE/LIVE-X")
	akka.Folders["y"] = folder("y", "LIVE-X inner", "/share/CACHEDEV1_DATA/AKKA-LIVE-1SOURCE/LIVE-X/inner")
	fs := run(t, c, akka)
	s2 := mustFind(t, fs, "S2", "AKKA-LIVE-4FINALS on AKKA")
	if count(fs, "S2") != 1 || count(fs, "S1") != 1 {
		t.Fatalf("want 1 S2 and the 1 S1 of LIVE-X, got:\n%s", dump(fs))
	}
	mustFind(t, fs, "S1", "LIVE-X inner inside LIVE-X")
	want := "Share root AKKA-LIVE-4FINALS on AKKA is a Syncthing folder (AKKA-LIVE-4FINALS). It contains 3 other Syncthing folders. Sync each project folder as a separate Syncthing folder"
	if s2.Message != want {
		t.Fatalf("message\n got %s\nwant %s", s2.Message, want)
	}
	inside := rows(s2, "Syncthing folder inside")
	if len(inside) != 3 || inside[0] != "a — "+sh+"/LIVE-AURORA-4FINALS" || inside[2] != "n — "+sh+"/LIVE-NORRSKEN-4FINALS" {
		t.Fatalf("rows %q", inside)
	}
	if s2.DetailValue("More folders inside") != "" || s2.DetailValue("Folder ID") != "root" || !s2.Urgent {
		t.Fatalf("S2 %+v", s2)
	}

	// One folder inside: the singular.
	for _, id := range []string{"f", "n"} {
		delete(akka.Folders, id)
	}
	if s2 = mustFind(t, run(t, c, akka), "S2", "AKKA-LIVE-4FINALS on AKKA"); !strings.Contains(s2.Message, "It contains 1 other Syncthing folder. Sync") {
		t.Fatalf("1 folder inside: %s", s2.Message)
	}

	// A share with 23 Syncthing folders inside: 20 rows and the count of the others.
	for i := 0; i < 22; i++ {
		id := fmt.Sprintf("p%02d", i)
		akka.Folders[id] = folder(id, "LIVE-P"+id, sh+"/LIVE-P"+id)
	}
	fs = run(t, c, akka)
	s2 = mustFind(t, fs, "S2", "AKKA-LIVE-4FINALS on AKKA")
	if n := len(rows(s2, "Syncthing folder inside")); n != 20 || s2.DetailValue("More folders inside") != "3" || !strings.Contains(s2.Message, "It contains 23 other Syncthing folders") {
		t.Fatalf("23 inside: %d rows, more %q, %s", n, s2.DetailValue("More folders inside"), s2.Message)
	}
	if count(fs, "S1") != 1 {
		t.Fatalf("S1 for folders inside the share root:\n%s", dump(fs))
	}

	// Without S2, S1 reports the folders inside the share root as before.
	off := false
	c.Checks["S2"] = config.Check{Enabled: &off}
	fs = run(t, c, akka)
	if count(fs, "S2") != 0 || count(fs, "S1") != 24 {
		t.Fatalf("S2 disabled: want 24 S1, got:\n%s", dump(fs))
	}
}

// W-046 (a): a full disk of Syncthing's database stops every folder of the
// server. That is 1 urgent H4 for the server that lists the folders, not 1
// for each folder. A folder with another error gets its own H4 as well.
func TestDatabaseDiskIsOneFinding(t *testing.T) {
	c := ivarConfig(t)
	sol := server("sol", "SOL", solID)
	dbErr := "insufficient space on disk for database (/var/syncthing/config/index-v2): current 89.54 % < required 99.9 %"
	first := now.Add(-3 * time.Minute)
	for i, l := range []string{"LIVE-AURORA-1SOURCE", "LIVE-AURORA-2PROJECTFILES", "LIVE-FJORD-1SOURCE", "LIVE-FJORD-2PROJECTFILES", "LIVE-FJORD-4FINALS"} {
		id := strings.ToLower(l)
		f := folder(id, l, "/s/SOL-LIVE/"+l)
		f.State, f.Error, f.StateChanged = "error", dbErr, first.Add(time.Duration(i)*time.Second)
		f.NeedItems = 2
		sol.Folders[id] = f
	}
	fs := run(t, c, sol)
	h4 := mustFind(t, fs, "H4", "Syncthing database on SOL")
	if count(fs, "H4") != 1 || count(fs, "H5") != 0 {
		t.Fatalf("want 1 H4 and no H5, got:\n%s", dump(fs))
	}
	want := "Syncthing on SOL stopped 5 folders: the disk of its database is full (" + dbErr + ")"
	if h4.Message != want || h4.Subject != "Syncthing database on SOL" {
		t.Fatalf("message\n got %s\nwant %s", h4.Message, want)
	}
	if h4.ID != model.FindingID("H4", "sol", "database-disk") || h4.Server != "sol" || !h4.Urgent || h4.Severity != model.Error || h4.MinDuration != 0 || !h4.Since.Equal(first) {
		t.Fatalf("H4 %+v", h4)
	}
	if got := h4.DetailValue("Stopped folders"); got != "LIVE-AURORA-1SOURCE, LIVE-AURORA-2PROJECTFILES, LIVE-FJORD-1SOURCE, LIVE-FJORD-2PROJECTFILES, LIVE-FJORD-4FINALS" {
		t.Fatalf("stopped folders %q", got)
	}
	if h4.DetailValue("Folder IDs") != "live-aurora-1source, live-aurora-2projectfiles, live-fjord-1source, live-fjord-2projectfiles, live-fjord-4finals" || h4.DetailValue("Error") != dbErr {
		t.Fatalf("details %v", h4.Details)
	}

	// A folder that also has failed items gets its own H4 for them.
	f := sol.Folders["live-fjord-4finals"]
	f.PullErrors = 2
	f.FileErrors = []model.FileError{{Path: "a.blend", Error: "permission denied"}}
	fs = run(t, c, sol)
	own := mustFind(t, fs, "H4", "Folder LIVE-FJORD-4FINALS on SOL cannot sync")
	if own.Message != "Folder LIVE-FJORD-4FINALS on SOL cannot sync: 2 failed items" || own.MinDuration != time.Hour || !own.Since.IsZero() {
		t.Fatalf("own H4: %+v", own)
	}
	if count(fs, "H4") != 2 || !strings.Contains(mustFind(t, fs, "H4", "database").Message, "stopped 5 folders") {
		t.Fatalf("want the database H4 and 1 folder H4:\n%s", dump(fs))
	}

	// More than 20 folders: 20 labels and the count of the others.
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("x%02d", i)
		g := folder(id, "LIVE-X"+id, "/s/SOL-LIVE/LIVE-X"+id)
		g.State, g.Error = "error", dbErr
		sol.Folders[id] = g
	}
	h4 = mustFind(t, run(t, c, sol), "H4", "Syncthing database on SOL")
	if labels := h4.DetailValue("Stopped folders"); !strings.HasSuffix(labels, ", …and 5 more") || strings.Count(labels, ", ") != 20 || strings.Count(h4.DetailValue("Folder IDs"), ", ") != 24 {
		t.Fatalf("25 folders: %q", labels)
	}
}

// W-046 (orchestrator DECISION): a pull that Syncthing refuses for lack of
// space in the folder (minDiskFree) is a failed item with that error. H4
// opens at once with the reason "out of space", not after failing_files.
func TestOutOfSpaceInFolderOpensAtOnce(t *testing.T) {
	c := ivarConfig(t)
	sol := server("sol", "SOL", solID)
	f := folder("a", "LIVE-AURORA-2PROJECTFILES", "/s/SOL-LIVE-2PROJECTFILES/LIVE-AURORA-2PROJECTFILES")
	f.PullErrors = 1
	f.FileErrors = []model.FileError{{Path: "render.mov", Error: "insufficient space in folder /s/SOL-LIVE-2PROJECTFILES/LIVE-AURORA-2PROJECTFILES: current 12.3 % < required 99.9 %"}}
	sol.Folders["a"] = f
	h4 := mustFind(t, run(t, c, sol), "H4", "LIVE-AURORA-2PROJECTFILES")
	if h4.Message != "Folder LIVE-AURORA-2PROJECTFILES on SOL cannot sync: out of space · 1 failed item" || h4.MinDuration != 0 || !h4.Urgent {
		t.Fatalf("out of space in a folder: %+v", h4)
	}
	// Other failed items keep the wait.
	f.FileErrors[0].Error = "permission denied"
	if h4 = mustFind(t, run(t, c, sol), "H4", "LIVE-AURORA-2PROJECTFILES"); h4.MinDuration != time.Hour || strings.Contains(h4.Message, "out of space") {
		t.Fatalf("failed item: %+v", h4)
	}
}

// W-047 (a): a device that always connects through a relay (devices[].relay)
// gets no H11. H12 opens when it has no connection to any server that is up
// for relay_lost. A device without the flag keeps H11.
func TestRelayDevice(t *testing.T) {
	c := ivarConfig(t)
	c.Timezone = "Europe/Stockholm"
	c.Devices = []config.Device{{ID: pcID, Relay: true}}
	akka := server("akka", "AKKA", akkaID)
	sol := server("sol", "SOL", solID)
	for _, sv := range []*model.Server{akka, sol} {
		sv.Devices[pcID] = &model.Device{ID: pcID, Name: "LAPTOP-DANI", LastSeen: now}
		sv.Connections[pcID] = &model.Connection{Connected: true, Type: "relay-client", Address: "relay://10.115.1.60:22067"}
		sv.Devices[pc2ID] = &model.Device{ID: pc2ID, Name: "VENUS", LastSeen: now}
		sv.Connections[pc2ID] = &model.Connection{Connected: true, Type: "relay-client", Address: "relay://10.115.1.60:22067"}
	}
	fs := run(t, c, akka, sol)
	mustNot(t, fs, "H11", "LAPTOP-DANI")
	mustNot(t, fs, "H12", "")
	if count(fs, "H11") != 2 {
		t.Fatalf("VENUS has no flag: want its 2 H11, got:\n%s", dump(fs))
	}

	// The relay stops: the laptop has no connection.
	last := now.Add(-34 * time.Minute)
	for _, sv := range []*model.Server{akka, sol} {
		sv.Connections[pcID].Connected = false
		sv.Devices[pcID].LastSeen = last
	}
	fs = run(t, c, akka, sol)
	h12 := mustFind(t, fs, "H12", "LAPTOP-DANI")
	want := "Device LAPTOP-DANI has no connection for more than 30 minutes (last seen 2026-10-05 13:26). It connects only through a relay"
	if h12.Message != want {
		t.Fatalf("message\n got %s\nwant %s", h12.Message, want)
	}
	if h12.ID != model.FindingID("H12", "", "relay-device:"+pcID) || h12.Server != "" || strings.Join(h12.Related, ",") != "akka,sol" ||
		!h12.Since.Equal(last) || h12.MinDuration != 30*time.Minute || h12.Urgent || h12.Severity != model.Warn || h12.Subject != "LAPTOP-DANI" {
		t.Fatalf("H12 %+v", h12)
	}
	if h12.DetailValue("Device ID") != pcID || h12.DetailValue("Last seen") != "2026-10-05T11:26:00Z" || h12.DetailValue("Possible causes") != "The relay is off or unreachable, or the device is off. H12 for the servers shows a relay that is off." {
		t.Fatalf("details %v", h12.Details)
	}
	mustNot(t, fs, "H11", "LAPTOP-DANI")
	mustNot(t, fs, "H7", "LAPTOP-DANI")

	// Connected to one server again (also without the relay): no H12.
	sol.Connections[pcID] = &model.Connection{Connected: true, Type: "tcp-client"}
	mustNot(t, run(t, c, akka, sol), "H12", "")
	// The connection of a server that is down is old data.
	sol.Status = model.StatusDown
	mustFind(t, run(t, c, akka, sol), "H12", "LAPTOP-DANI")
	// A device that was never seen gets no H12 (C2 reports it).
	sol.Status = model.StatusUp
	sol.Connections[pcID].Connected = false
	akka.Devices[pcID].LastSeen, sol.Devices[pcID].LastSeen = time.Time{}, time.Time{}
	mustNot(t, run(t, c, akka, sol), "H12", "")
	// Without the flag: no H12.
	akka.Devices[pcID].LastSeen = last
	c.Devices = nil
	mustNot(t, run(t, c, akka, sol), "H12", "")
}

// W-047 (b): H12 for a server whose relay listener has no connection to the
// relay, after relay_lost. A connected listener or a server that is down
// gives none.
func TestRelayListener(t *testing.T) {
	c := ivarConfig(t)
	akka := server("akka", "AKKA", akkaID)
	since := now.Add(-31 * time.Minute)
	refused := "dial tcp 10.115.1.60:22067: connect: connection refused"
	akka.RelayListeners = []model.RelayListener{
		{URI: "relay://10.115.1.60:22067", Error: refused, Since: since},
		{URI: "dynamic+https://relays.syncthing.net/endpoint", Connected: true, Since: since},
	}
	fs := run(t, c, akka)
	h12 := mustFind(t, fs, "H12", "10.115.1.60:22067 on AKKA")
	if count(fs, "H12") != 1 || h12.Message != "Server AKKA has no connection to its relay 10.115.1.60:22067: "+refused {
		t.Fatalf("H12:\n%s", dump(fs))
	}
	if h12.ID != model.FindingID("H12", "akka", "relay-listener:relay://10.115.1.60:22067") || h12.Server != "akka" || !h12.Since.Equal(since) ||
		h12.MinDuration != 30*time.Minute || h12.Urgent || h12.Severity != model.Warn {
		t.Fatalf("H12 %+v", h12)
	}
	if h12.DetailValue("Relay") != "relay://10.115.1.60:22067" || h12.DetailValue("Error") != refused || !strings.Contains(h12.DetailValue("Possible causes"), "Devices that connect only through this relay cannot sync") {
		t.Fatalf("details %v", h12.Details)
	}
	// The dynamic relay pool, without an error text.
	akka.RelayListeners[1].Connected = false
	if d := mustFind(t, run(t, c, akka), "H12", "relays.syncthing.net on AKKA"); d.Message != "Server AKKA has no connection to its relay relays.syncthing.net" {
		t.Fatalf("dynamic: %s", d.Message)
	}
	// Connected: none.
	akka.RelayListeners = []model.RelayListener{{URI: "relay://10.115.1.60:22067", Connected: true, Since: since}}
	mustNot(t, run(t, c, akka), "H12", "")
	// The listener state of a server that is down is old data.
	akka.RelayListeners[0].Connected = false
	akka.Status, akka.LastError = model.StatusDown, "timeout"
	mustNot(t, run(t, c, akka), "H12", "")
}

// W-057 (c): with the sites set, H1 for a network failure names the site of
// the server and the site of syncwatch, and the VPN between them; H2 names
// the 2 sites. Without sites, the texts of r1 stay. When another server is
// connected to the unreachable server, a row says so.
func TestSiteTexts(t *testing.T) {
	c := ivarConfig(t)
	akka := server("akka", "AKKA", akkaID)
	sol := server("sol", "SOL", solID)
	akka.Devices[solID] = &model.Device{ID: solID, Name: "SOL", LastSeen: now.Add(-time.Hour)}
	sol.Devices[akkaID] = &model.Device{ID: akkaID, Name: "AKKA", LastSeen: now.Add(-time.Hour)}
	h2 := mustFind(t, run(t, c, akka, sol), "H2", "AKKA ↔ SOL")
	if got := h2.DetailValue("What to check"); got != "The VPN or network between the servers, and Syncthing on both servers." {
		t.Fatalf("H2 without sites: %q", got)
	}
	c.Site = "IVAR Office"
	c.Servers[0].Site, c.Servers[1].Site = "IVAR Office", "Branch office"
	h2 = mustFind(t, run(t, c, akka, sol), "H2", "AKKA ↔ SOL")
	if got := h2.DetailValue("What to check"); got != "The VPN between IVAR Office and Branch office, and Syncthing on both servers." {
		t.Fatalf("H2 with sites: %q", got)
	}
	if h2.Message != "Servers AKKA and SOL are not connected to each other" {
		t.Fatalf("H2 message: %s", h2.Message)
	}

	// SOL is unreachable from syncwatch at IVAR Office.
	sol.Status, sol.LastError, sol.DownSince, sol.LastContact = model.StatusDown, "timeout", now.Add(-10*time.Minute), now.Add(-11*time.Minute)
	h1 := mustFind(t, run(t, c, akka, sol), "H1", "SOL")
	if h1.Message != "Server SOL (Branch office) is unreachable from IVAR Office: timeout" {
		t.Fatalf("H1 with sites: %s", h1.Message)
	}
	if got := h1.DetailValue("Possible causes"); got != "The VPN between IVAR Office and Branch office is down, or the server is off or does not answer." {
		t.Fatalf("causes with sites: %q", got)
	}
	if h1.DetailValue("What AKKA sees") != "" {
		t.Fatal("AKKA is not connected to SOL: no row")
	}
	// AKKA still has a connection to SOL: Syncthing on SOL runs.
	akka.Connections[solID] = &model.Connection{Connected: true, Type: "tcp-client"}
	h1 = mustFind(t, run(t, c, akka, sol), "H1", "SOL")
	if got := h1.DetailValue("What AKKA sees"); got != "AKKA is connected to SOL. Thus Syncthing on SOL runs, and the network path from syncwatch to SOL is down." {
		t.Fatalf("row: %q", got)
	}
	// In the first minutes after the last contact, Syncthing can still keep
	// a connection whose network path is down (a silent cut of the VPN: up
	// to 7.5 minutes on the kit).
	sol.LastContact = now.Add(-8 * time.Minute)
	if f := mustFind(t, run(t, c, akka, sol), "H1", "Server SOL"); f.DetailValue("What AKKA sees") != "" {
		t.Fatal("a row 8 minutes after the last contact")
	}
	sol.LastContact = now.Add(-11 * time.Minute)
	// A refused connection can come from SOL itself (its gate is down): the
	// row does not say that the network path is down.
	sol.LastError = "connection refused"
	if got := mustFind(t, run(t, c, akka, sol), "H1", "Server SOL").DetailValue("What AKKA sees"); got != "AKKA is connected to SOL. Thus Syncthing on SOL runs, and syncwatch cannot connect to the API address of SOL." {
		t.Fatalf("row for a refused connection: %q", got)
	}
	sol.LastError = "timeout"
	// The connections of a server that is down are old data.
	akka.Status = model.StatusDown
	if f := mustFind(t, run(t, c, akka, sol), "H1", "Server SOL"); f.DetailValue("What AKKA sees") != "" {
		t.Fatal("a row from a server that is down")
	}
	akka.Status = model.StatusUp

	// syncwatch runs at the site of SOL: no "from".
	c.Site = "Branch office"
	if h1 = mustFind(t, run(t, c, akka, sol), "H1", "SOL"); h1.Message != "Server SOL is unreachable: timeout" || !strings.Contains(h1.DetailValue("Possible causes"), "(for example the VPN between the sites)") {
		t.Fatalf("same site: %s %v", h1.Message, h1.Details)
	}
	// No site of syncwatch: the texts of r1.
	c.Site = ""
	if h1 = mustFind(t, run(t, c, akka, sol), "H1", "SOL"); h1.Message != "Server SOL is unreachable: timeout" {
		t.Fatalf("no site of syncwatch: %s", h1.Message)
	}
	// A refused API key is not a network failure: no site text, no row.
	c.Site = "IVAR Office"
	sol.LastError = "/rest/system/status: HTTP 403 (check the API key)"
	if h1 = mustFind(t, run(t, c, akka, sol), "H1", "SOL"); h1.Message != "Server SOL refuses the API key or the gate token: "+sol.LastError || h1.DetailValue("What AKKA sees") != "" {
		t.Fatalf("refused: %s %v", h1.Message, h1.Details)
	}
}

// W-058 (b): H5 for a device names the files that it still needs, from the
// list that the collector reads from the server (Server.NeedLists).
func TestStuckDeviceNamesNeededFiles(t *testing.T) {
	c := ivarConfig(t)
	c.Timezone = "Europe/Stockholm"
	akka := server("akka", "AKKA", akkaID)
	akka.Devices[pcID] = &model.Device{ID: pcID, Name: "PC-SHARED-ANNA", LastSeen: now}
	akka.Connections[pcID] = &model.Connection{Connected: true}
	f := folder("f", "LIVE-FJORD-4FINALS", "/s/AKKA-LIVE-4FINALS/LIVE-FJORD-4FINALS", pcID)
	f.Completion[pcID] = &model.Completion{Percent: 99.99, NeedItems: 18, NeedBytes: 28, RemoteState: "valid", UpdatedAt: now.Add(-time.Hour)}
	akka.Folders["f"] = f
	h5 := mustFind(t, run(t, c, akka), "H5", "on PC-SHARED-ANNA")
	if h5.DetailValue("Needed files") != "" {
		t.Fatalf("no list: no row, got %v", h5.Details)
	}
	msg := h5.Message

	akka.NeedLists = map[string]map[string]model.NeedList{"f": {pcID: {Names: []string{"aux.txt", "final:cut.mov", "notes."}, Items: 18, At: now.Add(-30 * time.Minute)}}}
	h5 = mustFind(t, run(t, c, akka), "H5", "on PC-SHARED-ANNA")
	if got := h5.DetailValue("Needed files"); got != "aux.txt · final:cut.mov · notes. · …and 15 more" {
		t.Fatalf("row %q", got)
	}
	if h5.Message != msg {
		t.Fatalf("the message changed: %s", h5.Message)
	}
	// A list older than the completion gives its time in the configured zone.
	f.Completion[pcID].UpdatedAt = now
	if got := mustFind(t, run(t, c, akka), "H5", "on PC-SHARED-ANNA").DetailValue("Needed files"); got != "aux.txt · final:cut.mov · notes. · …and 15 more (list from 13:30)" {
		t.Fatalf("older list: %q", got)
	}
	// A list from the day before gives the date too; all items named: no "more".
	akka.NeedLists["f"][pcID] = model.NeedList{Names: []string{"aux.txt", "final:cut.mov", "notes."}, Items: 3, At: now.Add(-25 * time.Hour)}
	if got := mustFind(t, run(t, c, akka), "H5", "on PC-SHARED-ANNA").DetailValue("Needed files"); got != "aux.txt · final:cut.mov · notes. (list from 2026-10-04 13:00)" {
		t.Fatalf("list of the day before: %q", got)
	}
	// The device is away: the held H5 keeps the row.
	akka.Connections[pcID].Connected = false
	f.Completion[pcID].RemoteState = "unknown"
	akka.RemoteStates = map[string]map[string]string{"f": {pcID: "valid"}}
	if h5 = mustFind(t, run(t, c, akka), "H5", "on PC-SHARED-ANNA"); !h5.Held || !strings.HasPrefix(h5.DetailValue("Needed files"), "aux.txt · final:cut.mov · notes.") {
		t.Fatalf("held: %+v", h5)
	}
}

// samplesAt gives a folder and device the progress samples of a sync that
// needs need bytes now and received perMinute bytes each minute for the
// last minutes.
func samplesAt(sv *model.Server, folderID, dev string, need, global, perMinute int64, minutes int) {
	for i := minutes; i >= 0; i-- {
		sv.AddProgress(folderID, dev, model.ProgressSample{At: now.Add(-time.Duration(i) * time.Minute), NeedBytes: need + int64(i)*perMinute, GlobalBytes: global})
	}
}

// W-059 (b): H5 keeps its time rule and shows the progress of the sync:
// the bytes received, the speed over the last 10 minutes and the time left.
// The values are rounded, thus 2 evaluations 30 seconds apart give the same
// message.
func TestStuckSyncShowsProgress(t *testing.T) {
	c := ivarConfig(t)
	akka := server("akka", "AKKA", akkaID)
	// AKKA receives 2.15 GB at 2.4 MB each second: 590 MB are left.
	f := folder("n", "LIVE-NORRSKEN-2PROJECTFILES", "/s/AKKA-LIVE-2PROJECTFILES/LIVE-NORRSKEN-2PROJECTFILES")
	f.State, f.NeedItems, f.NeedBytes, f.GlobalBytes = "syncing", 1, 590_000_000, 2_150_000_000
	akka.Folders["n"] = f
	samplesAt(akka, "n", "", 590_000_000, 2_150_000_000, 144_000_000, 10)
	h5 := mustFind(t, run(t, c, akka), "H5", "Folder LIVE-NORRSKEN-2PROJECTFILES on AKKA")
	want := "Folder LIVE-NORRSKEN-2PROJECTFILES on AKKA did not finish the sync in 24 hours (syncing, 1 remaining item, 1.6 of 2.1 GB received, approximately 4 minutes remaining)"
	if h5.Message != want {
		t.Fatalf("message\n got %s\nwant %s", h5.Message, want)
	}
	if h5.DetailValue("Received") != "1.6 of 2.1 GB" || h5.DetailValue("Speed") != "2.4 MB each second" || h5.DetailValue("Time left") != "approximately 4 minutes" {
		t.Fatalf("rows %v", h5.Details)
	}
	if h5.DetailValue("Bytes left") != "590000000" || h5.DetailValue("Items left") != "1" || h5.MinDuration != 24*time.Hour {
		t.Fatalf("the time rule and the machine values stay: %+v", h5)
	}
	// 30 seconds later 72 MB more arrived (no new sample in less than 1 minute).
	f.NeedBytes = 518_000_000
	later := Evaluate(Input{Snapshot: &model.Snapshot{At: now.Add(30 * time.Second), Servers: []*model.Server{akka}}, Config: c, Now: now.Add(30 * time.Second)})
	h5b := mustFind(t, later, "H5", "Folder LIVE-NORRSKEN-2PROJECTFILES on AKKA")
	if h5b.Message != h5.Message || h5b.DetailValue("Speed") != h5.DetailValue("Speed") || h5b.DetailValue("Time left") != h5.DetailValue("Time left") || h5b.DetailValue("Received") != h5.DetailValue("Received") {
		t.Fatalf("30 seconds later:\n%s %v\n%s %v", h5.Message, h5.Details, h5b.Message, h5b.Details)
	}

	// The PC view: the completion of the device and its samples.
	akka.Devices[pcID] = &model.Device{ID: pcID, Name: "LAPTOP-DANI", LastSeen: now}
	akka.Connections[pcID] = &model.Connection{Connected: true, Type: "relay-client"}
	g := folder("g", "LIVE-FJORD-4FINALS", "/s/AKKA-LIVE-4FINALS/LIVE-FJORD-4FINALS", pcID)
	g.Completion[pcID] = &model.Completion{Percent: 72.55, NeedItems: 3, NeedBytes: 59_000_000_000, GlobalBytes: 215_000_000_000, RemoteState: "valid"}
	akka.Folders["g"] = g
	samplesAt(akka, "g", pcID, 59_000_000_000, 215_000_000_000, 30_000_000, 15) // 0.5 MB each second
	h5 = mustFind(t, run(t, c, akka), "H5", "on LAPTOP-DANI")
	want = "Device LAPTOP-DANI is stuck at 72.5% of folder LIVE-FJORD-4FINALS for more than 24 hours (3 remaining items, approximately 33 hours remaining, seen from AKKA)"
	if h5.Message != want {
		t.Fatalf("device message\n got %s\nwant %s", h5.Message, want)
	}
	if h5.DetailValue("Speed") != "0.5 MB each second" || h5.DetailValue("Time left") != "approximately 33 hours" || h5.DetailValue("Received") != "160 of 220 GB" {
		t.Fatalf("device rows %v", h5.Details)
	}
	// Away: the held H5 shows what it received, but no speed.
	akka.Connections[pcID].Connected = false
	g.Completion[pcID].RemoteState = "unknown"
	akka.LastCompletions = map[string]map[string]model.Completion{"g": {pcID: {Percent: 72.55, NeedItems: 3, NeedBytes: 59_000_000_000, GlobalBytes: 215_000_000_000, RemoteState: "valid"}}}
	if h5 = mustFind(t, run(t, c, akka), "H5", "on LAPTOP-DANI"); !h5.Held || h5.DetailValue("Speed") != "" || h5.DetailValue("Time left") != "" || h5.DetailValue("Received") != "160 of 220 GB" || strings.Contains(h5.Message, "remaining,") {
		t.Fatalf("held: %s %v", h5.Message, h5.Details)
	}

	// No data arrived for 12 minutes (no progress events while the sync
	// waits): speed 0 and no time left.
	f.NeedBytes = 590_000_000
	stalled := Evaluate(Input{Snapshot: &model.Snapshot{At: now.Add(12 * time.Minute), Servers: []*model.Server{akka}}, Config: c, Now: now.Add(12 * time.Minute)})
	h5 = mustFind(t, stalled, "H5", "Folder LIVE-NORRSKEN-2PROJECTFILES on AKKA")
	if h5.DetailValue("Speed") != "0 (no data arrived in the last 10 minutes)" || h5.DetailValue("Time left") != "" || !strings.HasSuffix(h5.Message, "(syncing, 1 remaining item, 1.6 of 2.1 GB received)") {
		t.Fatalf("stalled: %s %v", h5.Message, h5.Details)
	}

	// Samples of less than 10 minutes: no speed. No size: no Received.
	akka.Progress = nil
	samplesAt(akka, "n", "", 518_000_000, 2_150_000_000, 144_000_000, 5)
	f.GlobalBytes = 0
	h5 = mustFind(t, run(t, c, akka), "H5", "Folder LIVE-NORRSKEN-2PROJECTFILES on AKKA")
	if h5.DetailValue("Speed") != "" || h5.DetailValue("Time left") != "" || h5.DetailValue("Received") != "" || h5.Message != "Folder LIVE-NORRSKEN-2PROJECTFILES on AKKA did not finish the sync in 24 hours (syncing, 1 remaining item)" {
		t.Fatalf("no speed, no size: %s %v", h5.Message, h5.Details)
	}
}

// The rounding of the progress texts of H5.
func TestProgressTexts(t *testing.T) {
	for _, tc := range []struct {
		v    float64
		want string
	}{{2.4, "2.4"}, {2.15, "2.1"}, {1.56, "1.6"}, {0.59, "0.59"}, {9.96, "10"}, {12.3, "12"}, {590, "590"}, {1234, "1200"}, {1, "1"}, {0.0512, "0.051"}, {0, "0"}} {
		if got := sig2(tc.v); got != tc.want {
			t.Errorf("sig2(%v) = %q, want %q", tc.v, got, tc.want)
		}
	}
	for _, tc := range []struct {
		secs float64
		want string
	}{{10, "less than 1 minute"}, {60, "approximately 1 minute"}, {245.8, "approximately 4 minutes"}, {7100, "approximately 118 minutes"}, {7200, "approximately 2 hours"}, {47 * 3600, "approximately 47 hours"}, {3 * 86400, "approximately 3 days"}} {
		if got := timeLeft(tc.secs); got != tc.want {
			t.Errorf("timeLeft(%v) = %q, want %q", tc.secs, got, tc.want)
		}
	}
	if got := sizeOf(12_000_000, 590_000_000); got != "12 of 590 MB" {
		t.Errorf("sizeOf: %q", got)
	}
	if got := rate(16_384); got != "0.016 MB each second" {
		t.Errorf("rate: %q", got)
	}
}

// W-061 (b): an INFO hint when the certificate of a server expires in less
// than 30 days or expired. syncwatch continues to read the server.
func TestCertificateExpiresSoon(t *testing.T) {
	c := ivarConfig(t)
	c.Timezone = "Europe/Stockholm"
	akka := server("akka", "AKKA", akkaID)
	akka.Cert = model.CertInfo{Fingerprint: "b6cfed46a1b2c3d4e5f60718293a4b5c6d7e8f90", NotAfter: now.Add(10*24*time.Hour + time.Hour)}
	fs := run(t, c, akka)
	h13 := mustFind(t, fs, "H13", "AKKA")
	if want := "The certificate of server AKKA expires on 2026-10-15 (in 10 days). syncwatch continues to read the server"; h13.Message != want {
		t.Fatalf("message\n got %s\nwant %s", h13.Message, want)
	}
	if h13.ID != model.FindingID("H13", "akka", "cert-expiry") || h13.Server != "akka" || h13.Subject != "AKKA" || h13.Severity != model.Info || h13.Urgent || h13.MinDuration != 0 {
		t.Fatalf("H13 %+v", h13)
	}
	if h13.DetailValue("Certificate expires") != "2026-10-15T13:00:00Z" || h13.DetailValue("Fingerprint") != "B6:CF:ED:46:A1:B2:C3:D4" ||
		h13.DetailValue("What to do") != "Make a new certificate for the server or its gate. Then accept the new certificate in Settings → Servers." {
		t.Fatalf("details %v", h13.Details)
	}
	// 40 days: no hint.
	akka.Cert.NotAfter = now.Add(40 * 24 * time.Hour)
	mustNot(t, run(t, c, akka), "H13", "")
	// Expired.
	akka.Cert.NotAfter = now.Add(-2 * 24 * time.Hour)
	h13 = mustFind(t, run(t, c, akka), "H13", "AKKA")
	if want := "The certificate of server AKKA expired on 2026-10-03. syncwatch continues to read the server, because the fingerprint is the pinned fingerprint"; h13.Message != want {
		t.Fatalf("expired\n got %s\nwant %s", h13.Message, want)
	}
	// Not known, or a server that syncwatch does not read now: no hint.
	akka.Status = model.StatusDown
	mustNot(t, run(t, c, akka), "H13", "")
	akka.Status, akka.Cert.NotAfter = model.StatusUp, time.Time{}
	mustNot(t, run(t, c, akka), "H13", "")
}

// W-021: a project that nobody changed after the copy has no pulled file and
// no local change. C1 uses the time when syncwatch first saw the folder (the
// earliest of the servers). The message gives the date and the time in the
// configured zone; Since is zero.
func TestInactiveProjectFirstSeen(t *testing.T) {
	c := ivarConfig(t)
	c.Timezone = "Europe/Stockholm"
	akka := server("akka", "AKKA", akkaID)
	sol := server("sol", "SOL", solID)
	a := folder("ice", "LIVE-ORION2026-1SOURCE", "/s/AKKA-LIVE-1SOURCE/LIVE-ORION2026-1SOURCE")
	a.FirstSeen = now.Add(-200 * 24 * time.Hour)
	s := folder("ice", "LIVE-ORION2026-1SOURCE", "/s/SOL-LIVE-1SOURCE/LIVE-ORION2026-1SOURCE")
	s.FirstSeen = now.Add(-150 * 24 * time.Hour)
	akka.Folders["ice"], sol.Folders["ice"] = a, s
	c1 := mustFind(t, run(t, c, akka, sol), "C1", "LIVE-ORION2026-1SOURCE")
	want := "No file in folder LIVE-ORION2026-1SOURCE changed in the last 200 days (syncwatch first saw the folder on 2026-03-19 13:00). You can archive it"
	if c1.Message != want {
		t.Fatalf("message\n got %s\nwant %s", c1.Message, want)
	}
	if c1.DetailValue("Based on") != "the time when syncwatch first saw the folder" || c1.DetailValue("Last change") != "2026-03-19T12:00:00Z" || !c1.Since.IsZero() || c1.MinDuration != 0 {
		t.Fatalf("C1 %+v", c1)
	}
	// syncwatch saw it for less than 180 days: no C1.
	a.FirstSeen = now.Add(-100 * 24 * time.Hour)
	mustNot(t, run(t, c, akka, sol), "C1", "")
	// A local change 10 days ago: no C1.
	a.FirstSeen = now.Add(-200 * 24 * time.Hour)
	s.LastLocalChange = now.Add(-10 * 24 * time.Hour)
	mustNot(t, run(t, c, akka, sol), "C1", "")
	// A local change 190 days ago: that is the basis, with its date and time.
	s.LastLocalChange = now.Add(-190 * 24 * time.Hour)
	c1 = mustFind(t, run(t, c, akka, sol), "C1", "LIVE-ORION2026-1SOURCE")
	if want := "No file in folder LIVE-ORION2026-1SOURCE changed in the last 190 days (the last local change that syncwatch saw was on 2026-03-29 14:00). You can archive it"; c1.Message != want {
		t.Fatalf("local change\n got %s\nwant %s", c1.Message, want)
	}
	// Not known at all: no C1.
	s.LastLocalChange, a.FirstSeen, s.FirstSeen = time.Time{}, time.Time{}, time.Time{}
	mustNot(t, run(t, c, akka, sol), "C1", "")
}

// W-038: every finding has 1 sentence for its resolved message. It ends with
// a period and does not repeat the sentence of the open finding. The
// scenarios below give every check of the catalog.
func TestEveryFindingHasAResolvedSentence(t *testing.T) {
	c := ivarConfig(t)
	cs, _ := c.Structure.Compile()
	var all []model.Finding
	eval := func(in Input) {
		in.Config, in.Now = c, now
		in.Snapshot.At = now
		all = append(all, Evaluate(in)...)
	}

	// Health of the folders and devices of 2 servers.
	akka := server("akka", "AKKA", akkaID)
	sol := server("sol", "SOL", solID)
	sol.Version = "v2.1.4"
	akka.Devices[solID] = &model.Device{ID: solID, Name: "SOL", LastSeen: now.Add(-time.Hour)}
	sol.Devices[akkaID] = &model.Device{ID: akkaID, Name: "AKKA", LastSeen: now.Add(-time.Hour)}
	akka.Devices[pcID] = &model.Device{ID: pcID, Name: "MORTIS", LastSeen: now, Paused: true}
	akka.Connections[pcID] = &model.Connection{Connected: true, ClientVersion: "v2.0.9", Type: "relay-client"}
	akka.Devices[pc2ID] = &model.Device{ID: pc2ID, Name: "VENUS", LastSeen: now.Add(-9 * 24 * time.Hour)}
	e := folder("e", "LIVE-ERR", "/s/AKKA-LIVE-1SOURCE/LIVE-ERR")
	e.State, e.Error, e.WatchError = "error", "folder marker missing", "too many open files"
	akka.Folders["e"] = e
	s := folder("s", "LIVE-STUCK", "/s/AKKA-LIVE-1SOURCE/LIVE-STUCK", pcID)
	s.State, s.NeedItems = "syncing", 12
	s.Completion[pcID] = &model.Completion{Percent: 40, NeedItems: 7, RemoteState: "valid"}
	akka.Folders["s"] = s
	p := folder("p", "LIVE-PAUSED", "/s/AKKA-LIVE-1SOURCE/LIVE-PAUSED")
	p.Paused = true
	akka.Folders["p"] = p
	r := folder("r", "LIVE-REMOTE", "/s/AKKA-LIVE-1SOURCE/LIVE-REMOTE", pcID)
	r.Completion[pcID] = &model.Completion{Percent: 100, RemoteState: "paused"}
	akka.Folders["r"] = r
	akka.PendingDevices = []model.PendingDevice{{DeviceID: "PENDING-6666666-6666666-6666666-6666666-6666666-6666666-6666666", Name: "stranger", Time: now}}
	akka.PendingFolders = []model.PendingFolder{{FolderID: "xyz", Label: "LIVE-OFFER", OfferedBy: pcID, Time: now}}
	akka.RelayListeners = []model.RelayListener{{URI: "relay://10.115.1.60:22067", Since: now}}
	akka.Cert = model.CertInfo{Fingerprint: "b6cfed46", NotAfter: now.Add(10 * 24 * time.Hour)}
	db := folder("db", "LIVE-DB", "/s/SOL-LIVE-1SOURCE/LIVE-DB")
	db.State, db.Error = "error", "insufficient space on disk for database (/var/syncthing/config/index-v2): current 89.54 % < required 99.9 %"
	sol.Folders["db"] = db
	eval(Input{Snapshot: &model.Snapshot{Servers: []*model.Server{akka, sol}}, Conflicts: map[string]map[string]int64{"akka": {"e": 2}}})

	// H12 for a device, H1 and H3.
	c.Devices = []config.Device{{ID: pcID, Relay: true}}
	akka.Connections[pcID].Connected = false
	akka.Devices[pcID].LastSeen = now.Add(-time.Hour)
	eval(Input{Snapshot: &model.Snapshot{Servers: []*model.Server{akka, sol}}})
	c.Devices = nil
	down := server("sol", "SOL", solID)
	down.Status, down.LastError = model.StatusDown, "timeout"
	eval(Input{Snapshot: &model.Snapshot{Servers: []*model.Server{down}}})
	down.LastError = "/rest/system/status: HTTP 403 (check the API key)"
	eval(Input{Snapshot: &model.Snapshot{Servers: []*model.Server{down}}})
	down.Status, down.CertChange = model.StatusCertChanged, &model.CertChange{Old: "aa", New: "bb", SeenAt: now}
	eval(Input{Snapshot: &model.Snapshot{Servers: []*model.Server{down}}})

	// Cross-server checks.
	x, y := server("akka", "AKKA", akkaID), server("sol", "SOL", solID)
	x.Folders["id-a"] = folder("id-a", "LIVE-SAME", "/s/AKKA-LIVE-2PROJECTFILES/LIVE-SAME")
	y.Folders["id-b"] = folder("id-b", "LIVE-SAME", "/s/SOL-LIVE-2PROJECTFILES/LIVE-SAME")
	x.Folders["moved"] = folder("moved", "LIVE-MOVED", "/s/AKKA-LIVE-1SOURCE/LIVE-MOVED")
	y.Folders["moved"] = folder("moved", "LIVE-MOVED", "/s/SOL-LIVE-2PROJECTFILES/LIVE-MOVED")
	eval(Input{Snapshot: &model.Snapshot{Servers: []*model.Server{x, y}}})

	// Structure checks.
	st := server("akka", "AKKA", akkaID)
	src := "/share/X/AKKA-LIVE-1SOURCE"
	st.Folders["k"] = folder("k", "LIVE-KYRGYZ-2PROJECTFILES", src+"/LIVE-KYRGYZ-2PROJECTFILES")
	st.Folders["same"] = folder("same", "LIVE-KYRGYZ copy", src+"/LIVE-KYRGYZ-2PROJECTFILES/")
	st.Folders["in"] = folder("in", "LIVE-KYRGYZ inner", src+"/LIVE-KYRGYZ-2PROJECTFILES/inner")
	st.Folders["root"] = folder("root", "AKKA-LIVE-4FINALS", "/share/X/AKKA-LIVE-4FINALS")
	addShare(st, cs, src, "LIVE-KYRGYZ-2PROJECTFILES", "ME-LIVE-WORKSHOP", "random stuff", "LIVE-NOTSYNCED-1SOURCE")
	addShare(st, cs, "/share/X/AKKA-LIVE-2PROJECTFILES", "LIVE-NOTSYNCED-1SOURCE")
	st.Structure.Nested["n"] = &model.NestedDir{Path: src + "/LIVE-NOTSYNCED-1SOURCE/r/LIVE-NESTED", Parent: src + "/LIVE-NOTSYNCED-1SOURCE", Share: src, Source: "scan", FoundAt: now}
	eval(Input{Snapshot: &model.Snapshot{Servers: []*model.Server{st}}})

	// Cleanup checks.
	cl := server("akka", "AKKA", akkaID)
	never := "NEVERRR-7777777-7777777-7777777-7777777-7777777-7777777-7777777"
	cl.Devices[never] = &model.Device{ID: never, Name: "NEWPC"}
	cl.Devices[pc2ID] = &model.Device{ID: pc2ID, Name: "OLDPC", LastSeen: now.Add(-100 * 24 * time.Hour)}
	cl.Devices[pcID] = &model.Device{ID: pcID, Name: "MORTIS", LastSeen: now}
	cl.Connections[pcID] = &model.Connection{Connected: true}
	old := folder("old", "LIVE-OLD", "/s/AKKA-LIVE-1SOURCE/LIVE-OLD", pcID)
	old.LastFileAt = now.Add(-200 * 24 * time.Hour)
	old.Completion[pcID] = &model.Completion{Percent: 0, RemoteState: "notSharing"}
	cl.Folders["old"] = old
	eval(Input{Snapshot: &model.Snapshot{Servers: []*model.Server{cl}}})

	seen := map[string]bool{}
	for _, f := range all {
		for _, ch := range f.Checks {
			seen[ch] = true
		}
		rt := f.ResolvedText
		if !strings.HasSuffix(rt, ".") || strings.Count(rt, ". ") > 0 || rt == f.Message+"." || strings.Contains(rt, f.Message) {
			t.Errorf("%s %s: resolved sentence %q for the message %q", f.Check, f.Subject, rt, f.Message)
		}
		for _, open := range []string{" is unreachable", " refuses ", " is paused ", " cannot sync", " did not finish", " is stuck ", " is offline", " is inside ", " has no connection", " expires on", " expired on", " is a Syncthing folder", " requests ", " offers "} {
			if strings.Contains(rt, open) {
				t.Errorf("%s %s: the resolved sentence %q has the open form %q", f.Check, f.Subject, rt, open)
			}
		}
	}
	for _, info := range config.Catalog {
		if !seen[info.ID] {
			t.Errorf("no scenario gives %s", info.ID)
		}
	}
}

// W-059 (live re-test of wave 3, P2 a): a Windows PC received its files in
// 10 seconds after it connected and then stopped with 3 files (57 bytes)
// that it cannot store. H5 promises no end: no time left. When the 10
// minutes no longer hold the files that arrived, the speed is 0.
func TestStuckPCGetsNoTimeLeft(t *testing.T) {
	c := ivarConfig(t)
	akka := server("akka", "AKKA", akkaID)
	akka.Devices[pcID] = &model.Device{ID: pcID, Name: "PC-K2", LastSeen: now}
	akka.Connections[pcID] = &model.Connection{Connected: true}
	f := folder("g", "LIVE-FJORD-4FINALS", "/s/AKKA-LIVE-4FINALS/LIVE-FJORD-4FINALS", pcID)
	f.Completion[pcID] = &model.Completion{Percent: 99.98757837067448, NeedItems: 3, NeedBytes: 57, GlobalBytes: 458_877, RemoteState: "valid", UpdatedAt: now.Add(-time.Minute)}
	akka.Folders["g"] = f
	// The samples as the collector gives them: the completion events of the
	// first sync, then a full check.
	connected := now.Add(-10*time.Minute - 5*time.Second)
	for _, s := range []model.ProgressSample{
		{At: connected, NeedBytes: 458_934}, {At: connected.Add(5 * time.Second), NeedBytes: 200_000},
		{At: connected.Add(10 * time.Second), NeedBytes: 57}, {At: connected.Add(5 * time.Minute), NeedBytes: 57},
	} {
		s.GlobalBytes = 458_877
		akka.AddProgress("g", pcID, s)
	}
	at := func(d time.Duration) *model.Finding {
		t.Helper()
		fs := Evaluate(Input{Snapshot: &model.Snapshot{At: now.Add(d), Servers: []*model.Server{akka}}, Config: c, Now: now.Add(d)})
		return mustFind(t, fs, "H5", "on PC-K2")
	}
	want := "Device PC-K2 is stuck at 99.9% of folder LIVE-FJORD-4FINALS for more than 24 hours (3 remaining items, seen from AKKA)"
	// The files arrived 10 minutes 5 seconds to 9 minutes 55 seconds ago:
	// half of that time is in the 10 minutes. A speed, but no time left.
	h5 := at(0)
	if h5.Message != want || h5.DetailValue("Speed") != "0.00038 MB each second" || h5.DetailValue("Time left") != "" || h5.DetailValue("Received") != "0.46 of 0.46 MB" {
		t.Fatalf("10 minutes after the start:\n%s\n%v", h5.Message, h5.Details)
	}
	// 10 seconds later the files arrived more than 10 minutes ago.
	if h5 = at(10 * time.Second); h5.Message != want || h5.DetailValue("Speed") != "0 (no data arrived in the last 10 minutes)" || h5.DetailValue("Time left") != "" {
		t.Fatalf("10 seconds later:\n%s\n%v", h5.Message, h5.Details)
	}
}

// W-059 (live re-test of wave 3, P2 b): a need that rises inside the 10
// minutes does not hide the data that arrived. A sync that began inside
// the 10 minutes (the folder needed nothing before) has the speed from its
// start. New files that come while a sync runs do not stop the count.
func TestStuckSyncCountsDataAfterTheNeedRose(t *testing.T) {
	c := ivarConfig(t)
	const file, kib16 = 20_971_520, 16_384
	sync := func(ago time.Duration) (*model.Server, *model.Folder) {
		sol := server("sol", "SOL", solID)
		f := folder("s", "LIVE-FJORD-1SOURCE", "/s/SOL-LIVE-1SOURCE/LIVE-FJORD-1SOURCE")
		sol.Folders["s"] = f
		// SOL needed nothing; a 20 MB file came ago, and since then SOL
		// receives 16 KiB each second (a sample each minute and one now).
		sol.AddProgress("s", "", model.ProgressSample{At: now.Add(-12 * time.Minute), GlobalBytes: 655_419})
		start := now.Add(-ago)
		for d := time.Duration(0); d < ago; d += time.Minute {
			sol.AddProgress("s", "", model.ProgressSample{At: start.Add(d), NeedBytes: file - int64(d.Seconds())*kib16, GlobalBytes: 21_626_939})
		}
		f.State, f.NeedItems, f.NeedBytes, f.GlobalBytes = "syncing", 1, file-int64(ago.Seconds())*kib16, 21_626_939
		sol.AddProgress("s", "", model.ProgressSample{At: now, NeedBytes: f.NeedBytes, GlobalBytes: f.GlobalBytes})
		return sol, f
	}
	// The file came 9 minutes 50 seconds ago (the live re-test: before
	// the fix, "0 (no data arrived in the last 10 minutes)").
	sol, _ := sync(9*time.Minute + 50*time.Second)
	h5 := mustFind(t, run(t, c, sol), "H5", "Folder LIVE-FJORD-1SOURCE on SOL")
	if want := "Folder LIVE-FJORD-1SOURCE on SOL did not finish the sync in 24 hours (syncing, 1 remaining item, 10 of 22 MB received, approximately 12 minutes remaining)"; h5.Message != want ||
		h5.DetailValue("Speed") != "0.016 MB each second" || h5.DetailValue("Time left") != "approximately 12 minutes" {
		t.Fatalf("file of 9 minutes 50 seconds ago:\n%s\n%v", h5.Message, h5.Details)
	}
	// The file came 5 minutes ago: the speed of the 5 minutes, not an
	// average over 10.
	sol, _ = sync(5 * time.Minute)
	h5 = mustFind(t, run(t, c, sol), "H5", "Folder LIVE-FJORD-1SOURCE on SOL")
	if h5.DetailValue("Speed") != "0.016 MB each second" || h5.DetailValue("Time left") != "approximately 16 minutes" {
		t.Fatalf("file of 5 minutes ago:\n%s\n%v", h5.Message, h5.Details)
	}

	// A PC receives 1 MB each second; 3 minutes ago a new file of 1 GB
	// came (the need rose by 1 GB).
	akka := server("akka", "AKKA", akkaID)
	akka.Devices[pcID] = &model.Device{ID: pcID, Name: "LAPTOP-DANI", LastSeen: now}
	akka.Connections[pcID] = &model.Connection{Connected: true}
	g := folder("g", "LIVE-FJORD-4FINALS", "/s/AKKA-LIVE-4FINALS/LIVE-FJORD-4FINALS", pcID)
	akka.Folders["g"] = g
	need := func(m int) int64 {
		n := int64(5_000_000_000) - int64(15-m)*60_000_000
		if m <= 3 {
			n += 1_000_000_000
		}
		return n
	}
	for m := 15; m >= 0; m-- {
		akka.AddProgress("g", pcID, model.ProgressSample{At: now.Add(-time.Duration(m) * time.Minute), NeedBytes: need(m), GlobalBytes: 20_000_000_000})
	}
	g.Completion[pcID] = &model.Completion{Percent: 74.5, NeedItems: 2, NeedBytes: need(0), GlobalBytes: 20_000_000_000, RemoteState: "valid"}
	h5 = mustFind(t, run(t, c, akka), "H5", "on LAPTOP-DANI")
	// 9 of the 10 minutes show a decrease of 60 MB; the minute with the
	// new file shows a rise.
	if h5.DetailValue("Speed") != "0.9 MB each second" || h5.DetailValue("Time left") != "approximately 94 minutes" ||
		h5.Message != "Device LAPTOP-DANI is stuck at 74.5% of folder LIVE-FJORD-4FINALS for more than 24 hours (2 remaining items, approximately 94 minutes remaining, seen from AKKA)" {
		t.Fatalf("new file while the PC receives:\n%s\n%v", h5.Message, h5.Details)
	}
}

// W-059 (live check of the repair on s19): a steady sync gives the same
// speed at each evaluation. The samples are about 1 minute apart (55
// seconds here) and the newest sample moves with each event: of the 2
// samples around the start of the 10 minutes, only the part after the start
// counts. The time left only goes down.
func TestStuckSyncSpeedIsSteady(t *testing.T) {
	c := ivarConfig(t)
	const size, bps = 30_000_000, 16_000
	t0 := now.Add(-20 * time.Minute)
	needAt := func(at time.Time) int64 { return size - int64(at.Sub(t0).Seconds())*bps }
	var lefts []string
	for k := 0; k < 12; k++ {
		at := now.Add(time.Duration(k) * 5 * time.Second)
		sol := server("sol", "SOL", solID)
		f := folder("s", "LIVE-FJORD-1SOURCE", "/s/SOL-LIVE-1SOURCE/LIVE-FJORD-1SOURCE")
		f.State, f.NeedItems, f.NeedBytes, f.GlobalBytes = "syncing", 1, needAt(at), size
		sol.Folders["s"] = f
		var ps []model.ProgressSample
		for s := t0; s.Before(at); s = s.Add(55 * time.Second) {
			ps = append(ps, model.ProgressSample{At: s, NeedBytes: needAt(s), GlobalBytes: size})
		}
		ps = append(ps, model.ProgressSample{At: at, NeedBytes: needAt(at), GlobalBytes: size})
		sol.Progress = map[string]map[string][]model.ProgressSample{"s": {"": ps}}
		fs := Evaluate(Input{Snapshot: &model.Snapshot{At: at, Servers: []*model.Server{sol}}, Config: c, Now: at})
		h5 := mustFind(t, fs, "H5", "Folder LIVE-FJORD-1SOURCE on SOL")
		if got := h5.DetailValue("Speed"); got != "0.016 MB each second" {
			t.Errorf("%s after the start: Speed %q, want 0.016 MB each second", at.Sub(t0), got)
		}
		lefts = append(lefts, h5.DetailValue("Time left"))
	}
	minutes := func(s string) int {
		n, _ := strconv.Atoi(strings.Fields(strings.TrimPrefix(s, "approximately "))[0])
		return n
	}
	for i := 1; i < len(lefts); i++ {
		if minutes(lefts[i]) > minutes(lefts[i-1]) {
			t.Fatalf("the time left went up: %q", lefts)
		}
	}
	if lefts[0] != "approximately 11 minutes" || lefts[len(lefts)-1] != "approximately 10 minutes" {
		t.Fatalf("time left: %q", lefts)
	}
}
