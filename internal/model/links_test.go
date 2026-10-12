package model

import (
	"reflect"
	"testing"
	"time"
)

// linked gives 2 servers each other as configured devices.
func linked(a, b *Server, aSawB, bSawA time.Time) {
	a.Devices[b.MyID] = &Device{ID: b.MyID, Name: b.Name, LastSeen: aSawB}
	b.Devices[a.MyID] = &Device{ID: a.MyID, Name: a.Name, LastSeen: bSawA}
}

func polled(id, name, myID string) *Server {
	return &Server{ID: id, Name: name, MyID: myID, Status: StatusUp, Loaded: true,
		Devices: map[string]*Device{}, Connections: map[string]*Connection{}}
}

// The links between the polled servers that have each other
// configured. Connected from the earlier start of the 2 sides; disconnected
// from the latest time they saw each other; unreachable when a server of
// the pair is not up or not loaded.
func TestServerLinks(t *testing.T) {
	t0 := time.Date(2026, 10, 11, 8, 12, 0, 0, time.UTC)
	akka, sol, render := polled("akka", "AKKA", "AKKA-ID"), polled("sol", "SOL", "SOL-ID"), polled("render", "RENDER", "RENDER-ID")
	other := polled("other", "OTHER", "OTHER-ID") // configured on nobody
	linked(akka, sol, t0, t0)
	linked(akka, render, t0.Add(10*time.Minute), t0.Add(20*time.Minute))
	linked(sol, render, t0, t0)
	akka.Connections["SOL-ID"] = &Connection{Connected: true, StartedAt: t0.Add(time.Minute)}
	sol.Connections["AKKA-ID"] = &Connection{Connected: true, StartedAt: t0}
	akka.Connections["RENDER-ID"] = &Connection{Connected: false, StartedAt: t0}
	render.Status = StatusDown
	snap := &Snapshot{Servers: []*Server{akka, sol, render, other}}
	want := []ServerLink{
		{A: "akka", B: "sol", State: LinkConnected, Since: t0},
		{A: "akka", B: "render", State: LinkUnreachable},
		{A: "sol", B: "render", State: LinkUnreachable},
	}
	if got := snap.ServerLinks(); !reflect.DeepEqual(got, want) {
		t.Fatalf("links\n got %+v\nwant %+v", got, want)
	}
	// RENDER is back, not connected to AKKA: from the latest last-seen time.
	render.Status = StatusUp
	if got := snap.ServerLinks()[1]; got != (ServerLink{A: "akka", B: "render", State: LinkDisconnected, Since: t0.Add(20 * time.Minute)}) {
		t.Fatalf("disconnected: %+v", got)
	}
	// Only one side connected, without a start time: connected, time not known.
	sol.Connections["AKKA-ID"].Connected = false
	akka.Connections["SOL-ID"].StartedAt = time.Time{}
	if got := snap.ServerLinks()[0]; got != (ServerLink{A: "akka", B: "sol", State: LinkConnected}) {
		t.Fatalf("one side: %+v", got)
	}
	// Not loaded since the start: syncwatch cannot tell.
	sol.Loaded = false
	if got := snap.ServerLinks()[0]; got.State != LinkUnreachable || !got.Since.IsZero() {
		t.Fatalf("not loaded: %+v", got)
	}
	// Configured on one side only, or a device ID not known: no link.
	delete(sol.Devices, "RENDER-ID")
	render.MyID = ""
	if got := snap.ServerLinks(); len(got) != 1 {
		t.Fatalf("pairs: %+v", got)
	}
	if (&Snapshot{}).ServerLinks() != nil {
		t.Fatal("no servers: nil")
	}
}

// The server links between 2 sites make 1 site link: connected when
// 1 is connected (from the earliest start), else unreachable when 1 is
// unreachable, else disconnected (from the latest time). Links inside a
// site and servers without a site are not included.
func TestSiteLinks(t *testing.T) {
	t0 := time.Date(2026, 10, 11, 8, 12, 0, 0, time.UTC)
	sites := map[string]string{"akka": "IVAR Office", "sol": "Branch office", "render": "IVAR Office"}
	siteOf := func(id string) string { return sites[id] }
	order := []string{"akka", "sol", "render", "lab"}
	links := []ServerLink{
		{A: "akka", B: "sol", State: LinkDisconnected, Since: t0},
		{A: "akka", B: "render", State: LinkConnected, Since: t0}, // inside IVAR Office
		{A: "sol", B: "render", State: LinkConnected, Since: t0.Add(time.Hour)},
		{A: "sol", B: "lab", State: LinkConnected, Since: t0}, // lab has no site
	}
	want := []SiteLink{{A: "IVAR Office", B: "Branch office", State: LinkConnected, Since: t0.Add(time.Hour), Servers: [][2]string{{"akka", "sol"}, {"sol", "render"}}}}
	if got := SiteLinks(links, siteOf, order); !reflect.DeepEqual(got, want) {
		t.Fatalf("site links\n got %+v\nwant %+v", got, want)
	}
	// Both disconnected: from the latest time they saw each other.
	links[2] = ServerLink{A: "sol", B: "render", State: LinkDisconnected, Since: t0.Add(2 * time.Hour)}
	if got := SiteLinks(links, siteOf, order)[0]; got.State != LinkDisconnected || !got.Since.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("disconnected: %+v", got)
	}
	// SOL is unreachable: syncwatch cannot tell.
	links[0].State, links[2].State = LinkUnreachable, LinkUnreachable
	links[0].Since, links[2].Since = time.Time{}, time.Time{}
	if got := SiteLinks(links, siteOf, order)[0]; got.State != LinkUnreachable || !got.Since.IsZero() {
		t.Fatalf("unreachable: %+v", got)
	}
	// 1 unreachable and 1 disconnected: unreachable.
	links[2] = ServerLink{A: "sol", B: "render", State: LinkDisconnected, Since: t0}
	if got := SiteLinks(links, siteOf, order)[0]; got.State != LinkUnreachable || !got.Since.IsZero() {
		t.Fatalf("unreachable and disconnected: %+v", got)
	}
	// Site A is the site of the first server in the configuration.
	if got := SiteLinks(links, siteOf, []string{"sol", "akka", "render"})[0]; got.A != "Branch office" || got.B != "IVAR Office" || got.Servers[0] != [2]string{"akka", "sol"} {
		t.Fatalf("order: %+v", got)
	}
	// 1 site only: no site links.
	sites["sol"] = "IVAR Office"
	if got := SiteLinks(links, siteOf, order); got != nil {
		t.Fatalf("1 site: %+v", got)
	}
	if got := SiteLinks(nil, siteOf, order); got != nil {
		t.Fatalf("no links: %+v", got)
	}
}
