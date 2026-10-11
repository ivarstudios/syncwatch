package model

import (
	"fmt"
	"strings"
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

// AddProgress keeps for each folder and device samples about 1 minute
// apart (at most 2 in a minute) for 2 hours before the newest sample, and
// at most 120. The newest sample always has the newest need, with the time
// when that need began (W-059).
func TestAddProgress(t *testing.T) {
	at := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	sv := &Server{ID: "akka"}
	add := func(dev string, d time.Duration, need int64) {
		sv.AddProgress("f", dev, ProgressSample{At: at.Add(d), NeedBytes: need, GlobalBytes: 1000})
	}
	needs := func(dev string) string {
		var parts []string
		for _, p := range sv.Progress["f"][dev] {
			parts = append(parts, fmt.Sprintf("%s=%d", p.At.Sub(at), p.NeedBytes))
		}
		return strings.Join(parts, " ")
	}
	add("PC", 0, 900)
	add("PC", 10*time.Second, 850)    // the newest sample
	add("PC", 30*time.Second, 800)    // less than 1 minute after the sample before the newest: replaces the newest
	add("PC", 50*time.Second, 800)    // the same need: the newest keeps its time
	add("PC", 20*time.Second, 950)    // not after the newest: not kept
	add("PC", time.Minute, 700)       // 1 minute after the sample before the newest: added
	add("", 10*time.Second, 500)      // the server's own copy is a pair of its own
	add("LAPTOP", 10*time.Second, 10) // so is another device
	if got := needs("PC"); got != "0s=900 30s=800 1m0s=700" || sv.Progress["f"]["PC"][2].GlobalBytes != 1000 {
		t.Fatalf("PC samples: %s %+v", got, sv.Progress["f"]["PC"])
	}
	if len(sv.Progress["f"][""]) != 1 || len(sv.Progress["f"]["LAPTOP"]) != 1 {
		t.Fatalf("other pairs: %+v", sv.Progress["f"])
	}

	// The window: a sample exactly 2 hours older than the newest stays, an
	// older one goes.
	add("PC", 2*time.Hour, 600)
	if got := needs("PC"); got != "0s=900 30s=800 1m0s=700 2h0m0s=600" {
		t.Fatalf("after 2 hours: %s", got)
	}
	add("PC", 2*time.Hour+30*time.Second, 500)
	if got := needs("PC"); got != "30s=800 1m0s=700 2h0m0s=600 2h0m30s=500" {
		t.Fatalf("after 2 hours and 30 seconds: %s", got)
	}

	// The limit: 1 sample a minute for 3 hours keeps the last 120.
	sv = &Server{ID: "akka"}
	for i := 0; i < 180; i++ {
		add("PC", time.Duration(i)*time.Minute, int64(1000-i))
	}
	ps := sv.Progress["f"]["PC"]
	if len(ps) != maxProgress || !ps[0].At.Equal(at.Add(60*time.Minute)) || !ps[len(ps)-1].At.Equal(at.Add(179*time.Minute)) {
		t.Fatalf("%d samples from %v to %v", len(ps), ps[0].At, ps[len(ps)-1].At)
	}
	// A new need every 10 seconds: a sample and the sample after the next
	// one are at least 1 minute apart, and the newest has the newest need.
	sv = &Server{ID: "akka"}
	for i := 0; i < 18; i++ {
		add("PC", time.Duration(i)*10*time.Second, int64(1000-i))
	}
	if got := needs("PC"); got != "0s=1000 50s=995 1m40s=990 2m30s=985 2m50s=983" {
		t.Fatalf("samples of 3 minutes: %s", got)
	}

	// W-059 (live re-test of wave 3, P2 a): a PC receives its files in 10
	// seconds after it connects and then stops with 57 bytes that it cannot
	// store. The end of the change stays a sample, also when the full
	// checks add samples with the same need later.
	sv = &Server{ID: "akka"}
	add("PC", 0, 458_934)
	add("PC", 5*time.Second, 200_000)
	add("PC", 10*time.Second, 57)
	add("PC", 5*time.Minute, 57)
	add("PC", 10*time.Minute, 57)
	if got := needs("PC"); got != "0s=458934 10s=57 5m0s=57 10m0s=57" {
		t.Fatalf("samples of a PC that stopped: %s", got)
	}
}
