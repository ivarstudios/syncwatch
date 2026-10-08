package rules

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/stclient"
)

func (e *evaluator) health() {
	for _, sv := range e.Snapshot.Servers {
		e.serverReachability(sv)
		if !sv.Loaded {
			continue
		}
		e.folderHealth(sv)
		e.pausedDevices(sv)
		e.pending(sv)
		e.conflicts(sv)
		e.relays(sv)
	}
	e.links()
	e.versions()
	e.offlinePCs()
}

// H1, H3
func (e *evaluator) serverReachability(sv *model.Server) {
	switch sv.Status {
	case model.StatusDown:
		msg := fmt.Sprintf("%s can't be reached", sv.Name)
		if sv.LastError != "" {
			msg += ": " + sv.LastError
		}
		e.add("H1", sv.ID, "server", model.Finding{
			Subject: sv.Name,
			Message: msg,
			Since:   sv.DownSince,
			Details: []model.Detail{
				detail("URL", sv.URL),
				timeDetail("Down since", sv.DownSince),
				timeDetail("Last contact", sv.LastContact),
				detail("Error", sv.LastError),
			},
		})
	case model.StatusCertChanged:
		cc := sv.CertChange
		if cc == nil {
			cc = &model.CertChange{}
		}
		e.add("H3", sv.ID, "cert", model.Finding{
			Subject: sv.Name,
			Message: fmt.Sprintf("%s presented a different TLS certificate; no data is read until it is accepted in Settings", sv.Name),
			Since:   cc.SeenAt,
			Details: []model.Detail{
				detail("Pinned fingerprint", stclient.ShortFP(cc.Old)),
				detail("New fingerprint", stclient.ShortFP(cc.New)),
				detail("New fingerprint (full)", cc.New),
				timeDetail("New certificate expires", cc.NotAfter),
			},
		})
	}
}

var busyStates = map[string]bool{
	"scanning": true, "scan-waiting": true, "syncing": true, "sync-waiting": true,
	"sync-preparing": true, "cleaning": true, "clean-waiting": true, "starting": true,
}

