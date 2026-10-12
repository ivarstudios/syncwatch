package collector

import (
	"context"
	"encoding/json"
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
	"github.com/ivarstudios/syncwatch/internal/engine"
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

// openRecord returns the open finding of a check and subject, or nil.
func openRecord(eng *engine.Engine, check, subject string) *store.Record {
	for _, r := range eng.View().Open {
		if r.Check == check && r.Subject == subject {
			return r
		}
	}
	return nil
}

// A Syncthing that stops reports its folders in error as idle and then
// closes its API. The folder used to lose its error at once: H4
// resolved while the server still counted as up, a "resolved" message
// followed, and a second urgent message when Syncthing started again. Now
// the folder keeps its error, H4 stays open (stale while the server is
// down) and the same record continues after the restart.
func TestFolderErrorKeptWhenSyncthingStops(t *testing.T) {
	shrinkBackoff(t, 100*time.Millisecond)
	fake := newFakeST(t)
	var mu sync.Mutex
	var stopAt int64 // Syncthing stops after the event with this ID
	stopped := false
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		mu.Lock()
		if r.URL.Path == "/rest/events" && stopAt > 0 {
			var since int64
			fmt.Sscan(q.Get("since"), &since)
			stopped = stopped || since >= stopAt
		}
		s := stopped
		mu.Unlock()
		switch {
		case s:
			// What the gate answers while Syncthing does not run.
			http.Error(w, "syncwatch gate: Syncthing unreachable: connection refused", http.StatusBadGateway)
		case r.URL.Path == "/rest/db/status" && q.Get("folder") == "p-beta":
			// The folder lost its marker: before the stop and after the start.
			var all map[string]map[string]any
			_ = json.Unmarshal(fake.fixture("db-status"), &all)
			ds := all["p-beta"]
			ds["state"], ds["error"] = "error", "folder marker missing"
			_ = json.NewEncoder(w).Encode(ds)
		default:
			fake.ServeHTTP(w, r)
		}
	}))
	defer srv.Close()
	hub, m, st, stop := startManager(t, srv, "collector: {event_timeout: 5s, down_after_failures: 2}", nil, nil)
	defer stop()
	eng, err := engine.New(st, hub, m.cfg, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	const subject = "LIVE-BETA-1SOURCE on AKKA"
	start := time.Now()
	waitFor(t, hub, "full check", func(s *model.Server) bool { return s.Loaded && s.Folders["p-beta"].Error != "" })
	waitUntil(t, "the event loop", func() bool { return fake.count("/rest/events") >= 2 })
	eng.Evaluate()
	rec := openRecord(eng, "H4", subject)
	if rec == nil {
		t.Fatal("no H4 for the folder error")
	}
	pinged := time.Now()
	eng.MarkPinged([]string{rec.ID}, pinged) // the urgent message went out

	// The last events before Syncthing's API goes away.
	fake.mu.Lock()
	next := fake.nextID + 2
	fake.mu.Unlock()
	mu.Lock()
	stopAt = next
	mu.Unlock()
	fake.push("StateChanged", map[string]any{"folder": "p-beta", "from": "error", "to": "idle"})
	fake.push("ListenAddressesChanged", map[string]any{})
	sv := waitFor(t, hub, "the first failed request", func(s *model.Server) bool { return s.LastError != "" })
	if sv.Status != model.StatusUp {
		t.Fatalf("status %s after the first failure; the server must still count as up here", sv.Status)
	}
	if f := sv.Folders["p-beta"]; f.Error != "folder marker missing" {
		t.Fatalf("p-beta lost its error at the stop: state %q, error %q", f.State, f.Error)
	}
	eng.Evaluate() // the evaluation that used to resolve H4
	if r := openRecord(eng, "H4", subject); r == nil || r.ID != rec.ID {
		t.Fatal("H4 resolved when Syncthing stopped")
	}
	waitFor(t, hub, "down", func(s *model.Server) bool { return s.Status == model.StatusDown })
	eng.Evaluate()
	if r := openRecord(eng, "H4", subject); r == nil || !r.Stale {
		t.Fatalf("H4 while the server is down: %+v, want open and stale", r)
	}

	// Syncthing starts again; the full check reads the error again.
	fake.mu.Lock()
	fake.startTime = "2026-10-11T09:00:00+02:00"
	fake.events, fake.nextID = nil, 0
	fake.mu.Unlock()
	mu.Lock()
	stopped, stopAt = false, 0
	mu.Unlock()
	waitFor(t, hub, "up again", func(s *model.Server) bool { return s.Status == model.StatusUp })
	eng.Evaluate()
	r := openRecord(eng, "H4", subject)
	if r == nil || r.ID != rec.ID || r.Stale || !r.OpenedAt.Equal(rec.OpenedAt) || r.PingedAt.UnixMilli() != pinged.UnixMilli() {
		t.Fatalf("H4 after the restart: %+v; want the same open record, not stale, urgent message already sent", r)
	}
	if due, _ := st.PendingResolved(); len(due) != 0 {
		t.Fatalf("resolved messages due: %d", len(due))
	}
	all, _ := st.HistoryBetween(start, time.Now().Add(time.Minute))
	var h []store.HistoryEntry
	for _, e := range all {
		if e.FindingID == rec.ID {
			h = append(h, e)
		}
	}
	if len(h) != 1 || !h[0].ResolvedAt.IsZero() {
		t.Fatalf("H4 history %+v; want one open occurrence", h)
	}
}

