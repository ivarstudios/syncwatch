package collector

import (
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/store"
)

// The store keeps, for each folder of a server, when syncwatch saw it
// first and its newest local change: C1 needs both, also after a
// restart of syncwatch.

// folderActivity records the folders of a full check in the store and
// returns what the store has about them. A store error costs only these
// times: the full check goes on.
func (c *Collector) folderActivity(ids []string, now time.Time) map[string]store.FolderActivity {
	if err := c.st.NoteFoldersSeen(c.id, ids, now); err != nil {
		c.log.Warn("cannot save the folders that syncwatch saw", "err", err)
	}
	act, err := c.st.FolderActivity(c.id)
	if err != nil {
		c.log.Warn("cannot read the folder activity", "err", err)
		return nil
	}
	return act
}

// localSaveEvery is the shortest time between 2 writes of the local change
// time of one folder.
var localSaveEvery = time.Minute

// setLastLocalChange writes the newest local change of a folder to the
// store. A variable for the tests.
var setLastLocalChange = (*store.Store).SetLastLocalChange

// saveLocalChange writes at, the newest local change of a folder, to the
// store at most once every localSaveEvery for each folder: a burst of
// 1,000 local changes is 1 write. A change that waits is written by
// flushLocalChanges. Only the session goroutine calls it.
func (c *Collector) saveLocalChange(folder string, at time.Time) {
	if at.IsZero() {
		return
	}
	if c.localDue == nil {
		c.localDue = map[string]bool{}
	}
	if last, ok := c.localSaved[folder]; ok && time.Since(last) < localSaveEvery {
		c.localDue[folder] = true
		return
	}
	c.writeLocalChange(folder, at)
}

// flushLocalChanges writes the local changes that wait for the store: the
// folders whose last write is localSaveEvery ago or more, or every folder
// when all is true (a full check, the stop of the collector). It writes
// the newest time that the hub has.
func (c *Collector) flushLocalChanges(all bool) {
	for folder := range c.localDue {
		if !all && time.Since(c.localSaved[folder]) < localSaveEvery {
			continue
		}
		var at time.Time
		c.hub.View(c.id, func(s *model.Server) {
			if f := s.Folders[folder]; f != nil {
				at = f.LastLocalChange
			}
		})
		if at.IsZero() {
			delete(c.localDue, folder) // the folder is gone
			continue
		}
		c.writeLocalChange(folder, at)
	}
}

// writeLocalChange writes one local change time. A folder whose write
// fails stays due.
func (c *Collector) writeLocalChange(folder string, at time.Time) {
	if c.localSaved == nil {
		c.localSaved = map[string]time.Time{}
	}
	c.localSaved[folder] = time.Now()
	if err := setLastLocalChange(c.st, c.id, folder, at); err != nil {
		c.log.Warn("cannot save the time of the last local change", "folder", folder, "err", err)
		if c.localDue == nil {
			c.localDue = map[string]bool{}
		}
		c.localDue[folder] = true
		return
	}
	delete(c.localDue, folder)
}
