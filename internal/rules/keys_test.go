package rules

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
)

func detailKeys(f *model.Finding) []string {
	var out []string
	for _, d := range f.Details {
		out = append(out, d.Key)
	}
	return out
}

// findings[].details[].key is a machine value of /api/v1: scripts read
// details by key, so a key must not change when display text changes. The
// keys below are those of 78ef2d8; the comments name the keys that were
// added since (r0, r1), which is the only change permitted. The dashboard
// shows its own labels for some keys.
func TestDetailKeysKeepTheirNames(t *testing.T) {
	c := ivarConfig(t)
	akka := server("akka", "AKKA", akkaID)
	sol := server("sol", "SOL", solID)
	akka.Devices[solID] = &model.Device{ID: solID, Name: "SOL", LastSeen: now.Add(-time.Hour)}
	sol.Devices[akkaID] = &model.Device{ID: akkaID, Name: "AKKA", LastSeen: now.Add(-time.Hour)}
	akka.Devices[pcID] = &model.Device{ID: pcID, Name: "MORTIS", LastSeen: now}
	akka.Connections[pcID] = &model.Connection{Connected: true, ClientVersion: "v2.0.9", Address: "10.0.0.5:22000", Type: "relay-client"}
	akka.Devices[pc2ID] = &model.Device{ID: pc2ID, Name: "VENUS", LastSeen: now.Add(-9 * 24 * time.Hour)}
	e := folder("e", "LIVE-ERR", "/s/AKKA-LIVE-1SOURCE/LIVE-ERR")
	e.State, e.Error, e.PullErrors, e.WatchError = "error", "folder marker missing", 2, "too many open files"
	akka.Folders["e"] = e
	s := folder("s", "LIVE-STUCK", "/s/AKKA-LIVE-1SOURCE/LIVE-STUCK", pcID)
	s.State, s.NeedItems, s.NeedBytes = "syncing", 12, 3000
	s.Completion[pcID] = &model.Completion{Percent: 40, NeedItems: 7, RemoteState: "valid"}
	akka.Folders["s"] = s
	p := folder("p", "LIVE-PAUSED", "/s/AKKA-LIVE-1SOURCE/LIVE-PAUSED")
	p.Paused = true
	akka.Folders["p"] = p
	r := folder("r", "LIVE-REMOTE", "/s/AKKA-LIVE-1SOURCE/LIVE-REMOTE", pcID)
	r.Completion[pcID] = &model.Completion{Percent: 100, RemoteState: "paused"}
	akka.Folders["r"] = r
	akka.PendingDevices = []model.PendingDevice{{DeviceID: "PENDING-6666666-6666666-6666666-6666666-6666666-6666666-6666666", Name: "stranger", Address: "10.0.0.9:22000", Time: now}}
	akka.PendingFolders = []model.PendingFolder{{FolderID: "xyz", Label: "LIVE-OFFER", OfferedBy: pcID, Time: now}}
	in := Input{Snapshot: &model.Snapshot{At: now, Servers: []*model.Server{akka, sol}}, Config: c, Now: now,
		Conflicts: map[string]map[string]int64{"akka": {"e": 2}}}
	fs := Evaluate(in)

	for _, tc := range []struct {
		check, part string
		keys        []string
	}{
		{"H4", "Folder LIVE-ERR on AKKA cannot sync", []string{"Folder ID", "Path", "State", "Error", "Watch error", "Failing items"}},
		// r0 made the watcher error a finding of its own, with the same key.
		{"H4", "The file watcher of folder LIVE-ERR", []string{"Folder ID", "Path", "Watch error"}},
		{"H5", "LIVE-STUCK on AKKA", []string{"Folder ID", "State", "Items left", "Bytes left"}},
		{"H5", "LIVE-STUCK on MORTIS", []string{"Folder ID", "Device ID", "Completion", "Items left"}},
		{"H6", "Folder LIVE-PAUSED is paused", []string{"Folder ID", "Path"}},
		{"H6", "Device MORTIS paused folder LIVE-REMOTE", []string{"Folder ID", "Device ID"}},
		{"H9", "stranger", []string{"Device ID", "Address", "First offered"}},
		{"H9", "LIVE-OFFER", []string{"Folder ID", "Offered by", "First offered"}},
		{"H10", "LIVE-ERR", []string{"Folder ID", "New conflicts"}},
		{"H11", "MORTIS", []string{"Device ID", "Address", "Type"}},
		{"H8", "MORTIS runs", []string{"Device ID", "Version"}},
		{"H7", "VENUS", []string{"Device ID", "Last seen", "Configured on"}},
		// r1 adds the row "What to check".
		{"H2", "AKKA ↔ SOL", []string{"AKKA last saw SOL", "SOL last saw AKKA", "What to check"}},
	} {
		f := mustFind(t, fs, tc.check, tc.part)
		if got := detailKeys(f); !slices.Equal(got, tc.keys) {
			t.Errorf("%s %q: detail keys %q, want %q", tc.check, tc.part, got, tc.keys)
		}
	}

	// H1 and H3 come from a server that is down or sent another certificate.
	sol.Status, sol.LastError, sol.DownSince = model.StatusDown, "timeout", now.Add(-10*time.Minute)
	// r1 adds a row with the possible causes of a network failure, or with
	// what to do about a refused API key.
	h1 := mustFind(t, Evaluate(in), "H1", "SOL")
	if got, want := detailKeys(h1), []string{"URL", "Down since", "Last contact", "Error", "Possible causes"}; !slices.Equal(got, want) {
		t.Errorf("H1: detail keys %q, want %q", got, want)
	}
	sol.Status, sol.CertChange = model.StatusCertChanged, &model.CertChange{Old: "aa", New: "bb", SeenAt: now}
	h3 := mustFind(t, Evaluate(in), "H3", "SOL")
	if got, want := detailKeys(h3), []string{"Pinned fingerprint", "New fingerprint", "New fingerprint (full)", "New certificate expires"}; !slices.Equal(got, want) {
		t.Errorf("H3: detail keys %q, want %q", got, want)
	}

	// No check writes a key that the text pass of r0 had renamed.
	renamed := []string{"File watcher error", "Failed items", "Remaining items", "Remaining bytes", "Connection type", "API URL", "Down from"}
	for _, f := range fs {
		for _, k := range detailKeys(&f) {
			if slices.Contains(renamed, k) {
				t.Errorf("%s %s: detail key %q is a display label, not the API key", f.Check, f.Subject, k)
			}
		}
		if strings.TrimSpace(f.ID) == "" {
			t.Fatalf("finding without an ID: %+v", f)
		}
	}
}