// A folder that is repaired while Syncthing runs loses its error once a
// long-poll answers clearConfirm after the event: never at once, and about
// one long-poll later than before. An error that comes back in the
// meantime stays.
func TestFolderRepairClearsAfterConfirmation(t *testing.T) {
	old := clearConfirm
	clearConfirm = 2 * time.Second
	t.Cleanup(func() { clearConfirm = old })
	fake, hub, _, _, stop := setup(t)
	defer stop()
	waitFor(t, hub, "full check", func(s *model.Server) bool { return s.Loaded })
	for _, id := range []string{"p-alpha", "p-beta", "p-gamma"} {
		fake.push("StateChanged", map[string]any{"folder": id, "from": "idle", "to": "error", "error": "folder marker missing"})
	}
	waitFor(t, hub, "the folder errors", func(s *model.Server) bool {
		return s.Folders["p-alpha"].Error != "" && s.Folders["p-beta"].Error != "" && s.Folders["p-gamma"].Error != ""
	})
	start := time.Now()
	fake.push("StateChanged", map[string]any{"folder": "p-alpha", "from": "error", "to": "idle"})
	fake.push("FolderSummary", map[string]any{"folder": "p-beta", "summary": map[string]any{"state": "idle", "error": ""}})
	fake.push("StateChanged", map[string]any{"folder": "p-gamma", "from": "error", "to": "idle"})
	fake.push("StateChanged", map[string]any{"folder": "p-gamma", "from": "idle", "to": "error", "error": "folder path missing"})
	sv := waitFor(t, hub, "the repairs", func(s *model.Server) bool {
		return s.Folders["p-alpha"].State == "idle" && s.Folders["p-beta"].State == "idle" && s.Folders["p-gamma"].Error == "folder path missing"
	})
	if took := time.Since(start); took < clearConfirm && (sv.Folders["p-alpha"].Error == "" || sv.Folders["p-beta"].Error == "") {
		t.Fatalf("a folder lost its error %v after the event; want at least %v", took, clearConfirm)
	}
	sv = waitFor(t, hub, "the repairs confirmed", func(s *model.Server) bool {
		return s.Folders["p-alpha"].Error == "" && s.Folders["p-beta"].Error == ""
	})
	if took := time.Since(start); took < clearConfirm {
		t.Fatalf("errors cleared %v after the events; want at least %v", took, clearConfirm)
	}
	if f := sv.Folders["p-gamma"]; f.State != "error" || f.Error != "folder path missing" {
		t.Fatalf("p-gamma: state %q, error %q; want the new error", f.State, f.Error)
	}
}

// When the session ends before Syncthing confirmed a repair, the folder
// keeps its error and the next session starts with a full check, which
// reads the folder again, also when Syncthing did not restart.
func TestUnconfirmedRepairReadAgain(t *testing.T) {
	fake, hub, _, m, stop := setup(t)
	defer stop()
	waitFor(t, hub, "full check", func(s *model.Server) bool { return s.Loaded })
	fake.push("StateChanged", map[string]any{"folder": "p-alpha", "from": "idle", "to": "error", "error": "folder marker missing"})
	waitFor(t, hub, "the folder error", func(s *model.Server) bool { return s.Folders["p-alpha"].Error != "" })
	fake.push("StateChanged", map[string]any{"folder": "p-alpha", "from": "error", "to": "idle"})
	sv := waitFor(t, hub, "the repair event", func(s *model.Server) bool { return s.Folders["p-alpha"].State == "idle" })
	if sv.Folders["p-alpha"].Error == "" {
		t.Fatal("the error was cleared at once")
	}
	full := fake.count("/rest/db/status")
	dropAndReconnect(t, fake, m, nil)
	waitUntil(t, "a full check after the reconnect", func() bool { return fake.count("/rest/db/status") > full })
	waitFor(t, hub, "the folder read again", func(s *model.Server) bool { return s.Folders["p-alpha"].Error == "" })
}

