package rules

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
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

// refusedKey matches the error of a server that refuses the API key or the
// gate token: the client keeps "HTTP 401" or "HTTP 403" in its text.
var refusedKey = regexp.MustCompile(`HTTP 40[13]\b`)

// httpAnswer matches the error of a server that answered with an HTTP
// status: the network path to it works.
var httpAnswer = regexp.MustCompile(`HTTP \d{3}\b`)

// netReasons are the texts that the collector gives the failures of a
// connection (see collector.shortErr).
var netReasons = []string{"timeout", "connection refused", "no route to host", "network unreachable",
	"connection reset", "connection closed early", "host name not found", "host name lookup failed"}

// networkFailure reports whether a server is down because syncwatch cannot
// connect to it, not because of an answer or the configuration. From one
// site, a server that is off and a network path that is down (the VPN
// between two sites) look the same.
func networkFailure(err string) bool {
	if httpAnswer.MatchString(err) {
		return false
	}
	for _, r := range netReasons {
		if strings.Contains(err, r) {
			return true
		}
	}
	return false
}

// H1, H3
func (e *evaluator) serverReachability(sv *model.Server) {
	switch sv.Status {
	case model.StatusDown:
		msg := fmt.Sprintf("Server %s is unreachable", sv.Name)
		details := []model.Detail{
			detail("URL", sv.URL),
			timeDetail("Down since", sv.DownSince),
			timeDetail("Last contact", sv.LastContact),
			detail("Error", sv.LastError),
		}
		if refusedKey.MatchString(sv.LastError) {
			// The server answers: it is not unreachable.
			msg = fmt.Sprintf("Server %s refuses the API key or the gate token", sv.Name)
			details = append(details, detail("What to do", "Type the API key or the gate token again in Settings → Servers."))
		} else if networkFailure(sv.LastError) {
			details = append(details, detail("Possible causes", "The server is off or does not answer, or the network path to it (for example the VPN between the sites) is down."))
		}
		if sv.LastError != "" {
			msg += ": " + sv.LastError
		}
		e.add("H1", sv.ID, "server", model.Finding{
			Subject: sv.Name,
			Message: msg,
			Since:   sv.DownSince,
			Details: details,
		})
	case model.StatusCertChanged:
		cc := sv.CertChange
		if cc == nil {
			cc = &model.CertChange{}
		}
		e.add("H3", sv.ID, "cert", model.Finding{
			Subject: sv.Name,
			Message: fmt.Sprintf("Server %s sent a different TLS certificate. syncwatch reads no data until you accept the certificate in Settings", sv.Name),
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

// watcherGrace is how long a file watcher error must last before it is
// reported; the folder keeps syncing through its periodic scans meanwhile.
const watcherGrace = time.Hour

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
			reasons = append(reasons, Plural(nErr, "failed item", "failed items"))
		}
		if f.WatchError != "" {
			// Syncthing keeps syncing with its periodic scans when the
			// watcher fails (inotify limits on a big share, typically), so
			// this is a warning once it has lasted a while, never urgent.
			id := e.add("H4", sv.ID, "watcher:"+f.ID, model.Finding{
				Subject:     subject,
				Severity:    model.Warn,
				Message:     fmt.Sprintf("The file watcher of folder %s on %s failed. Syncthing finds changes only with its periodic scans (%s)", name, sv.Name, f.WatchError),
				MinDuration: watcherGrace,
				Details:     []model.Detail{detail("Folder ID", f.ID), detail("Path", f.Path), detail("Watch error", f.WatchError)},
			})
			e.soft[id] = true
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
				Message:     fmt.Sprintf("Folder %s on %s cannot sync: %s", name, sv.Name, strings.Join(reasons, " · ")),
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
				Message:     fmt.Sprintf("Folder %s on %s did not finish the sync in %s (%s, %s)", name, sv.Name, Ago(e.Config.Thresholds.StuckSync.D()), f.State, Plural(int(f.NeedItems), "remaining item", "remaining items")),
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
			// The connections of a server that cannot be reached are old data.
			connected := sv.Status == model.StatusUp && conn != nil && conn.Connected
			state := sv.RemoteState(f.ID, dev)
			switch {
			case state == "paused" && e.Snapshot.ServerByDeviceID(dev) == nil:
				// A polled server reports its paused folders itself.
				e.add("H6", sv.ID, "remote-paused:"+f.ID+":"+dev, model.Finding{
					Subject:     fmt.Sprintf("%s on %s", name, dn),
					Message:     fmt.Sprintf("Device %s paused folder %s (seen from %s)", dn, name, sv.Name),
					MinDuration: e.Config.Thresholds.Paused.D(),
					Details:     []model.Detail{detail("Folder ID", f.ID), detail("Device ID", dev)},
				})
			case connected && c.RemoteState == "valid":
				if stuck(*c) {
					e.stuckDevice(sv, f, dev, *c, false)
				}
			case state == "valid" || state == "":
				// The device is away, or Syncthing does not report its state
				// yet: H5 holds the view of the last connection, so that the
				// time away neither counts as stuck nor ends the finding.
				// After a restart of syncwatch there is no such view; then
				// what Syncthing reports now must do.
				last, ok := sv.LastCompletion(f.ID, dev)
				if !ok {
					last = *c
				}
				if stuck(last) {
					e.stuckDevice(sv, f, dev, last, true)
				}
			}
		}
	}
}

// stuck reports whether a device still needs something of a folder.
// Syncthing reports 100 percent when only directories, empty files or
// deletions are missing, so the remaining items count too.
func stuck(c model.Completion) bool { return c.Percent < 100 || c.NeedItems > 0 }

// floorPercent cuts a percentage to one decimal, so that a device that
// still needs some bytes never shows 100 percent (99.994 is 99.9).
func floorPercent(p float64) float64 { return math.Floor(p*10) / 10 }

// H5 for a device. held: the device is away and c is its last known
// completion (see model.Finding.Held).
func (e *evaluator) stuckDevice(sv *model.Server, f *model.Folder, dev string, c model.Completion, held bool) {
	name, dn := f.DisplayName(), e.devName(dev)
	pct := strconv.FormatFloat(floorPercent(c.Percent), 'f', -1, 64)
	e.add("H5", sv.ID, "device:"+f.ID+":"+dev, model.Finding{
		Subject:     fmt.Sprintf("%s on %s", name, dn),
		Message:     fmt.Sprintf("Device %s is stuck at %s%% of folder %s for more than %s (%s, seen from %s)", dn, pct, name, Ago(e.Config.Thresholds.StuckSync.D()), Plural(int(c.NeedItems), "remaining item", "remaining items"), sv.Name),
		MinDuration: e.Config.Thresholds.StuckSync.D(),
		Held:        held,
		Details: []model.Detail{
			detail("Folder ID", f.ID), detail("Device ID", dev),
			detail("Completion", fmt.Sprintf("%.1f%%", floorPercent(c.Percent))), detail("Items left", fmt.Sprint(c.NeedItems)),
		},
	})
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
		msg := fmt.Sprintf("Unknown device %s requests a connection to %s", name, sv.Name)
		if on := e.configuredOn(p.DeviceID, sv.ID); len(on) > 0 || e.Snapshot.IsServerDevice(p.DeviceID) {
			// Not an unknown device: use the name that the servers know,
			// and show the name that the device sends as a value.
			name = e.devName(p.DeviceID)
			if e.Snapshot.IsServerDevice(p.DeviceID) {
				msg = fmt.Sprintf("Server %s requests a connection to %s", name, sv.Name)
			} else {
				msg = fmt.Sprintf("Device %s (known on %s) requests a connection to %s", name, strings.Join(on, ", "), sv.Name)
			}
			if p.Name != "" {
				msg += fmt.Sprintf(" as %q", p.Name)
			}
		}
		e.add("H9", sv.ID, "pending-device:"+p.DeviceID, model.Finding{
			Subject: fmt.Sprintf("%s on %s", name, sv.Name),
			Message: msg,
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
			Message: fmt.Sprintf("Device %s offers folder %s to %s, which did not accept it", by, label, sv.Name),
			Since:   p.Time,
			Details: []model.Detail{detail("Folder ID", p.FolderID), detail("Offered by", p.OfferedBy), timeDetail("First offered", p.Time)},
		})
	}
}