// H4, H5, H6 (folders), H5 (devices), H6 (remote paused)
func (e *evaluator) folderHealth(sv *model.Server) {
	ids := sortedFolderIDs(sv)
	for _, id := range ids {
		f := sv.Folders[id]
		name := f.DisplayName()
		subject := fmt.Sprintf("%s on %s", name, sv.Name)
		if f.Paused {
			e.add("H6", sv.ID, "folder-paused:"+f.ID, model.Finding{
				Subject:     subject,
				Message:     fmt.Sprintf("Folder %s is paused on %s", name, sv.Name),
				MinDuration: e.Config.Thresholds.Paused.D(),
				Details:     []model.Detail{detail("Folder ID", f.ID), detail("Path", f.Path)},
			})
			continue
		}

		var reasons []string
		lowErr := strings.ToLower(f.Error + " " + f.WatchError)
		if strings.Contains(lowErr, "free space") || strings.Contains(lowErr, "insufficient space") || strings.Contains(lowErr, "no space left") {
			reasons = append(reasons, "out of space")
		}
		if f.State == "error" || f.Error != "" {
			r := "folder error"
			if f.Error != "" {
				r += ": " + f.Error
			}
			reasons = append(reasons, r)
		}
		nErr := f.Errors
		if f.PullErrors > nErr {
			nErr = f.PullErrors
		}
		if len(f.FileErrors) > nErr {
			nErr = len(f.FileErrors)
		}
		if nErr > 0 {
			reasons = append(reasons, fmt.Sprintf("%d file(s) failing to sync", nErr))
		}
		if f.WatchError != "" {
			reasons = append(reasons, "file watcher error: "+f.WatchError)
		}
		if len(reasons) > 0 {
			var paths []string
			for i, fe := range f.FileErrors {
				if i == 5 {
					break
				}
				paths = append(paths, fe.Path+": "+fe.Error)
			}
			var since time.Time
			if f.State == "error" {
				since = f.StateChanged
			}
			// Single failing files are common and often fix themselves (a
			// file in use, the only peer with it gone), while the folder
			// keeps syncing everything else: wait before raising that.
			var minDur time.Duration
			if nErr > 0 && len(reasons) == 1 {
				minDur = e.Config.Thresholds.FailingFiles.D()
			}
			e.add("H4", sv.ID, "folder:"+f.ID, model.Finding{
				Subject:     subject,
				Message:     fmt.Sprintf("Sync stopped for %s on %s: %s", name, sv.Name, strings.Join(reasons, "; ")),
				Since:       since,
				MinDuration: minDur,
				Paths:       paths,
				Details: []model.Detail{
					detail("Folder ID", f.ID), detail("Path", f.Path), detail("State", f.State),
					detail("Error", f.Error), detail("Watch error", f.WatchError),
					detail("Failing items", fmt.Sprint(nErr)),
				},
			})
		} else if busyStates[f.State] || f.NeedItems > 0 {
			e.add("H5", sv.ID, "folder:"+f.ID, model.Finding{
				Subject:     subject,
				Message:     fmt.Sprintf("%s on %s has not finished syncing for over %s (%s, %d items left)", name, sv.Name, Ago(e.Config.Thresholds.StuckSync.D()), f.State, f.NeedItems),
				MinDuration: e.Config.Thresholds.StuckSync.D(),
				Details: []model.Detail{
					detail("Folder ID", f.ID), detail("State", f.State),
					detail("Items left", fmt.Sprint(f.NeedItems)), detail("Bytes left", fmt.Sprint(f.NeedBytes)),
				},
			})
		}

		devs := sortedKeys(f.Completion)
		for _, dev := range devs {
			c := f.Completion[dev]
			if !f.SharedWithDevice(dev) {
				continue
			}
			dn := e.devName(dev)
			conn := sv.Connections[dev]
			connected := conn != nil && conn.Connected
			switch {
			case c.RemoteState == "paused" && !e.Snapshot.IsServerDevice(dev):
				e.add("H6", sv.ID, "remote-paused:"+f.ID+":"+dev, model.Finding{
					Subject:     fmt.Sprintf("%s on %s", name, dn),
					Message:     fmt.Sprintf("%s has paused folder %s (seen from %s)", dn, name, sv.Name),
					MinDuration: e.Config.Thresholds.Paused.D(),
					Details:     []model.Detail{detail("Folder ID", f.ID), detail("Device ID", dev)},
				})
			case connected && c.RemoteState == "valid" && c.Percent < 100:
				e.add("H5", sv.ID, "device:"+f.ID+":"+dev, model.Finding{
					Subject:     fmt.Sprintf("%s on %s", name, dn),
					Message:     fmt.Sprintf("%s is stuck at %.0f%% of %s for over %s (seen from %s)", dn, c.Percent, name, Ago(e.Config.Thresholds.StuckSync.D()), sv.Name),
					MinDuration: e.Config.Thresholds.StuckSync.D(),
					Details: []model.Detail{
						detail("Folder ID", f.ID), detail("Device ID", dev),
						detail("Completion", fmt.Sprintf("%.1f%%", c.Percent)), detail("Items left", fmt.Sprint(c.NeedItems)),
					},
				})
			}
		}
	}
}

// H6 for devices paused in a server's configuration.
func (e *evaluator) pausedDevices(sv *model.Server) {
	for _, id := range sortedKeys(sv.Devices) {
		d := sv.Devices[id]
		if id == sv.MyID || !d.Paused {
			continue
		}
		dn := e.devName(id)
		e.add("H6", sv.ID, "device-paused:"+id, model.Finding{
			Subject:     fmt.Sprintf("%s on %s", dn, sv.Name),
			Message:     fmt.Sprintf("Device %s is paused on %s", dn, sv.Name),
			MinDuration: e.Config.Thresholds.Paused.D(),
			Details:     []model.Detail{detail("Device ID", id)},
		})
	}
}

