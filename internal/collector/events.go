package collector

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/pathx"
	"github.com/ivarstudios/syncwatch/internal/rules"
	"github.com/ivarstudios/syncwatch/internal/stclient"
)

// apply updates the server state from one event.
func (c *Collector) apply(ctx context.Context, e stclient.Event) {
	switch e.Type {
	case "StateChanged":
		var d stclient.StateChangedData
		if json.Unmarshal(e.Data, &d) != nil {
			return
		}
		c.withFolder(d.Folder, func(f *model.Folder) {
			f.State, f.StateChanged = d.To, e.Time
			if d.To == "error" && d.Error != "" {
				f.Error = d.Error
			}
			if d.From == "error" && d.To != "error" {
				f.Error = ""
			}
		})
	case "FolderSummary":
		var d stclient.FolderSummaryData
		if json.Unmarshal(e.Data, &d) != nil {
			return
		}
		c.withFolder(d.Folder, func(f *model.Folder) { applyDBStatus(f, d.Summary) })
	case "FolderCompletion":
		var d stclient.FolderCompletionData
		if json.Unmarshal(e.Data, &d) != nil || d.Device == "" {
			return
		}
		c.withFolder(d.Folder, func(f *model.Folder) {
			comp := completionFrom(d.Completion, e.Time)
			if comp.RemoteState == "" {
				// Older versions don't include it; keep what the full check found.
				if old := f.Completion[d.Device]; old != nil {
					comp.RemoteState = old.RemoteState
				}
			}
			f.Completion[d.Device] = comp
		})
	case "FolderErrors":
		var d stclient.FolderErrorsData
		if json.Unmarshal(e.Data, &d) != nil {
			return
		}
		c.withFolder(d.Folder, func(f *model.Folder) {
			f.FileErrors = fileErrors(d.Errors)
			if len(d.Errors) > f.PullErrors {
				f.PullErrors = len(d.Errors)
			}
		})
	case "FolderWatchStateChanged":
		var d stclient.FolderWatchStateData
		if json.Unmarshal(e.Data, &d) != nil {
			return
		}
		c.withFolder(d.Folder, func(f *model.Folder) { f.WatchError = d.To })
	case "FolderPaused", "FolderResumed":
		var d stclient.FolderIDData
		if json.Unmarshal(e.Data, &d) != nil {
			return
		}
		c.withFolder(d.ID, func(f *model.Folder) { f.Paused = e.Type == "FolderPaused" })
	case "DeviceConnected":
		var d stclient.DeviceConnectedData
		if json.Unmarshal(e.Data, &d) != nil {
			return
		}
		c.update(func(s *model.Server) {
			s.Connections[d.ID] = &model.Connection{Connected: true, Address: d.Addr, Type: d.Type, ClientVersion: d.ClientVersion, StartedAt: e.Time}
			if dev := s.Devices[d.ID]; dev != nil {
				dev.LastSeen = e.Time
			}
		})
	case "DeviceDisconnected":
		var d stclient.DeviceIDData
		if json.Unmarshal(e.Data, &d) != nil {
			return
		}
		id := firstNonEmpty(d.ID, d.Device)
		c.update(func(s *model.Server) {
			if cn := s.Connections[id]; cn != nil {
				cn.Connected = false
			}
			if dev := s.Devices[id]; dev != nil {
				dev.LastSeen = e.Time
			}
		})
	case "DevicePaused", "DeviceResumed":
		var d stclient.DeviceIDData
		if json.Unmarshal(e.Data, &d) != nil {
			return
		}
		id := firstNonEmpty(d.Device, d.ID)
		c.update(func(s *model.Server) {
			if dev := s.Devices[id]; dev != nil {
				dev.Paused = e.Type == "DevicePaused"
			}
		})
	case "PendingDevicesChanged", "PendingFoldersChanged":
		c.pendingDue = true // re-read once per batch, by applyBatch
	case "ConfigSaved":
		// The payload is never used: it contains the GUI credentials and is
		// stripped by the client. The folder and device config is re-read
		// instead, once per batch, by applyBatch.
		c.configDue = true
	case "LocalChangeDetected", "RemoteChangeDetected":
		var d stclient.DiskChangeData
		if json.Unmarshal(e.Data, &d) != nil {
			return
		}
		if e.Type == "LocalChangeDetected" {
			// lastFile.at only moves for pulled files, so this is the only
			// trace of local edits (e.g. over SMB); C1 uses the newer of the two.
			c.withFolder(firstNonEmpty(d.FolderID, d.Folder), func(f *model.Folder) {
				if e.Time.After(f.LastLocalChange) {
					f.LastLocalChange = e.Time
				}
			})
		}
		c.diskChange(d, e.Time)
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// gapState is what missedEvents keeps between gaps in the event stream.
type gapState struct {
	mu          sync.Mutex
	folders     map[string]bool // Syncthing folders with events since the last re-scan
	first       time.Time       // when the pending folders were first noted
	timer       *time.Timer     // the pending re-scan
	rescanning  bool
	lastFull    time.Time // the last full re-check caused by a gap
	fullPending bool      // a deferred one is scheduled
}

// Gaps in the event IDs: a full re-check at most every gapFullEvery, and a
// re-scan of the affected folders gapScanAfter after the last gap, or at the
// latest gapScanMax after the first pending one.
var (
	gapFullEvery = 10 * time.Minute
	gapScanAfter = 30 * time.Second
	gapScanMax   = 10 * time.Minute
)

// missedEvents handles a gap in the event IDs: Syncthing's buffer of 1000
// events overflowed, which the per-file disk events do during any big sync
// (20,000 new files over a 100 ms link gave a gap after every re-check). A
// full re-check holds the event stream for seconds and so causes the next
// gap, which chained re-checks for as long as the sync ran. A gap-caused
// re-check therefore runs at most every gapFullEvery (one deferred re-check
// catches up), and the folders in the batch, whose directory events may be
// among the lost ones, are re-scanned once the gaps stop.
func (c *Collector) missedEvents(ctx context.Context, from, to int64, evs []stclient.Event) {
	g := &c.gaps
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	if g.folders == nil {
		g.folders = map[string]bool{}
	}
	for _, e := range evs {
		if id := eventFolder(e); id != "" {
			g.folders[id] = true
		}
	}
	if g.first.IsZero() {
		g.first = now
	}
	if g.timer != nil {
		g.timer.Stop()
	}
	g.timer = time.AfterFunc(min(gapScanAfter, g.first.Add(gapScanMax).Sub(now)), func() { c.rescanAfterGap(ctx) })

	if now.Sub(g.lastFull) >= gapFullEvery {
		g.lastFull = now
		c.log.Info("missed events; running a full re-check", "from", from, "to", to)
		c.FullCheckNow()
		return
	}
	c.log.Info("missed events; a full re-check ran recently, deferring the next", "from", from, "to", to)
	if !g.fullPending {
		g.fullPending = true
		time.AfterFunc(g.lastFull.Add(gapFullEvery).Sub(now), func() {
			g.mu.Lock()
			g.fullPending, g.lastFull = false, time.Now()
			g.mu.Unlock()
			if ctx.Err() == nil {
				c.FullCheckNow()
			}
		})
	}
}

// rescanAfterGap runs rescanFolders for the folders noted by missedEvents,
// one run at a time.
func (c *Collector) rescanAfterGap(ctx context.Context) {
	g := &c.gaps
	g.mu.Lock()
	if g.rescanning {
		g.timer = time.AfterFunc(gapScanAfter, func() { c.rescanAfterGap(ctx) })
		g.mu.Unlock()
		return
	}
	ids := g.folders
	g.folders, g.first, g.rescanning = nil, time.Time{}, true
	g.mu.Unlock()
	if ctx.Err() == nil && len(ids) > 0 {
		c.rescanFolders(ctx, ids)
	}
	g.mu.Lock()
	g.rescanning = false
	g.mu.Unlock()
}

// eventFolder returns the Syncthing folder an event concerns, or "".
func eventFolder(e stclient.Event) string {
	var d struct{ Folder, FolderID, ID string }
	if json.Unmarshal(e.Data, &d) != nil {
		return ""
	}
	switch e.Type {
	case "FolderPaused", "FolderResumed":
		return d.ID
	case "LocalChangeDetected", "RemoteChangeDetected":
		return firstNonEmpty(d.FolderID, d.Folder)
	}
	return d.Folder
}

func (c *Collector) withFolder(id string, fn func(*model.Folder)) {
	c.update(func(s *model.Server) {
		if f := s.Folders[id]; f != nil {
			fn(f)
		}
	})
}

func (c *Collector) refreshPending(ctx context.Context) error {
	pdev, err := c.client.PendingDevices(ctx)
	if err != nil {
		return err
	}
	pfold, err := c.client.PendingFolders(ctx)
	if err != nil {
		return err
	}
	var devs []model.PendingDevice
	for id, p := range pdev {
		devs = append(devs, model.PendingDevice{DeviceID: id, Name: p.Name, Address: p.Address, Time: p.Time})
	}
	var folds []model.PendingFolder
	for id, p := range pfold {
		for by, o := range p.OfferedBy {
			folds = append(folds, model.PendingFolder{FolderID: id, Label: o.Label, OfferedBy: by, Time: o.Time})
		}
	}
	c.update(func(s *model.Server) { s.PendingDevices, s.PendingFolders = devs, folds })
	return nil
}

// diskChange catches nested project folders inside synced folders as they
// appear, by the scan's rules (nestedIn). Syncthing 2.1 reports new
// directories as action "modified" (and sometimes "added"); deletions remove
// known nested folders.
func (c *Collector) diskChange(d stclient.DiskChangeData, at time.Time) {
	if d.Type != "dir" {
		return
	}
	cs := c.cfg.Structure()
	if !cs.HasProjectPattern() {
		return
	}
	folderID := firstNonEmpty(d.FolderID, d.Folder)
	cfg := c.cfg.Get()
	c.update(func(s *model.Server) {
		f := s.Folders[folderID]
		if f == nil || f.Path == "" {
			return
		}
		st := pathx.StyleFor(s.PathSep)
		full := st.Join(f.Path, d.Path)
		if s.Structure == nil {
			s.Structure = &model.Structure{Nested: map[string]*model.NestedDir{}}
		}
		if d.Action == "deleted" {
			for k, n := range s.Structure.Nested {
				if st.Equal(n.Path, full) || st.Within(n.Path, full) {
					delete(s.Structure.Nested, k)
				}
			}
			return
		}
		name := st.Base(full)
		if cs.Ignored(name) || !cs.IsProject(name) {
			return // cheap pre-check; nestedIn decides
		}
		var add, exclude []string
		if sc, ok := cfg.ServerByKey(s.ID); ok {
			add, exclude = cfg.SharesFor(sc)
		}
		shares := rules.DetectShares(s, add, exclude, cs)
		share := rules.ShareOf(shares, full, st)
		if share == nil {
			return
		}
		// The same test as the scan's, so a top-level directory, a directory
		// under a non-project top, one behind an ignored directory or one
		// deeper than nested_scan_depth is not reported.
		top, ok := nestedIn(st, cs, share.Path, full)
		if !ok {
			return
		}
		key := nestedKey(st, full)
		if _, ok := s.Structure.Nested[key]; ok {
			return
		}
		s.Structure.Nested[key] = &model.NestedDir{Path: full, Parent: top, Share: share.Path, FolderID: folderID, Source: "event", FoundAt: at}
		c.log.Info("nested project folder created", "path", full)
	})
}

func nestedKey(st pathx.Style, p string) string {
	p = st.Clean(p)
	if st.CaseInsensitive {
		return lowerKey(p)
	}
	return p
}

// refreshConfig re-reads only the folder and device configuration after a
// ConfigSaved event. Existing folders keep their state; only new folders get
// the (expensive) db/status call and their completion.
func (c *Collector) refreshConfig(ctx context.Context) error {
	folders, err := c.client.ConfigFolders(ctx)
	if err != nil {
		return err
	}
	devices, err := c.client.ConfigDevices(ctx)
	if err != nil {
		return err
	}
	var myID string
	known := map[string]bool{}
	c.hub.View(c.id, func(s *model.Server) {
		myID = s.MyID
		for id := range s.Folders {
			known[id] = true
		}
	})
	fresh := map[string]*model.Folder{}
	for _, fc := range folders {
		f := folderFromConfig(fc, myID)
		if !known[fc.ID] {
			if ds, err := c.client.DBStatus(ctx, fc.ID); err == nil {
				applyDBStatus(f, ds)
			} else if !perFolder(err) {
				return err
			}
			if !f.Paused {
				for _, dev := range f.SharedWith {
					if comp, err := c.client.Completion(ctx, fc.ID, dev); err == nil {
						f.Completion[dev] = completionFrom(comp, time.Now())
					} else if !perFolder(err) {
						return err
					}
				}
			}
		}
		fresh[fc.ID] = f
	}
	c.update(func(s *model.Server) {
		next := map[string]*model.Folder{}
		for id, f := range fresh {
			if old, ok := s.Folders[id]; ok {
				// Keep live state; take the configuration from the new read.
				old.Label, old.Path, old.Type, old.Paused, old.SharedWith = f.Label, f.Path, f.Type, f.Paused, f.SharedWith
				if old.Paused {
					old.State = "paused"
				} else if old.State == "paused" {
					old.State = "idle"
				}
				for dev := range old.Completion {
					if !old.SharedWithDevice(dev) {
						delete(old.Completion, dev)
					}
				}
				next[id] = old
			} else {
				next[id] = f
			}
		}
		s.Folders = next
		devs := map[string]*model.Device{}
		for _, d := range devices {
			md := &model.Device{ID: d.DeviceID, Name: d.Name, Paused: d.Paused, Addresses: d.Addresses}
			if old, ok := s.Devices[d.DeviceID]; ok {
				md.LastSeen = old.LastSeen
			}
			devs[d.DeviceID] = md
		}
		s.Devices = devs
	})
	return nil
}
