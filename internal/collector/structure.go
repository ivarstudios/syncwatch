package collector

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/pathx"
	"github.com/ivarstudios/syncwatch/internal/rules"
)

func lowerKey(s string) string { return strings.ToLower(s) }

// scanTimerEvery is how often scanTimer checks whether a scan is due.
var scanTimerEvery = 10 * time.Second

// maybeScan starts a structure scan in the background when one is due. In
// events mode the loop calls it between long-polls, which can be
// event_timeout apart, so scanTimer also calls it when a scan falls due.
// ctx is the context of the session: the scan stops with the session.
func (c *Collector) maybeScan(ctx context.Context) {
	cfg := c.cfg.Get().Collector
	c.scanMu.Lock()
	defer c.scanMu.Unlock()
	if ctx != c.sessCtx {
		// A new session. Its scanTimer ends with it (the timer of the first
		// session ran with that session only, and no timer followed it
		// after a reconnect). ScanNow starts its scans with this context.
		c.sessCtx = ctx
		if cfg.Mode == config.ModeEvents {
			go c.scanTimer(ctx)
		}
	}
	if c.scanning || time.Since(c.lastScan) < cfg.StructureInterval.D() {
		return
	}
	c.startScan(ctx, cfg)
}

// startScan starts a structure scan in the background: a baseline scan when
// one is due, else a regular scan. The timed scans (maybeScan) and the scans
// that an administrator starts (ScanNow) both start here. The caller holds
// scanMu, and no scan runs. A scan that stops before the end (its session
// ends) writes no result and does not count: the next regular scan
// is due as if it did not start, and manualEvery does not count it. A
// stopped baseline scan still counts for the next baseline scan: it is long,
// and sessions that end during it must not start it again and again.
func (c *Collector) startScan(ctx context.Context, cfg config.Collector) {
	c.scanning = true
	done, end := make(chan struct{}), &ScanEnd{}
	c.scanDone, c.scanEnd = done, end
	baseline := time.Since(c.lastBase) >= c.baselineEvery(cfg)
	prev, start := c.lastScan, time.Now()
	c.lastScan = start
	if baseline {
		c.lastBase = start
	}
	go func() {
		var err error
		defer func() {
			c.scanMu.Lock()
			if err != nil {
				end.Stopped = ScanStopSession
				if errors.Is(err, errNotLoaded) {
					end.Stopped = ScanStopNotLoaded
				}
				if c.lastScan.Equal(start) {
					c.lastScan = prev
				}
				if c.manualAt.Equal(start) {
					c.manualAt = time.Time{}
				}
			}
			c.scanning, c.scanDone, c.scanEnd = false, nil, nil
			c.scanMu.Unlock()
			close(done)
		}()
		if err = c.scan(ctx, baseline); err != nil {
			c.log.Warn("structure scan failed", "err", shortErr(err))
		}
	}()
}

// manualEvery is the shortest time between 2 structure scans of a server
// that an administrator starts (ScanNow).
var manualEvery = 30 * time.Second

// The states of a request for a structure scan (ScanStart.State).
const (
	ScanStarted = "started"  // the scan started now
	ScanRunning = "running"  // a scan runs: syncwatch starts no second scan
	ScanTooSoon = "too-soon" // an administrator started a scan less than 30 seconds ago
	ScanNotUp   = "not-up"   // the server is not up, or its first full check is not complete
)

// ScanStart is the answer to a request for a structure scan (ScanNow).
type ScanStart struct {
	State string
	// Started is the start of the last scan of the server: the new scan,
	// or the scan that runs. Zero for ScanNotUp.
	Started time.Time
	// Next is the earliest start of the next scan that an administrator
	// can start (ScanTooSoon).
	Next time.Time
	// Done is closed at the end of the last scan. It is nil when no scan
	// runs.
	Done <-chan struct{}
	// End tells how the scan of Done ended: complete, or stopped before the
	// end. Read it only after Done is closed. It is nil when Done is nil.
	End *ScanEnd
}

// ScanEnd tells how a structure scan ended. The scan writes it before its
// Done channel closes.
type ScanEnd struct {
	// Stopped is empty for a complete scan: the scan wrote its result to
	// the hub. Else it tells why the scan stopped before the end.
	Stopped string
}

