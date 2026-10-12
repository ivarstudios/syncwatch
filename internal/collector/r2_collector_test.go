package collector

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/rules"
	"github.com/ivarstudios/syncwatch/internal/stclient"
	"github.com/ivarstudios/syncwatch/internal/store"
)

// Devices of the fixture: SIRIUS shares p-beta and p-gamma, SOL p-alpha
// and p-beta. Both are connected to AKKA.
const (
	devSirius = "OQSKX2A-JN66XDT-SLLKWHV-JFRH22Q-CIWTGF2-XE4FYJ4-4OHDIZW-EGSFRQ7"
	devSol    = "VAO5AMX-52V3GKC-HC5PP6N-TEESAVH-GWYJG4Q-JUOUQOP-FJ6RBTS-TJTKEAF"
)

const (
	kitRelay  = "relay://10.123.1.60:22067/?id=JYSLS6U-5C7LIXN-KXWTAPB-PMJBIYG-OGMFVCC-GQTJJRG-IKYZ2ZX-2QBLQAC&token=private-relay-token"
	dynRelays = "dynamic+https://relays.syncthing.net/endpoint"
)

func errText(s string) *string { return &s }

// W-047: the relay listeners of /rest/system/status. The WAN address of a
// relay listener stays when its relay stops (Syncthing 2.1.6 in the kit):
// the error tells it. Since changes only when Connected changes.
func TestRelayListeners(t *testing.T) {
	t0 := time.Date(2026, 10, 11, 18, 33, 40, 0, time.UTC)
	status := map[string]stclient.ListenerStatus{
		kitRelay:               {LANAddresses: []string{kitRelay}, WANAddresses: []string{kitRelay}},
		dynRelays:              {}, // no relay found yet
		"tcp://0.0.0.0:22000":  {LANAddresses: []string{"tcp://0.0.0.0:22000"}, WANAddresses: []string{"tcp://0.0.0.0:22000"}},
		"quic://0.0.0.0:22000": {Error: errText("quic: listen failed")},
	}
	ok := map[string]time.Time{}
	got := relayListeners(nil, ok, status, t0)
	want := []model.RelayListener{
		{URI: dynRelays, Connected: false, Since: t0},
		{URI: "relay://10.123.1.60:22067", Connected: true, Since: t0},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("listeners\n got %+v\nwant %+v", got, want)
	}

	// The relay stops: an error, the WAN address stays.
	t1 := t0.Add(time.Minute)
	status[kitRelay] = stclient.ListenerStatus{Error: errText("dial tcp 10.123.1.60:22067: connect: connection refused"),
		LANAddresses: []string{kitRelay}, WANAddresses: []string{kitRelay}}
	got = relayListeners(got, ok, status, t1)
	if r := got[1]; r.Connected || r.Since != t1 || r.Error != "dial tcp 10.123.1.60:22067: connect: connection refused" {
		t.Fatalf("stopped relay: %+v", r)
	}
	if d := got[0]; d.Since != t0 {
		t.Fatalf("the unchanged listener got a new time: %+v", d)
	}
	got = relayListeners(got, ok, status, t1.Add(time.Minute))
	if r := got[1]; r.Since != t1 {
		t.Fatalf("still stopped: Since %v, want %v", r.Since, t1)
	}

	// The relay is back, and the dynamic listener finds a relay. The first
	// sample without an error is not a connection yet (see
	// TestRelayListenerRetryIsNotAConnection); the sample 1 minute later
	// confirms it, from the time of the first one.
	t3 := t1.Add(5 * time.Minute)
	status[kitRelay] = stclient.ListenerStatus{WANAddresses: []string{kitRelay}}
	status[dynRelays] = stclient.ListenerStatus{WANAddresses: []string{"relay://203.0.113.7:22067/?id=X"}}
	got = relayListeners(got, ok, status, t3)
	if got[0].Connected || got[0].Since != t0 || got[1].Connected || got[1].Since != t1 || got[1].Error == "" {
		t.Fatalf("one sample without an error: %+v", got)
	}
	got = relayListeners(got, ok, status, t3.Add(time.Minute))
	for _, l := range got {
		if !l.Connected || l.Since != t3 || l.Error != "" {
			t.Fatalf("connected again: %+v", l)
		}
		if strings.Contains(l.URI, "token") || strings.Contains(l.URI, "?") {
			t.Fatalf("URI with its query: %q", l.URI)
		}
	}
	if len(ok) != 0 {
		t.Fatalf("confirmations left: %v", ok)
	}
	if got := relayListeners(got, ok, nil, t3); got != nil {
		t.Fatalf("no listeners: %+v", got)
	}
}

