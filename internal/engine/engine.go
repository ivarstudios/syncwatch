// Package engine runs the rules on every snapshot change and manages the
// lifecycle of findings: observation timers, opening, freezing while a server
// is unreachable, resolving, history and snoozes.
package engine

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"reflect"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/rules"
	"github.com/ivarstudios/syncwatch/internal/store"
)

// Engine evaluates rules and owns the set of open findings.
type Engine struct {
	st  *store.Store
	hub *model.Hub
	cfg *config.Holder
	now func() time.Time
	log *slog.Logger

	mu          sync.Mutex
	open        map[string]*store.Record
	obs         map[string]store.Observation
	snoozes     map[string]store.Snooze
	persisted   map[string]time.Time // last LastSeen written per open record
	snap        *model.Snapshot
	version     uint64
	evaluated   time.Time
	fingerprint uint64
	changedAt   time.Time

	subMu sync.Mutex
	subs  map[chan uint64]struct{}

	trigger chan struct{}
	onEval  []func()
}

// New creates an engine and loads its state from the store.
func New(st *store.Store, hub *model.Hub, cfg *config.Holder, now func() time.Time, log *slog.Logger) (*Engine, error) {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	e := &Engine{
		st: st, hub: hub, cfg: cfg, now: now, log: log,
		open: map[string]*store.Record{}, persisted: map[string]time.Time{},
		subs: map[chan uint64]struct{}{}, trigger: make(chan struct{}, 1),
		snap: &model.Snapshot{},
	}
	recs, err := st.OpenRecords()
	if err != nil {
		return nil, err
	}
	for _, r := range recs {
		e.open[r.ID] = r
		e.persisted[r.ID] = r.LastSeen
	}
	if e.obs, err = st.Observations(); err != nil {
		return nil, err
	}
	if e.snoozes, err = st.Snoozes(); err != nil {
		return nil, err
	}
	return e, nil
}

// OnEvaluate registers fn to run after every evaluation (the notifier uses it).
func (e *Engine) OnEvaluate(fn func()) { e.onEval = append(e.onEval, fn) }

// Trigger asks for an evaluation soon, e.g. after a configuration change.
func (e *Engine) Trigger() {
	select {
	case e.trigger <- struct{}{}:
	default:
	}
}

// Run evaluates on every hub change (coalesced), on Trigger, and every 30 s
// so time-based thresholds fire without new data.
func (e *Engine) Run(ctx context.Context) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	prune := time.NewTicker(6 * time.Hour)
	defer prune.Stop()
	e.Prune()
	e.Evaluate()
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.hub.Changed():
			// Coalesce bursts of events into one evaluation.
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		case <-e.trigger:
		case <-tick.C:
		case <-prune.C:
			e.Prune()
			continue
		}
		e.Evaluate()
	}
}

// Prune removes old history, expired snoozes and sessions.
func (e *Engine) Prune() {
	now := e.now()
	for _, err := range []error{
		e.st.PruneResolved(now.Add(-90 * 24 * time.Hour)),
		e.st.PruneHistory(now.Add(-400 * 24 * time.Hour)),
		e.st.PruneConflicts(now.Add(-60 * 24 * time.Hour)),
		e.st.PruneTraffic(now.Add(-8 * 24 * time.Hour)),
		e.st.PruneSnoozes(now),
		e.st.PruneSessions(now),
	} {
		if err != nil {
			e.log.Warn("pruning the database", "err", err)
		}
	}
	if z, err := e.st.Snoozes(); err == nil {
		e.mu.Lock()
		e.snoozes = z
		e.mu.Unlock()
	}
}

func staleServers(snap *model.Snapshot) map[string]bool {
	out := map[string]bool{}
	for _, sv := range snap.Servers {
		if sv.Status != model.StatusUp || !sv.Loaded {
			out[sv.ID] = true
		}
	}
	return out
}

func frozen(stale map[string]bool, check, server string, related []string) bool {
	if check == "H1" || check == "H3" {
		return false
	}
	if server != "" && stale[server] {
		return true
	}
	for _, r := range related {
		if stale[r] {
			return true
		}
	}
	return false
}

func findingServers(f model.Finding) []string {
	var out []string
	if f.Server != "" {
		out = append(out, f.Server)
	}
	return append(out, f.Related...)
}

