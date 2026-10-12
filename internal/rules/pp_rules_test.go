package rules

import (
	"slices"
	"testing"
	"time"

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
