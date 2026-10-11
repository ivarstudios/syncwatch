package rules

import (
	"fmt"
	"math"
	"net/url"
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
		e.relayListeners(sv)
		e.certExpiry(sv)
	}
	e.links()
	e.versions()
	e.offlinePCs()
	e.relayDevices()
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
		resolved := fmt.Sprintf("Server %s is reachable again.", sv.Name)
		details := []model.Detail{
			detail("URL", sv.URL),
			timeDetail("Down since", sv.DownSince),
			timeDetail("Last contact", sv.LastContact),
			detail("Error", sv.LastError),
		}
		if refusedKey.MatchString(sv.LastError) {
			// The server answers: it is not unreachable.
			msg = fmt.Sprintf("Server %s refuses the API key or the gate token", sv.Name)
			resolved = fmt.Sprintf("Server %s accepts the API key or the gate token again.", sv.Name)
			details = append(details, detail("What to do", "Type the API key or the gate token again in Settings → Servers."))
		} else if networkFailure(sv.LastError) {
			causes := "The server is off or does not answer, or the network path to it (for example the VPN between the sites) is down."
			// With the sites set, the message and the causes name them.
			if home, there := e.Config.Site, e.Config.SiteOf(sv.ID); home != "" && there != "" && home != there {
				msg = fmt.Sprintf("Server %s (%s) is unreachable from %s", sv.Name, there, home)
				causes = fmt.Sprintf("The VPN between %s and %s is down, or the server is off or does not answer.", home, there)
			}
			details = append(details, detail("Possible causes", causes))
			details = append(details, e.peerViews(sv)...)
		}
		if sv.LastError != "" {
			msg += ": " + sv.LastError
		}
		e.add("H1", sv.ID, "server", model.Finding{
			Subject:      sv.Name,
			Message:      msg,
			Since:        sv.DownSince,
			Details:      details,
			ResolvedText: resolved,
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
			ResolvedText: fmt.Sprintf("syncwatch reads server %s again.", sv.Name),
		})
	}
}

// deadLinkTime is how long Syncthing can keep a connection whose network
// path is down: it closes a connection that receives nothing for 5 minutes
// (the kit measured 300 to 421 seconds after a silent cut of the VPN).
const deadLinkTime = 7 * time.Minute

// peerViews gives a row for each other polled server that is up and has a
// live connection to a server that syncwatch cannot reach: Syncthing on that
// server runs, and the network path from syncwatch to it is down. Until
// syncwatch has had no contact with the server for deadLinkTime, the
// connection can be one that Syncthing did not close yet: no row.
func (e *evaluator) peerViews(sv *model.Server) []model.Detail {
	if sv.MyID == "" || sv.LastContact.IsZero() || e.Now.Sub(sv.LastContact) < deadLinkTime {
		return nil
	}
	var out []model.Detail
	for _, o := range e.Snapshot.Servers {
		if o.ID == sv.ID || o.Status != model.StatusUp {
			continue
		}
		if c := o.Connections[sv.MyID]; c != nil && c.Connected {
			out = append(out, detail("What "+o.Name+" sees", fmt.Sprintf("%s is connected to %s. Thus Syncthing on %s runs, and the network path from syncwatch to %s is down.", o.Name, sv.Name, sv.Name, sv.Name)))
		}
	}
	return out
}

// certWarning is how long before the end of the certificate H13 opens.
const certWarning = 30 * 24 * time.Hour

// H13: the certificate of the server expires in less than 30 days, or it
// expired. syncwatch continues to read the server while the fingerprint is
// the pinned fingerprint: this is a hint, not an alert.
func (e *evaluator) certExpiry(sv *model.Server) {
	end := sv.Cert.NotAfter
	if sv.Status != model.StatusUp || end.IsZero() || end.Sub(e.Now) >= certWarning {
		return
	}
	date := end.In(e.Config.Location()).Format("2006-01-02")
	msg := fmt.Sprintf("The certificate of server %s expires on %s (in %s). syncwatch continues to read the server", sv.Name, date, Ago(end.Sub(e.Now)))
	if !end.After(e.Now) {
		msg = fmt.Sprintf("The certificate of server %s expired on %s. syncwatch continues to read the server, because the fingerprint is the pinned fingerprint", sv.Name, date)
	}
	e.add("H13", sv.ID, "cert-expiry", model.Finding{
		Subject: sv.Name,
		Message: msg,
		Details: []model.Detail{
			timeDetail("Certificate expires", end),
			detail("Fingerprint", stclient.ShortFP(sv.Cert.Fingerprint)),
			detail("What to do", "Make a new certificate for the server or its gate. Then accept the new certificate in Settings → Servers."),
		},
		ResolvedText: fmt.Sprintf("The certificate of server %s is valid for more than 30 days now.", sv.Name),
	})
}

