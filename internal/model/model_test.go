package model

import (
	"testing"
	"time"
)

// A server seeded with the device ID it had last time keeps it when the
// collector manager sets the server list, and that ID is a server for
// Devices() although the server hasn't been reached yet.
func TestHubKeepsSeededDeviceID(t *testing.T) {
	h := NewHub(nil)
	h.SetServers([]*Server{{ID: "sol", Name: "SOL", MyID: "SOL-ID"}})
	h.SetServers([]*Server{{ID: "akka", Name: "AKKA"}, {ID: "sol", Name: "SOL (renamed)"}})
	sol := h.Snapshot().Server("sol")
	if sol == nil || sol.MyID != "SOL-ID" || sol.Name != "SOL (renamed)" || sol.Status != StatusUnknown || sol.Loaded {
		t.Fatalf("seeded server: %+v", sol)
	}
	h.Update("akka", func(s *Server) {
		s.Loaded, s.Status, s.MyID = true, StatusUp, "AKKA-ID"
		s.Devices["SOL-ID"] = &Device{ID: "SOL-ID", Name: "SOL-NAS", LastSeen: time.Now().Add(-8 * 24 * time.Hour)}
	})
	var found *DeviceView
	for _, d := range h.Snapshot().Devices() {
		if d.ID == "SOL-ID" {
			found = d
		}
	}
	if found == nil || !found.IsServer || found.ServerID != "sol" || found.Name != "SOL (renamed)" || found.Connected {
		t.Fatalf("SOL-ID: %+v", found)
	}
}

// Syncthing reports remoteState "unknown" for a disconnected device; the hub
// keeps the last known state per folder and device until it is unshared.
func TestRemoteStateRemembered(t *testing.T) {
	h := NewHub(nil)
	h.SetServers([]*Server{{ID: "a"}})
	shared := func(state string) *Folder {
		return &Folder{ID: "f", SharedWith: []string{"PC"}, Completion: map[string]*Completion{"PC": {RemoteState: state}}}
	}
	h.Update("a", func(s *Server) { s.Folders["f"] = shared("paused") })
	// A full check replaces the folders while the PC is disconnected.
	h.Update("a", func(s *Server) { s.Folders = map[string]*Folder{"f": shared("unknown")} })
	snap := h.Snapshot()
	if got := snap.Server("a").RemoteState("f", "PC"); got != "paused" {
		t.Fatalf("after disconnect: %q", got)
	}
	if got := snap.Server("a").RemoteState("f", "OTHER"); got != "" {
		t.Fatalf("unknown device: %q", got)
	}
	// The snapshot is a copy.
	snap.Server("a").RemoteStates["f"]["PC"] = "valid"
	if got := h.Snapshot().Server("a").RemoteState("f", "PC"); got != "paused" {
		t.Fatalf("snapshot shares state with the hub: %q", got)
	}
	// Reported again while connected: the live value wins and is remembered.
	h.Update("a", func(s *Server) { s.Folders["f"].Completion["PC"].RemoteState = "notSharing" })
	h.Update("a", func(s *Server) { s.Folders["f"].Completion["PC"].RemoteState = "" })
	if got := h.Snapshot().Server("a").RemoteState("f", "PC"); got != "notSharing" {
		t.Fatalf("after a new report: %q", got)
	}
	// Unshared: forgotten.
	h.Update("a", func(s *Server) { s.Folders["f"].SharedWith = nil })
	if got := h.Snapshot().Server("a").RemoteState("f", "PC"); got != "" {
		t.Fatalf("after unsharing: %q", got)
	}
	h.Update("a", func(s *Server) { s.Folders["f"] = shared("valid") })
	h.Update("a", func(s *Server) { delete(s.Folders, "f") })
	if n := len(h.Snapshot().Server("a").RemoteStates); n != 0 {
		t.Fatalf("removed folder still remembered: %d", n)
	}
}

func TestSyncStatus(t *testing.T) {
	up := func(folders ...*Folder) *Server {
		s := &Server{Status: StatusUp, Loaded: true, Folders: map[string]*Folder{}}
		for i, f := range folders {
			s.Folders[string(rune('a'+i))] = f
		}
		return s
	}
	idle := func() *Folder { return &Folder{State: "idle"} }
	for name, c := range map[string]struct {
		s    *Server
		want string
	}{
		"all idle":            {up(idle(), idle()), SyncUpToDate},
		"no folders":          {up(), SyncUpToDate},
		"one scanning":        {up(idle(), &Folder{State: "scanning"}), SyncScanning},
		"one syncing":         {up(idle(), &Folder{State: "scanning"}, &Folder{State: "syncing"}), SyncSyncing},
		"idle with items due": {up(&Folder{State: "idle", NeedItems: 3}), SyncSyncing},
		"failing files":       {up(&Folder{State: "syncing"}, &Folder{State: "idle", PullErrors: 1}), SyncError},
		"folder error":        {up(&Folder{State: "error", Error: "folder marker missing"}), SyncError},
		"paused and idle":     {up(&Folder{Paused: true}, idle()), SyncUpToDate},
		"all paused":          {up(&Folder{Paused: true}, &Folder{Paused: true, State: "error"}), SyncPaused},
		"down":                {&Server{Status: StatusDown, Loaded: true}, SyncUnknown},
		"not read yet":        {&Server{Status: StatusUp}, SyncUnknown},
	} {
		if got := c.s.SyncStatus(); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

// The hub keeps the completion that a device had in its last connection
// with a valid state, for H5 while the device is away.
func TestLastCompletionRemembered(t *testing.T) {
	h := NewHub(nil)
	h.SetServers([]*Server{{ID: "a"}})
	report := func(connected bool, state string, percent float64) {
		h.Update("a", func(s *Server) {
			s.Folders = map[string]*Folder{"f": {ID: "f", SharedWith: []string{"PC"}, Completion: map[string]*Completion{"PC": {Percent: percent, NeedItems: 3, RemoteState: state}}}}
			s.Connections["PC"] = &Connection{Connected: connected}
		})
	}
	last := func() (Completion, bool) { return h.Snapshot().Server("a").LastCompletion("f", "PC") }
	report(true, "unknown", 0) // just connected, the state is not known yet
	if _, ok := last(); ok {
		t.Fatal("a completion without a valid state is not remembered")
	}
	report(true, "valid", 99.5)
	// Disconnected: Syncthing reports "unknown" and another percentage.
	report(false, "unknown", 40)
	if c, ok := last(); !ok || c.Percent != 99.5 || c.NeedItems != 3 {
		t.Fatalf("after the disconnect: %+v %v", c, ok)
	}
	// A valid state from a server that has lost the connection is old data.
	report(false, "valid", 10)
	if c, _ := last(); c.Percent != 99.5 {
		t.Fatalf("remembered without a connection: %+v", c)
	}
	snap := h.Snapshot()
	snap.Server("a").LastCompletions["f"]["PC"] = Completion{Percent: 1}
	if c, _ := last(); c.Percent != 99.5 {
		t.Fatalf("the snapshot shares state with the hub: %+v", c)
	}
	report(true, "valid", 100)
	if c, _ := last(); c.Percent != 100 {
		t.Fatalf("a new report while connected: %+v", c)
	}
	h.Update("a", func(s *Server) { s.Folders["f"].SharedWith = nil })
	if _, ok := last(); ok {
		t.Fatal("an unshared folder is still remembered")
	}
}
