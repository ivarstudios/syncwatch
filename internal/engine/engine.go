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
	holds       map[string]time.Time // held observations: when their timer stopped (see hold)
	snoozes     map[string]store.Snooze
	persisted   map[string]time.Time // last LastSeen written per open record
	snap        *model.Snapshot
	version     uint64
	evaluated   time.Time
	evalStarted uint64 // the number of evaluations that started; see Settle
	evalDone    uint64 // the number of the last complete evaluation
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
		open: map[string]*store.Record{}, holds: map[string]time.Time{}, persisted: map[string]time.Time{},
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

// settleEvery is how often Settle looks for the evaluation it waits for.
const settleEvery = 10 * time.Millisecond

// Settle asks for an evaluation and waits until an evaluation that started
// after the call is complete, at most for d or until ctx ends. It reports
// whether one completed. Such an evaluation has the state of the hub at the
// call: the answer to "Scan now" (W-056) waits with it, thus the page after
// the answer shows the findings of the scan. Only Run evaluates, one
// evaluation at a time.
func (e *Engine) Settle(ctx context.Context, d time.Duration) bool {
	e.mu.Lock()
	want := e.evalStarted + 1
	e.mu.Unlock()
	e.Trigger()
	limit := time.NewTimer(d)
	defer limit.Stop()
	tick := time.NewTicker(settleEvery)
	defer tick.Stop()
	for {
		e.mu.Lock()
		done := e.evalDone >= want
		e.mu.Unlock()
		if done {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-limit.C:
			return false
		case <-tick.C:
		}
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
			// Coalesce bursts of events into one evaluation. A Trigger
			// (Settle, a configuration change) ends the wait.
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			case <-e.trigger:
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
var errUnknownFinding = errors.New("No open or recent finding has this ID.")

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
			e.log.Warn("cannot delete old records from the database", "err", err)
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

// anyPresentStale reports whether one of the servers that are still in the
// configuration is stale.
func (z *freezer) anyPresentStale(ids []string) bool {
	for _, id := range ids {
		if z.present[id] && z.stale[id] {
			return true
		}
	}
	return false
}

// absenceCheck reports whether a finding says that a device is absent: H7
// (PC offline), C2 (inactive device) and H12 for a device that always
// connects through a relay (no server: the device has no connection). A
// server that cannot be reached can still have the device connected, so
// such a finding can only be judged while every server that has the device
// configured is reachable.
func absenceCheck(f model.Finding) bool {
	switch f.Check {
	case "H7", "C2":
		return true
	case "H12":
		return f.Server == ""
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
		if absenceCheck(f) && z.anyPresentStale(f.Related) {
			return true
		}
		// Other cross-server findings are judged on whichever related
		// servers are reachable; only when none is can nothing be said
		// about them.
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

// heldPersist is how often the moved timer of a held observation is
// written to the database. After a restart of syncwatch, at most this much
// of the time held counts as time towards the minimum duration.
const heldPersist = 10 * time.Minute

// hold stops the timer of a held finding's observation: the time from now
// until the hold ends does not count towards its minimum duration (H5
// counts only the time a device is connected). A held finding starts no
// observation.
func (e *Engine) hold(id string, now time.Time, add map[string]store.Observation) {
	o, ok := e.obs[id]
	if !ok {
		return
	}
	from, ok := e.holds[id]
	if !ok {
		e.holds[id] = now
		return
	}
	if now.Sub(from) >= heldPersist {
		o.Since = o.Since.Add(now.Sub(from))
		e.obs[id], e.holds[id], add[id] = o, now, o
	}
}

// release starts the timer of an observation again after a hold: the
// observation starts later by the time it was held.
func (e *Engine) release(id string, now time.Time, add map[string]store.Observation) {
	from, ok := e.holds[id]
	if !ok {
		return
	}
	delete(e.holds, id)
	if o, ok := e.obs[id]; ok {
		o.Since = o.Since.Add(now.Sub(from))
		e.obs[id], add[id] = o, o
	}
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
	// The number of this evaluation, before the snapshot: see Settle.
	e.mu.Lock()
	e.evalStarted++
	run := e.evalStarted
	e.mu.Unlock()
	cfg := e.cfg.Get()
	snap := e.hub.Snapshot()
	now := e.now()
	snap.At = now
	snap.Disabled = e.disabledServers(cfg)
	snap.Links = snap.ServerLinks()
	snap.SiteLinks = model.SiteLinks(snap.Links, cfg.SiteOf, serverOrder(cfg))
	conflicts, err := e.st.ConflictsSince(now.Add(-cfg.Thresholds.ConflictWindow.D()))
	if err != nil {
		e.log.Warn("cannot read the conflict history", "err", err)
	}
	fs := rules.Evaluate(rules.Input{Snapshot: snap, Config: cfg, Structure: e.cfg.Structure(), Now: now, Conflicts: conflicts})
	z := newFreezer(snap)

	e.mu.Lock()
	produced := map[string]bool{}
	held := map[string]bool{}
	current := map[string]model.Finding{}
	obsAdd := map[string]store.Observation{}
	var obsRemove []string
	for _, f := range fs {
		produced[f.ID] = true
		if f.Held {
			// Judged from a last known state: like a frozen finding, and
			// its timer stops.
			held[f.ID] = true
			e.hold(f.ID, now, obsAdd)
			continue
		}
		if z.frozen(f) {
			continue
		}
		e.release(f.ID, now, obsAdd)
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
		delete(e.holds, id)
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
				// carry on as the same, already-announced problem. The time
				// of the last repeat goes with it, or the notifier repeats
				// the urgent message again at once (W-050).
				rec.PingedAt = old.PingedAt
				rec.RepeatedAt = old.RepeatedAt
			}
			e.open[id] = rec
			e.save(rec)
			if err := e.st.HistoryOpen(rec); err != nil {
				e.log.Warn("cannot write the finding history", "err", err)
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
			rec.ResolvedAt, rec.ResolvedNotifiedAt = now, now
			e.save(rec)
			if err := e.st.HistoryResolveNote(id, now, "server removed or disabled"); err != nil {
				e.log.Warn("cannot write the finding history", "err", err)
			}
			delete(e.open, id)
			delete(e.persisted, id)
			changed = true
			e.log.Info("finding closed", "reason", "server removed or disabled", "check", rec.Check, "subject", rec.Subject, "id", id)
			continue
		}
		if z.frozen(rec.Finding) || held[id] {
			if !rec.Stale {
				rec.Stale = true
				e.save(rec)
				changed = true
			}
			continue
		}
		// LastSeen stays the time of the last evaluation that saw the
		// finding open: the resolved message tells from it whether the
		// fault ended while syncwatch did not run (W-038).
		rec.ResolvedAt = now
		e.save(rec)
		if err := e.st.HistoryResolve(id, now); err != nil {
			e.log.Warn("cannot write the finding history", "err", err)
		}
		delete(e.open, id)
		delete(e.persisted, id)
		changed = true
		e.log.Info("finding resolved", "check", rec.Check, "subject", rec.Subject, "id", id)
	}
	e.snap = snap
	e.evaluated = now
	e.evalDone = max(e.evalDone, run)
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
		e.log.Warn("cannot save the observations", "err", err)
	}
	if publish {
		e.publish(ver)
	}
	for _, fn := range e.onEval {
		fn()
	}
}

// disabledServers lists the servers that are disabled in the configuration,
// with the device ID each had when it was polled last. The collector does
// not poll them, but their devices are servers, not PCs (no H7, C2, C3 or
// PC counts for a NAS that an administrator disabled).
func (e *Engine) disabledServers(cfg *config.Config) []model.DisabledServer {
	var out []model.DisabledServer
	for _, s := range cfg.Servers {
		if !s.Disabled {
			continue
		}
		id, _ := e.st.ServerDeviceID(s.ID)
		out = append(out, model.DisabledServer{ID: s.ID, Name: s.Name, MyID: id})
	}
	return out
}

// serverOrder lists the server IDs in configuration order, disabled servers
// included (the order of the sites follows their first server).
func serverOrder(cfg *config.Config) []string {
	ids := make([]string, len(cfg.Servers))
	for i, s := range cfg.Servers {
		ids[i] = s.ID
	}
	return ids
}

func (e *Engine) save(r *store.Record) {
	if err := e.st.SaveRecord(r); err != nil {
		e.log.Error("cannot save the finding", "id", r.ID, "err", err)
	}
}

// sameContent compares the parts of a finding that matter to viewers, and
// the sentence of its resolved message.
func sameContent(a, b model.Finding) bool {
	return a.Check == b.Check && a.Severity == b.Severity && a.Subject == b.Subject && a.Message == b.Message &&
		a.Urgent == b.Urgent && a.Link == b.Link && slices.Equal(a.Checks, b.Checks) && slices.Equal(a.Paths, b.Paths) &&
		slices.Equal(a.Related, b.Related) && reflect.DeepEqual(a.Details, b.Details) && a.ResolvedText == b.ResolvedText
}

// visibleFingerprint hashes what the dashboard shows of the snapshot (server
// status, folder states, completion, connections), ignoring timestamps that
// change on every event such as "last contact".
func visibleFingerprint(snap *model.Snapshot) uint64 {
	h := fnv.New64a()
	w := func(parts ...any) {
		fmt.Fprintln(h, parts...)
	}
	for _, ds := range snap.Disabled {
		w("disabled", ds.ID, ds.Name, ds.MyID)
	}
	// The connection rows show the state and its start time.
	for _, l := range snap.Links {
		w("link", l.A, l.B, l.State, l.Since.Unix())
	}
	for _, l := range snap.SiteLinks {
		w("site link", l.A, l.B, l.State, l.Since.Unix())
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

// MarkRepeated records that a repeat of the urgent message went out for
// findings. Only open findings are repeated: an ID that is not open is
// skipped.
func (e *Engine) MarkRepeated(ids []string, at time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, id := range ids {
		if r := e.open[id]; r != nil {
			r.RepeatedAt = at
			e.save(r)
		}
	}
}

func (e *Engine) mark(ids []string, at time.Time, resolved bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, id := range ids {
		r := e.open[id]
		if r != nil && resolved {
			// The "resolved" message was about an earlier occurrence that
			// has re-opened since, inheriting its PingedAt and RepeatedAt
			// (see Evaluate). That occurrence is done with; the new one is
			// unannounced.
			r.PingedAt, r.RepeatedAt = time.Time{}, time.Time{}
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
