package rules

import (
	"fmt"
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
	if h12.DetailValue("Device ID") != pcID || h12.DetailValue("Last seen") != "2026-10-05T11:26:00Z" || !strings.Contains(h12.DetailValue("Possible causes"), "The relay is off") {
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
	sol.Status, sol.LastError, sol.DownSince = model.StatusDown, "timeout", now.Add(-10*time.Minute)
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
