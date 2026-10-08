package rules

import (
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/model"
)

var now = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

const ivarRules = `
servers:
  - {name: AKKA, url: "https://akka:8384"}
  - {name: SOL, url: "https://sol:8384"}
structure:
  project_pattern: '^(ME-)?LIVE-[A-Za-z0-9]'
  share_type_from_path: '-(?P<me>ME-)?LIVE-(?P<type>[1-5][A-Z]+)$'
  type_tokens: [1SOURCE, 2PROJECTFILES, 3INTERMEDIATE, 4FINALS, 5COLLECT]
  placement:
    - when_name_matches: '^ME-LIVE-'
      share_must_have: { me: true }
    - when_name_matches: '^LIVE-'
      share_must_have: { me: false }
  ignore_dirs: ['@Recycle', '.*']
`

func ivarConfig(t *testing.T) *config.Config {
	t.Helper()
	c, err := config.ParseYAML([]byte(ivarRules))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const (
	akkaID = "AKKAAAA-1111111-1111111-1111111-1111111-1111111-1111111-1111111"
	solID  = "SOLSOLS-2222222-2222222-2222222-2222222-2222222-2222222-2222222"
	pcID   = "MORTISS-3333333-3333333-3333333-3333333-3333333-3333333-3333333"
	pc2ID  = "VENUSSS-4444444-4444444-4444444-4444444-4444444-4444444-4444444"
)

func server(id, name, myID string) *model.Server {
	return &model.Server{
		ID: id, Name: name, MyID: myID, Status: model.StatusUp, Loaded: true, PathSep: "/",
		Version: "v2.1.5", LastContact: now,
		Folders: map[string]*model.Folder{}, Devices: map[string]*model.Device{myID: {ID: myID, Name: name}},
		Connections: map[string]*model.Connection{}, Conflicts: map[string]int64{},
		Structure: &model.Structure{Nested: map[string]*model.NestedDir{}},
	}
}

func folder(id, label, path string, shared ...string) *model.Folder {
	return &model.Folder{ID: id, Label: label, Path: path, State: "idle", SharedWith: shared, Completion: map[string]*model.Completion{}}
}

func run(t *testing.T, c *config.Config, servers ...*model.Server) []model.Finding {
	t.Helper()
	return Evaluate(Input{Snapshot: &model.Snapshot{At: now, Servers: servers}, Config: c, Now: now})
}

func find(fs []model.Finding, check, subjectPart string) *model.Finding {
	for i := range fs {
		for _, c := range fs[i].Checks {
			if c == check && strings.Contains(fs[i].Subject+" "+fs[i].Message, subjectPart) {
				return &fs[i]
			}
		}
	}
	return nil
}

func mustFind(t *testing.T, fs []model.Finding, check, part string) *model.Finding {
	t.Helper()
	f := find(fs, check, part)
	if f == nil {
		t.Fatalf("expected %s finding containing %q; got:\n%s", check, part, dump(fs))
	}
	return f
}

func mustNot(t *testing.T, fs []model.Finding, check, part string) {
	t.Helper()
	if f := find(fs, check, part); f != nil {
		t.Fatalf("unexpected %s finding: %s", check, f.Message)
	}
}

func dump(fs []model.Finding) string {
	var b strings.Builder
	for _, f := range fs {
		b.WriteString("  " + strings.Join(f.Checks, "+") + " " + f.Severity.String() + " " + f.Subject + " — " + f.Message + "\n")
	}
	return b.String()
}

// shares builds the structure scan result for a server.
func addShare(sv *model.Server, cs *config.CompiledStructure, path string, dirs ...string) *model.Share {
	name := path[strings.LastIndex(path, "/")+1:]
	attrs, _ := cs.ShareAttrs(name)
	sh := &model.Share{Path: path, Name: name, Attrs: attrs}
	for _, d := range dirs {
		dp := path + "/" + d
		var ids []string
		for _, f := range sv.Folders {
			if f.Path == dp || strings.HasPrefix(f.Path, dp+"/") || strings.HasPrefix(dp, f.Path+"/") {
				ids = append(ids, f.ID)
			}
		}
		sh.Dirs = append(sh.Dirs, &model.Dir{Name: d, Path: dp, FolderIDs: ids})
	}
	sv.Structure.Shares = append(sv.Structure.Shares, sh)
	return sh
}

func TestIVARStructureCases(t *testing.T) {
	c := ivarConfig(t)
	cs, _ := c.Structure.Compile()
	akka := server("akka", "AKKA", akkaID)
	src := "/share/CACHEDEV2_DATA/AKKA-LIVE-1SOURCE"
	pf := "/share/CACHEDEV2_DATA/AKKA-LIVE-2PROJECTFILES"
	akka.Folders["k1"] = folder("k1", "LIVE-KYRGYZ-2PROJECTFILES-BLENDER", pf+"/LIVE-KYRGYZ-2PROJECTFILES-BLENDER")
	akka.Folders["k2"] = folder("k2", "LIVE-KYRGYZ-2PROJECTFILES-BLENDER", src+"/LIVE-KYRGYZ-2PROJECTFILES-BLENDER")
	akka.Folders["a1"] = folder("a1", "LIVE-ARCTICVIS-4FINALS-3DTiles", src+"/LIVE-ARCTICVIS-4FINALS-3DTiles")
	akka.Folders["ok"] = folder("ok", "LIVE-GOOD-1SOURCE", src+"/LIVE-GOOD-1SOURCE")
	akka.Folders["sw"] = folder("sw", "LIVE-SKÅNE-1SOURCE", src+"/LIVE-SKÅNE-1SOURCE")
	addShare(akka, cs, src, "LIVE-KYRGYZ-2PROJECTFILES-BLENDER", "LIVE-ARCTICVIS-4FINALS-3DTiles", "ME-LIVE-WORKSHOP",
		"LIVE-GOOD-1SOURCE", "LIVE-SKÅNE-1SOURCE", "random stuff", "LIVE-X[1]-1SOURCE", "@Recycle", ".streams", "LIVE-NOTSYNCED-1SOURCE")
	addShare(akka, cs, pf, "LIVE-KYRGYZ-2PROJECTFILES-BLENDER")
	akka.Structure.Nested["x"] = &model.NestedDir{
		Path: src + "/LIVE-GOOD-1SOURCE/renders/LIVE-NESTED", Parent: src + "/LIVE-GOOD-1SOURCE", Share: src, FolderID: "ok", Source: "event", FoundAt: now,
	}

	fs := run(t, c, akka)

	// KYRGYZ in the SOURCE share: wrong type token (S5) and duplicate (S6), one row.
	k := mustFind(t, fs, "S5", "AKKA-LIVE-1SOURCE/LIVE-KYRGYZ")
	if len(k.Checks) != 2 || k.Checks[1] != "S6" || k.Severity != model.Error || k.Check != "S5" {
		t.Fatalf("KYRGYZ in SOURCE: %+v", k.Checks)
	}
	// ... and the PROJECTFILES copy is only a duplicate.
	k2 := mustFind(t, fs, "S6", "AKKA-LIVE-2PROJECTFILES/LIVE-KYRGYZ")
	if len(k2.Checks) != 1 || k2.Severity != model.Warn {
		t.Fatalf("KYRGYZ in PROJECTFILES: %v", k2.Checks)
	}
	mustFind(t, fs, "S5", "LIVE-ARCTICVIS-4FINALS-3DTiles")
	ws := mustFind(t, fs, "S5", "ME-LIVE-WORKSHOP")
	if !strings.Contains(ws.Message, "me is true") {
		t.Fatalf("workshop: %s", ws.Message)
	}
	mustFind(t, fs, "S7", "ME-LIVE-WORKSHOP")
	mustFind(t, fs, "S4", "random stuff")
	mustFind(t, fs, "S7", "LIVE-NOTSYNCED-1SOURCE")
	mustFind(t, fs, "S7", "LIVE-X[1]-1SOURCE")
	n := mustFind(t, fs, "S3", "LIVE-NESTED")
	if !n.Urgent || n.Severity != model.Error {
		t.Fatal("S3 must be an urgent error")
	}
	mustNot(t, fs, "S4", "@Recycle")
	mustNot(t, fs, "S4", ".streams")
	for _, f := range fs {
		if strings.Contains(f.Subject, "LIVE-GOOD-1SOURCE on") || strings.Contains(f.Subject, "SKÅNE") {
			t.Fatalf("clean folder flagged: %s %s", f.Check, f.Message)
		}
	}
	// IDs are stable across runs.
	fs2 := run(t, c, akka)
	if fs2[0].ID != fs[0].ID || len(fs2) != len(fs) {
		t.Fatal("unstable IDs")
	}
}

func TestOverlapAndShareRoot(t *testing.T) {
	c := ivarConfig(t)
	akka := server("akka", "AKKA", akkaID)
	sh := "/share/X/AKKA-LIVE-2PROJECTFILES"
	akka.Folders["root"] = folder("root", "whole share", sh)
	akka.Folders["p"] = folder("p", "LIVE-P", sh+"/LIVE-P")
	akka.Folders["sub"] = folder("sub", "LIVE-P sub", sh+"/LIVE-P/sub")
	fs := run(t, c, akka)
	mustFind(t, fs, "S2", "AKKA-LIVE-2PROJECTFILES on AKKA")
	mustFind(t, fs, "S1", "LIVE-P sub inside LIVE-P")
	mustFind(t, fs, "S1", "LIVE-P inside whole share")
	if f := find(fs, "S2", ""); !f.Urgent {
		t.Fatal("S2 urgent by default")
	}
}

func TestShareRootWithoutRules(t *testing.T) {
	c, _ := config.ParseYAML([]byte("servers: [{name: A, url: 'https://a:1'}]"))
	a := server("a", "A", akkaID)
	a.Folders["root"] = folder("root", "root", "/data/share")
	a.Folders["p"] = folder("p", "p", "/data/share/proj")
	fs := run(t, c, a)
	mustFind(t, fs, "S2", "share on A")
	mustFind(t, fs, "S1", "p inside root")
}

func TestServerHealth(t *testing.T) {
	c := ivarConfig(t)
	akka := server("akka", "AKKA", akkaID)
	sol := server("sol", "SOL", solID)
	akka.Devices[solID] = &model.Device{ID: solID, Name: "SOL", LastSeen: now.Add(-time.Hour)}
	sol.Devices[akkaID] = &model.Device{ID: akkaID, Name: "AKKA", LastSeen: now.Add(-time.Hour)}

	fs := run(t, c, akka, sol)
	h2 := mustFind(t, fs, "H2", "AKKA ↔ SOL")
	if h2.Server != "akka" || h2.Related[0] != "sol" || !h2.Since.Equal(now.Add(-time.Hour)) {
		t.Fatalf("H2 %+v", h2)
	}
	akka.Connections[solID] = &model.Connection{Connected: true, Type: "tcp-client"}
	mustNot(t, run(t, c, akka, sol), "H2", "")

	sol.Status, sol.LastError, sol.DownSince = model.StatusDown, "connection refused", now.Add(-10*time.Minute)
	fs = run(t, c, akka, sol)
	h1 := mustFind(t, fs, "H1", "SOL")
	if !h1.Urgent || !strings.Contains(h1.Message, "connection refused") {
		t.Fatal("H1")
	}
	sol.Status, sol.CertChange = model.StatusCertChanged, &model.CertChange{Old: "aa", New: "bb", SeenAt: now}
	fs = run(t, c, akka, sol)
	mustFind(t, fs, "H3", "SOL")
	mustNot(t, fs, "H1", "SOL")
}

func TestFolderHealth(t *testing.T) {
	c := ivarConfig(t)
	akka := server("akka", "AKKA", akkaID)
	akka.Devices[pcID] = &model.Device{ID: pcID, Name: "MORTIS", LastSeen: now}
	akka.Connections[pcID] = &model.Connection{Connected: true, ClientVersion: "v2.1.5", Type: "tcp-server"}
	f := folder("e", "LIVE-ERR", "/s/AKKA-LIVE-1SOURCE/LIVE-ERR", pcID)
	f.State, f.Error = "error", "folder marker missing (this indicates potential data loss)"
	akka.Folders["e"] = f
	f2 := folder("p", "LIVE-PULL", "/s/AKKA-LIVE-1SOURCE/LIVE-PULL", pcID)
	f2.PullErrors = 2
	f2.FileErrors = []model.FileError{{Path: "a.blend", Error: "permission denied"}}
	akka.Folders["p"] = f2
	f3 := folder("s", "LIVE-STUCK", "/s/AKKA-LIVE-1SOURCE/LIVE-STUCK", pcID)
	f3.State, f3.NeedItems = "syncing", 12
	f3.Completion[pcID] = &model.Completion{Percent: 40, RemoteState: "valid"}
	akka.Folders["s"] = f3
	f4 := folder("z", "LIVE-PAUSED", "/s/AKKA-LIVE-1SOURCE/LIVE-PAUSED")
	f4.Paused = true
	akka.Folders["z"] = f4
	f5 := folder("r", "LIVE-REMOTE", "/s/AKKA-LIVE-1SOURCE/LIVE-REMOTE", pcID)
	f5.Completion[pcID] = &model.Completion{Percent: 100, RemoteState: "paused"}
	akka.Folders["r"] = f5
	f6 := folder("w", "LIVE-WATCH", "/s/AKKA-LIVE-1SOURCE/LIVE-WATCH")
	f6.WatchError = "too many open files"
	akka.Folders["w"] = f6
	akka.Conflicts["p"] = 3

	in := Input{Snapshot: &model.Snapshot{At: now, Servers: []*model.Server{akka}}, Config: c, Now: now,
		Conflicts: map[string]map[string]int64{"akka": {"p": 2}}}
	fs := Evaluate(in)
	if h4 := mustFind(t, fs, "H4", "LIVE-ERR"); h4.MinDuration != 0 {
		t.Fatalf("a folder error must open H4 at once, not after %v", h4.MinDuration)
	}
	h4 := mustFind(t, fs, "H4", "LIVE-PULL")
	if len(h4.Paths) != 1 || !strings.Contains(h4.Paths[0], "a.blend") {
		t.Fatalf("paths %v", h4.Paths)
	}
	// Only failing files: wait for thresholds.failing_files.
	if h4.MinDuration != time.Hour {
		t.Fatalf("H4 for failing files only: min duration %v, want 1h", h4.MinDuration)
	}
	if h4 := mustFind(t, fs, "H4", "file watcher error"); h4.MinDuration != 0 {
		t.Fatalf("a watcher error must open H4 at once, not after %v", h4.MinDuration)
	}
	h5 := mustFind(t, fs, "H5", "LIVE-STUCK on AKKA")
	if h5.MinDuration != 24*time.Hour {
		t.Fatal("H5 min duration")
	}
	mustFind(t, fs, "H5", "LIVE-STUCK on MORTIS")
	h6 := mustFind(t, fs, "H6", "LIVE-PAUSED")
	if h6.MinDuration != 72*time.Hour {
		t.Fatal("H6 min duration")
	}
	mustFind(t, fs, "H6", "MORTIS has paused folder LIVE-REMOTE")
	mustFind(t, fs, "H10", "2 new sync conflict")
}

func TestDevicesAndVersions(t *testing.T) {
	c := ivarConfig(t)
	akka := server("akka", "AKKA", akkaID)
	sol := server("sol", "SOL", solID)
	sol.Version = "v2.1.4"
	akka.Devices[pcID] = &model.Device{ID: pcID, Name: "MORTIS", LastSeen: now.Add(-9 * 24 * time.Hour)}
	akka.Devices[pc2ID] = &model.Device{ID: pc2ID, Name: "VENUS", LastSeen: now}
	akka.Connections[pc2ID] = &model.Connection{Connected: true, ClientVersion: "v2.0.9", Type: "relay-client"}
	old := "OLDOLDD-5555555-5555555-5555555-5555555-5555555-5555555-5555555"
	akka.Devices[old] = &model.Device{ID: old, Name: "OLDPC", LastSeen: now.Add(-100 * 24 * time.Hour)}
	pend := "PENDING-6666666-6666666-6666666-6666666-6666666-6666666-6666666"
	akka.PendingDevices = []model.PendingDevice{{DeviceID: pend, Name: "stranger", Time: now}}
	akka.PendingFolders = []model.PendingFolder{{FolderID: "xyz", Label: "LIVE-OFFER", OfferedBy: pc2ID, Time: now}}

	fs := run(t, c, akka, sol)
	h7 := mustFind(t, fs, "H7", "MORTIS")
	if !strings.Contains(h7.Message, "9 days") {
		t.Fatal(h7.Message)
	}
	mustNot(t, fs, "H7", "VENUS")
	mustNot(t, fs, "H7", "OLDPC")
	mustFind(t, fs, "C2", "OLDPC")
	mustFind(t, fs, "H8", "Servers run different")
	mustFind(t, fs, "H8", "VENUS runs Syncthing v2.0.9")
	mustFind(t, fs, "H11", "VENUS")
	mustFind(t, fs, "H9", "stranger")
	mustFind(t, fs, "H9", "LIVE-OFFER")
}

func TestCrossServer(t *testing.T) {
	c := ivarConfig(t)
	akka := server("akka", "AKKA", akkaID)
	sol := server("sol", "SOL", solID)
	akka.Folders["id-a"] = folder("id-a", "LIVE-SAME", "/s/AKKA-LIVE-2PROJECTFILES/LIVE-SAME")
	sol.Folders["id-b"] = folder("id-b", "LIVE-SAME", "/s/SOL-LIVE-2PROJECTFILES/LIVE-SAME")
	akka.Folders["shared"] = folder("shared", "LIVE-MOVED", "/s/AKKA-LIVE-1SOURCE/LIVE-MOVED")
	sol.Folders["shared"] = folder("shared", "LIVE-MOVED", "/s/SOL-LIVE-2PROJECTFILES/LIVE-MOVED")
	akka.Folders["fine"] = folder("fine", "LIVE-FINE", "/s/AKKA-LIVE-1SOURCE/LIVE-FINE")
	sol.Folders["fine"] = folder("fine", "LIVE-FINE", "/s/SOL-LIVE-1SOURCE/LIVE-FINE")
	fs := run(t, c, akka, sol)
	mustFind(t, fs, "X1", "LIVE-SAME")
	mustFind(t, fs, "X2", "LIVE-MOVED")
	mustNot(t, fs, "X2", "LIVE-FINE")
}

func TestCleanup(t *testing.T) {
	c := ivarConfig(t)
	akka := server("akka", "AKKA", akkaID)
	never := "NEVERRR-7777777-7777777-7777777-7777777-7777777-7777777-7777777"
	akka.Devices[never] = &model.Device{ID: never, Name: "NEWPC"}
	akka.Devices[pcID] = &model.Device{ID: pcID, Name: "MORTIS", LastSeen: now}
	akka.Connections[pcID] = &model.Connection{Connected: true}
	f := folder("old", "LIVE-OLD", "/s/AKKA-LIVE-1SOURCE/LIVE-OLD", never, pcID)
	f.LastFileAt = now.Add(-200 * 24 * time.Hour)
	f.Completion[pcID] = &model.Completion{Percent: 0, RemoteState: "notSharing"}
	akka.Folders["old"] = f
	akka.Folders["new"] = folder("new", "LIVE-NEW", "/s/AKKA-LIVE-1SOURCE/LIVE-NEW")
	fs := run(t, c, akka)
	mustFind(t, fs, "C1", "LIVE-OLD")
	mustNot(t, fs, "C1", "LIVE-NEW")
	c3 := mustFind(t, fs, "C3", "never connected")
	if c3.MinDuration != 30*24*time.Hour {
		t.Fatal("C3 min duration")
	}
	mustFind(t, fs, "C3", "doesn't share it back")
	c2 := mustFind(t, fs, "C2", "NEWPC")
	if c2.MinDuration != 90*24*time.Hour {
		t.Fatal("C2 never-seen uses min duration")
	}
	h7 := mustFind(t, fs, "H7", "NEWPC")
	if h7.MinDuration != 7*24*time.Hour {
		t.Fatal("H7 never-seen uses min duration")
	}
}

func TestDisabledCheckAndUrgentOverride(t *testing.T) {
	c := ivarConfig(t)
	f, tr := false, true
	c.Checks["S6"] = config.Check{Enabled: &f}
	c.Checks["S4"] = config.Check{Urgent: &tr}
	cs, _ := c.Structure.Compile()
	akka := server("akka", "AKKA", akkaID)
	addShare(akka, cs, "/s/AKKA-LIVE-1SOURCE", "LIVE-DUP-1SOURCE", "junk")
	addShare(akka, cs, "/s/AKKA-LIVE-2PROJECTFILES", "LIVE-DUP-1SOURCE")
	fs := run(t, c, akka)
	mustNot(t, fs, "S6", "")
	j := mustFind(t, fs, "S4", "junk")
	if !j.Urgent {
		t.Fatal("urgent override")
	}
	// The DUP in PROJECTFILES is still S5 (wrong type) and S7, without the S6 reason.
	d := mustFind(t, fs, "S5", "AKKA-LIVE-2PROJECTFILES/LIVE-DUP")
	if strings.Join(d.Checks, ",") != "S5,S7" {
		t.Fatal(d.Checks)
	}
}

func TestWindowsPaths(t *testing.T) {
	c := ivarConfig(t)
	a := server("a", "WIN", akkaID)
	a.PathSep = `\`
	a.Folders["x"] = folder("x", "LIVE-X", `D:\data\WIN-LIVE-1SOURCE\LIVE-X`)
	a.Folders["y"] = folder("y", "LIVE-X inner", `d:\Data\win-live-1source\live-x\Inner`)
	fs := run(t, c, a)
	mustFind(t, fs, "S1", "LIVE-X inner inside LIVE-X")
}

func TestDetectShares(t *testing.T) {
	c := ivarConfig(t)
	cs, _ := c.Structure.Compile()
	a := server("a", "A", akkaID)
	a.Folders["1"] = folder("1", "", "/s/A-LIVE-1SOURCE/LIVE-P")
	a.Folders["2"] = folder("2", "", "/s/A-LIVE-1SOURCE/LIVE-Q/deeper")
	a.Folders["3"] = folder("3", "", "/elsewhere/random/folder")
	got := DetectShares(a, []string{"/s/A-LIVE-5COLLECT"}, nil, cs)
	if len(got) != 2 || got[0].Path != "/s/A-LIVE-1SOURCE" || got[1].Path != "/s/A-LIVE-5COLLECT" || !got[1].Manual {
		t.Fatalf("%+v", got)
	}
	if got[0].Attrs["type"] != "1SOURCE" {
		t.Fatal(got[0].Attrs)
	}
	got = DetectShares(a, nil, []string{"/s/A-LIVE-1SOURCE/"}, cs)
	if len(got) != 0 {
		t.Fatalf("exclude: %+v", got)
	}
}
