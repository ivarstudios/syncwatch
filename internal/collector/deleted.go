package collector

// Deleted project folders (S8). A project folder outside Syncthing has no
// copy on another device: when a person deletes it by mistake, only the
// recycle bin or a backup of the server has it. The structure scan compares
// each listing of a share with the listing before. A project folder outside
// Syncthing that the listing before had and the new listing does not have
// is deleted. It stays deleted, also over a restart, until an
// administrator removes it from syncwatch (the Folders page) or a listing
// has it again.

import (
	"errors"
	"slices"
	"sort"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/pathx"
	"github.com/ivarstudios/syncwatch/internal/rules"
	"github.com/ivarstudios/syncwatch/internal/store"
)

// ErrNotDeleted: an administrator removes a folder that is not marked as
// deleted (any more).
var ErrNotDeleted = errors.New("the folder is not marked as deleted now: reload the page")

// noteListings compares the listings of the shares that a scan listed (at)
// with the listings before, and keeps them, also in the store. It returns
// the project folders outside Syncthing that the listings found deleted,
// and the paths of the deleted folders that a listing has again. A share
// that the scan cannot list changes nothing. A share that lists no
// directory where it listed 2 or more marks its folders with All. The
// caller writes the result to the hub. Only the scan calls it, and one scan
// runs at a time (scanMu orders them), so c.known needs no lock.
func (c *Collector) noteListings(st pathx.Style, cs *config.CompiledStructure, shares []*model.Share, at time.Time) (gone []*model.DeletedDir, back []string) {
	var marked map[string]bool
	var sizes map[string]model.DirSize
	c.hub.View(c.id, func(s *model.Server) {
		marked = map[string]bool{}
		for p := range s.Deleted {
			marked[p] = true
		}
		if s.Structure != nil {
			sizes = map[string]model.DirSize{}
			for p, sz := range s.Structure.Sizes {
				sizes[p] = *sz
			}
		}
	})
	if c.known == nil {
		known, err := c.st.ShareListings(c.id)
		if err != nil {
			c.log.Warn("cannot read the last listings of the shares: the scans find deleted folders from this scan on", "err", shortErr(err))
			known = map[string]store.ShareListing{}
		}
		c.known = known
	}

	changed := map[string]store.ShareListing{}
	listed := map[string]bool{} // nestedKey of each directory that a share lists now
	for _, sh := range shares {
		if sh.Error != "" {
			continue
		}
		now := store.ShareListing{Dirs: len(sh.Dirs)}
		names := map[string]bool{}
		for _, d := range sh.Dirs {
			k := nestedKey(st, d.Path)
			names[k], listed[k] = true, true
			if rules.NotInSyncthing(cs, d) {
				now.Unsynced = append(now.Unsynced, d.Path)
			}
		}
		prev, ok := c.known[sh.Path]
		if ok {
			all := len(sh.Dirs) == 0 && prev.Dirs >= 2
			for _, p := range prev.Unsynced {
				if names[nestedKey(st, p)] || marked[p] {
					continue
				}
				d := &model.DeletedDir{Path: p, Name: st.Base(p), Share: sh.Path, ShareName: sh.Name, DetectedAt: at, All: all}
				if sz, ok := sizes[p]; ok && !sz.MeasuredAt.IsZero() {
					d.Size = &model.DirSize{Bytes: sz.Bytes, Files: sz.Files, Complete: sz.Complete, MeasuredAt: sz.MeasuredAt, Took: sz.Took}
				}
				gone = append(gone, d)
			}
		}
		if !ok || prev.Dirs != now.Dirs || !slices.Equal(prev.Unsynced, now.Unsynced) {
			changed[sh.Path] = now
			c.known[sh.Path] = now
		}
	}
	for p := range marked {
		if listed[nestedKey(st, p)] {
			back = append(back, p)
		}
	}
	sort.Strings(back)
	if err := c.st.SaveListings(c.id, changed, gone, back); err != nil {
		// The hub still has them: a restart loses them, and the first scan
		// after it compares with the listings that the store has.
		c.log.Error("cannot save the listings of the shares", "err", err)
	}
	for _, d := range gone {
		c.log.Warn("project folder outside Syncthing deleted", "path", d.Path, "share", d.ShareName, "whole_share", d.All)
	}
	for _, p := range back {
		c.log.Info("deleted project folder is back", "path", p)
	}
	return gone, back
}

// ForgetDeleted removes a deleted project folder of a server from
// syncwatch, for an administrator: its S8 finding resolves. It changes
// nothing on the server, and needs no running collector.
func (m *Manager) ForgetDeleted(serverID, path string) error {
	found, err := m.st.RemoveDeletedDir(serverID, path)
	if err != nil {
		return err
	}
	m.hub.Update(serverID, func(sv *model.Server) {
		if sv.Deleted[path] != nil {
			found = true
			delete(sv.Deleted, path)
		}
	})
	if !found {
		return ErrNotDeleted
	}
	m.log.Info("deleted project folder removed from syncwatch", "server", serverID, "path", path)
	return nil
}
