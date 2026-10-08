package model

import "testing"

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