// Why a structure scan stopped before the end (ScanEnd.Stopped).
const (
	ScanStopSession   = "session"    // the session of the server ended: a lost connection, a change of the settings, or the stop of syncwatch
	ScanStopNotLoaded = "not-loaded" // the first full check of the server is not complete
)

// ScanNow starts a structure scan of the server at once, for an
// administrator. The scan is the same as a timed scan (startScan),
// thus it only reads. One scan of a server runs at a time, and an
// administrator can start 1 scan of a server in manualEvery. The timed
// scans continue from the start of this scan. A scan that stops before the
// end counts for neither (startScan).
func (c *Collector) ScanNow() ScanStart {
	up := false
	c.hub.View(c.id, func(s *model.Server) { up = s.Status == model.StatusUp && s.Loaded })
	c.scanMu.Lock()
	defer c.scanMu.Unlock()
	ctx := c.sessCtx
	if !up || ctx == nil || ctx.Err() != nil {
		return ScanStart{State: ScanNotUp}
	}
	if !c.manualAt.IsZero() && time.Since(c.manualAt) < manualEvery {
		return ScanStart{State: ScanTooSoon, Started: c.lastScan, Next: c.manualAt.Add(manualEvery), Done: c.scanDone, End: c.scanEnd}
	}
	if c.scanning {
		return ScanStart{State: ScanRunning, Started: c.lastScan, Done: c.scanDone, End: c.scanEnd}
	}
	c.startScan(ctx, c.cfg.Get().Collector)
	c.manualAt = c.lastScan
	return ScanStart{State: ScanStarted, Started: c.lastScan, Done: c.scanDone, End: c.scanEnd}
}

// A baseline scan that took less than fastBaseline makes the next one due
// after at most dailyBaseline. Only a baseline scan walks the
// project folders that Syncthing syncs (in events mode), and on some NAS
// storage Syncthing's file watcher misses a copied project folder without
// an error: with a fast baseline scan, syncwatch finds it within a day.
const (
	fastBaseline  = 10 * time.Second
	dailyBaseline = 24 * time.Hour
)

// baselineEvery is the time from one baseline scan to the next: the
// baseline interval, or at most a day when the last baseline scan of this
// collector was fast. The caller holds scanMu.
func (c *Collector) baselineEvery(cfg config.Collector) time.Duration {
	every := cfg.BaselineInterval.D()
	if c.baseKnown && c.baseTook < fastBaseline {
		every = min(every, dailyBaseline)
	}
	return every
}

// noteBaseline keeps the duration of a completed baseline scan for
// baselineEvery, and logs the interval of the next baseline scans when it
// changes. Nothing is logged while the baseline interval is a day or less:
// then the duration changes nothing.
func (c *Collector) noteBaseline(took time.Duration) {
	every := c.cfg.Get().Collector.BaselineInterval
	c.scanMu.Lock()
	c.baseTook, c.baseKnown = took, true
	choice := ""
	if every.D() > dailyBaseline {
		choice = every.Words()
		if took < fastBaseline {
			choice = "day"
		}
	}
	changed := choice != c.baseChoice
	c.baseChoice = choice
	c.scanMu.Unlock()
	switch {
	case !changed || choice == "":
	case choice == "day":
		c.log.Info("baseline scan every day: the last one took less than 10 seconds", "took", took.Round(time.Millisecond))
	default:
		c.log.Info("baseline scan every "+choice+": the last one took 10 seconds or more", "took", took.Round(time.Millisecond))
	}
}

// scanTimer starts due structure scans in events mode, where the loop sits
// in a long-poll for up to event_timeout and would start them that late. It
// runs for one session (ctx) and only scans while the server is up.
func (c *Collector) scanTimer(ctx context.Context) {
	t := time.NewTicker(scanTimerEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		up := false
		c.hub.View(c.id, func(s *model.Server) { up = s.Status == model.StatusUp && s.Loaded })
		if up {
			c.maybeScan(ctx)
		}
	}
}

type folderRef struct {
	id, path string
}

// browser returns a directory lister throttled to the configured concurrency,
// and the counter of listings made through it.
func (c *Collector) browser(ctx context.Context, st pathx.Style) (func(string) ([]string, error), *atomic.Int64) {
	sem := make(chan struct{}, max(1, c.cfg.Get().Collector.Concurrency))
	listings := new(atomic.Int64)
	return func(dir string) ([]string, error) {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		defer func() { <-sem }()
		listings.Add(1)
		return c.client.Browse(ctx, dir, st.Sep)
	}, listings
}

