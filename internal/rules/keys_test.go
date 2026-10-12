package rules

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
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

// The detail keys of version 0.9: the new checks H12 and H13, the H4 of a
// full database disk and the rows of S2 are new; H1 and H5 get rows after
// their keys of 78ef2d8. Advice is only under "Possible causes", "What to
// check" and "What to do"; device IDs only under "Device ID" and "Offered by".
func TestR2DetailKeys(t *testing.T) {
	c := ivarConfig(t)
	c.Devices = []config.Device{{ID: pcID, Relay: true}}
	akka := server("akka", "AKKA", akkaID)
	sol := server("sol", "SOL", solID)
	akka.Devices[solID] = &model.Device{ID: solID, Name: "SOL", LastSeen: now}
	akka.Connections[solID] = &model.Connection{Connected: true}
	akka.Devices[pcID] = &model.Device{ID: pcID, Name: "LAPTOP-DANI", LastSeen: now.Add(-time.Hour)}
	akka.Devices[pc2ID] = &model.Device{ID: pc2ID, Name: "VENUS", LastSeen: now}
	akka.Connections[pc2ID] = &model.Connection{Connected: true}
	s := folder("s", "LIVE-STUCK", "/s/AKKA-LIVE-1SOURCE/LIVE-STUCK", pc2ID)
	s.State, s.NeedItems, s.NeedBytes, s.GlobalBytes = "syncing", 12, 3_000_000, 9_000_000
	s.Completion[pc2ID] = &model.Completion{Percent: 40, NeedItems: 7, NeedBytes: 3_000_000, GlobalBytes: 9_000_000, RemoteState: "valid"}
	akka.Folders["s"] = s
	for i := 10; i >= 0; i-- {
		akka.AddProgress("s", "", model.ProgressSample{At: now.Add(-time.Duration(i) * time.Minute), NeedBytes: 3_000_000 + int64(i)*60_000})
		akka.AddProgress("s", pc2ID, model.ProgressSample{At: now.Add(-time.Duration(i) * time.Minute), NeedBytes: 3_000_000 + int64(i)*60_000})
	}
	akka.NeedLists = map[string]map[string]model.NeedList{"s": {pc2ID: {Names: []string{"a.blend"}, Items: 7, At: now}}}
	akka.Folders["root"] = folder("root", "AKKA-LIVE-1SOURCE", "/s/AKKA-LIVE-1SOURCE")
	akka.RelayListeners = []model.RelayListener{{URI: "relay://10.115.1.60:22067", Error: "connection refused", Since: now.Add(-time.Hour)}}
	akka.Cert = model.CertInfo{Fingerprint: "aabbccdd", NotAfter: now.Add(24 * time.Hour)}
	db := folder("db", "LIVE-DB", "/s/AKKA-LIVE-1SOURCE/LIVE-DB")
	db.State, db.Error = "error", "insufficient space on disk for database (/config/index-v2): current 1 % < required 99.9 %"
	akka.Folders["db"] = db
	sol.Status, sol.LastError, sol.LastContact = model.StatusDown, "timeout", now.Add(-11*time.Minute)
	fs := run(t, c, akka, sol)

	for _, tc := range []struct {
		check, part string
		keys        []string
	}{
		{"H4", "Syncthing database on AKKA", []string{"Error", "Stopped folders", "Folder IDs"}},
		{"H5", "LIVE-STUCK on AKKA", []string{"Folder ID", "State", "Items left", "Bytes left", "Received", "Speed", "Time left"}},
		{"H5", "LIVE-STUCK on VENUS", []string{"Folder ID", "Device ID", "Completion", "Items left", "Needed files", "Received", "Speed", "Time left"}},
		{"H1", "Server SOL", []string{"URL", "Down since", "Last contact", "Error", "Possible causes", "What AKKA sees"}},
		{"H12", "LAPTOP-DANI", []string{"Device ID", "Last seen", "Possible causes"}},
		{"H12", "10.115.1.60:22067 on AKKA", []string{"Relay", "Error", "Possible causes"}},
		{"H13", "AKKA", []string{"Certificate expires", "Fingerprint", "What to do"}},
		{"S2", "AKKA-LIVE-1SOURCE on AKKA", []string{"Folder ID", "Path", "Syncthing folder inside", "Syncthing folder inside"}},
	} {
		f := mustFind(t, fs, tc.check, tc.part)
		if got := detailKeys(f); !slices.Equal(got, tc.keys) {
			t.Errorf("%s %q: detail keys %q, want %q", tc.check, tc.part, got, tc.keys)
		}
	}
	for _, f := range fs {
		for _, d := range f.Details {
			if d.Key != "Device ID" && d.Key != "Offered by" && (strings.Contains(d.Value, pcID) || strings.Contains(d.Value, pc2ID) || strings.Contains(d.Value, solID)) {
				t.Errorf("%s %s: a device ID under %q", f.Check, f.Subject, d.Key)
			}
		}
	}
}
