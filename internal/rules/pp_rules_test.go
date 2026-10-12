package rules

import (
	"slices"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/model"
)

// Tests of the pre-publication review (pp-rules).

// text-4: a file name with white space at the start or the end, or with a
// control character, shows as a quoted Go string in the H5 row "Needed
// files" and in the H4 lines of the failed items. HTML does not show these
// characters: "brief " showed as "brief", and "a\r\nb" as "a b", the same
// as names that Windows accepts.
func TestFileNamesShowHiddenCharacters(t *testing.T) {
	c := ivarConfig(t)
	akka := server("akka", "AKKA", akkaID)
	akka.Devices[pcID] = &model.Device{ID: pcID, Name: "PC-SHARED-BJORN", LastSeen: now}
	akka.Connections[pcID] = &model.Connection{Connected: true}
	f := folder("f", "LIVE-FJORD-4FINALS", "/s/AKKA-LIVE-4FINALS/LIVE-FJORD-4FINALS", pcID)
	f.Completion[pcID] = &model.Completion{Percent: 99.99, NeedItems: 6, NeedBytes: 28, RemoteState: "valid", UpdatedAt: now.Add(-time.Hour)}
	akka.Folders["f"] = f
	// The names as collector/need.go keeps them: unchanged from Syncthing.
	akka.NeedLists = map[string]map[string]model.NeedList{"f": {pcID: {Names: []string{
		"delivery/from-mac/brief ", "delivery/from-mac/old version.", "delivery/LIVE-Y\r\nline2.txt", "delivery/tab\there.txt",
	}, Items: 6, At: now.Add(-30 * time.Minute)}}}
	want := `"delivery/from-mac/brief " · delivery/from-mac/old version. · "delivery/LIVE-Y\r\nline2.txt" · "delivery/tab\there.txt" · …and 2 more`
	if got := mustFind(t, run(t, c, akka), "H5", "on PC-SHARED-BJORN").DetailValue("Needed files"); got != want {
		t.Errorf("Needed files\n got %s\nwant %s", got, want)
	}

	e := folder("e", "LIVE-ERR-A", "/s/AKKA-LIVE-4FINALS/LIVE-ERR-A")
	e.FileErrors = []model.FileError{
		{Path: "delivery/from-mac/brief ", Error: "name is invalid, must not end in space or period on Windows"},
		{Path: "delivery/LIVE-Y\r\nline2.txt", Error: "name is invalid, contains Windows reserved character"},
		{Path: "delivery/tab here.txt", Error: "permission denied"},
	}
	akka.Folders["e"] = e
	wantPaths := []string{
		`"delivery/from-mac/brief ": name is invalid, must not end in space or period on Windows`,
		`"delivery/LIVE-Y\r\nline2.txt": name is invalid, contains Windows reserved character`,
		"delivery/tab here.txt: permission denied",
	}
	if got := mustFind(t, run(t, c, akka), "H4", "LIVE-ERR-A").Paths; !slices.Equal(got, wantPaths) {
		t.Errorf("H4 paths\n got %q\nwant %q", got, wantPaths)
	}
}