// Evaluate runs the rules once and updates the findings.
func (e *Engine) Evaluate() {
	cfg := e.cfg.Get()
	snap := e.hub.Snapshot()
	now := e.now()
	snap.At = now
	conflicts, err := e.st.ConflictsSince(now.Add(-cfg.Thresholds.ConflictWindow.D()))
	if err != nil {
		e.log.Warn("reading conflict history", "err", err)
	}
	fs := rules.Evaluate(rules.Input{Snapshot: snap, Config: cfg, Structure: e.cfg.Structure(), Now: now, Conflicts: conflicts})
	stale := staleServers(snap)

	e.mu.Lock()
	produced := map[string]bool{}
	current := map[string]model.Finding{}
	obsAdd := map[string]store.Observation{}
	var obsRemove []string
	for _, f := range fs {
		produced[f.ID] = true
		if frozen(stale, f.Check, f.Server, f.Related) {
			continue
		}
		o, ok := e.obs[f.ID]
		if !ok {
			o = store.Observation{Since: now, Servers: findingServers(f)}
			if !f.Since.IsZero() && f.Since.Before(now) {
				o.Since = f.Since
			}
			e.obs[f.ID] = o
			obsAdd[f.ID] = o
		}
		if now.Sub(o.Since) < f.MinDuration {
			continue
		}
		current[f.ID] = f
	}
	for id, o := range e.obs {
		if produced[id] {
			continue
		}
		if frozen(stale, "", "", o.Servers) {
			continue
		}
		delete(e.obs, id)
		obsRemove = append(obsRemove, id)
	}

	changed := false
	for id, f := range current {
		rec := e.open[id]
		if rec == nil {
			rec = &store.Record{Finding: f, Since: e.obs[id].Since, OpenedAt: now, LastSeen: now}
			if old, err := e.st.Record(id); err == nil && old != nil && !old.Open() && !old.PingedAt.IsZero() && old.ResolvedNotifiedAt.IsZero() {
				// It flapped back before its "resolved" message went out:
				// carry on as the same, already-announced problem.
				rec.PingedAt = old.PingedAt
			}
			e.open[id] = rec
			e.save(rec)
			if err := e.st.HistoryOpen(rec); err != nil {
				e.log.Warn("writing history", "err", err)
			}
			e.persisted[id] = now
			changed = true
			e.log.Info("finding opened", "check", f.Check, "severity", f.Severity.String(), "subject", f.Subject, "id", id)
			continue
		}
		upd := sameContent(rec.Finding, f)
		rec.LastSeen = now
		if !upd || rec.Stale {
			rec.Finding = f
			rec.Stale = false
			e.save(rec)
			e.persisted[id] = now
			changed = true
		} else if now.Sub(e.persisted[id]) > 10*time.Minute {
			e.save(rec)
			e.persisted[id] = now
		}
	}
	for id, rec := range e.open {
		if _, ok := current[id]; ok {
			continue
		}
		if frozen(stale, rec.Check, rec.Server, rec.Related) {
			if !rec.Stale {
				rec.Stale = true
				e.save(rec)
				changed = true
			}
			continue
		}
		rec.ResolvedAt = now
		rec.LastSeen = now
		e.save(rec)
		if err := e.st.HistoryResolve(id, now); err != nil {
			e.log.Warn("writing history", "err", err)
		}
		delete(e.open, id)
		delete(e.persisted, id)
		changed = true
		e.log.Info("finding resolved", "check", rec.Check, "subject", rec.Subject, "id", id)
	}
	e.snap = snap
	e.evaluated = now
	// Only tell dashboards to refresh when something they show changed.
	fp := visibleFingerprint(snap)
	publish := changed || fp != e.fingerprint
	e.fingerprint = fp
	var ver uint64
	if publish {
		e.version++
		ver = e.version
		e.changedAt = now
	}
	e.mu.Unlock()

	if err := e.st.ApplyObservations(obsAdd, obsRemove); err != nil {
		e.log.Warn("writing observations", "err", err)
	}
	if publish {
		e.publish(ver)
	}
	for _, fn := range e.onEval {
		fn()
	}
}

func (e *Engine) save(r *store.Record) {
	if err := e.st.SaveRecord(r); err != nil {
		e.log.Error("saving finding", "id", r.ID, "err", err)
	}
}

// sameContent compares the parts of a finding that matter to viewers.
func sameContent(a, b model.Finding) bool {
	return a.Check == b.Check && a.Severity == b.Severity && a.Subject == b.Subject && a.Message == b.Message &&
		a.Urgent == b.Urgent && a.Link == b.Link && slices.Equal(a.Checks, b.Checks) && slices.Equal(a.Paths, b.Paths) &&
		slices.Equal(a.Related, b.Related) && reflect.DeepEqual(a.Details, b.Details)
}

