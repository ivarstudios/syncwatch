package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/stclient"
	"github.com/ivarstudios/syncwatch/internal/store"
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

// setupWith is setup with extra YAML appended to the configuration.
func setupWith(t *testing.T, extra string) (*fakeST, *model.Hub, func()) {
	t.Helper()
	fake := newFakeST(t)
	srv := httptest.NewTLSServer(fake)
	cfg, err := config.ParseYAML([]byte(rulesYAML + fmt.Sprintf("servers: [{name: AKKA, url: %q}]\ncollector: {event_timeout: 5s}\n", srv.URL) + extra))
	if err != nil {
		t.Fatal(err)
	}
	h, _ := config.NewHolder(cfg)
	st, _ := store.OpenMemory(make([]byte, 32))
	_ = st.SetServerKey("akka", "k", srv.URL)
	hub := model.NewHub(nil)
	m := NewManager(hub, st, h, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	go m.Run(ctx)
	return fake, hub, func() { cancel(); srv.Close() }
}

func dirEvent(folder, path string) map[string]any {
	return map[string]any{"folder": folder, "folderID": folder, "type": "dir", "action": "modified", "path": path}
}

// The event path uses the scan's definition of a nested project folder:
// no ignored directory on the way and at most nested_scan_depth levels below
// the top-level project folder.
func TestEventNestedUsesScanRules(t *testing.T) {
	fake, hub, _, _, stop := setup(t)
	defer stop()
	waitFor(t, hub, "full check", func(s *model.Server) bool { return s.Loaded && s.Structure != nil })
	fake.push("LocalChangeDetected", dirEvent("p-alpha", `.stversions\LIVE-IGNORED-2PROJECTFILES`))
	fake.push("LocalChangeDetected", dirEvent("p-alpha", `a\b\c\LIVE-DEEP-2PROJECTFILES`))
	fake.push("LocalChangeDetected", dirEvent("p-alpha", `a\b\LIVE-OK-2PROJECTFILES`))
	sv := waitFor(t, hub, "nested folder from events", func(s *model.Server) bool { return len(s.Structure.Nested) > 0 })
	for _, n := range sv.Structure.Nested {
		if !strings.HasSuffix(n.Path, `LIVE-ALPHA-2PROJECTFILES\a\b\LIVE-OK-2PROJECTFILES`) || !strings.HasSuffix(n.Parent, `LIVE-ALPHA-2PROJECTFILES`) {
			t.Errorf("nested entry %+v", n)
		}
	}
	if len(sv.Structure.Nested) != 1 {
		t.Fatalf("%d nested entries, want only the one within depth and not behind an ignored directory", len(sv.Structure.Nested))
	}
}

// A matching directory below a top-level directory that isn't a project
// folder isn't nested "inside a project folder", as the scan never reports it.
func TestEventNestedNeedsProjectTop(t *testing.T) {
	// The 2PROJECTFILES share is replaced by its parent, so p-alpha's top-level
	// directory in the share is AKKA-LIVE-2PROJECTFILES, not a project folder.
	fake, hub, stop := setupWith(t, `shares:
  add: {akka: ['D:\tmp\swsim\st\akka\share']}
  exclude: {akka: ['D:\tmp\swsim\st\akka\share\AKKA-LIVE-2PROJECTFILES']}
`)
	defer stop()
	waitFor(t, hub, "full check", func(s *model.Server) bool { return s.Loaded && s.Structure != nil })
	fake.push("LocalChangeDetected", dirEvent("p-alpha", `renders\LIVE-NESTED-2PROJECTFILES`))
	fake.push("LocalChangeDetected", dirEvent("p-beta", `x\LIVE-OK-1SOURCE`))
	sv := waitFor(t, hub, "nested folder from events", func(s *model.Server) bool { return len(s.Structure.Nested) > 0 })
	for _, n := range sv.Structure.Nested {
		if !strings.HasSuffix(n.Path, `LIVE-BETA-1SOURCE\x\LIVE-OK-1SOURCE`) {
			t.Errorf("nested entry %+v", n)
		}
	}
	if len(sv.Structure.Nested) != 1 {
		t.Fatalf("%d nested entries, want 1", len(sv.Structure.Nested))
	}
}