// H9
func (e *evaluator) pending(sv *model.Server) {
	for _, p := range sv.PendingDevices {
		name := p.Name
		if name == "" {
			name = model.ShortID(p.DeviceID)
		}
		e.add("H9", sv.ID, "pending-device:"+p.DeviceID, model.Finding{
			Subject: fmt.Sprintf("%s on %s", name, sv.Name),
			Message: fmt.Sprintf("Unknown device %s wants to connect to %s", name, sv.Name),
			Since:   p.Time,
			Details: []model.Detail{detail("Device ID", p.DeviceID), detail("Address", p.Address), timeDetail("First offered", p.Time)},
		})
	}
	for _, p := range sv.PendingFolders {
		label := p.Label
		if label == "" {
			label = p.FolderID
		}
		by := e.devName(p.OfferedBy)
		e.add("H9", sv.ID, "pending-folder:"+p.FolderID+":"+p.OfferedBy, model.Finding{
			Subject: fmt.Sprintf("%s on %s", label, sv.Name),
			Message: fmt.Sprintf("%s offers folder %s to %s, which hasn't accepted it", by, label, sv.Name),
			Since:   p.Time,
			Details: []model.Detail{detail("Folder ID", p.FolderID), detail("Offered by", p.OfferedBy), timeDetail("First offered", p.Time)},
		})
	}
}

// H10
func (e *evaluator) conflicts(sv *model.Server) {
	per := e.Conflicts[sv.ID]
	for _, fid := range sortedKeys(per) {
		n := per[fid]
		if n <= 0 {
			continue
		}
		name := fid
		if f, ok := sv.Folders[fid]; ok {
			name = f.DisplayName()
		}
		e.add("H10", sv.ID, "conflicts:"+fid, model.Finding{
			Subject: fmt.Sprintf("%s on %s", name, sv.Name),
			Message: fmt.Sprintf("%d new sync conflict(s) in %s on %s in the last %s", n, name, sv.Name, Ago(e.Config.Thresholds.ConflictWindow.D())),
			Details: []model.Detail{detail("Folder ID", fid), detail("New conflicts", fmt.Sprint(n))},
		})
	}
}

// H11
func (e *evaluator) relays(sv *model.Server) {
	if sv.Status != model.StatusUp {
		return
	}
	for _, id := range sortedKeys(sv.Connections) {
		c := sv.Connections[id]
		if !c.Connected || !c.IsRelay() {
			continue
		}
		dn := e.devName(id)
		e.add("H11", sv.ID, "relay:"+id, model.Finding{
			Subject: fmt.Sprintf("%s ↔ %s", sv.Name, dn),
			Message: fmt.Sprintf("%s is connected to %s through a relay, which is slow", dn, sv.Name),
			Details: []model.Detail{detail("Device ID", id), detail("Address", c.Address), detail("Type", c.Type)},
		})
	}
}

// H2: two polled servers that have each other configured should always be connected.
func (e *evaluator) links() {
	ss := e.Snapshot.Servers
	for i := 0; i < len(ss); i++ {
		for j := i + 1; j < len(ss); j++ {
			a, b := ss[i], ss[j]
			if !a.Loaded || !b.Loaded || a.MyID == "" || b.MyID == "" {
				continue
			}
			da, okA := a.Devices[b.MyID]
			db, okB := b.Devices[a.MyID]
			if !okA || !okB {
				continue
			}
			if a.Status != model.StatusUp || b.Status != model.StatusUp {
				// H1/H3 covers it; an open H2 stays frozen until both are back.
				continue
			}
			if da.Paused || db.Paused {
				continue // H6
			}
			ca, cb := a.Connections[b.MyID], b.Connections[a.MyID]
			if (ca != nil && ca.Connected) || (cb != nil && cb.Connected) {
				continue
			}
			since := da.LastSeen
			if db.LastSeen.After(since) {
				since = db.LastSeen
			}
			e.add("H2", a.ID, "link:"+b.ID, model.Finding{
				Subject: fmt.Sprintf("%s ↔ %s", a.Name, b.Name),
				Message: fmt.Sprintf("%s and %s are not connected to each other", a.Name, b.Name),
				Related: []string{b.ID},
				Since:   since,
				Details: []model.Detail{
					timeDetail(a.Name+" last saw "+b.Name, da.LastSeen),
					timeDetail(b.Name+" last saw "+a.Name, db.LastSeen),
				},
			})
		}
	}
}

