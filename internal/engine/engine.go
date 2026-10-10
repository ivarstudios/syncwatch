// Package engine runs the rules on every snapshot change and manages the
// lifecycle of findings: observation timers, opening, freezing while a server
// is unreachable, resolving, history and snoozes.
package engine

import (
	"context"
	"errors"
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
	"github.com/ivarstudios/syncwatch/internal/pathx"
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

// resolvedMemory is how long a resolved finding is kept: it can still be
// snoozed or ignored (a re-open stays hidden), and an ignore outlives it
// by this much.
const resolvedMemory = 90 * 24 * time.Hour

// errUnknownFinding is returned for snoozes of findings that don't exist.
var errUnknownFinding = errors.New("No open or recent problem has this ID.")

// Prune removes old history, expired snoozes and sessions.
func (e *Engine) Prune() {
	now := e.now()
	for _, err := range []error{
		e.st.PruneResolved(now.Add(-resolvedMemory)),
		e.st.PruneHistory(now.Add(-400 * 24 * time.Hour)),
		e.st.PruneConflicts(now.Add(-60 * 24 * time.Hour)),
		e.st.PruneTraffic(now.Add(-8 * 24 * time.Hour)),
		e.st.PruneSnoozes(now),
		e.st.PruneIgnores(now.Add(-resolvedMemory)),
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

// freezer decides which findings are frozen: kept as they are, neither
// opening nor resolving, because the data they would be judged on can't be
// trusted right now.
type freezer struct {
	present   map[string]bool // in the snapshot at all (configured and enabled)
	stale     map[string]bool // not up, or not loaded since the start
	unknown   map[string]bool // not contacted since the start
	unscanned map[string]bool // up, but no structure scan has finished since the start
	failed    map[string]failedShares
}

// failedShares are the shares of one server whose last listing failed.
type failedShares struct {
	st    pathx.Style
	paths []string
}

func newFreezer(snap *model.Snapshot) *freezer {
	z := &freezer{present: map[string]bool{}, stale: map[string]bool{}, unknown: map[string]bool{}, unscanned: map[string]bool{}, failed: map[string]failedShares{}}
	for _, sv := range snap.Servers {
		z.present[sv.ID] = true
		if sv.Status == model.StatusUnknown {
			z.unknown[sv.ID] = true
		}
		if sv.Status != model.StatusUp || !sv.Loaded {
			z.stale[sv.ID] = true
			continue
		}
		if sv.Structure == nil || sv.Structure.ScannedAt.IsZero() {
			z.unscanned[sv.ID] = true
			continue
		}
		var paths []string
		for _, sh := range sv.Structure.Shares {
			if sh.Error != "" {
				paths = append(paths, sh.Path)
			}
		}
		if len(paths) > 0 {
			z.failed[sv.ID] = failedShares{st: pathx.StyleFor(sv.PathSep), paths: paths}
		}
	}
	return z
}

// isStale reports whether a server can't vouch for anything right now. A
// server missing from the snapshot can't either.
func (z *freezer) isStale(id string) bool { return z.stale[id] || !z.present[id] }

// allStale reports whether every one of the servers is stale (none for an
// empty list).
func (z *freezer) allStale(ids []string) bool {
	if len(ids) == 0 {
		return false
	}
	for _, id := range ids {
		if !z.isStale(id) {
			return false
		}
	}
	return true
}

func (z *freezer) anyStale(ids []string) bool {
	for _, id := range ids {
		if z.isStale(id) {
			return true
		}
	}
	return false
}

// gone reports whether the servers a finding depends on have left the
// configuration (removed or disabled): it can't be judged again, and there
// is nobody to report it resolved either.
func (z *freezer) gone(f model.Finding) bool {
	if f.Server != "" && !z.present[f.Server] {
		return true
	}
	return len(f.Related) > 0 && !z.anyPresent(f.Related)
}

// keepObservation reports whether a condition that is no longer produced
// keeps its timer: while the servers it depends on can't be judged (the
// open record's rule, else all of its servers stale), unless they have
// left the configuration.
func (z *freezer) keepObservation(rec *store.Record, o store.Observation) bool {
	if rec != nil {
		return !z.gone(rec.Finding) && z.frozen(rec.Finding)
	}
	return z.anyPresent(o.Servers) && z.allStale(o.Servers)
}

func (z *freezer) anyPresent(ids []string) bool {
	for _, id := range ids {
		if z.present[id] {
			return true
		}
	}
	return false
}

// structureCheck reports whether a check needs the structure scan (S3 and
// the per-directory checks S4–S7).
func structureCheck(check string) bool {
	switch check {
	case "S3", "S4", "S5", "S6", "S7":
		return true
	}
	return false
}

// frozen reports whether a finding must keep its current state.
func (z *freezer) frozen(f model.Finding) bool {
	if f.Check == "H1" || f.Check == "H3" {
		// Reachability findings follow the server's status; only "not
		// contacted yet" (after a start) says nothing about it.
		return z.unknown[f.Server]
	}
	if f.Server == "" {
		// A cross-server finding is judged on whichever related servers
		// are reachable; only when none is can nothing be said about it.
		return z.allStale(f.Related)
	}
	if z.isStale(f.Server) || z.anyStale(f.Related) {
		return true
	}
	if structureCheck(f.Check) {
		if z.unscanned[f.Server] {
			return true
		}
		if fs, ok := z.failed[f.Server]; ok {
			p := f.DetailValue("Path")
			for _, sp := range fs.paths {
				if fs.st.Within(p, sp) {
					return true
				}
			}
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
	if !e.hub.Ready() {
		// The collector manager hasn't published the server set yet; an
		// empty snapshot would resolve every open finding.
		return
	}
	cfg := e.cfg.Get()
	snap := e.hub.Snapshot()
	now := e.now()
	snap.At = now
	conflicts, err := e.st.ConflictsSince(now.Add(-cfg.Thresholds.ConflictWindow.D()))
	if err != nil {
		e.log.Warn("reading conflict history", "err", err)
	}
	fs := rules.Evaluate(rules.Input{Snapshot: snap, Config: cfg, Structure: e.cfg.Structure(), Now: now, Conflicts: conflicts})
	z := newFreezer(snap)

	e.mu.Lock()
	produced := map[string]bool{}
	current := map[string]model.Finding{}
	obsAdd := map[string]store.Observation{}
	var obsRemove []string
	for _, f := range fs {
		produced[f.ID] = true
		if z.frozen(f) {
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
		// The minimum duration gates opening only: a finding that is open
		// stays open while its condition holds, also after the threshold
		// was raised.
		if now.Sub(o.Since) < f.MinDuration && e.open[f.ID] == nil {
			continue
		}
		current[f.ID] = f
	}
	for id, o := range e.obs {
		if produced[id] {
			continue
		}
		if z.keepObservation(e.open[id], o) {
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
			if f.Urgent {
				rec.UrgentSince = now
			}
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
			// A finding that becomes urgent later (a check switched to
			// urgent, a directory gaining an urgent reason) gets its own
			// debounce from that moment rather than pinging at once.
			switch {
			case f.Urgent && !rec.Urgent:
				rec.UrgentSince = now
			case !f.Urgent:
				rec.UrgentSince = time.Time{}
			}
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
		if z.gone(rec.Finding) {
			// The server was removed or disabled in the settings: close the
			// finding quietly, a "resolved" message would be misleading.
			rec.ResolvedAt, rec.LastSeen, rec.ResolvedNotifiedAt = now, now, now
			e.save(rec)
			if err := e.st.HistoryResolveNote(id, now, "server removed or disabled"); err != nil {
				e.log.Warn("writing history", "err", err)
			}
			delete(e.open, id)
			delete(e.persisted, id)
			changed = true
			e.log.Info("finding closed", "reason", "server removed or disabled", "check", rec.Check, "subject", rec.Subject, "id", id)
			continue
		}
		if z.frozen(rec.Finding) {
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
		if r != nil && resolved {
			// The "resolved" message was about an earlier occurrence that
			// has re-opened since, inheriting its PingedAt (see Evaluate).
			// That occurrence is done with; the new one is unannounced.
			r.PingedAt = time.Time{}
			e.save(r)
			continue
		}
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
	r := e.open[z.FindingID]
	if r != nil {
		z.Subject = r.Check + " " + r.Subject
	}
	e.mu.Unlock()
	if r == nil {
		// Only a problem that exists, or did recently, can be hidden. IDs
		// are predictable, so otherwise a server's future H1/H3 could be
		// muted in advance without anyone seeing it.
		seen, err := e.st.FindingSeenSince(z.FindingID, z.CreatedAt.Add(-resolvedMemory))
		if err != nil {
			return err
		}
		if !seen {
			return errUnknownFinding
		}
	}
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
