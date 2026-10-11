package engine

import (
	"reflect"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/model"
)

// MarkRepeated records the time of a repeat of the urgent message on an
// open finding, keeps it across a restart and a new message text, and
// skips a finding that is not open.
func TestMarkRepeated(t *testing.T) {
	r := newRig(t)
	r.hub.Update("b", func(s *model.Server) {
		s.Status, s.DownSince, s.LastError = model.StatusDown, r.now, "connection refused"
	})
	h1 := find(r.at(0), "H1", "B")
	if h1 == nil {
		t.Fatal("H1 missing")
	}
	pinged := r.now
	r.eng.MarkPinged([]string{h1.ID}, pinged)
	repeated := r.now.Add(2 * time.Hour)
	r.eng.MarkRepeated([]string{h1.ID, "no-such-finding"}, repeated)
	if got := find(r.eng.View(), "H1", "B"); got == nil || !got.RepeatedAt.Equal(repeated) || !got.PingedAt.Equal(pinged) {
		t.Fatalf("after the mark: %+v", got)
	}
	// Another error text changes the message: the times stay.
	r.hub.Update("b", func(s *model.Server) { s.LastError = "timed out" })
	r.at(time.Minute)
	r.restart(t)
	got := find(r.at(time.Minute), "H1", "B")
	if got == nil || !got.RepeatedAt.Equal(repeated) || !got.PingedAt.Equal(pinged) {
		t.Fatalf("after a new message and a restart: %+v", got)
	}

	// Resolved: a mark changes nothing.
	r.hub.Update("b", func(s *model.Server) { s.Status, s.LastError = model.StatusUp, "" })
	if find(r.at(time.Minute), "H1", "B") != nil {
		t.Fatal("H1 still open")
	}
	r.eng.MarkRepeated([]string{h1.ID}, repeated.Add(time.Hour))
	rec, err := r.st.Record(h1.ID)
	if err != nil || rec == nil || rec.Open() || !rec.RepeatedAt.Equal(repeated) {
		t.Fatalf("resolved record: %+v %v", rec, err)
	}
}

// laptopID is a device ID that the configuration accepts (devices[].id).
const laptopID = "LAPTOPD-AAAAAAA-BBBBBBB-CCCCCCC-DDDDDDD-EEEEEEE-FFFFFFF-GGGGGGG"