var busyStates = map[string]bool{
	"scanning": true, "scan-waiting": true, "syncing": true, "sync-waiting": true,
	"sync-preparing": true, "cleaning": true, "clean-waiting": true, "starting": true,
}

// watcherGrace is how long a file watcher error must last before it is
// reported; the folder keeps syncing through its periodic scans meanwhile.
const watcherGrace = time.Hour

// databaseFull reports whether a folder error says that the disk of
// Syncthing's database is full (minHomeDiskFree): Syncthing then stops every
// folder of the server with this error.
func databaseFull(err string) bool {
	return strings.Contains(strings.ToLower(err), "insufficient space on disk for database")
}

// noSpace reports whether an error says that a disk is full.
func noSpace(err string) bool {
	low := strings.ToLower(err)
	return strings.Contains(low, "free space") || strings.Contains(low, "insufficient space") || strings.Contains(low, "no space left")
}

// H4 for a full database disk: one finding for the server lists the folders
// that Syncthing stopped. These folders get no H4 for this error.
func (e *evaluator) databaseDisk(sv *model.Server, ids []string) {
	var stopped []*model.Folder
	for _, id := range ids {
		if f := sv.Folders[id]; !f.Paused && databaseFull(f.Error) {
			stopped = append(stopped, f)
		}
	}
	if len(stopped) == 0 {
		return
	}
	var since time.Time
	var labels, fids []string
	for i, f := range stopped {
		if f.State == "error" && !f.StateChanged.IsZero() && (since.IsZero() || f.StateChanged.Before(since)) {
			since = f.StateChanged
		}
		if i < maxListed {
			labels = append(labels, f.DisplayName())
		}
		fids = append(fids, f.ID)
	}
	if n := len(stopped) - maxListed; n > 0 {
		labels = append(labels, fmt.Sprintf("…and %d more", n))
	}
	err := stopped[0].Error
	e.add("H4", sv.ID, "database-disk", model.Finding{
		Subject: "Syncthing database on " + sv.Name,
		Message: fmt.Sprintf("Syncthing on %s stopped %s: the disk of its database is full (%s)", sv.Name, Plural(len(stopped), "folder", "folders"), err),
		Since:   since,
		Details: []model.Detail{
			detail("Error", err),
			detail("Stopped folders", strings.Join(labels, ", ")),
			detail("Folder IDs", strings.Join(fids, ", ")),
		},
		ResolvedText: fmt.Sprintf("Syncthing on %s has space for its database again.", sv.Name),
	})
}