// visibleFingerprint hashes what the dashboard shows of the snapshot (server
// status, folder states, completion, connections), ignoring timestamps that
// change on every event such as "last contact".
func visibleFingerprint(snap *model.Snapshot) uint64 {
	h := fnv.New64a()
	w := func(parts ...any) {
		fmt.Fprintln(h, parts...)
	}
	for _, sv := range snap.Servers {
		w(sv.ID, sv.Status, sv.Version, sv.LastError, sv.Loaded, sv.Mode, sv.Cert.NotAfter.Unix())
		for _, id := range sortedKeys(sv.Folders) {
			f := sv.Folders[id]
			w(id, f.Label, f.State, f.Paused, f.Error, f.WatchError, f.Errors, f.PullErrors, f.NeedItems, int(f.LocalPercent()))
			for _, dev := range sortedKeys(f.Completion) {
				c := f.Completion[dev]
				w(dev, int(c.Percent), c.RemoteState)
			}
		}
		for _, id := range sortedKeys(sv.Connections) {
			w(id, sv.Connections[id].Connected)
		}
		for _, id := range sortedKeys(sv.Devices) {
			w(id, sv.Devices[id].Name, sv.Devices[id].Paused)
		}
		if sv.Structure != nil {
			w(sv.Structure.ScannedAt.Unix(), len(sv.Structure.Shares), len(sv.Structure.Nested))
		}
	}
	return h.Sum64()
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// View is a consistent copy of the engine's state.
type View struct {
	Version   uint64
	Evaluated time.Time
	Changed   time.Time // last time anything visible changed
	Snapshot  *model.Snapshot
	Open      []*store.Record // sorted most severe first
	Snoozes   map[string]store.Snooze
}

// Snoozed returns the active snooze for a finding, if any.
func (v *View) Snoozed(id string, at time.Time) (store.Snooze, bool) {
	z, ok := v.Snoozes[id]
	if ok && z.Active(at) {
		return z, true
	}
	return store.Snooze{}, false
}

// View returns copies of the open findings, snoozes and the last snapshot.
func (e *Engine) View() *View {
	e.mu.Lock()
	defer e.mu.Unlock()
	v := &View{Version: e.version, Evaluated: e.evaluated, Changed: e.changedAt, Snapshot: e.snap, Snoozes: make(map[string]store.Snooze, len(e.snoozes))}
	for k, z := range e.snoozes {
		v.Snoozes[k] = z
	}
	for _, r := range e.open {
		rc := *r
		v.Open = append(v.Open, &rc)
	}
	SortRecords(v.Open)
	return v
}

// SortRecords orders by severity, then urgency, then age (oldest first).
func SortRecords(rs []*store.Record) {
	sort.SliceStable(rs, func(i, j int) bool {
		a, b := rs[i], rs[j]
		if a.Severity != b.Severity {
			return a.Severity > b.Severity
		}
		if a.Urgent != b.Urgent {
			return a.Urgent
		}
		if !a.OpenedAt.Equal(b.OpenedAt) {
			return a.OpenedAt.Before(b.OpenedAt)
		}
		return a.Subject < b.Subject
	})
}

// MarkPinged records that urgent messages went out for findings.
func (e *Engine) MarkPinged(ids []string, at time.Time) { e.mark(ids, at, false) }

// MarkResolvedNotified records that "resolved" messages went out.
func (e *Engine) MarkResolvedNotified(ids []string, at time.Time) { e.mark(ids, at, true) }

func (e *Engine) mark(ids []string, at time.Time, resolved bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, id := range ids {
		r := e.open[id]
		if r == nil {
			var err error
			if r, err = e.st.Record(id); err != nil || r == nil {
				continue
			}
		}
		if resolved {
			r.ResolvedNotifiedAt = at
		} else {
			r.PingedAt = at
		}
		e.save(r)
	}
}

// Snooze hides a finding until a time.
func (e *Engine) Snooze(id string, until time.Time, reason, by string) error {
	return e.setSnooze(store.Snooze{FindingID: id, Kind: store.SnoozeKind, Until: until, Reason: reason, CreatedBy: by})
}

// Ignore hides a finding for good, with a reason.
func (e *Engine) Ignore(id, reason, by string) error {
	return e.setSnooze(store.Snooze{FindingID: id, Kind: store.IgnoreKind, Reason: reason, CreatedBy: by})
}

func (e *Engine) setSnooze(z store.Snooze) error {
	z.CreatedAt = e.now()
	e.mu.Lock()
	if r := e.open[z.FindingID]; r != nil {
		z.Subject = r.Check + " " + r.Subject
	}
	e.mu.Unlock()
	if err := e.st.SetSnooze(z); err != nil {
		return err
	}
	e.mu.Lock()
	e.snoozes[z.FindingID] = z
	e.version++
	ver := e.version
	e.mu.Unlock()
	e.publish(ver)
	return nil
}

// Unsnooze removes a snooze or ignore.
func (e *Engine) Unsnooze(id string) error {
	if err := e.st.ClearSnooze(id); err != nil {
		return err
	}
	e.mu.Lock()
	delete(e.snoozes, id)
	e.version++
	ver := e.version
	e.mu.Unlock()
	e.publish(ver)
	return nil
}

// Subscribe returns a channel that receives the version after every change.
func (e *Engine) Subscribe() (<-chan uint64, func()) {
	ch := make(chan uint64, 1)
	e.subMu.Lock()
	e.subs[ch] = struct{}{}
	e.subMu.Unlock()
	return ch, func() {
		e.subMu.Lock()
		delete(e.subs, ch)
		e.subMu.Unlock()
	}
}

func (e *Engine) publish(ver uint64) {
	e.subMu.Lock()
	defer e.subMu.Unlock()
	for ch := range e.subs {
		select {
		case ch <- ver:
		default:
			// Drop the stale value and replace it with the newest.
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- ver:
			default:
			}
		}
	}
}