// withConfig gives the rig another configuration.
func (r *rig) withConfig(t *testing.T, yaml string) {
	t.Helper()
	c, err := config.ParseYAML([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if r.cfg, err = config.NewHolder(c); err != nil {
		t.Fatal(err)
	}
	r.restart(t)
}

// W-047: H12 for a device that always connects through a relay opens after
// relay_lost without a connection. Like H7, it is judged only while every
// server that has the device configured is reachable: no new H12 while one
// is not, and an open H12 stays as it is (stale).
func TestRelayDeviceWaitsForEveryServer(t *testing.T) {
	r := newRig(t)
	r.withConfig(t, `
servers: [{name: A, url: "https://a:1"}, {name: B, url: "https://b:1"}]
devices: [{id: `+laptopID+`, relay: true}]
thresholds: {relay_lost: 2m}
`)
	laptop := func(id string, connected bool) {
		r.hub.Update(id, func(s *model.Server) {
			s.Devices[laptopID] = &model.Device{ID: laptopID, Name: "LAPTOP-DANI", LastSeen: r.now}
			s.Connections[laptopID] = &model.Connection{Connected: connected, Type: "relay-client", StartedAt: r.now}
		})
	}
	laptop("a", true)
	laptop("b", true)
	if v := r.at(0); find(v, "H11", "LAPTOP-DANI") != nil || find(v, "H12", "") != nil {
		t.Fatal("a device that always uses a relay must get no H11 and no H12 while connected")
	}
	// The relay stops.
	laptop("a", false)
	laptop("b", false)
	if find(r.at(time.Minute), "H12", "LAPTOP-DANI") != nil {
		t.Fatal("H12 opened before relay_lost")
	}
	h12 := find(r.at(70*time.Second), "H12", "LAPTOP-DANI")
	if h12 == nil || h12.Stale || h12.Server != "" {
		t.Fatalf("H12 should open after relay_lost: %+v", h12)
	}
	// B is unreachable: B can have the device connected. H12 stays stale.
	r.hub.Update("b", func(s *model.Server) { s.Status, s.DownSince, s.LastError = model.StatusDown, r.now, "timeout" })
	if got := find(r.at(time.Minute), "H12", "LAPTOP-DANI"); got == nil || !got.Stale || got.ID != h12.ID {
		t.Fatalf("H12 must stay open and stale while B is unreachable: %+v", got)
	}
	// The device connects to A: while B is unreachable, H12 waits.
	laptop("a", true)
	if got := find(r.at(time.Minute), "H12", "LAPTOP-DANI"); got == nil || !got.Stale {
		t.Fatalf("H12 must wait for B: %+v", got)
	}
	r.hub.Update("b", func(s *model.Server) { s.Status = model.StatusUp })
	if find(r.at(time.Minute), "H12", "LAPTOP-DANI") != nil {
		t.Fatal("H12 should resolve when the device is connected and no server is stale")
	}
	// No new H12 opens while a server of the device is unreachable.
	laptop("a", false)
	laptop("b", false)
	r.hub.Update("b", func(s *model.Server) { s.Status, s.DownSince = model.StatusDown, r.now })
	if find(r.at(10*time.Minute), "H12", "LAPTOP-DANI") != nil {
		t.Fatal("H12 opened while a server of the device is unreachable")
	}
	r.hub.Update("b", func(s *model.Server) { s.Status = model.StatusUp })
	if find(r.at(time.Second), "H12", "LAPTOP-DANI") == nil {
		t.Fatal("H12 should open when B is back and the device is still away")
	}
}

// W-057: each evaluation sets the links between the servers and, with 2
// sites, between the sites in the snapshot of the view.
func TestEvaluateSetsLinks(t *testing.T) {
	r := newRig(t)
	r.hub.Update("a", func(s *model.Server) {
		s.Devices["B-ID"] = &model.Device{ID: "B-ID", Name: "B", LastSeen: r.now.Add(-time.Hour)}
		s.Connections["B-ID"] = &model.Connection{Connected: true, StartedAt: r.now.Add(-time.Hour)}
	})
	r.hub.Update("b", func(s *model.Server) {
		s.Devices["A-ID"] = &model.Device{ID: "A-ID", Name: "A", LastSeen: r.now.Add(-time.Hour)}
	})
	v := r.at(0)
	if want := []model.ServerLink{{A: "a", B: "b", State: model.LinkConnected, Since: r.now.Add(-time.Hour)}}; !reflect.DeepEqual(v.Snapshot.Links, want) || v.Snapshot.SiteLinks != nil {
		t.Fatalf("without sites: %+v %+v", v.Snapshot.Links, v.Snapshot.SiteLinks)
	}
	r.withConfig(t, `
servers: [{name: A, url: "https://a:1", site: IVAR Office}, {name: B, url: "https://b:1", site: Branch office}]
`)
	r.hub.Update("b", func(s *model.Server) { s.Status, s.DownSince = model.StatusDown, r.now })
	v = r.at(time.Minute)
	if len(v.Snapshot.Links) != 1 || v.Snapshot.Links[0].State != model.LinkUnreachable {
		t.Fatalf("B down: %+v", v.Snapshot.Links)
	}
	want := []model.SiteLink{{A: "IVAR Office", B: "Branch office", State: model.LinkUnreachable, Servers: [][2]string{{"a", "b"}}}}
	if !reflect.DeepEqual(v.Snapshot.SiteLinks, want) {
		t.Fatalf("site links: %+v", v.Snapshot.SiteLinks)
	}
	r.hub.Update("b", func(s *model.Server) { s.Status = model.StatusUp })
	if v = r.at(time.Minute); v.Snapshot.SiteLinks[0].State != model.LinkConnected || !v.Snapshot.SiteLinks[0].Since.Equal(r.now.Add(-time.Hour-2*time.Minute)) {
		t.Fatalf("B back: %+v", v.Snapshot.SiteLinks)
	}
	// A reconnect between 2 evaluations changes only the start of the
	// connection: the dashboards show it ("connected from 12:04").
	before := v.Version
	r.hub.Update("a", func(s *model.Server) { s.Connections["B-ID"].StartedAt = r.now })
	if v = r.at(time.Second); v.Version == before || !v.Snapshot.Links[0].Since.Equal(r.now.Add(-time.Second)) {
		t.Fatalf("reconnect: version %d, links %+v", v.Version, v.Snapshot.Links)
	}
}