// rescanFolders walks the top-level project folders that hold the given
// Syncthing folders, as a baseline scan does, and replaces the nested
// entries under them. It runs after a gap in the event stream, when
// directory events of those folders may have been lost. Entries are only
// removed when every listing of the walk succeeded.
func (c *Collector) rescanFolders(ctx context.Context, ids map[string]bool) {
	var sv *model.Server
	c.hub.View(c.id, func(s *model.Server) { sv = s.Clone() })
	cs := c.cfg.Structure()
	if sv == nil || !sv.Loaded || !cs.HasProjectPattern() {
		return
	}
	cfg := c.cfg.Get()
	st := pathx.StyleFor(sv.PathSep)
	var add, exclude []string
	if sc, ok := cfg.ServerByKey(sv.ID); ok {
		add, exclude = cfg.SharesFor(sc)
	}
	refs := rules.DetectShares(sv, add, exclude, cs)
	var folders []folderRef
	for _, f := range sv.Folders {
		if f.Path != "" {
			folders = append(folders, folderRef{f.ID, f.Path})
		}
	}
	type topRef struct{ share, top string }
	tops := map[string]topRef{}
	for id := range ids {
		f := sv.Folders[id]
		if f == nil || f.Path == "" {
			continue
		}
		share := rules.ShareOf(refs, f.Path, st)
		if share == nil {
			continue
		}
		rel := st.Split(st.Rel(f.Path, share.Path))
		if len(rel) == 0 || cs.Ignored(rel[0]) || !cs.IsProject(rel[0]) {
			continue
		}
		top := st.Join(share.Path, rel[0])
		tops[nestedKey(st, top)] = topRef{share.Path, top}
	}
	if len(tops) == 0 {
		return
	}

	browse, listings := c.browser(ctx, st)
	var failed atomic.Int64
	list := func(dir string) ([]string, error) {
		out, err := browse(dir)
		if err != nil {
			failed.Add(1)
		}
		return out, err
	}
	now := time.Now()
	found := map[string]*model.NestedDir{}
	var fmu sync.Mutex
	var wg sync.WaitGroup
	for _, t := range tops {
		t := t
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.descend(ctx, list, st, cs, &model.Share{Path: t.share}, &model.Dir{Path: t.top}, t.top, 1, cs.Depth, folders, now, func(n *model.NestedDir) {
				fmu.Lock()
				found[nestedKey(st, n.Path)] = n
				fmu.Unlock()
			})
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return
	}
	c.update(func(s *model.Server) {
		if s.Structure == nil {
			s.Structure = &model.Structure{}
		}
		if s.Structure.Nested == nil {
			s.Structure.Nested = map[string]*model.NestedDir{}
		}
		for k, n := range s.Structure.Nested {
			if failed.Load() > 0 || found[k] != nil || n.FoundAt.After(now) {
				continue
			}
			for _, t := range tops {
				if st.Within(n.Path, t.top) {
					delete(s.Structure.Nested, k)
					break
				}
			}
		}
		for k, n := range found {
			if old := s.Structure.Nested[k]; old != nil {
				n.FoundAt = old.FoundAt
				if old.Source == "event" {
					n.Source = old.Source
				}
			}
			s.Structure.Nested[k] = n
		}
	})
	c.log.Info("project folders scanned again after missed events", "folders", len(tops), "listings", listings.Load(), "failed", failed.Load(), "took", time.Since(now).Round(time.Millisecond))
}

// errNotLoaded stops a structure scan of a server whose first full check is
// not complete: the scan needs the paths of its Syncthing folders.
var errNotLoaded = errors.New("the first full check of the server is not complete")

