package collector

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/stclient"
)

// pushAt is push with an explicit event time.
func (f *fakeST) pushAt(typ string, at time.Time, data any) {
	b, _ := json.Marshal(data)
	f.mu.Lock()
	f.nextID++
	f.events = append(f.events, stclient.Event{ID: f.nextID, Type: typ, Time: at, Data: b})
	f.mu.Unlock()
	select {
	case f.notify <- struct{}{}:
	default:
	}
}

// Local edits never move lastFile.at (Syncthing sets it for pulled files
// only), so the collector keeps the newest LocalChangeDetected time per
// folder for C1. It must survive a full re-check and not go backwards.
func TestLocalChangeTimeTracked(t *testing.T) {
	fake, hub, _, mgr, stop := setup(t)
	defer stop()
	waitFor(t, hub, "full check", func(s *model.Server) bool { return s.Loaded && s.Structure != nil })

	at := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	fake.pushAt("RemoteChangeDetected", at.Add(time.Hour), map[string]any{"folder": "p-alpha", "folderID": "p-alpha", "type": "file", "action": "modified", "path": `a.blend`})
	fake.pushAt("LocalChangeDetected", at, map[string]any{"folder": "p-alpha", "folderID": "p-alpha", "type": "file", "action": "modified", "path": `b.blend`})
	sv := waitFor(t, hub, "local change recorded", func(s *model.Server) bool { return !s.Folders["p-alpha"].LastLocalChange.IsZero() })
	if got := sv.Folders["p-alpha"].LastLocalChange; !got.Equal(at) {
		t.Fatalf("LastLocalChange %v, want %v (remote changes must not count)", got, at)
	}
	if !sv.Folders["p-beta"].LastLocalChange.IsZero() {
		t.Fatal("another folder got the change time")
	}

	// An older event (events can be re-read after a reconnect) keeps the newest time.
	fake.pushAt("LocalChangeDetected", at.Add(-time.Hour), map[string]any{"folder": "p-alpha", "type": "file", "action": "deleted", "path": `old.blend`})
	fake.push("StateChanged", map[string]any{"folder": "p-alpha", "from": "scanning", "to": "idle"})
	sv = waitFor(t, hub, "later event applied", func(s *model.Server) bool {
		return s.Folders["p-alpha"].State == "idle" && s.Folders["p-alpha"].StateChanged.After(at)
	})
	if got := sv.Folders["p-alpha"].LastLocalChange; !got.Equal(at) {
		t.Fatalf("LastLocalChange went backwards to %v", got)
	}

	// A full re-check rebuilds the folders from the config; the time only
	// exists in events, so it must be carried over.
	var lastFull time.Time
	hub.View("akka", func(s *model.Server) { lastFull = s.LastFullCheck })
	mgr.RescanNow()
	sv = waitFor(t, hub, "full re-check", func(s *model.Server) bool { return s.LastFullCheck.After(lastFull) })
	if got := sv.Folders["p-alpha"].LastLocalChange; !got.Equal(at) {
		t.Fatalf("LastLocalChange lost by the full re-check: %v", got)
	}
}
