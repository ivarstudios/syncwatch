package collector

// Folder sizes: the project folders on the disk of a server that are not in
// Syncthing (rules.NotInSyncthing, the S7 directories) get their size from
// the gate of the server (/syncwatch/size). The gate measures them each
// night at collector.size_time, and when an administrator selects Measure
// on the Folders page. syncwatch only reads the results: the gate keeps
// them, so a restart of syncwatch loses nothing.

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/rules"
	"github.com/ivarstudios/syncwatch/internal/stclient"
)

// Variables, so that tests can change them.
var (
	sizeTick    = time.Minute // how often the size loop runs; a measurement that runs is read again at each tick
	sizeRefresh = time.Hour   // a known size is read from the gate again after this
	sizeWindow  = time.Hour   // the nightly measurements are requested in this time after size_time
)

// Why a size measurement cannot start for an administrator (MeasureNow).
var (
	ErrSizeNotUp     = errors.New("the server is not up, or its first full check is not complete")
	ErrSizeNotOnDisk = errors.New("the folder is not a project folder outside Syncthing on this server now: scan the shares again")
)

// sizeLoop keeps the folder sizes of the server current while the session
// (ctx) runs. A new session asks again whether the server measures sizes:
// the gate may have been updated.
func (c *Collector) sizeLoop(ctx context.Context) {
	c.sizeMu.Lock()
	c.sizeNone = false
	c.sizeMu.Unlock()
	t := time.NewTicker(sizeTick)
	defer t.Stop()
	for {
		c.syncSizes(ctx, time.Now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// syncSizes reads the sizes that are new, old or in a measurement from the
// gate, and in the nightly window it asks the gate to measure each folder
// once. It writes to the hub only when something changed.
func (c *Collector) syncSizes(ctx context.Context, now time.Time) {
	var st *model.Structure
	c.hub.View(c.id, func(s *model.Server) {
		if s.Status == model.StatusUp && s.Loaded && s.Structure != nil {
			st = s.Structure.Clone()
		}
	})
	if st == nil {
		return
	}
	cfg := c.cfg.Get()
	dirs := unsyncedDirs(st, c.cfg.Structure())
	night := nightKey(cfg, now)

	c.sizeMu.Lock()
	none := c.sizeNone
	if c.sizeFetched == nil {
		c.sizeFetched, c.sizeNight = map[string]time.Time{}, map[string]string{}
	}
	for p := range c.sizeFetched {
		if !slices.Contains(dirs, p) {
			delete(c.sizeFetched, p)
			delete(c.sizeNight, p)
		}
	}
	c.sizeMu.Unlock()

	got := map[string]*model.DirSize{}
	asked := 0
	for _, p := range dirs {
		if none || ctx.Err() != nil {
			break
		}
		old := st.Sizes[p]
		c.sizeMu.Lock()
		measure := night != "" && c.sizeNight[p] != night
		fresh := old != nil && old.State == "" && now.Sub(c.sizeFetched[p]) < sizeRefresh
		c.sizeMu.Unlock()
		if !measure && fresh {
			continue
		}
		res, err := c.client.Size(ctx, p, measure)
		if errors.Is(err, stclient.ErrNoSize) {
			none = true
			break
		}
		if ctx.Err() != nil {
			return
		}
		ds := fromGate(res, old)
		if err != nil {
			// Keep the last result, with the reason why syncwatch cannot
			// read a new one.
			ds = &model.DirSize{}
			if old != nil {
				*ds = *old
			}
			ds.State, ds.Error = "", shortErr(err)
		}
		c.sizeMu.Lock()
		c.sizeFetched[p] = now
		if measure && err == nil {
			c.sizeNight[p] = night
			asked++
		}
		c.sizeMu.Unlock()
		got[p] = ds
	}
	if asked > 0 {
		c.log.Info("nightly folder size measurement requested", "folders", asked)
	}

	c.sizeMu.Lock()
	c.sizeNone = none
	c.sizeMu.Unlock()
	changed := st.NoSizes != none
	for p, ds := range got {
		if !sameSize(st.Sizes[p], ds) {
			changed = true
		}
	}
	for p := range st.Sizes {
		if !slices.Contains(dirs, p) || none {
			changed = true
		}
	}
	if !changed {
		return
	}
	c.update(func(s *model.Server) {
		if s.Structure == nil {
			return
		}
		s.Structure.NoSizes = none
		if none {
			s.Structure.Sizes = nil
			return
		}
		if s.Structure.Sizes == nil {
			s.Structure.Sizes = map[string]*model.DirSize{}
		}
		for p := range s.Structure.Sizes {
			if !slices.Contains(dirs, p) {
				delete(s.Structure.Sizes, p)
			}
		}
		for p, ds := range got {
			s.Structure.Sizes[p] = ds
		}
	})
}

// MeasureNow asks the gate of the server to measure path, a project folder
// of the server that is not in Syncthing, for an administrator. The gate
// measures one folder at a time; the size loop reads the result.
func (c *Collector) MeasureNow(path string) error {
	var st *model.Structure
	c.hub.View(c.id, func(s *model.Server) {
		if s.Status == model.StatusUp && s.Loaded && s.Structure != nil {
			st = s.Structure.Clone()
		}
	})
	c.scanMu.Lock()
	ctx := c.sessCtx
	c.scanMu.Unlock()
	if st == nil || ctx == nil || ctx.Err() != nil {
		return ErrSizeNotUp
	}
	if !slices.Contains(unsyncedDirs(st, c.cfg.Structure()), path) {
		return ErrSizeNotOnDisk
	}
	res, err := c.client.Size(ctx, path, true)
	if err != nil {
		return err
	}
	ds := fromGate(res, st.Sizes[path])
	c.sizeMu.Lock()
	if c.sizeFetched != nil {
		c.sizeFetched[path] = time.Now()
	}
	c.sizeMu.Unlock()
	c.update(func(s *model.Server) {
		if s.Structure == nil {
			return
		}
		if s.Structure.Sizes == nil {
			s.Structure.Sizes = map[string]*model.DirSize{}
		}
		s.Structure.Sizes[path] = ds
	})
	return nil
}

// unsyncedDirs lists the paths of the project folders of the shares that
// no Syncthing folder holds.
func unsyncedDirs(st *model.Structure, cs *config.CompiledStructure) []string {
	var out []string
	for _, sh := range st.Shares {
		for _, d := range sh.Dirs {
			if rules.NotInSyncthing(cs, d) && !slices.Contains(out, d.Path) {
				out = append(out, d.Path)
			}
		}
	}
	return out
}

// nightKey names the night whose measurements are due at now: the start of
// the window after collector.size_time, in the time zone of the
// configuration. It is empty outside the window and when size_time is off.
func nightKey(cfg *config.Config, now time.Time) string {
	if cfg.Collector.SizeTime == "off" {
		return ""
	}
	m, err := config.ParseClock(cfg.Collector.SizeTime)
	if err != nil {
		return ""
	}
	loc := cfg.Location()
	l := now.In(loc)
	start := time.Date(l.Year(), l.Month(), l.Day(), m/60, m%60, 0, 0, loc)
	if l.Before(start) {
		start = start.AddDate(0, 0, -1) // a window that started yesterday, for a size_time shortly before midnight
	}
	if l.Sub(start) >= sizeWindow {
		return ""
	}
	return start.Format("2006-01-02T15:04")
}

// fromGate converts a result of the gate. A result with an error and no
// size is a measurement that failed (the directory does not exist, the gate
// cannot see the share): it keeps the last size that syncwatch read (old),
// with the error, or it has no size (MeasuredAt zero). Thus a failed
// measurement never shows as a size of 0 bytes.
func fromGate(r stclient.DirSize, old *model.DirSize) *model.DirSize {
	ds := &model.DirSize{Bytes: r.Bytes, Files: r.Files, Complete: r.Complete, MeasuredAt: r.MeasuredAt,
		Took: time.Duration(r.TookMS) * time.Millisecond, State: r.State, Error: r.Error}
	if r.Error == "" || r.Bytes != 0 || r.Files != 0 {
		return ds
	}
	ds.Bytes, ds.Files, ds.Complete, ds.MeasuredAt, ds.Took = 0, 0, false, time.Time{}, 0
	if old != nil && !old.MeasuredAt.IsZero() {
		ds.Bytes, ds.Files, ds.Complete, ds.MeasuredAt, ds.Took = old.Bytes, old.Files, old.Complete, old.MeasuredAt, old.Took
	}
	return ds
}

func sameSize(a, b *model.DirSize) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Bytes == b.Bytes && a.Files == b.Files && a.Complete == b.Complete && a.MeasuredAt.Equal(b.MeasuredAt) &&
		a.Took == b.Took && a.State == b.State && a.Error == b.Error
}
