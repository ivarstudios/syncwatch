package collector

import (
	"context"
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

// maybeScan starts a structure scan in the background when one is due.
func (c *Collector) maybeScan(ctx context.Context) {
	cfg := c.cfg.Get().Collector
	if time.Since(c.lastScan) < cfg.StructureInterval.D() {
		return
	}
	c.scanMu.Lock()
	if c.scanning {
		c.scanMu.Unlock()
		return
	}
	c.scanning = true
	c.scanMu.Unlock()
	baseline := time.Since(c.lastBase) >= cfg.BaselineInterval.D()
	c.lastScan = time.Now()
	if baseline {
		c.lastBase = c.lastScan
	}
	go func() {
		defer func() {
			c.scanMu.Lock()
			c.scanning = false
			c.scanMu.Unlock()
		}()
		if err := c.scan(ctx, baseline); err != nil {
			c.log.Warn("structure scan failed", "err", shortErr(err))
		}
	}()
}

type folderRef struct {
	id, path string
}

// scan lists every share's top level and searches project folders for
// nested project folders: folders not in Syncthing on every scan, synced
// folders only on a baseline scan (events cover them in between).
func (c *Collector) scan(ctx context.Context, baseline bool) error {
	var sv *model.Server
	c.hub.View(c.id, func(s *model.Server) { sv = s.Clone() })
	if sv == nil || !sv.Loaded {
		return nil
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

	sem := make(chan struct{}, max(1, cfg.Collector.Concurrency))
	var listings atomic.Int64
	browse := func(dir string) ([]string, error) {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		defer func() { <-sem }()
		listings.Add(1)
		return c.client.Browse(ctx, dir, st.Sep)
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
				return
			}
			if len(entries) == 0 && !c.exists(browse, st, r.Path) {
				sh.Error = "not found on the server (is it mounted in the Syncthing container?)"
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
				if synced && !baseline {
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

	// Keep earlier findings (from events or deeper baselines) that still exist.
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
		if c.exists(browse, st, n.Path) {
			found[k] = n
		}
	}

	took := time.Since(now)
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
		s.LastStructureScan = now
		s.Structure.ScanListings, s.Structure.ScanTook, s.Structure.ScanBaseline = int(listings.Load()), took, baseline
	})
	// Info level: what the scan costs the server is worth seeing.
	c.log.Info("structure scan done", "baseline", baseline, "shares", len(shares), "listings", listings.Load(), "took", took.Round(time.Millisecond))
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
		name := st.Base(p)
		if cs.Ignored(name) {
			continue
		}
		if cs.IsProject(name) {
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