// While the relay is off, Syncthing (2.1.5 and 2.1.6 in kit slot s20)
// clears the error of the relay listener for some seconds at each new try to
// connect, approximately every 70 seconds; the WAN address stays. A sample
// in that time is no connection: the listener stays not connected from the
// first failure, thus H12 opens after relay_lost and stays open. The
// samples below are the 1-minute samples of an outage as the kit gave them.
func TestRelayListenerRetryIsNotAConnection(t *testing.T) {
	t0 := time.Date(2026, 10, 11, 19, 39, 43, 0, time.UTC)
	refused := stclient.ListenerStatus{Error: errText("dial tcp 10.120.1.60:22067: connect: no route to host"), WANAddresses: []string{kitRelay}}
	retry := stclient.ListenerStatus{WANAddresses: []string{kitRelay}} // the error is cleared during a new try
	ok := map[string]time.Time{}
	got := relayListeners([]model.RelayListener{{URI: "relay://10.123.1.60:22067", Connected: true, Since: t0.Add(-time.Hour)}}, ok,
		map[string]stclient.ListenerStatus{kitRelay: refused}, t0)
	if got[0].Connected || got[0].Since != t0 {
		t.Fatalf("the relay stops: %+v", got[0])
	}
	for i, ls := range []stclient.ListenerStatus{refused, retry, refused, retry, refused, refused, retry, refused} {
		at := t0.Add(time.Duration(i+1) * time.Minute)
		got = relayListeners(got, ok, map[string]stclient.ListenerStatus{kitRelay: ls}, at)
		if l := got[0]; l.Connected || l.Since != t0 || !strings.Contains(l.Error, "no route to host") {
			t.Fatalf("sample %d at %s: %+v", i+1, at.Format("15:04:05"), l)
		}
	}
	// 2 samples without an error less than relayConfirm apart (a full check
	// right after the minute's sample) are not enough either.
	at := t0.Add(10 * time.Minute)
	got = relayListeners(got, ok, map[string]stclient.ListenerStatus{kitRelay: retry}, at)
	got = relayListeners(got, ok, map[string]stclient.ListenerStatus{kitRelay: retry}, at.Add(5*time.Second))
	if got[0].Connected {
		t.Fatalf("5 seconds without an error: %+v", got[0])
	}
	got = relayListeners(got, ok, map[string]stclient.ListenerStatus{kitRelay: retry}, at.Add(time.Minute))
	if l := got[0]; !l.Connected || l.Since != at || l.Error != "" {
		t.Fatalf("the relay is back: %+v", l)
	}
}

