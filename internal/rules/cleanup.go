package rules

import (
	"fmt"
	"strings"
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
)

func (e *evaluator) cleanup() {
	e.inactiveProjects()
	e.oldDevices()
	e.deadShares()
}

// firstSeenBasis is the basis of C1 for a folder without a pulled file and
// without a local change that syncwatch saw.
const firstSeenBasis = "the time when syncwatch first saw the folder"

// C1: no file change for a long time, across every server holding the folder.
// The age of the finding counts from its opening (Since is zero): the time
// without a change is in the message.
func (e *evaluator) inactiveProjects() {
	type agg struct {
		label   string
		last    string
		basis   string
		servers []string
		ids     []string
		first   time.Time // the earliest time when syncwatch saw the folder on a server
	}
	latest := map[string]*agg{}
	lastAt := map[string]time.Time{}
	for _, sv := range e.Snapshot.Servers {
		if !sv.Loaded {
			continue
		}
		for _, f := range sv.Folders {
			a, ok := latest[f.ID]
			if !ok {
				a = &agg{label: f.DisplayName()}
				latest[f.ID] = a
			}
			a.servers = append(a.servers, sv.Name)
			a.ids = append(a.ids, sv.ID)
			// Syncthing's lastFile only counts files pulled from other
			// devices; local edits (over SMB, say) show up as events.
			if f.LastFileAt.After(lastAt[f.ID]) {
				lastAt[f.ID] = f.LastFileAt
				a.last, a.basis = f.LastFileName, "the last file Syncthing pulled from another device"
			}
			if f.LastLocalChange.After(lastAt[f.ID]) {
				lastAt[f.ID] = f.LastLocalChange
				a.last, a.basis = "", "the last local change that syncwatch saw"
			}
			if !f.FirstSeen.IsZero() && (a.first.IsZero() || f.FirstSeen.Before(a.first)) {
				a.first = f.FirstSeen
			}
		}
	}
	th := e.Config.Thresholds.InactiveProject.D()
	loc := e.Config.Location()
	for _, id := range sortedKeys(latest) {
		a := latest[id]
		last, ok := lastAt[id]
		if !ok {
			// A project that nobody changed after the copy: no pulled file
			// and no local change. The collector keeps the time when
			// syncwatch first saw the folder, also over a restart.
			if a.first.IsZero() {
				continue // unknown
			}
			last, a.basis = a.first, firstSeenBasis
		}
		idle := e.Now.Sub(last)
		if idle < th {
			continue
		}
		details := []model.Detail{detail("Folder ID", id), timeDetail("Last change", last), detail("Based on", a.basis)}
		if a.last != "" {
			details = append(details, detail("Last file", a.last))
		}
		details = append(details, detail("On servers", strings.Join(a.servers, ", ")))
		when := last.In(loc).Format("2006-01-02 15:04")
		msg := fmt.Sprintf("No file in folder %s changed in the last %s (%s was on %s). You can archive it", a.label, Ago(idle), a.basis, when)
		if a.basis == firstSeenBasis {
			msg = fmt.Sprintf("No file in folder %s changed in the last %s (syncwatch first saw the folder on %s). You can archive it", a.label, Ago(idle), when)
		}
		e.add("C1", "", "folder:"+id, model.Finding{
			Subject:      a.label,
			Message:      msg,
			Related:      a.ids,
			Details:      details,
			ResolvedText: fmt.Sprintf("Folder %s is not inactive now.", a.label),
		})
	}
}

// C2: a device not seen for a long time but still configured.
func (e *evaluator) oldDevices() {
	th := e.Config.Thresholds.OldDevice.D()
	for _, dv := range e.Snapshot.Devices() {
		if dv.IsServer || dv.Connected || len(dv.SeenBy) == 0 || !e.anyLoaded(dv.SeenBy) {
			continue
		}
		f := model.Finding{
			Subject:      dv.Name,
			Related:      dv.SeenBy,
			ResolvedText: fmt.Sprintf("Device %s is not inactive now.", dv.Name),
			Details:      []model.Detail{detail("Device ID", dv.ID), timeDetail("Last seen", dv.LastSeen), detail("Configured on", strings.Join(e.names(dv.SeenBy), ", "))},
		}
		if dv.LastSeen.IsZero() {
			// Opens once the device has been watched for the threshold.
			f.Message = fmt.Sprintf("Device %s never connected after syncwatch started to monitor it more than %s ago, but is still configured on %s. You can remove the device", dv.Name, Ago(th), strings.Join(e.names(dv.SeenBy), ", "))
			f.MinDuration = th
		} else {
			off := e.Now.Sub(dv.LastSeen)
			if off < th {
				continue
			}
			f.Message = fmt.Sprintf("Device %s is offline for %s but is still configured on %s. You can remove the device", dv.Name, Ago(off), strings.Join(e.names(dv.SeenBy), ", "))
			f.Since = dv.LastSeen
		}
		e.add("C2", "", "device:"+dv.ID, f)
	}
}

// C3: a folder shared to a device that never connected or doesn't share it back.
func (e *evaluator) deadShares() {
	th := e.Config.Thresholds.DeadShare.D()
	// A device no server has ever seen is C2's business; one C3 per folder
	// on top of it would only repeat that.
	neverSeen := map[string]bool{}
	if e.Config.CheckEnabled("C2") {
		for _, dv := range e.Snapshot.Devices() {
			if !dv.IsServer && !dv.Connected && dv.LastSeen.IsZero() {
				neverSeen[dv.ID] = true
			}
		}
	}
	for _, sv := range e.Snapshot.Servers {
		if !sv.Loaded {
			continue
		}
		for _, fid := range sortedFolderIDs(sv) {
			f := sv.Folders[fid]
			for _, dev := range f.SharedWith {
				d, ok := sv.Devices[dev]
				if !ok || neverSeen[dev] {
					continue
				}
				dn := e.devName(dev)
				var msg string
				if sv.RemoteState(fid, dev) == "notSharing" {
					msg = fmt.Sprintf("Folder %s is shared with %s, which does not share it back", f.DisplayName(), dn)
				} else if d.LastSeen.IsZero() && !e.connected(sv, dev) && !e.Snapshot.IsServerDevice(dev) {
					// A polled server that never connected is H1/H2's business.
					msg = fmt.Sprintf("Folder %s is shared with %s, which never connected to %s", f.DisplayName(), dn, sv.Name)
				} else {
					continue
				}
				e.add("C3", sv.ID, "dead:"+fid+":"+dev, model.Finding{
					Subject:      fmt.Sprintf("%s → %s", f.DisplayName(), dn),
					Message:      msg + ". You can remove the device from the folder",
					MinDuration:  th,
					Details:      []model.Detail{detail("Folder ID", fid), detail("Device ID", dev)},
					ResolvedText: fmt.Sprintf("Device %s uses folder %s, or the folder is not shared with it now.", dn, f.DisplayName()),
				})
			}
		}
	}
}

func (e *evaluator) connected(sv *model.Server, dev string) bool {
	c, ok := sv.Connections[dev]
	return ok && c.Connected
}