// scan lists every share's top level and searches project folders for
// nested project folders: folders not in Syncthing on every scan, synced
// folders only on a baseline scan (events cover them in between) except in
// polled mode, which has no events and therefore walks them every time.
// It returns an error, and writes nothing, when it stops before the end.
func (c *Collector) scan(ctx context.Context, baseline bool) error {
	var sv *model.Server
	c.hub.View(c.id, func(s *model.Server) { sv = s.Clone() })
	if sv == nil || !sv.Loaded {
		return errNotLoaded
	}
	cfg := c.cfg.Get()
	cs := c.cfg.Structure()
	st := pathx.StyleFor(sv.PathSep)
	var add, exclude []string
	if sc, ok := cfg.ServerByKey(sv.ID); ok {
		add, exclude = cfg.SharesFor(sc)
	}
	refs := rules.DetectShares(sv, add, exclude, cs)
	var folders []folderRef
	for _, f := range sv.Folders {
		if f.Path != "" {
			folders = append(folders, folderRef{f.ID, f.Path})
		}
	}

	browse, listings := c.browser(ctx, st)

	// The previous results. A share that can't be listed now keeps its last
	// directory list, marked stale by the error and the old ScannedAt, so
	// that S4–S7 don't resolve and re-open around every outage or slow listing.
	// It also keeps the end of its last listing: its findings show that
	// time as their last scan.
	var prevShares []*model.Share
	c.hub.View(c.id, func(s *model.Server) {
		if s.Structure != nil {
			prevShares = s.Structure.Clone().Shares
		}
	})
	stale := func(sh *model.Share) {
		for _, old := range prevShares {
			if st.Equal(old.Path, sh.Path) {
				sh.Dirs, sh.ScannedAt, sh.ListedAt = old.Dirs, old.ScannedAt, old.ListedAt
			}
		}
	}

	now := time.Now()
	shares := make([]*model.Share, len(refs))
	var wg sync.WaitGroup
	for i, r := range refs {
		i, r := i, r
		wg.Add(1)
		go func() {
			defer wg.Done()
			sh := &model.Share{Path: r.Path, Name: r.Name, Attrs: r.Attrs, Manual: r.Manual, ScannedAt: now}
			shares[i] = sh
			entries, err := browse(r.Path)
			if err != nil {
				sh.Error = shortErr(err)
				stale(sh)
				return
			}
			if len(entries) == 0 && !c.exists(browse, st, r.Path) {
				sh.Error = "not found on the server (check that the directory is mounted in the Syncthing container)"
				stale(sh)
				return
			}
			for _, p := range entries {
				p = st.Clean(p)
				d := &model.Dir{Name: st.Base(p), Path: p}
				for _, f := range folders {
					if st.Equal(f.path, p) || st.Within(p, f.path) || st.Within(f.path, p) {
						d.FolderIDs = append(d.FolderIDs, f.id)
					}
				}
				sort.Strings(d.FolderIDs)
				sh.Dirs = append(sh.Dirs, d)
			}
			sort.Slice(sh.Dirs, func(a, b int) bool { return sh.Dirs[a].Name < sh.Dirs[b].Name })
		}()
	}
	wg.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}

	found := map[string]*model.NestedDir{}
	var fmu sync.Mutex
	if cs.HasProjectPattern() {
		var nwg sync.WaitGroup
		for _, sh := range shares {
			for _, d := range sh.Dirs {
				if cs.Ignored(d.Name) || !cs.IsProject(d.Name) {
					continue
				}
				synced := false
				for _, f := range folders {
					if st.Equal(f.path, d.Path) || st.Within(d.Path, f.path) {
						synced = true
					}
				}
				if synced && !baseline && cfg.Collector.Mode != config.ModePolled {
					continue
				}
				sh, d := sh, d
				nwg.Add(1)
				go func() {
					defer nwg.Done()
					c.descend(ctx, browse, st, cs, sh, d, d.Path, 1, cs.Depth, folders, now, func(n *model.NestedDir) {
						fmu.Lock()
						found[nestedKey(st, n.Path)] = n
						fmu.Unlock()
					})
				}()
			}
		}
		nwg.Wait()
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	// Keep earlier findings (from events or deeper baselines) that still exist
	// and still count under the current rules: the pattern, ignore_dirs, the
	// depth and the shares may have changed since they were found.
	var prev map[string]*model.NestedDir
	c.hub.View(c.id, func(s *model.Server) {
		if s.Structure != nil {
			prev = map[string]*model.NestedDir{}
			for k, n := range s.Structure.Nested {
				nc := *n
				prev[k] = &nc
			}
		}
	})
	for k, n := range prev {
		if _, ok := found[k]; ok {
			found[k].FoundAt = n.FoundAt
			if n.Source == "event" {
				found[k].Source = "event"
			}
			continue
		}
		share := rules.ShareOf(refs, n.Path, st)
		if share == nil {
			continue
		}
		top, ok := nestedIn(st, cs, share.Path, n.Path)
		if !ok {
			continue
		}
		if c.exists(browse, st, n.Path) {
			n.Parent, n.Share = top, share.Path
			found[k] = n
		}
	}

	took := time.Since(now)
	end, failed := now.Add(took), 0
	for _, sh := range shares {
		if sh.Error != "" {
			failed++
		} else {
			sh.ListedAt = end
		}
	}
	c.update(func(s *model.Server) {
		if s.Structure == nil {
			s.Structure = &model.Structure{}
		}
		s.Structure.Shares = shares
		s.Structure.ScannedAt = now
		if baseline {
			s.Structure.BaselineAt = now
		}
		// Merge with nested folders reported by events during the scan.
		for k, n := range s.Structure.Nested {
			if _, ok := found[k]; !ok && n.FoundAt.After(now) {
				found[k] = n
			}
		}
		s.Structure.Nested = found
		// The end of the scan: the structure findings show it as the time
		// of the last scan. A scan that could list none of the
		// shares checked nothing: the time stays that of the last scan that
		// listed a share (or found no share).
		if failed == 0 || failed < len(shares) {
			s.LastStructureScan = end
		}
		s.Structure.ScanListings, s.Structure.ScanTook, s.Structure.ScanBaseline = int(listings.Load()), took, baseline
	})
	// Info level: what the scan costs the server is worth seeing.
	c.log.Info("structure scan done", "baseline", baseline, "shares", len(shares), "share_errors", failed, "listings", listings.Load(), "took", took.Round(time.Millisecond))
	if baseline {
		c.noteBaseline(took)
	}
	return nil
}