// NormVersion strips the leading v and anything after major.minor.patch.
func NormVersion(v string) string {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+ "); i >= 0 {
		v = v[:i]
	}
	return v
}

// H8
func (e *evaluator) versions() {
	count := map[string]int{}
	var servers, ids []string
	for _, sv := range e.Snapshot.Servers {
		if sv.Version == "" || !sv.Loaded {
			continue
		}
		count[NormVersion(sv.Version)]++
		servers = append(servers, fmt.Sprintf("%s %s", sv.Name, sv.Version))
		ids = append(ids, sv.ID)
	}
	if len(count) == 0 {
		return
	}
	// The reference version is the most common one, ties broken by the highest.
	ref := ""
	for v, n := range count {
		if ref == "" || n > count[ref] || (n == count[ref] && v > ref) {
			ref = v
		}
	}
	if len(count) > 1 {
		e.add("H8", "", "servers", model.Finding{
			Subject: "Servers",
			Message: "Servers run different Syncthing versions: " + strings.Join(servers, ", "),
			Related: ids,
			Details: []model.Detail{detail("Versions", strings.Join(servers, ", "))},
		})
	}
	for _, dv := range e.Snapshot.Devices() {
		if dv.IsServer || !dv.Connected || dv.Version == "" {
			continue
		}
		if NormVersion(dv.Version) != ref {
			e.add("H8", "", "device:"+dv.ID, model.Finding{
				Subject: dv.Name,
				Message: fmt.Sprintf("%s runs Syncthing %s; the servers run v%s", dv.Name, dv.Version, ref),
				Related: dv.SeenBy,
				Details: []model.Detail{detail("Device ID", dv.ID), detail("Version", dv.Version)},
			})
		}
	}
}

// H7 (offline 7–90 days); beyond the old-device threshold C2 takes over.
func (e *evaluator) offlinePCs() {
	th := e.Config.Thresholds
	for _, dv := range e.Snapshot.Devices() {
		if dv.IsServer || dv.Connected || len(dv.SeenBy) == 0 || !e.anyLoaded(dv.SeenBy) {
			continue
		}
		f := model.Finding{
			Subject: dv.Name,
			Related: dv.SeenBy,
			Details: []model.Detail{detail("Device ID", dv.ID), timeDetail("Last seen", dv.LastSeen), detail("Configured on", strings.Join(e.names(dv.SeenBy), ", "))},
		}
		if dv.LastSeen.IsZero() {
			f.Message = fmt.Sprintf("%s has not connected since syncwatch started watching it", dv.Name)
			f.MinDuration = th.PCOffline.D()
		} else {
			off := e.Now.Sub(dv.LastSeen)
			if off < th.PCOffline.D() {
				continue
			}
			if off >= th.OldDevice.D() && e.Config.CheckEnabled("C2") {
				continue
			}
			f.Message = fmt.Sprintf("%s has been offline for %s (last seen %s)", dv.Name, Ago(off), dv.LastSeen.Format("2006-01-02"))
			f.Since = dv.LastSeen
		}
		e.add("H7", "", "device:"+dv.ID, f)
	}
}

func (e *evaluator) anyLoaded(ids []string) bool {
	for _, id := range ids {
		if sv := e.Snapshot.Server(id); sv != nil && sv.Loaded {
			return true
		}
	}
	return false
}

func (e *evaluator) names(ids []string) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = e.serverName(id)
	}
	return out
}

func sortedFolderIDs(sv *model.Server) []string {
	ids := make([]string, 0, len(sv.Folders))
	for id := range sv.Folders {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := sv.Folders[ids[i]], sv.Folders[ids[j]]
		if a.DisplayName() != b.DisplayName() {
			return a.DisplayName() < b.DisplayName()
		}
		return ids[i] < ids[j]
	})
	return ids
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
