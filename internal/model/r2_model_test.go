package model

import (
	"testing"
	"time"
)

// A clone shares no map or slice of the wave 3 fields with the original:
// the engine and the hub change their copies while pages read the others.
func TestCloneOfR2Fields(t *testing.T) {
	at := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	sv := &Server{ID: "akka", Folders: map[string]*Folder{"f": {ID: "f", FirstSeen: at, Completion: map[string]*Completion{"PC": {GlobalBytes: 100, NeedBytes: 40}}}}}
	sv.NeedLists = map[string]map[string]NeedList{"f": {"PC": {Names: []string{"a.txt", "b.txt"}, Items: 18, At: at}}}
	sv.AddProgress("f", "PC", ProgressSample{At: at, NeedBytes: 40, GlobalBytes: 100})
	sv.RelayListeners = []RelayListener{{URI: "relay://10.115.1.60:22067", Connected: true, Since: at}}
	snap := &Snapshot{Servers: []*Server{sv},
		Links:     []ServerLink{{A: "akka", B: "sol", State: LinkConnected, Since: at}},
		SiteLinks: []SiteLink{{A: "IVAR Office", B: "Branch office", State: LinkConnected, Since: at, Servers: [][2]string{{"akka", "sol"}}}}}

	cl := snap.Clone()
	csv := cl.Servers[0]
	if csv.NeedLists["f"]["PC"].Names[1] != "b.txt" || len(csv.Progress["f"]["PC"]) != 1 || csv.RelayListeners[0].URI != "relay://10.115.1.60:22067" ||
		!csv.Folders["f"].FirstSeen.Equal(at) || csv.Folders["f"].Completion["PC"].GlobalBytes != 100 ||
		cl.Links[0] != snap.Links[0] || cl.SiteLinks[0].Servers[0] != [2]string{"akka", "sol"} {
		t.Fatalf("the clone lost values: %+v", cl)
	}
	csv.NeedLists["f"]["PC"].Names[0] = "changed"
	csv.NeedLists["f"]["OTHER"] = NeedList{}
	csv.Progress["f"]["PC"][0].NeedBytes = 1
	csv.AddProgress("f", "PC", ProgressSample{At: at.Add(time.Hour), NeedBytes: 2})
	csv.Progress["g"] = nil
	csv.RelayListeners[0].Connected = false
	csv.Folders["f"].Completion["PC"].GlobalBytes = 1
	cl.Links[0].State = LinkDisconnected
	cl.SiteLinks[0].Servers[0][1] = "changed"
	cl.SiteLinks[0].State = LinkUnreachable
	if sv.NeedLists["f"]["PC"].Names[0] != "a.txt" || len(sv.NeedLists["f"]) != 1 {
		t.Error("NeedLists are shared")
	}
	if ps := sv.Progress["f"]["PC"]; len(ps) != 1 || ps[0].NeedBytes != 40 || len(sv.Progress) != 1 {
		t.Errorf("Progress is shared: %+v", sv.Progress)
	}
	if !sv.RelayListeners[0].Connected {
		t.Error("RelayListeners are shared")
	}
	if sv.Folders["f"].Completion["PC"].GlobalBytes != 100 {
		t.Error("Completion is shared")
	}
	if snap.Links[0].State != LinkConnected || snap.SiteLinks[0].State != LinkConnected || snap.SiteLinks[0].Servers[0][1] != "sol" {
		t.Errorf("links are shared: %+v %+v", snap.Links, snap.SiteLinks)
	}
	// A server without the new values clones without them.
	if c := (&Server{ID: "sol"}).Clone(); len(c.NeedLists) != 0 || len(c.Progress) != 0 || c.RelayListeners != nil {
		t.Errorf("empty server: %+v", c)
	}
	if c := (&Snapshot{}).Clone(); c.Links != nil || c.SiteLinks != nil {
		t.Errorf("empty snapshot: %+v", c)
	}
}

// AddProgress keeps samples at least 1 minute apart for each folder and
// device, for 2 hours before the newest sample, and at most 120.
func TestAddProgress(t *testing.T) {
	at := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	sv := &Server{ID: "akka"}
	add := func(dev string, d time.Duration, need int64) {
		sv.AddProgress("f", dev, ProgressSample{At: at.Add(d), NeedBytes: need, GlobalBytes: 1000})
	}
	add("PC", 0, 900)
	add("PC", 30*time.Second, 800)    // too soon: not kept
	add("PC", -time.Minute, 950)      // older than the newest: not kept
	add("PC", time.Minute, 700)       // 1 minute later: kept
	add("", 10*time.Second, 500)      // the server's own copy is a pair of its own
	add("LAPTOP", 10*time.Second, 10) // so is another device
	ps := sv.Progress["f"]["PC"]
	if len(ps) != 2 || ps[0].NeedBytes != 900 || ps[1].NeedBytes != 700 || ps[1].GlobalBytes != 1000 {
		t.Fatalf("PC samples: %+v", ps)
	}
	if len(sv.Progress["f"][""]) != 1 || len(sv.Progress["f"]["LAPTOP"]) != 1 {
		t.Fatalf("other pairs: %+v", sv.Progress["f"])
	}

	// The window: a sample exactly 2 hours older than the newest stays, an
	// older one goes.
	add("PC", 2*time.Hour, 600)
	if ps := sv.Progress["f"]["PC"]; len(ps) != 3 || !ps[0].At.Equal(at) {
		t.Fatalf("after 2 hours: %+v", ps)
	}
	add("PC", 2*time.Hour+time.Minute, 500)
	if ps := sv.Progress["f"]["PC"]; len(ps) != 3 || !ps[0].At.Equal(at.Add(time.Minute)) || ps[2].NeedBytes != 500 {
		t.Fatalf("after 2 hours and 1 minute: %+v", ps)
	}

	// The limit: 1 sample a minute for 3 hours keeps the last 120.
	sv = &Server{ID: "akka"}
	for i := 0; i < 180; i++ {
		add("PC", time.Duration(i)*time.Minute, int64(1000-i))
	}
	ps = sv.Progress["f"]["PC"]
	if len(ps) != maxProgress || !ps[0].At.Equal(at.Add(60*time.Minute)) || !ps[len(ps)-1].At.Equal(at.Add(179*time.Minute)) {
		t.Fatalf("%d samples from %v to %v", len(ps), ps[0].At, ps[len(ps)-1].At)
	}
	// Samples 30 seconds apart: every second one is kept.
	sv = &Server{ID: "akka"}
	for i := 0; i < 10; i++ {
		add("PC", time.Duration(i)*30*time.Second, 0)
	}
	if n := len(sv.Progress["f"]["PC"]); n != 5 {
		t.Fatalf("%d samples kept of 10 in 4.5 minutes, want 5", n)
	}
}