// configuredOn returns the names of the servers, other than except, that
// have a device configured.
func (e *evaluator) configuredOn(dev, except string) []string {
	var on []string
	for _, sv := range e.Snapshot.Servers {
		if _, ok := sv.Devices[dev]; ok && sv.ID != except && sv.MyID != dev {
			on = append(on, sv.Name)
		}
	}
	return on
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
			Message: fmt.Sprintf("%s in folder %s on %s in the last %s", Plural(int(n), "new sync conflict", "new sync conflicts"), name, sv.Name, Ago(e.Config.Thresholds.ConflictWindow.D())),
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
			Message: fmt.Sprintf("Device %s is connected to %s through a relay, which is slow", dn, sv.Name),
			Details: []model.Detail{detail("Device ID", id), detail("Address", c.Address), detail("Type", c.Type)},
		})
	}
}

// linkGrace is how long two servers can be without a connection to each
// other after a start before H2 opens: Syncthing needs some seconds to
// connect, and a short reconnect is no fault.
const linkGrace = 2 * time.Minute

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
			// The condition starts when the link dropped, or when it could
			// form again: a start of Syncthing on a server, or syncwatch
			// reaching a server again. A link that dropped before that
			// opens at once, a new one after linkGrace.
			since := da.LastSeen
			for _, t := range []time.Time{db.LastSeen, a.StartTime, b.StartTime, a.UpSince, b.UpSince} {
				if t.After(since) {
					since = t
				}
			}
			e.add("H2", a.ID, "link:"+b.ID, model.Finding{
				Subject:     fmt.Sprintf("%s ↔ %s", a.Name, b.Name),
				Message:     fmt.Sprintf("Servers %s and %s are not connected to each other", a.Name, b.Name),
				Related:     []string{b.ID},
				Since:       since,
				MinDuration: linkGrace,
				Details: []model.Detail{
					timeDetail(a.Name+" last saw "+b.Name, da.LastSeen),
					timeDetail(b.Name+" last saw "+a.Name, db.LastSeen),
					detail("What to check", "The VPN or network between the servers, and Syncthing on both servers."),
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

// versionLess reports whether version a is older than b, comparing the parts
// of major.minor.patch as numbers (2.1.9 < 2.1.10). Parts that aren't
// numbers compare as strings, and a missing part counts as older.
func versionLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		x, errX := strconv.Atoi(as[i])
		y, errY := strconv.Atoi(bs[i])
		switch {
		case errX == nil && errY == nil && x != y:
			return x < y
		case (errX != nil || errY != nil) && as[i] != bs[i]:
			return as[i] < bs[i]
		}
	}
	return len(as) < len(bs)
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
	// The reference version is the most common one, ties broken by the newest.
	ref := ""
	for _, v := range sortedKeys(count) {
		if n := count[v]; ref == "" || n > count[ref] || (n == count[ref] && versionLess(ref, v)) {
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
				Message: fmt.Sprintf("PC %s runs Syncthing %s. The servers run v%s", dv.Name, dv.Version, ref),
				Related: dv.SeenBy,
				Details: []model.Detail{detail("Device ID", dv.ID), detail("Version", dv.Version)},
			})
		}
	}
}