// The URI that H12 shows has no secret: no query (the token of a private
// relay) and no user information (the password of a private relay pool,
// which Syncthing sends to the pool as basic authentication).
func TestRelayURI(t *testing.T) {
	for in, want := range map[string]string{
		kitRelay:                         "relay://10.123.1.60:22067",
		"relay://relay.sim.invalid:443/": "relay://relay.sim.invalid:443",
		dynRelays:                        dynRelays,
		"dynamic+https://relays.example.invalid/endpoint?x=1":                  "dynamic+https://relays.example.invalid/endpoint",
		"dynamic+https://ivar:pool-pass-123@relays.example.invalid/endpoint":   "dynamic+https://relays.example.invalid/endpoint",
		"dynamic+http://ivar@relays.example.invalid/endpoint":                  "dynamic+http://relays.example.invalid/endpoint",
		"relay://ivar:relay-pass-456@10.115.1.60:22067/?id=X&token=t":          "relay://10.115.1.60:22067",
		"relay://%zz/?token=secret":                                            "relay://%zz", // not a URL: the query still goes
		"relay://ivar:relay-pass-789@%zz/?token=secret":                        "relay://%zz", // and the user information
		"dynamic+https://ivar:pass/with@slash@relays.example.invalid/endpoint": "dynamic+https://relays.example.invalid/endpoint",
		"tcp://0.0.0.0:22000":  "",
		"quic://0.0.0.0:22000": "",
		"relay":                "",
	} {
		got, ok := relayURI(in)
		if got != want || ok != (want != "") {
			t.Errorf("relayURI(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}

// The relay listeners come from the session start and the full check, and
// each minute with the traffic counters: a stopped relay shows without a
// new session or full check.
func TestRelayListenersSampled(t *testing.T) {
	old := trafficEvery
	trafficEvery = 200 * time.Millisecond
	defer func() { trafficEvery = old }()
	fake := newFakeST(t)
	fake.listeners = map[string]stclient.ListenerStatus{
		kitRelay:              {WANAddresses: []string{kitRelay}},
		"tcp://0.0.0.0:22000": {WANAddresses: []string{"tcp://0.0.0.0:22000"}},
	}
	srv := httptest.NewTLSServer(fake)
	defer srv.Close()
	hub, _, _, stop := startManager(t, srv, "collector: {event_timeout: 5s}", nil, nil)
	defer stop()
	sv := waitFor(t, hub, "the relay listener", func(s *model.Server) bool { return s.Loaded && len(s.RelayListeners) == 1 })
	l := sv.RelayListeners[0]
	if !l.Connected || l.URI != "relay://10.123.1.60:22067" || l.Since.IsZero() {
		t.Fatalf("listener %+v", l)
	}
	full := fake.count("/rest/db/status")
	fake.mu.Lock()
	fake.listeners[kitRelay] = stclient.ListenerStatus{Error: errText("dial tcp 10.123.1.60:22067: connect: connection refused"), WANAddresses: []string{kitRelay}}
	fake.mu.Unlock()
	sv = waitFor(t, hub, "the stopped relay", func(s *model.Server) bool { return len(s.RelayListeners) == 1 && !s.RelayListeners[0].Connected })
	if s := sv.RelayListeners[0]; !s.Since.After(l.Since) || !strings.Contains(s.Error, "connection refused") {
		t.Fatalf("stopped relay: %+v", s)
	}
	if fake.count("/rest/db/status") != full {
		t.Fatal("a full check ran: the minute's sample must see the relay")
	}
}

// W-058: the full check reads the first 20 names of the files that a
// connected device still needs, for each folder with need items, through
// the throttle of the check, and replaces the lists at each full check.
func TestNeedListsFromFullCheck(t *testing.T) {
	fake := newFakeST(t)
	fake.need = map[string]int64{"p-beta/" + devSirius: 25}
	for i := 0; i < 25; i++ {
		fake.needNames = append(fake.needNames, fmt.Sprintf("delivery/from-mac/shot-%02d.mov", i))
	}
	srv := httptest.NewTLSServer(fake)
	defer srv.Close()
	hub, m, _, stop := startManager(t, srv, "collector: {event_timeout: 5s}", nil, nil)
	defer stop()
	sv := waitFor(t, hub, "full check", func(s *model.Server) bool { return s.Loaded })
	nl, ok := sv.NeedLists["p-beta"][devSirius]
	if !ok || len(sv.NeedLists) != 1 || len(sv.NeedLists["p-beta"]) != 1 {
		t.Fatalf("need lists %+v", sv.NeedLists)
	}
	comp := sv.Folders["p-beta"].Completion[devSirius]
	if len(nl.Names) != 20 || nl.Names[0] != "delivery/from-mac/shot-00.mov" || nl.Names[19] != "delivery/from-mac/shot-19.mov" || nl.Items != 25 || !nl.At.Equal(comp.UpdatedAt) {
		t.Fatalf("need list %+v (completion %+v)", nl, comp)
	}
	if n := fake.count("/rest/db/remoteneed"); n != 1 {
		t.Fatalf("%d remoteneed requests, want 1 (only the device with need items)", n)
	}

	// The device has its files now: the next full check has no list.
	fake.mu.Lock()
	fake.need = nil
	fake.mu.Unlock()
	last := sv.LastFullCheck
	m.RescanNow()
	sv = waitFor(t, hub, "the next full check", func(s *model.Server) bool { return s.LastFullCheck.After(last) })
	if len(sv.NeedLists) != 0 || fake.count("/rest/db/remoteneed") != 1 {
		t.Fatalf("need lists %+v after the sync, %d requests", sv.NeedLists, fake.count("/rest/db/remoteneed"))
	}
}

// Syncthing lists the deletions that a device did not do yet in
// /rest/db/remoteneed, in the order of the names, but counts them in
// needDeletes and not in needItems: Syncthing 2.1.5 with a PC that cannot
// delete 25 files and cannot replace 2 files gives needItems 2 and
// needDeletes 25. A deletion is not a file that the device needs: the full
// check reads 100 entries and keeps the first 20 names that are not
// deletions, and H5 shows the 2 files and no "…and N more".
func TestNeedListsWithoutDeletions(t *testing.T) {
	fake := newFakeST(t)
	fake.need = map[string]int64{"p-beta/" + devSirius: 2}
	fake.needNames = []string{"busy.txt", "zz-late.txt"} // before and after the deletions
	for i := 0; i < 25; i++ {
		fake.needDeleted = append(fake.needDeleted, fmt.Sprintf("keep%02d.txt", i))
	}
	srv := httptest.NewTLSServer(fake)
	defer srv.Close()
	hub, _, _, stop := startManager(t, srv, "collector: {event_timeout: 5s}", nil, nil)
	defer stop()
	sv := waitFor(t, hub, "full check", func(s *model.Server) bool { return s.Loaded })
	if nl := sv.NeedLists["p-beta"][devSirius]; fmt.Sprint(nl.Names) != "[busy.txt zz-late.txt]" || nl.Items != 2 {
		t.Fatalf("need list %+v, want the 2 files and no deletion", nl)
	}
	cfg, err := config.ParseYAML([]byte(fmt.Sprintf("servers: [{name: AKKA, url: %q}]\n", srv.URL)))
	if err != nil {
		t.Fatal(err)
	}
	var row string
	for _, f := range rules.Evaluate(rules.Input{Snapshot: hub.Snapshot(), Config: cfg, Now: time.Now()}) {
		if f.Check == "H5" && f.DetailValue("Folder ID") == "p-beta" && f.DetailValue("Device ID") == devSirius {
			row = f.DetailValue("Needed files")
		}
	}
	if row != "busy.txt · zz-late.txt" {
		t.Fatalf("H5 row \"Needed files\" %q", row)
	}
}

// A gate from before /rest/db/remoteneed refuses it with 404: no names, no
// folder error, the full check completes, one INFO line, and the session
// asks no more. The next session asks again (a new gate). The gate of the
// published version 78ef2d8 says "endpoint not allowed", the later gates
// "endpoint not permitted".
func TestNeedListWithOldGate(t *testing.T) {
	for _, refusal := range []string{"syncwatch gate: endpoint not permitted", "syncwatch gate: endpoint not allowed"} {
		t.Run(refusal, func(t *testing.T) { needListWithOldGate(t, refusal) })
	}
}

func needListWithOldGate(t *testing.T, refusal string) {
	fake := newFakeST(t)
	fake.need = map[string]int64{"p-beta/" + devSirius: 3, "p-gamma/" + devSirius: 2}
	fake.needNames = []string{"a.txt", "b.txt", "c.txt"}
	fake.needStatus, fake.needBody = http.StatusNotFound, refusal
	srv := httptest.NewTLSServer(fake)
	defer srv.Close()
	var logs lockedBuf
	log := slog.New(slog.NewTextHandler(&logs, nil))
	hub, m, _, stop := startManager(t, srv, "collector: {event_timeout: 5s}", log, nil)
	defer stop()
	sv := waitFor(t, hub, "full check", func(s *model.Server) bool { return s.Loaded })
	if sv.Status != model.StatusUp || sv.LastError != "" || len(sv.NeedLists) != 0 {
		t.Fatalf("server %s %q, need lists %+v", sv.Status, sv.LastError, sv.NeedLists)
	}
	for id, f := range sv.Folders {
		if f.Error != "" {
			t.Fatalf("folder %s has the error %q", id, f.Error)
		}
	}
	const line = `level=INFO msg="the gate does not permit /rest/db/remoteneed: update the gate to see the needed files in H5"`
	asked := fake.count("/rest/db/remoteneed")
	if logs.count(line) != 1 || asked < 1 || asked > 2 {
		t.Fatalf("%d INFO lines, %d requests:\n%s", logs.count(line), asked, logs.b.String())
	}
	last := sv.LastFullCheck
	m.RescanNow()
	waitFor(t, hub, "the next full check", func(s *model.Server) bool { return s.LastFullCheck.After(last) })
	if n := fake.count("/rest/db/remoteneed"); n != asked || logs.count(line) != 1 {
		t.Fatalf("the same session asked again: %d requests, %d lines", n, logs.count(line))
	}

	// The gate is updated: a new session (here after a restart of Syncthing)
	// reads the names.
	fake.mu.Lock()
	fake.needStatus = 0
	fake.mu.Unlock()
	dropAndReconnect(t, fake, m, func() {
		fake.mu.Lock()
		fake.startTime = "2026-10-11T09:00:00+02:00"
		fake.mu.Unlock()
	})
	sv = waitFor(t, hub, "the names", func(s *model.Server) bool { return len(s.NeedLists["p-beta"]) == 1 && len(s.NeedLists["p-gamma"]) == 1 })
	if nl := sv.NeedLists["p-gamma"][devSirius]; len(nl.Names) != 3 || nl.Items != 2 {
		t.Fatalf("need list %+v", nl)
	}
}

// Another error of /rest/db/remoteneed costs only the names: a DEBUG line,
// no folder error, and the next full check asks again.
func TestNeedListErrorIsNotAFolderError(t *testing.T) {
	fake := newFakeST(t)
	fake.need = map[string]int64{"p-alpha/" + devSol: 4}
	fake.needStatus, fake.needBody = http.StatusNotFound, "no such folder"
	srv := httptest.NewTLSServer(fake)
	defer srv.Close()
	var logs lockedBuf
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	hub, m, _, stop := startManager(t, srv, "collector: {event_timeout: 5s}", log, nil)
	defer stop()
	sv := waitFor(t, hub, "full check", func(s *model.Server) bool { return s.Loaded })
	if sv.Status != model.StatusUp || sv.Folders["p-alpha"].Error != "" || len(sv.NeedLists) != 0 {
		t.Fatalf("server %s, p-alpha error %q, need lists %+v", sv.Status, sv.Folders["p-alpha"].Error, sv.NeedLists)
	}
	if logs.count(`level=DEBUG msg="cannot read the files that a device needs"`) != 1 || logs.count("level=WARN") != 0 {
		t.Fatalf("log:\n%s", logs.b.String())
	}
	last := sv.LastFullCheck
	m.RescanNow()
	waitFor(t, hub, "the next full check", func(s *model.Server) bool { return s.LastFullCheck.After(last) })
	if n := fake.count("/rest/db/remoteneed"); n != 2 {
		t.Fatalf("%d requests, want 1 for each full check", n)
	}
}

// W-059: the full check and the events record progress samples with the
// need and the size of the folder, for each device and for the server's
// own copy (device ""). An event less than 1 minute after the sample before
// the newest one replaces the newest sample: the newest sample has the
// newest need (model.AddProgress).
func TestProgressSamples(t *testing.T) {
	fake, hub, _, _, stop := setup(t)
	defer stop()
	sv := waitFor(t, hub, "full check", func(s *model.Server) bool { return s.Loaded })
	own, dev := sv.Progress["p-alpha"][""], sv.Progress["p-alpha"][devSol]
	comp := sv.Folders["p-alpha"].Completion[devSol]
	if len(own) != 1 || own[0].GlobalBytes != 43 || own[0].NeedBytes != 0 {
		t.Fatalf("samples of the own copy: %+v", own)
	}
	if len(dev) != 1 || dev[0].GlobalBytes != 43 || comp.GlobalBytes != 43 || !dev[0].At.Equal(comp.UpdatedAt) {
		t.Fatalf("samples of SOL: %+v, completion %+v", dev, comp)
	}

	base := own[0].At
	fake.pushAt("FolderCompletion", base.Add(90*time.Second), map[string]any{"folder": "p-alpha", "device": devSol,
		"completion": 50, "globalBytes": 2000, "needBytes": 1000, "needItems": 1, "remoteState": "valid"})
	fake.pushAt("FolderSummary", base.Add(90*time.Second), map[string]any{"folder": "p-alpha",
		"summary": map[string]any{"state": "syncing", "globalBytes": 2000, "needBytes": 500, "needTotalItems": 1}})
	fake.pushAt("FolderCompletion", base.Add(100*time.Second), map[string]any{"folder": "p-alpha", "device": devSol,
		"completion": 55, "globalBytes": 2000, "needBytes": 900, "needItems": 1, "remoteState": "valid"})
	fake.pushAt("FolderCompletion", base.Add(110*time.Second), map[string]any{"folder": "p-alpha", "device": devSol,
		"completion": 57, "globalBytes": 2000, "needBytes": 850, "needItems": 1, "remoteState": "valid"})
	sv = waitFor(t, hub, "the events", func(s *model.Server) bool { return s.Folders["p-alpha"].Completion[devSol].NeedBytes == 850 })
	own, dev = sv.Progress["p-alpha"][""], sv.Progress["p-alpha"][devSol]
	if len(own) != 2 || !sameSample(own[1], model.ProgressSample{At: base.Add(90 * time.Second), NeedBytes: 500, GlobalBytes: 2000}) {
		t.Fatalf("samples of the own copy: %+v", own)
	}
	// The event at 100 s is a sample: it is 100 s after the sample of the
	// full check. The event at 110 s is 20 s after the sample at 90 s: it
	// replaces the sample at 100 s.
	if len(dev) != 3 || !sameSample(dev[1], model.ProgressSample{At: base.Add(90 * time.Second), NeedBytes: 1000, GlobalBytes: 2000}) ||
		!sameSample(dev[2], model.ProgressSample{At: base.Add(110 * time.Second), NeedBytes: 850, GlobalBytes: 2000}) {
		t.Fatalf("samples of SOL: %+v", dev)
	}
	if c := sv.Folders["p-alpha"].Completion[devSol]; c.GlobalBytes != 2000 {
		t.Fatalf("completion from the event: %+v", c)
	}
}

func sameSample(a, b model.ProgressSample) bool {
	return a.At.Equal(b.At) && a.NeedBytes == b.NeedBytes && a.GlobalBytes == b.GlobalBytes
}

// The samples of a folder or device that the server no longer has go at
// the full check.
func TestPruneProgress(t *testing.T) {
	s := &model.Server{}
	at := time.Now()
	for _, k := range [][2]string{{"p-alpha", ""}, {"p-alpha", devSol}, {"p-alpha", devSirius}, {"gone", ""}} {
		s.AddProgress(k[0], k[1], model.ProgressSample{At: at})
	}
	pruneProgress(s, map[string]*model.Folder{"p-alpha": {ID: "p-alpha", SharedWith: []string{devSol}}})
	if len(s.Progress) != 1 || len(s.Progress["p-alpha"]) != 2 || s.Progress["p-alpha"][devSirius] != nil {
		t.Fatalf("progress %+v", s.Progress)
	}
}

// W-021: the first-seen time of each folder and its last local change are
// in the store: a new collector on the same store (a restart of syncwatch)
// has the values of before, also when Syncthing has no events of them now.
func TestFolderActivitySurvivesRestart(t *testing.T) {
	fake := newFakeST(t)
	srv := httptest.NewTLSServer(fake)
	defer srv.Close()
	cfg, err := config.ParseYAML([]byte(fmt.Sprintf("servers: [{name: AKKA, url: %q}]\ncollector: {event_timeout: 5s}\n", srv.URL)))
	if err != nil {
		t.Fatal(err)
	}
	h, _ := config.NewHolder(cfg)
	st, _ := store.OpenMemory(make([]byte, 32))
	_ = st.SetServerKey("akka", "k", srv.URL)
	start := func() (*model.Hub, func()) {
		hub := model.NewHub(nil)
		return hub, runManager(NewManager(hub, st, h, slog.New(slog.NewTextHandler(io.Discard, nil))))
	}
	hub, stop := start()
	sv := waitFor(t, hub, "full check", func(s *model.Server) bool { return s.Loaded })
	first := map[string]time.Time{}
	for id, f := range sv.Folders {
		if f.FirstSeen.IsZero() || time.Since(f.FirstSeen) > time.Minute {
			t.Fatalf("%s: first seen %v", id, f.FirstSeen)
		}
		first[id] = f.FirstSeen
	}
	at := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	fake.pushAt("LocalChangeDetected", at, map[string]any{"folder": "p-alpha", "folderID": "p-alpha", "type": "file", "action": "modified", "path": "b.blend"})
	waitFor(t, hub, "the local change", func(s *model.Server) bool { return s.Folders["p-alpha"].LastLocalChange.Equal(at) })
	stop()

	fake.mu.Lock()
	fake.events = nil // the restarted collector reads no old events
	fake.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	hub, stop = start()
	defer stop()
	sv = waitFor(t, hub, "full check after the restart", func(s *model.Server) bool { return s.Loaded })
	for id, f := range sv.Folders {
		if !f.FirstSeen.Equal(first[id]) {
			t.Errorf("%s: first seen %v after the restart, want %v", id, f.FirstSeen, first[id])
		}
	}
	if got := sv.Folders["p-alpha"].LastLocalChange; !got.Equal(at) {
		t.Fatalf("p-alpha: last local change %v after the restart, want %v", got, at)
	}
	if got := sv.Folders["p-beta"].LastLocalChange; !got.IsZero() {
		t.Fatalf("p-beta: last local change %v, want none", got)
	}
}

// A burst of 1,000 local changes is 1 write; the newest time goes to the
// store at the next full check, and at the stop of the collector.
func TestLocalChangeBurstWritesFewRows(t *testing.T) {
	var mu sync.Mutex
	writes := 0
	old := setLastLocalChange
	setLastLocalChange = func(st *store.Store, server, folder string, at time.Time) error {
		mu.Lock()
		writes++
		mu.Unlock()
		return old(st, server, folder, at)
	}
	defer func() { setLastLocalChange = old }()
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return writes
	}
	fake, hub, _, m, stop := setup(t)
	stopped := false
	defer func() {
		if !stopped {
			stop()
		}
	}()
	waitFor(t, hub, "full check", func(s *model.Server) bool { return s.Loaded })
	t0 := time.Now().Truncate(time.Second)
	fake.mu.Lock()
	for i := 0; i < 1000; i++ {
		fake.nextID++
		fake.events = append(fake.events, stclient.Event{ID: fake.nextID, Type: "LocalChangeDetected", Time: t0.Add(time.Duration(i) * time.Millisecond),
			Data: []byte(fmt.Sprintf(`{"folder":"p-alpha","folderID":"p-alpha","type":"file","action":"modified","path":"f%d.blend"}`, i))})
	}
	fake.mu.Unlock()
	fake.push("StateChanged", map[string]any{"folder": "p-beta", "from": "idle", "to": "scanning"})
	newest := t0.Add(999 * time.Millisecond)
	waitFor(t, hub, "the burst", func(s *model.Server) bool {
		return s.Folders["p-alpha"].LastLocalChange.Equal(newest) && s.Folders["p-beta"].State == "scanning"
	})
	if n := count(); n != 1 {
		t.Fatalf("%d writes for 1,000 local changes, want 1", n)
	}
	stored := func() time.Time {
		t.Helper()
		act, err := m.st.FolderActivity("akka")
		if err != nil {
			t.Fatal(err)
		}
		return act["p-alpha"].LastLocalChange
	}
	if got := stored(); !got.Equal(t0) {
		t.Fatalf("stored %v, want the first change %v", got, t0)
	}

	last := time.Now()
	m.RescanNow()
	waitFor(t, hub, "the next full check", func(s *model.Server) bool { return s.LastFullCheck.After(last) })
	if got := stored(); !got.Equal(newest) || count() != 2 {
		t.Fatalf("after the full check: stored %v (want %v), %d writes", got, newest, count())
	}

	// One more change, within the minute: the stop writes it.
	fake.pushAt("LocalChangeDetected", newest.Add(time.Second), map[string]any{"folder": "p-alpha", "folderID": "p-alpha", "type": "file", "action": "modified", "path": "g.blend"})
	waitFor(t, hub, "the last change", func(s *model.Server) bool { return s.Folders["p-alpha"].LastLocalChange.Equal(newest.Add(time.Second)) })
	if count() != 2 {
		t.Fatalf("%d writes; the change within the minute must wait", count())
	}
	stop()
	stopped = true
	if got := stored(); !got.Equal(newest.Add(time.Second)) || count() != 3 {
		t.Fatalf("after the stop: stored %v, %d writes", got, count())
	}
}