// While folder errors wait for confirmation, the long-poll returns when the
// first of them can be confirmed, in whole seconds and at least 1 second.
func TestClearWait(t *testing.T) {
	c := &Collector{}
	if _, ok := c.clearWait(); ok {
		t.Fatal("a wait with no error to confirm")
	}
	now := time.Now()
	c.clearDue = map[string]time.Time{"a": now.Add(-3500 * time.Millisecond), "b": now}
	if w, ok := c.clearWait(); !ok || w != 7*time.Second {
		t.Fatalf("wait %v, want 7s (6.5 s rounded up)", w)
	}
	c.clearDue = map[string]time.Time{"a": now.Add(-time.Minute)}
	if w, _ := c.clearWait(); w != time.Second {
		t.Fatalf("wait %v for an overdue confirmation, want 1s", w)
	}
}

// A conflict copy that another device made reaches the server as an
// ordinary file. Syncthing counts it only on the device that made
// it, so H10 counts it from RemoteChangeDetected: once per copy. A copy
// that the server made itself comes as LocalChangeDetected and is in the
// server's own counter.
func TestConflictCopiesFromEvents(t *testing.T) {
	fake, hub, _, m, stop := setup(t)
	defer stop()
	waitFor(t, hub, "full check", func(s *model.Server) bool { return s.Loaded })
	ev := func(typ, action, kind, path string) {
		fake.push(typ, map[string]any{"folder": "p-alpha", "folderID": "p-alpha", "type": kind, "action": action, "path": path, "modifiedBy": "OHP4C7G"})
	}
	const copy = "docs/notes.sync-conflict-20261010-205040-OHP4C7G.md"
	ev("RemoteChangeDetected", "modified", "file", copy)
	ev("RemoteChangeDetected", "modified", "file", copy)                                                 // the same copy again
	ev("RemoteChangeDetected", "added", "file", `docs\plan.sync-conflict-20261010-205041-ABCDEF2`)       // a Windows server, no extension
	ev("RemoteChangeDetected", "deleted", "file", "docs/old.sync-conflict-20261010-205042-OHP4C7G.md")   // a copy deleted
	ev("RemoteChangeDetected", "modified", "file", "docs/notes.md")                                      // a normal file
	ev("RemoteChangeDetected", "modified", "dir", "docs/x.sync-conflict-20261010-205043-OHP4C7G")        // a directory
	ev("RemoteChangeDetected", "modified", "file", "docs.sync-conflict-20261010-205044-OHP4C7G/a.md")    // inside such a directory
	ev("LocalChangeDetected", "modified", "file", "docs/notes.sync-conflict-20261010-205045-AKKAAAA.md") // the server's own copy
	fake.push("StateChanged", map[string]any{"folder": "p-alpha", "from": "idle", "to": "scanning"})
	waitFor(t, hub, "the events applied", func(s *model.Server) bool { return s.Folders["p-alpha"].State == "scanning" })
	got, err := m.st.ConflictsSince(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got["akka"]["p-alpha"] != 2 || len(got["akka"]) != 1 {
		t.Fatalf("conflicts %v; want 2 in p-alpha (the two copies, the first one once)", got)
	}
}

// The set of counted copies stays bounded: the oldest key goes first.
func TestCopySetBounded(t *testing.T) {
	var s copySet
	for i := 0; i < maxCopies; i++ {
		if !s.add(fmt.Sprint(i)) {
			t.Fatalf("key %d not new", i)
		}
	}
	if s.add("0") || !s.add("new") || len(s.keys) != maxCopies {
		t.Fatalf("%d keys", len(s.keys))
	}
	if !s.add("0") || s.add("2") {
		t.Fatal("the oldest key was not forgotten first")
	}
}
