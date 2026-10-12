package collector

import (
	"context"
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/stclient"
)

// needPerPage is how many names of the files that a device still needs the
// full check keeps for each folder and device (H5, W-058). needPage is how
// many entries it reads for them: Syncthing's list also has the deletions
// that the device did not do yet, which are not names that it needs (see
// needList). The gate permits at most this page (internal/gate).
const (
	needPerPage = 20
	needPage    = 100
)

// needList reads the first names of the files that a device still needs
// from a folder; items is the device's number of need items. Syncthing
// counts the deletions in needDeletes, not in the need items: the names
// have no deletions either. A failure costs only the names: the full check
// goes on and the folder gets no error. A gate from before
// /rest/db/remoteneed refuses it with 404: the session then asks no more,
// with one INFO line.
func (c *Collector) needList(ctx context.Context, folder, dev string, items int64, at time.Time) (model.NeedList, bool) {
	if c.needRefused.Load() {
		return model.NeedList{}, false
	}
	rn, err := c.client.RemoteNeed(ctx, folder, dev, needPage)
	if err != nil {
		switch {
		case stclient.GateRefusedEndpoint(err):
			if !c.needRefused.Swap(true) {
				c.log.Info("the gate does not permit /rest/db/remoteneed: update the gate to see the needed files in H5")
			}
		case ctx.Err() == nil:
			c.log.Debug("cannot read the files that a device needs", "folder", folder, "device", model.ShortID(dev), "err", shortErr(err))
		}
		return model.NeedList{}, false
	}
	nl := model.NeedList{Items: items, At: at}
	for _, f := range rn.Files {
		if len(nl.Names) == needPerPage {
			break
		}
		if !f.Deleted {
			nl.Names = append(nl.Names, f.Name)
		}
	}
	return nl, true
}

// pruneProgress drops the progress samples of the folders and devices that
// the server no longer has (a full check read folders).
func pruneProgress(s *model.Server, folders map[string]*model.Folder) {
	for id, devs := range s.Progress {
		f := folders[id]
		if f == nil {
			delete(s.Progress, id)
			continue
		}
		for dev := range devs {
			if dev != "" && !f.SharedWithDevice(dev) {
				delete(devs, dev)
			}
		}
	}
}
