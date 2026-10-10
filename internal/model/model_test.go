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
