package model

import "testing"

// FolderSync is the one place that turns Syncthing's folder fields into a
// state; every view and API counts folders by it.
func TestFolderSync(t *testing.T) {
	for name, c := range map[string]struct {
		f    *Folder
		want string
	}{
		"idle":                         {&Folder{State: "idle"}, SyncUpToDate},
		"idle with items due":          {&Folder{State: "idle", NeedItems: 2}, SyncSyncing},
		"syncing":                      {&Folder{State: "syncing"}, SyncSyncing},
		"scanning":                     {&Folder{State: "scanning"}, SyncScanning},
		"scan-waiting":                 {&Folder{State: "scan-waiting"}, SyncScanning},
		"starting":                     {&Folder{State: "starting"}, SyncScanning},
		"error state":                  {&Folder{State: "error"}, SyncError},
		"folder error text":            {&Folder{State: "idle", Error: "folder marker missing"}, SyncError},
		"failing files":                {&Folder{State: "idle", PullErrors: 1}, SyncError},
		"watch error":                  {&Folder{State: "idle", WatchError: "inotify limit"}, SyncWatchError},
		"watch error while syncing":    {&Folder{State: "syncing", NeedItems: 3, WatchError: "inotify limit"}, SyncWatchError},
		"watch error and folder error": {&Folder{State: "error", WatchError: "inotify limit"}, SyncError},
		"paused":                       {&Folder{Paused: true, State: "error", WatchError: "x"}, SyncPaused},
	} {
		if got := c.f.FolderSync(); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

// A watcher error is a warning of its own in the server's overall status:
// below a folder error, above the activity states.
func TestSyncStatusWatchError(t *testing.T) {
	up := func(folders ...*Folder) *Server {
		s := &Server{Status: StatusUp, Loaded: true, Folders: map[string]*Folder{}}
		for i, f := range folders {
			s.Folders[string(rune('a'+i))] = f
		}
		return s
	}
	for name, c := range map[string]struct {
		s    *Server
		want string
	}{
		"watch error only":          {up(&Folder{State: "idle"}, &Folder{State: "idle", WatchError: "x"}), SyncWatchError},
		"watch error beats syncing": {up(&Folder{State: "syncing"}, &Folder{State: "idle", WatchError: "x"}), SyncWatchError},
		"folder error beats it":     {up(&Folder{State: "error"}, &Folder{State: "idle", WatchError: "x"}), SyncError},
		"paused one ignored":        {up(&Folder{Paused: true, WatchError: "x"}, &Folder{State: "idle"}), SyncUpToDate},
	} {
		if got := c.s.SyncStatus(); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}