// text-4: which names quoteName quotes. Names that the dashboard shows
// correctly stay as they are; it marks the direction characters itself.
func TestQuoteName(t *testing.T) {
	for _, tc := range []struct{ name, want string }{
		{"delivery/brief.txt", "delivery/brief.txt"},
		{"old version.", "old version."},
		{"two  spaces", "two  spaces"},
		{"Björn ✂ cut.mov", "Björn ✂ cut.mov"},
		{"rtl\u202egnp.exe", "rtl\u202egnp.exe"},
		{"", ""},
		{"brief ", `"brief "`},
		{" lead.txt", `" lead.txt"`},
		{"LIVE-Y\r\nline2.txt", `"LIVE-Y\r\nline2.txt"`},
		{"tab\there.txt", `"tab\there.txt"`},
		{"Icon\r", `"Icon\r"`},
		{"bell\x07", `"bell\a"`},
		{"del\x7f.txt", `"del\x7f.txt"`},
		{"nbsp\u00a0", `"nbsp\u00a0"`},
		{"quote\" end ", `"quote\" end "`},
	} {
		if got := quoteName(tc.name); got != tc.want {
			t.Errorf("quoteName(%q) = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// data-4: in polled mode, the connections of AKKA are from its last full
// check. They show that Syncthing on SOL runs only when that check came 10
// minutes (deadLinkTime) or more after the first failed request to SOL:
// syncwatch sends no request between 2 full checks, thus SOL can be off
// from any time after its last contact.
func TestPolledPeerViewNeedsNewerData(t *testing.T) {
	at := func(hhmm string) time.Time {
		c, err := time.Parse("15:04", hhmm)
		if err != nil {
			t.Fatal(err)
		}
		return time.Date(2026, 10, 5, c.Hour(), c.Minute(), 0, 0, time.UTC)
	}
	c := ivarConfig(t)
	c.Site = "IVAR Office"
	c.Servers[0].Site, c.Servers[1].Site = "IVAR Office", "Branch office"
	const runs = "AKKA is connected to SOL. Thus Syncthing on SOL runs, and the network path from syncwatch to SOL is down."
	const api = "AKKA is connected to SOL. Thus Syncthing on SOL runs, and syncwatch cannot connect to the API address of SOL."
	for _, tc := range []struct {
		name                  string
		mode                  string
		eval, akkaCheck, down string // the time of the evaluation, of AKKA's last full check, of the first failed request to SOL
		err                   string
		want                  string // the row "What AKKA sees"
	}{
		// 11:50 the last full check of SOL; 11:55 the full check of AKKA,
		// connected to SOL; 11:56 SOL is switched off; 12:05 the full check
		// of SOL fails, H1 opens; 12:10 the next full check of AKKA.
		{"H1 opens, data of AKKA from 11:55", "polled", "12:05", "11:55", "12:05", "timeout", ""},
		{"4 minutes later, data of AKKA from 11:55", "polled", "12:09", "11:55", "12:05", "timeout", ""},
		// AKKA's check is 11 minutes after the last contact, but SOL is
		// switched off at 12:03, after that check.
		{"data of AKKA from 12:01", "polled", "12:06", "12:01", "12:05", "timeout", ""},
		// SOL's gate stops at 11:56 and Syncthing on SOL runs. 5 minutes
		// after the first failed request, the connection can still be one
		// that Syncthing did not close: no row yet.
		{"gate stopped, data of AKKA from 12:10", "polled", "12:11", "12:10", "12:05", "connection refused", ""},
		{"gate stopped, data of AKKA from 12:25", "polled", "12:26", "12:25", "12:05", "connection refused", api},
		{"VPN down, data of AKKA from 12:25", "polled", "12:26", "12:25", "12:05", "timeout", runs},
		// Events mode: the connections are live (no change).
		{"events mode, live data", "events", "12:05", "08:00", "11:51", "timeout", runs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			akka := server("akka", "AKKA", akkaID)
			sol := server("sol", "SOL", solID)
			akka.Devices[solID] = &model.Device{ID: solID, Name: "SOL"}
			sol.Devices[akkaID] = &model.Device{ID: akkaID, Name: "AKKA"}
			akka.Mode, sol.Mode = tc.mode, tc.mode
			akka.LastFullCheck, akka.LastContact = at(tc.akkaCheck), at(tc.akkaCheck)
			if tc.mode == "events" {
				akka.LastContact = at(tc.eval)
			}
			akka.Connections[solID] = &model.Connection{Connected: true, Type: "tcp-server", StartedAt: at("09:00")}
			sol.LastFullCheck, sol.LastContact = at("11:50"), at("11:50")
			sol.Status, sol.LastError, sol.DownSince = model.StatusDown, tc.err, at(tc.down)
			fs := Evaluate(Input{Snapshot: &model.Snapshot{At: at(tc.eval), Servers: []*model.Server{akka, sol}}, Config: c, Now: at(tc.eval)})
			if got := mustFind(t, fs, "H1", "Server SOL").DetailValue("What AKKA sees"); got != tc.want {
				t.Errorf("at %s: row %q, want %q", tc.eval, got, tc.want)
			}
		})
	}
}

// data-5 (a): a device that always uses a relay, and that an administrator
// paused on every server that has it configured, has no connection because
// of the pause: no H12 (H6 reports the pause after thresholds.paused). A
// server that does not pause the device can have it connected: H12 as
// before.
func TestRelayDevicePausedOnEveryServer(t *testing.T) {
	c := ivarConfig(t)
	c.Devices = []config.Device{{ID: pcID, Relay: true}}
	akka := server("akka", "AKKA", akkaID)
	sol := server("sol", "SOL", solID)
	for _, sv := range []*model.Server{akka, sol} {
		sv.Devices[pcID] = &model.Device{ID: pcID, Name: "LAPTOP-DANI", Paused: true, LastSeen: now.Add(-2 * time.Hour)}
		sv.Connections[pcID] = &model.Connection{Connected: false, Paused: true}
	}
	mustNot(t, run(t, c, akka, sol), "H12", "LAPTOP-DANI")
	sol.Devices[pcID].Paused = false
	sol.Connections[pcID].Paused = false
	mustFind(t, run(t, c, akka, sol), "H12", "LAPTOP-DANI")
}