// H4, H5, H6 (folders), H5 (devices), H6 (remote paused)
func (e *evaluator) folderHealth(sv *model.Server) {
	ids := sortedFolderIDs(sv)
	e.databaseDisk(sv, ids)
	for _, id := range ids {
		f := sv.Folders[id]
		name := f.DisplayName()
		subject := fmt.Sprintf("%s on %s", name, sv.Name)
		if f.Paused {
			e.add("H6", sv.ID, "folder-paused:"+f.ID, model.Finding{
				Subject:      subject,
				Message:      fmt.Sprintf("Folder %s is paused on %s", name, sv.Name),
				MinDuration:  e.Config.Thresholds.Paused.D(),
				Details:      []model.Detail{detail("Folder ID", f.ID), detail("Path", f.Path)},
				ResolvedText: fmt.Sprintf("Folder %s on %s is not paused now.", name, sv.Name),
			})
			continue
		}

		// The error of a full database disk belongs to the H4 of the server.
		dbFull := databaseFull(f.Error)
		var reasons []string
		space := noSpace(f.WatchError) || (!dbFull && noSpace(f.Error))
		for _, fe := range f.FileErrors {
			// A pull that Syncthing refuses for lack of space in the folder
			// (minDiskFree) stops the sync as a full disk does: no wait.
			space = space || noSpace(fe.Error)
		}
		if space {
			reasons = append(reasons, "out of space")
		}
		if !dbFull && (f.State == "error" || f.Error != "") {
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
				Subject:      subject,
				Severity:     model.Warn,
				Message:      fmt.Sprintf("The file watcher of folder %s on %s failed. Syncthing finds changes only with its periodic scans (%s)", name, sv.Name, f.WatchError),
				MinDuration:  watcherGrace,
				Details:      []model.Detail{detail("Folder ID", f.ID), detail("Path", f.Path), detail("Watch error", f.WatchError)},
				ResolvedText: fmt.Sprintf("The file watcher of folder %s on %s works again.", name, sv.Name),
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
			if f.State == "error" && !dbFull {
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
				ResolvedText: fmt.Sprintf("Folder %s on %s can sync again.", name, sv.Name),
			})
		} else if !dbFull && (busyStates[f.State] || f.NeedItems > 0) {
			// A folder that Syncthing stopped for its database is in the H4
			// of the server, not stuck.
			p := e.progressOf(sv.Progress[f.ID][""], f.NeedBytes, f.GlobalBytes)
			e.add("H5", sv.ID, "folder:"+f.ID, model.Finding{
				Subject:     subject,
				Message:     fmt.Sprintf("Folder %s on %s did not finish the sync in %s (%s, %s%s)", name, sv.Name, Ago(e.Config.Thresholds.StuckSync.D()), f.State, Plural(int(f.NeedItems), "remaining item", "remaining items"), p.suffix(true)),
				MinDuration: e.Config.Thresholds.StuckSync.D(),
				Details: append([]model.Detail{
					detail("Folder ID", f.ID), detail("State", f.State),
					detail("Items left", fmt.Sprint(f.NeedItems)), detail("Bytes left", fmt.Sprint(f.NeedBytes)),
				}, p.details()...),
				ResolvedText: fmt.Sprintf("Folder %s on %s is in sync.", name, sv.Name),
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
					Subject:      fmt.Sprintf("%s on %s", name, dn),
					Message:      fmt.Sprintf("Device %s paused folder %s (seen from %s)", dn, name, sv.Name),
					MinDuration:  e.Config.Thresholds.Paused.D(),
					Details:      []model.Detail{detail("Folder ID", f.ID), detail("Device ID", dev)},
					ResolvedText: fmt.Sprintf("Folder %s is not paused on device %s now (seen from %s).", name, dn, sv.Name),
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
	details := []model.Detail{
		detail("Folder ID", f.ID), detail("Device ID", dev),
		detail("Completion", fmt.Sprintf("%.1f%%", floorPercent(c.Percent))), detail("Items left", fmt.Sprint(c.NeedItems)),
	}
	if nl, ok := sv.NeedLists[f.ID][dev]; ok && len(nl.Names) > 0 {
		details = append(details, detail("Needed files", e.neededFiles(nl, c)))
	}
	// A device that is away receives nothing: no speed for it.
	var samples []model.ProgressSample
	if !held {
		samples = sv.Progress[f.ID][dev]
	}
	p := e.progressOf(samples, c.NeedBytes, c.GlobalBytes)
	details = append(details, p.details()...)
	e.add("H5", sv.ID, "device:"+f.ID+":"+dev, model.Finding{
		Subject:      fmt.Sprintf("%s on %s", name, dn),
		Message:      fmt.Sprintf("Device %s is stuck at %s%% of folder %s for more than %s (%s%s, seen from %s)", dn, pct, name, Ago(e.Config.Thresholds.StuckSync.D()), Plural(int(c.NeedItems), "remaining item", "remaining items"), p.suffix(false), sv.Name),
		MinDuration:  e.Config.Thresholds.StuckSync.D(),
		Held:         held,
		Details:      details,
		ResolvedText: fmt.Sprintf("Device %s has all files of folder %s (seen from %s).", dn, name, sv.Name),
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
			Subject:      fmt.Sprintf("%s on %s", dn, sv.Name),
			Message:      fmt.Sprintf("Device %s is paused on %s", dn, sv.Name),
			MinDuration:  e.Config.Thresholds.Paused.D(),
			Details:      []model.Detail{detail("Device ID", id)},
			ResolvedText: fmt.Sprintf("Device %s is not paused on %s now.", dn, sv.Name),
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
		kind := "device"
		if on := e.configuredOn(p.DeviceID, sv.ID); len(on) > 0 || e.Snapshot.IsServerDevice(p.DeviceID) {
			// Not an unknown device: use the name that the servers know,
			// and show the name that the device sends as a value.
			name = e.devName(p.DeviceID)
			if e.Snapshot.IsServerDevice(p.DeviceID) {
				msg, kind = fmt.Sprintf("Server %s requests a connection to %s", name, sv.Name), "server"
			} else {
				msg = fmt.Sprintf("Device %s (known on %s) requests a connection to %s", name, strings.Join(on, ", "), sv.Name)
			}
			if p.Name != "" {
				msg += fmt.Sprintf(" as %q", p.Name)
			}
		}
		e.add("H9", sv.ID, "pending-device:"+p.DeviceID, model.Finding{
			Subject:      fmt.Sprintf("%s on %s", name, sv.Name),
			Message:      msg,
			Since:        p.Time,
			Details:      []model.Detail{detail("Device ID", p.DeviceID), detail("Address", p.Address), timeDetail("First offered", p.Time)},
			ResolvedText: fmt.Sprintf("The request of %s %s is not pending on %s now.", kind, name, sv.Name),
		})
	}
	for _, p := range sv.PendingFolders {
		label := p.Label
		if label == "" {
			label = p.FolderID
		}
		by := e.devName(p.OfferedBy)
		e.add("H9", sv.ID, "pending-folder:"+p.FolderID+":"+p.OfferedBy, model.Finding{
			Subject:      fmt.Sprintf("%s on %s", label, sv.Name),
			Message:      fmt.Sprintf("Device %s offers folder %s to %s, which did not accept it", by, label, sv.Name),
			Since:        p.Time,
			Details:      []model.Detail{detail("Folder ID", p.FolderID), detail("Offered by", p.OfferedBy), timeDetail("First offered", p.Time)},
			ResolvedText: fmt.Sprintf("The offer of folder %s from device %s is not pending on %s now.", label, by, sv.Name),
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
			Subject:      fmt.Sprintf("%s on %s", name, sv.Name),
			Message:      fmt.Sprintf("%s in folder %s on %s in the last %s", Plural(int(n), "new sync conflict", "new sync conflicts"), name, sv.Name, Ago(e.Config.Thresholds.ConflictWindow.D())),
			Details:      []model.Detail{detail("Folder ID", fid), detail("New conflicts", fmt.Sprint(n))},
			ResolvedText: fmt.Sprintf("Folder %s on %s has no new sync conflicts now.", name, sv.Name),
		})
	}
}

// H11. A device that always connects through a relay (devices[].relay) gets
// no H11: the relay is its normal path, and H12 reports when it has none.
func (e *evaluator) relays(sv *model.Server) {
	if sv.Status != model.StatusUp {
		return
	}
	for _, id := range sortedKeys(sv.Connections) {
		c := sv.Connections[id]
		if !c.Connected || !c.IsRelay() || e.Config.RelayExpected(id) {
			continue
		}
		dn := e.devName(id)
		e.add("H11", sv.ID, "relay:"+id, model.Finding{
			Subject:      fmt.Sprintf("%s ↔ %s", sv.Name, dn),
			Message:      fmt.Sprintf("Device %s is connected to %s through a relay, which is slow", dn, sv.Name),
			Details:      []model.Detail{detail("Device ID", id), detail("Address", c.Address), detail("Type", c.Type)},
			ResolvedText: fmt.Sprintf("Device %s is not connected to %s through a relay now.", dn, sv.Name),
		})
	}
}

// relayHost is the host and port of a relay listener's address
// (relay://10.115.1.60:22067 gives 10.115.1.60:22067), or the address when
// it has no host.
func relayHost(uri string) string {
	if u, err := url.Parse(uri); err == nil && u.Host != "" {
		return u.Host
	}
	return uri
}

// H12 for a server: Syncthing reports that its relay listener has no
// connection to the relay. Devices that connect only through this relay
// cannot sync.
func (e *evaluator) relayListeners(sv *model.Server) {
	if sv.Status != model.StatusUp {
		return
	}
	for _, l := range sv.RelayListeners {
		if l.Connected {
			continue
		}
		relay := relayHost(l.URI)
		msg := fmt.Sprintf("Server %s has no connection to its relay %s", sv.Name, relay)
		if l.Error != "" {
			msg += ": " + l.Error
		}
		e.add("H12", sv.ID, "relay-listener:"+l.URI, model.Finding{
			Subject:     fmt.Sprintf("%s on %s", relay, sv.Name),
			Message:     msg,
			Since:       l.Since,
			MinDuration: e.Config.Thresholds.RelayLost.D(),
			Details: []model.Detail{
				detail("Relay", l.URI), detail("Error", l.Error),
				detail("Possible causes", "The relay is off, or the network path from the server to the relay is down. Devices that connect only through this relay cannot sync."),
			},
			ResolvedText: fmt.Sprintf("Server %s is connected to its relay again.", sv.Name),
		})
	}
}

// H12 for a device that always connects through a relay (devices[].relay):
// it has no connection to a server that is up for more than relay_lost.
// Like H7, the absence of a device is known only while every server that
// has it configured can be reached (the engine keeps the finding stale
// meanwhile).
func (e *evaluator) relayDevices() {
	if len(e.Config.Devices) == 0 {
		return
	}
	th := e.Config.Thresholds.RelayLost.D()
	loc := e.Config.Location()
	for _, dv := range e.Snapshot.Devices() {
		if dv.IsServer || dv.Connected || dv.LastSeen.IsZero() || len(dv.SeenBy) == 0 || !e.anyLoaded(dv.SeenBy) || !e.Config.RelayExpected(dv.ID) {
			continue
		}
		e.add("H12", "", "relay-device:"+dv.ID, model.Finding{
			Subject:     dv.Name,
			Message:     fmt.Sprintf("Device %s has no connection for more than %s (last seen %s). It connects only through a relay", dv.Name, Ago(th), dv.LastSeen.In(loc).Format("2006-01-02 15:04")),
			Since:       dv.LastSeen,
			MinDuration: th,
			Related:     dv.SeenBy,
			Details: []model.Detail{
				detail("Device ID", dv.ID), timeDetail("Last seen", dv.LastSeen),
				detail("Possible causes", "The relay is off or unreachable, or the device is off. H12 for the servers shows a relay that is off."),
			},
			ResolvedText: fmt.Sprintf("Device %s is connected again.", dv.Name),
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
			check := "The VPN or network between the servers, and Syncthing on both servers."
			if sa, sb := e.Config.SiteOf(a.ID), e.Config.SiteOf(b.ID); sa != "" && sb != "" && sa != sb {
				check = fmt.Sprintf("The VPN between %s and %s, and Syncthing on both servers.", sa, sb)
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
					detail("What to check", check),
				},
				ResolvedText: fmt.Sprintf("Servers %s and %s are connected again.", a.Name, b.Name),
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
			Subject:      "Servers",
			Message:      "Servers run different Syncthing versions: " + strings.Join(servers, ", "),
			Related:      ids,
			Details:      []model.Detail{detail("Versions", strings.Join(servers, ", "))},
			ResolvedText: "The servers run the same Syncthing version again.",
		})
	}
	for _, dv := range e.Snapshot.Devices() {
		if dv.IsServer || !dv.Connected || dv.Version == "" {
			continue
		}
		if NormVersion(dv.Version) != ref {
			e.add("H8", "", "device:"+dv.ID, model.Finding{
				Subject:      dv.Name,
				Message:      fmt.Sprintf("PC %s runs Syncthing %s. The servers run v%s", dv.Name, dv.Version, ref),
				Related:      dv.SeenBy,
				Details:      []model.Detail{detail("Device ID", dv.ID), detail("Version", dv.Version)},
				ResolvedText: fmt.Sprintf("PC %s runs the Syncthing version of the servers now.", dv.Name),
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
			Subject:      dv.Name,
			Message:      fmt.Sprintf("PC %s is offline for %s (last seen %s)", dv.Name, Ago(off), dv.LastSeen.In(loc).Format("2006-01-02 15:04")),
			Since:        dv.LastSeen,
			Related:      dv.SeenBy,
			Details:      []model.Detail{detail("Device ID", dv.ID), timeDetail("Last seen", dv.LastSeen), detail("Configured on", strings.Join(e.names(dv.SeenBy), ", "))},
			ResolvedText: fmt.Sprintf("PC %s is online again.", dv.Name),
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
