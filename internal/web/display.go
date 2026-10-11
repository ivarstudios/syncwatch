package web

import (
	"fmt"
	"strings"
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
)

// What the dashboard shows of finding details, names and counts. The JSON
// API keeps the raw values: details[].key and details[].value are machine
// values, only the HTML shows labels and local times.

// detailLabels maps the key of a finding detail to the label that the
// dashboard shows. The keys of 78ef2d8 are the machine values of
// /api/v1/status; the labels follow the text style. A key that is already
// a label, or that is not in the map, shows as it is.
var detailLabels = map[string]string{
	"Watch error":   "File watcher error",
	"Failing items": "Failed items",
	"Items left":    "Remaining items",
	"Bytes left":    "Remaining bytes",
	"Type":          "Connection type",
	"URL":           "API URL",
	"Down since":    "Down from",
	"First offered": "Last request", // H9: Syncthing gives the time of the latest request
}

func detailLabel(key string) string {
	if l, ok := detailLabels[key]; ok {
		return l
	}
	return key
}

// detailRow is one line of a finding's details on the Findings page.
type detailRow struct {
	Label, Value string
}

// deviceIDKeys are the detail keys whose values are device IDs (C4).
var deviceIDKeys = map[string]bool{"Device ID": true, "Offered by": true}

// shortDeviceID is the form of a device ID that viewers see (C12, W-048):
// the first group and "…". Viewers cannot use the full ID, and the pages
// show less of the studio network.
func shortDeviceID(id string) string {
	if sh := model.ShortID(id); sh != id {
		return sh + "…"
	}
	return id
}

// viewerDetails returns the details with the device IDs in their short form.
func viewerDetails(ds []model.Detail) []model.Detail {
	out := make([]model.Detail, len(ds))
	for i, d := range ds {
		if deviceIDKeys[d.Key] {
			d.Value = shortDeviceID(d.Value)
		}
		out[i] = d
	}
	return out
}

// detailRows gives the details that have a value, with their labels. A time
// (UTC, RFC 3339 in the finding) shows in the configured time zone.
func (s *Server) detailRows(ds []model.Detail) []detailRow {
	var out []detailRow
	for _, d := range ds {
		if d.Value == "" {
			continue
		}
		v := d.Value
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			v = s.fmtTime(t)
		}
		out = append(out, detailRow{Label: detailLabel(d.Key), Value: showControls(v)})
	}
	return out
}

// showControls makes the invisible characters that change the direction of
// text visible, for example U+202E as ‹U+202E›. In a file, folder or device
// name they could show the rest of a line in reverse ("gnp.exe" as
// "exe.png"). The zero-width joiners stay: names in some scripts and emoji
// need them.
func showControls(s string) string {
	if !strings.ContainsFunc(s, hiddenControl) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		if hiddenControl(r) {
			fmt.Fprintf(&b, "‹U+%04X›", r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func hiddenControl(r rune) bool {
	switch {
	case r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069: // embeddings, overrides, isolates
		return true
	case r == 0x200E, r == 0x200F, r == 0x061C: // direction marks
		return true
	case r == 0x200B, r == 0xFEFF: // zero-width space, byte order mark
		return true
	}
	return false
}

// showAll applies showControls to each string.
func showAll(list []string) []string {
	if list == nil {
		return nil
	}
	out := make([]string, len(list))
	for i, s := range list {
		out[i] = showControls(s)
	}
	return out
}

// folderStateKnown reports whether a server's folder states are current:
// the server is up and finished a full check. Otherwise the tile, the grid
// and the status API show the last known state, marked as such.
func folderStateKnown(sv *model.Server) bool {
	return sv.Status == model.StatusUp && sv.Loaded
}

// folderCounts are a server's folders by state. The overview tile and the
// status API count with it, so they always agree.
type folderCounts struct {
	Total, Idle, Syncing, Scanning, Errors, WatchErrors, Paused int
}

func countFolders(sv *model.Server) folderCounts {
	var c folderCounts
	for _, f := range sv.Folders {
		c.Total++
		switch f.FolderSync() {
		case model.SyncPaused:
			c.Paused++
		case model.SyncError:
			c.Errors++
		case model.SyncWatchError:
			c.WatchErrors++
		case model.SyncScanning:
			c.Scanning++
		case model.SyncUpToDate:
			c.Idle++
		default:
			c.Syncing++
		}
	}
	return c
}

// staleWhy says in a few words why a server's folder state is not current.
func staleWhy(sv *model.Server) string {
	switch sv.Status {
	case model.StatusDown:
		return "unreachable"
	case model.StatusCertChanged:
		return "certificate changed"
	case model.StatusUp:
		return "full check not complete"
	}
	return "not contacted"
}

// pcCounts are the PCs of a snapshot for the PCs tile and the status API.
type pcCounts struct {
	Total, Online, Offline, Unknown int
}

// countPCs counts the PCs. A PC that is not connected counts as offline
// (after the pc_offline threshold) only when every server that knows it is
// up and finished a full check. When one of them is not, the PC can be
// connected to that server, and it counts as unknown.
func countPCs(snap *model.Snapshot, offlineAfter time.Duration, now time.Time) pcCounts {
	var c pcCounts
	if snap == nil {
		return c
	}
	for _, d := range snap.Devices() {
		if d.IsServer {
			continue
		}
		c.Total++
		switch {
		case d.Connected:
			c.Online++
		case !d.LastSeen.IsZero() && now.Sub(d.LastSeen) <= offlineAfter:
			// Not connected for a short time: not offline yet.
		case serversKnown(snap, d.SeenBy):
			c.Offline++
		default:
			c.Unknown++
		}
	}
	return c
}

// serversKnown reports whether every server in ids is up and finished a
// full check.
func serversKnown(snap *model.Snapshot, ids []string) bool {
	for _, id := range ids {
		if sv := snap.Server(id); sv == nil || !folderStateKnown(sv) {
			return false
		}
	}
	return true
}
