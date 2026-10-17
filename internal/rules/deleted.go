package rules

import (
	"fmt"

	"github.com/ivarstudios/syncwatch/internal/model"
)

// S8: a project folder outside Syncthing that a structure scan found
// deleted (model.DeletedDir). No other device has a copy of it. One finding
// for each folder; the folders that went with all the other directories of
// their share in one listing get one finding for the share, because a
// share that is not mounted now looks the same. The IDs use the paths as
// listed: the server's case rules are not known before the first contact
// after a start, and the finding must keep its ID over a restart.
func (e *evaluator) deleted(sv *model.Server) {
	if len(sv.Deleted) == 0 {
		return
	}
	whole := map[string][]*model.DeletedDir{} // by share path
	var shares []string
	for _, k := range sortedKeys(sv.Deleted) {
		d := sv.Deleted[k]
		if d.All {
			if whole[d.Share] == nil {
				shares = append(shares, d.Share)
			}
			whole[d.Share] = append(whole[d.Share], d)
			continue
		}
		details := []model.Detail{detail("Path", d.Path), detail("Share", d.ShareName), timeDetail("Detected", d.DetectedAt)}
		if d.Size != nil {
			details = append(details, detail("Last size", deletedSize(d.Size)), timeDetail("Size measured", d.Size.MeasuredAt))
		}
		e.add("S8", sv.ID, "deleted:"+d.Path, model.Finding{
			Subject: fmt.Sprintf("%s/%s on %s", d.ShareName, d.Name, sv.Name),
			Message: fmt.Sprintf("Project folder %s in share %s on %s is deleted. It was not in Syncthing: no other device has a copy. "+
				"If the deletion was a mistake, restore the folder from the recycle bin or a backup of the server. Else remove it from syncwatch on the Folders page (✕)",
				d.Name, d.ShareName, sv.Name),
			Since:        d.DetectedAt,
			Details:      details,
			ResolvedText: fmt.Sprintf("Project folder %s in share %s on %s is back, or an administrator removed it from syncwatch.", d.Name, d.ShareName, sv.Name),
		})
	}
	for _, share := range shares {
		list := whole[share]
		name, since := list[0].ShareName, list[0].DetectedAt
		details := []model.Detail{detail("Path", share)}
		for i, d := range list {
			if d.DetectedAt.Before(since) {
				since = d.DetectedAt
			}
			if i < maxListed {
				v := d.Name
				if d.Size != nil {
					v += " (" + deletedSize(d.Size) + ")"
				}
				details = append(details, detail("Deleted folder", v))
			}
		}
		if len(list) > maxListed {
			details = append(details, detail("More deleted folders", fmt.Sprint(len(list)-maxListed)))
		}
		details = append(details, timeDetail("Detected", since))
		e.add("S8", sv.ID, "share-empty:"+share, model.Finding{
			Subject: fmt.Sprintf("%s on %s", name, sv.Name),
			Message: fmt.Sprintf("Share %s on %s lists no directory now: %s outside Syncthing went in one scan. "+
				"Make sure that the share is mounted in the Syncthing container. If the folders were deleted by mistake, restore them from the recycle bin or a backup of the server. "+
				"Else remove them from syncwatch on the Folders page (✕)",
				name, sv.Name, Plural(len(list), "project folder", "project folders")),
			Since:        since,
			Details:      details,
			ResolvedText: fmt.Sprintf("The deleted project folders of share %s on %s are back, or an administrator removed them from syncwatch.", name, sv.Name),
		})
	}
}

// deletedSize is the last size of a deleted folder: "≥ " for a lower limit.
func deletedSize(sz *model.DirSize) string {
	t := Size(sz.Bytes)
	if !sz.Complete {
		t = "≥ " + t
	}
	return t + " in " + Plural(int(sz.Files), "file", "files")
}
