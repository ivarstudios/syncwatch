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

// C1: no file change for a long time, across every server holding the folder.
func (e *evaluator) inactiveProjects() {
	type agg struct {
		label   string
		last    string
		servers []string
		ids     []string
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
			if f.LastFileAt.IsZero() {
				continue
			}
			if f.LastFileAt.After(lastAt[f.ID]) {
				lastAt[f.ID] = f.LastFileAt
				a.last = f.LastFileName
			}
		}
	}
	th := e.Config.Thresholds.InactiveProject.D()
	for _, id := range sortedKeys(latest) {
		last, ok := lastAt[id]
		if !ok {
			continue // no change recorded yet: unknown
		}
		idle := e.Now.Sub(last)
		if idle < th {
			continue
		}
		a := latest[id]
		e.add("C1", "", "folder:"+id, model.Finding{
			Subject: a.label,
			Message: fmt.Sprintf("No file in %s has changed for %s (last change %s); consider archiving it", a.label, Ago(idle), last.Format("2006-01-02")),
			Since:   last,
			Related: a.ids,
			Details: []model.Detail{
				detail("Folder ID", id), timeDetail("Last change", last), detail("Last file", a.last),
				detail("On servers", strings.Join(a.servers, ", ")),
			},
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
			Subject: dv.Name,
			Related: dv.SeenBy,
			Details: []model.Detail{detail("Device ID", dv.ID), timeDetail("Last seen", dv.LastSeen), detail("Configured on", strings.Join(e.names(dv.SeenBy), ", "))},
		}
		if dv.LastSeen.IsZero() {
			f.Message = fmt.Sprintf("%s has never connected but is still configured on %s; consider removing it", dv.Name, strings.Join(e.names(dv.SeenBy), ", "))
			f.MinDuration = th
		} else {
			off := e.Now.Sub(dv.LastSeen)
			if off < th {
				continue
			}
			f.Message = fmt.Sprintf("%s hasn't been seen for %s but is still configured on %s; consider removing it", dv.Name, Ago(off), strings.Join(e.names(dv.SeenBy), ", "))
			f.Since = dv.LastSeen
		}
		e.add("C2", "", "device:"+dv.ID, f)
	}
}

// C3: a folder shared to a device that never connected or doesn't share it back.
func (e *evaluator) deadShares() {
	th := e.Config.Thresholds.DeadShare.D()
	for _, sv := range e.Snapshot.Servers {
		if !sv.Loaded {
			continue
		}
		for _, fid := range sortedFolderIDs(sv) {
			f := sv.Folders[fid]
			for _, dev := range f.SharedWith {
				d, ok := sv.Devices[dev]
				if !ok {
					continue
				}
				dn := e.devName(dev)
				var msg string
				if c := f.Completion[dev]; c != nil && c.RemoteState == "notSharing" {
					msg = fmt.Sprintf("%s is shared with %s, which doesn't share it back", f.DisplayName(), dn)
				} else if d.LastSeen.IsZero() && !e.connected(sv, dev) {
					msg = fmt.Sprintf("%s is shared with %s, which has never connected to %s", f.DisplayName(), dn, sv.Name)
				} else {
					continue
				}
				e.add("C3", sv.ID, "dead:"+fid+":"+dev, model.Finding{
					Subject:     fmt.Sprintf("%s → %s", f.DisplayName(), dn),
					Message:     msg + "; consider unsharing it",
					MinDuration: th,
					Details:     []model.Detail{detail("Folder ID", fid), detail("Device ID", dev)},
				})
			}
		}
	}
}

func (e *evaluator) connected(sv *model.Server, dev string) bool {
	c, ok := sv.Connections[dev]
	return ok && c.Connected
}