func (c *Collector) descend(ctx context.Context, browse func(string) ([]string, error), st pathx.Style, cs *config.CompiledStructure,
	sh *model.Share, top *model.Dir, dir string, depth, maxDepth int, folders []folderRef, now time.Time, emit func(*model.NestedDir)) {
	if depth > maxDepth || ctx.Err() != nil {
		return
	}
	entries, err := browse(dir)
	if err != nil {
		return
	}
	for _, p := range entries {
		p = st.Clean(p)
		if cs.Ignored(st.Base(p)) {
			continue // with its subtree
		}
		if _, ok := nestedIn(st, cs, sh.Path, p); ok {
			n := &model.NestedDir{Path: p, Parent: top.Path, Share: sh.Path, Source: "scan", FoundAt: now}
			for _, f := range folders {
				if st.Equal(f.path, p) || st.Within(p, f.path) {
					n.FolderID = f.id
				}
			}
			emit(n)
		}
		c.descend(ctx, browse, st, cs, sh, top, p, depth+1, maxDepth, folders, now, emit)
	}
}

// nestedIn reports whether p is a nested project folder of the share: its
// top-level directory in the share is a project folder, p lies one to
// cs.Depth levels below it, and no directory on the way is ignored. It
// returns that top-level directory. The scan, the event path and the entries
// kept between scans all use it, so they agree on what counts.
func nestedIn(st pathx.Style, cs *config.CompiledStructure, share, p string) (top string, ok bool) {
	if !st.Within(p, share) {
		return "", false
	}
	rel := st.Split(st.Rel(p, share))
	if len(rel) < 2 || len(rel) > cs.Depth+1 {
		return "", false
	}
	for i, name := range rel {
		if cs.Ignored(name) || ((i == 0 || i == len(rel)-1) && !cs.IsProject(name)) {
			return "", false
		}
	}
	return st.Join(share, rel[0]), true
}

// exists checks that a directory is still there by listing its parent.
// Browse matches the last segment by prefix, so the result is filtered.
func (c *Collector) exists(browse func(string) ([]string, error), st pathx.Style, p string) bool {
	parent := st.Parent(p)
	if parent == "" {
		return true
	}
	entries, err := browse(parent)
	if err != nil {
		return true // unknown: keep it
	}
	for _, e := range entries {
		if st.Equal(e, p) {
			return true
		}
	}
	return false
}