// H7 (offline 7–90 days); beyond the old-device threshold C2 takes over. A
// device that never connected at all isn't "offline": only C2 reports it.
func (e *evaluator) offlinePCs() {
	th := e.Config.Thresholds
	loc := e.Config.Location()
	for _, dv := range e.Snapshot.Devices() {
		if dv.IsServer || dv.Connected || dv.LastSeen.IsZero() || len(dv.SeenBy) == 0 || !e.anyLoaded(dv.SeenBy) {
			continue
		}
		off := e.Now.Sub(dv.LastSeen)
		if off < th.PCOffline.D() {
			continue
		}
		if off >= th.OldDevice.D() && e.Config.CheckEnabled("C2") {
			continue
		}
		e.add("H7", "", "device:"+dv.ID, model.Finding{
			Subject: dv.Name,
			Message: fmt.Sprintf("PC %s is offline for %s (last seen %s)", dv.Name, Ago(off), dv.LastSeen.In(loc).Format("2006-01-02 15:04")),
			Since:   dv.LastSeen,
			Related: dv.SeenBy,
			Details: []model.Detail{detail("Device ID", dv.ID), timeDetail("Last seen", dv.LastSeen), detail("Configured on", strings.Join(e.names(dv.SeenBy), ", "))},
		})
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
