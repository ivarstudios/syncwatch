package web

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/engine"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/notify"
	"github.com/ivarstudios/syncwatch/internal/pathx"
	"github.com/ivarstudios/syncwatch/internal/rules"
	"github.com/ivarstudios/syncwatch/internal/settings"
	"github.com/ivarstudios/syncwatch/internal/stclient"
	"github.com/ivarstudios/syncwatch/internal/store"
)

type problemRow struct {
	ID, Check, Checks, Severity, SeverityLabel, Icon, Category string
	Server, ServerName, Subject, Message, Title                string
	Urgent, Stale, Snoozed, Ignored                            bool
	SnoozeLabel, OpenFor, Since, OpenedAt, Pinged              string
	Details                                                    []detailRow
	Paths                                                      []string
	Link                                                       string
	Sites                                                      []string // the sites of its servers, for the site filter of the Overview (findingSites)
	Over                                                       bool     // past the limit of the Overview list until the site filter selects it
	Scan                                                       lastScan // structure findings: the last structure scan of the server
	ScanServer                                                 string   // structure findings: the server that "Scan now" scans
}

func (s *Server) fmtTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.In(s.cfg.Get().Location()).Format("2006-01-02 15:04")
}

// fmtClock writes a time of today (in the configured zone) as 21:52, and
// another time as fmtTime does. The rule of the gap line: the date shows
// only when it is not today.
func (s *Server) fmtClock(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	loc := s.cfg.Get().Location()
	if lt := t.In(loc); lt.Format("2006-01-02") == s.now().In(loc).Format("2006-01-02") {
		return lt.Format("15:04")
	}
	return s.fmtTime(t)
}

// problemRows builds the rows of findings. For a viewer (admin false) the
// device IDs in the details are short and there is no link to Syncthing.
func (s *Server) problemRows(v *engine.View, recs []*store.Record, admin bool) []problemRow {
	now := s.now()
	var out []problemRow
	for _, r := range recs {
		row := problemRow{
			ID: r.ID, Check: r.Check, Checks: strings.Join(r.Checks, "+"), Severity: r.Severity.Lower(),
			SeverityLabel: r.Severity.Label(), Icon: notify.Icon(r.Severity), Category: r.Category,
			Server: r.Server, Subject: showControls(r.Subject), Message: showControls(r.Message), Urgent: r.Urgent, Stale: r.Stale,
			Paths: showAll(r.Paths), Title: config.CatalogByID[r.Check].Title,
		}
		if admin {
			row.Details, row.Link = s.detailRows(r.Details), r.Link
		} else {
			row.Details = s.detailRows(viewerDetails(r.Details))
		}
		if sv := v.Snapshot.Server(r.Server); sv != nil {
			row.ServerName = showControls(sv.Name)
			if r.Category == model.CatStructure {
				row.Scan, row.ScanServer = s.lastScanOf(sv, &r.Finding), sv.ID
			}
		}
		since := r.Since
		if since.IsZero() || since.After(r.OpenedAt) {
			since = r.OpenedAt
		}
		row.OpenFor = rules.Ago(now.Sub(since))
		row.Since = s.fmtTime(since)
		row.OpenedAt = s.fmtTime(r.OpenedAt)
		if !r.PingedAt.IsZero() {
			row.Pinged = s.fmtTime(r.PingedAt)
		}
		if z, ok := v.Snoozed(r.ID, now); ok {
			row.Snoozed = true
			if z.Kind == store.IgnoreKind {
				row.Ignored = true
				row.SnoozeLabel = "ignored: " + showControls(z.Reason)
			} else {
				row.SnoozeLabel = "snoozed until " + s.fmtTime(z.Until)
				if z.Reason != "" {
					row.SnoozeLabel += " (" + showControls(z.Reason) + ")"
				}
			}
		}
		out = append(out, row)
	}
	return out
}

// Overview

type serverTile struct {
	ID, Name, Status, StatusLabel, StatusIcon, Version, Uptime, Link        string
	Folders, Idle, Syncing, Scanning, Errors, WatchErrors, Paused, Problems int
	CertExpires                                                             string
	CertWarn                                                                bool
	LastContact, Mode, LastError                                            string
	ShareErrors                                                             []string
	Shares                                                                  int
	LastScan, ScanCost                                                      string
	FoldersStale                                                            bool   // the folder counts are the last known state
	NoContact                                                               bool   // never contacted: no folder state at all
	Disabled                                                                bool   // in the configuration, but syncwatch does not poll it
	Site                                                                    string // servers[].site, "" when not set
}

type overviewVM struct {
	Health                   string
	Servers                  []serverTile
	Links                    []linkRow // the Connections section
	Sites                    []string  // the choices of the site filter; nil with fewer than 2 sites
	PCTotal, PCOnline, PCOff int
	PCUnknown                int // not connected, but a server that knows the PC is not up: maybe online
	PCOffLabel               string
	Top                      []problemRow
	TopLimit                 int // the rows that the list shows; with the site filter, Top has more (Over)
	OpenCount, UrgentOpen    int
	SnoozedCount             int // open findings hidden by a snooze or ignore
	Evaluated                string
	Day                      string // today in the configured zone: times of today show without the date
}

// topLimit is the number of findings in the Overview list.
const topLimit = 8

// filterSites returns the sites of the configured servers, in the order of
// their first server, when 2 or more are set: the choices of the site filter.
// With fewer sites there is no filter, and it returns nil.
func filterSites(cfg *config.Config) []string {
	var out []string
	for _, sc := range cfg.Servers {
		if sc.Site != "" && !slices.Contains(out, sc.Site) {
			out = append(out, sc.Site)
		}
	}
	if len(out) < 2 {
		return nil
	}
	return out
}

// siteList adds a site to a list of sites, one time each.
func siteList(list []string, site string) []string {
	if slices.Contains(list, site) {
		return list
	}
	return append(list, site)
}

// findingSites gives the sites of the servers of a finding: its server and
// the related servers ("" for a server without a site). The site filter
// shows the finding with each of these sites; a finding between the servers
// of 2 sites shows with both. A finding without a server (nil) always shows.
func findingSites(cfg *config.Config, r *store.Record) []string {
	var out []string
	if r.Server != "" {
		out = siteList(out, cfg.SiteOf(r.Server))
	}
	for _, id := range r.Related {
		out = siteList(out, cfg.SiteOf(id))
	}
	return out
}

// siteMatch reports whether the site filter shows an element with these
// sites when the site is selected.
func siteMatch(sites []string, site string) bool {
	return len(sites) == 0 || slices.Contains(sites, site)
}

// linkRow is one row of the Connections section of the Overview: the
// connection between 2 sites, or between 2 servers when fewer than 2 sites
// are set.
type linkRow struct {
	A, B  string   // the names of the 2 sites or servers
	Sites []string // the sites of the 2 ends, for the site filter
	Kind  string   // model.LinkConnected, LinkDisconnected or LinkUnreachable
	Class string   // ok (connected), err (not connected), off (unknown)
	Icon  string
	State string // "connected from 08:12", "not connected from 21:04", "state unknown: SOL is unreachable"
}

// Text is the row in one line, as the lobby page shows it:
// "IVAR Office ↔ Branch office: connected from 08:12".
func (r linkRow) Text() string { return r.A + " ↔ " + r.B + ": " + r.State }

// linkRows gives the rows of the Connections section: one row for each site
// link (2 or more sites), else one for each server link. An empty list (one
// server, or links not computed) gives no section.
func (s *Server) linkRows(v *engine.View) []linkRow {
	snap := v.Snapshot
	if snap == nil {
		return nil
	}
	cfg := s.cfg.Get()
	var out []linkRow
	if len(snap.SiteLinks) > 0 {
		for _, l := range snap.SiteLinks {
			var ids []string
			for _, p := range l.Servers {
				ids = append(ids, p[0], p[1])
			}
			row := s.linkState(snap, l.State, l.Since, ids)
			row.A, row.B, row.Sites = showControls(l.A), showControls(l.B), siteList([]string{l.A}, l.B)
			out = append(out, row)
		}
		return out
	}
	name := func(id string) string {
		if sv := snap.Server(id); sv != nil {
			return showControls(sv.Name)
		}
		if sc, ok := cfg.ServerByKey(id); ok {
			return showControls(sc.Name)
		}
		return showControls(id)
	}
	for _, l := range snap.Links {
		row := s.linkState(snap, l.State, l.Since, []string{l.A, l.B})
		row.A, row.B, row.Sites = name(l.A), name(l.B), siteList([]string{cfg.SiteOf(l.A)}, cfg.SiteOf(l.B))
		out = append(out, row)
	}
	return out
}

// linkState gives the state part of a Connections row. For a link whose
// state is not known it names the servers of the link (ids) that are not up
// or did not finish a full check.
func (s *Server) linkState(snap *model.Snapshot, state string, since time.Time, ids []string) linkRow {
	from := ""
	if !since.IsZero() {
		from = " from " + s.fmtClock(since)
	}
	switch state {
	case model.LinkConnected:
		return linkRow{Kind: model.LinkConnected, Class: "ok", Icon: "✔", State: "connected" + from}
	case model.LinkDisconnected:
		return linkRow{Kind: model.LinkDisconnected, Class: "err", Icon: "✖", State: "not connected" + from}
	}
	var why []string
	var seen []string
	for _, id := range ids {
		sv := snap.Server(id)
		if sv == nil || folderStateKnown(sv) || slices.Contains(seen, id) {
			continue
		}
		seen = append(seen, id)
		why = append(why, linkUnknownWhy(sv))
	}
	row := linkRow{Kind: model.LinkUnreachable, Class: "off", Icon: "?", State: "state unknown"}
	if len(why) > 0 {
		row.State += ": " + strings.Join(why, " and ")
	}
	return row
}

// linkUnknownWhy says why syncwatch does not know the connections of a
// server: the server is not up, or its full check is not complete.
func linkUnknownWhy(sv *model.Server) string {
	name := showControls(sv.Name)
	switch sv.Status {
	case model.StatusDown:
		return name + " is unreachable"
	case model.StatusCertChanged:
		return "the certificate of " + name + " changed"
	case model.StatusUp:
		return "the full check of " + name + " is not complete"
	}
	return "syncwatch has no contact with " + name
}

func statusLabel(st model.ServerStatus) (label, icon, class string) {
	switch st {
	case model.StatusUp:
		return "Up", "✔", "ok"
	case model.StatusDown:
		return "Unreachable", "✖", "err"
	case model.StatusCertChanged:
		return "Certificate changed", "⚠", "err"
	default:
		return "Connecting…", "…", "wait"
	}
}

// topFindings picks the findings of the Overview list: the first topLimit
// of recs. With the site filter (sites set) it adds the first topLimit that
// each site shows (siteMatch of findingSites), so that the list is complete
// for each site. first has the findings that the list shows without a
// filter.
func topFindings(cfg *config.Config, recs []*store.Record, sites []string) (top []*store.Record, first map[string]bool) {
	keep := map[string]bool{}
	take := func(match func(r *store.Record) bool) {
		n := 0
		for _, r := range recs {
			if n == topLimit {
				return
			}
			if match(r) {
				keep[r.ID] = true
				n++
			}
		}
	}
	take(func(*store.Record) bool { return true })
	first = maps.Clone(keep)
	for _, site := range sites {
		take(func(r *store.Record) bool { return siteMatch(findingSites(cfg, r), site) })
	}
	for _, r := range recs {
		if keep[r.ID] {
			top = append(top, r)
		}
	}
	return top, first
}

func (s *Server) overview(v *engine.View) overviewVM {
	cfg := s.cfg.Get()
	now := s.now()
	vm := overviewVM{Health: showControls(notify.HealthLine(cfg, v, now)), Evaluated: s.fmtTime(v.Changed),
		Links: s.linkRows(v), Sites: filterSites(cfg), TopLimit: topLimit, Day: now.In(cfg.Location()).Format("2006-01-02")}
	perServer := map[string]int{}
	var visible []*store.Record
	for _, r := range v.Open {
		if _, ok := v.Snoozed(r.ID, now); ok {
			vm.SnoozedCount++
			continue
		}
		visible = append(visible, r)
		perServer[r.Server]++
		if r.Urgent {
			vm.UrgentOpen++
		}
	}
	vm.OpenCount = len(visible)
	top, first := topFindings(cfg, visible, vm.Sites)
	// The list shows no details and no links: the viewer form is enough
	// for every role.
	vm.Top = s.problemRows(v, top, false)
	for i, r := range top {
		vm.Top[i].Over, vm.Top[i].Sites = !first[r.ID], findingSites(cfg, r)
	}
	for _, sv := range v.Snapshot.Servers {
		label, icon, class := statusLabel(sv.Status)
		t := serverTile{ID: sv.ID, Name: showControls(sv.Name), Status: class, StatusLabel: label, StatusIcon: icon,
			Version: sv.Version, Link: sv.GUIURL, Problems: perServer[sv.ID], Mode: sv.Mode,
			LastContact: s.fmtTime(sv.LastContact), LastError: showControls(sv.LastError), Site: cfg.SiteOf(sv.ID)}
		if !sv.StartTime.IsZero() && sv.Status == model.StatusUp {
			t.Uptime = rules.Ago(now.Sub(sv.StartTime))
		}
		c := countFolders(sv)
		t.Folders, t.Idle, t.Syncing, t.Scanning, t.Errors, t.WatchErrors, t.Paused = c.Total, c.Idle, c.Syncing, c.Scanning, c.Errors, c.WatchErrors, c.Paused
		t.FoldersStale = !folderStateKnown(sv)
		t.NoContact = t.FoldersStale && sv.LastContact.IsZero()
		if !sv.Cert.NotAfter.IsZero() {
			t.CertExpires = sv.Cert.NotAfter.In(cfg.Location()).Format("2006-01-02")
			t.CertWarn = sv.Cert.NotAfter.Sub(now) < 30*24*time.Hour
		}
		if sv.Structure != nil {
			t.Shares = len(sv.Structure.Shares)
			// The end of the last scan that listed a share, as on the
			// structure findings. Empty before the first one.
			if !sv.LastStructureScan.IsZero() {
				t.LastScan = s.fmtClock(sv.LastStructureScan)
			}
			if n := sv.Structure.ScanListings; n > 0 {
				t.ScanCost = fmt.Sprintf("%s in %s", plural(n, "directory listing", "directory listings"), sv.Structure.ScanTook.Round(100*time.Millisecond))
			}
			for _, sh := range sv.Structure.Shares {
				if sh.Error != "" {
					t.ShareErrors = append(t.ShareErrors, showControls(sh.Name+": "+sh.Error))
				}
			}
		}
		vm.Servers = append(vm.Servers, t)
	}
	// A disabled server keeps a grey tile, so it does not disappear without a trace.
	for _, sc := range cfg.Servers {
		if sc.Disabled && v.Snapshot.Server(sc.ID) == nil {
			vm.Servers = append(vm.Servers, serverTile{ID: sc.ID, Name: showControls(sc.Name), Status: "wait", StatusLabel: "Disabled", StatusIcon: "○", Disabled: true, Site: sc.Site})
		}
	}
	pcs := countPCs(v.Snapshot, cfg.Thresholds.PCOffline.D(), now)
	vm.PCTotal, vm.PCOnline, vm.PCOff, vm.PCUnknown = pcs.Total, pcs.Online, pcs.Offline, pcs.Unknown
	vm.PCOffLabel = rules.Ago(cfg.Thresholds.PCOffline.D())
	return vm
}

// Grid

type gridDevice struct {
	ID, Name, Short string
	Server, Online  bool
	Site            string // the site of a server column, for the site filter
}

type gridCell struct {
	Class, Icon, Text, Title string
	Problem                  bool
	Measure                  *measureTarget // a folder on a disk that an administrator can measure now (⟳ next to the box)
}

type gridRow struct {
	ID, Label, Search, Servers string
	Anchor                     string // the id of the row on the page, for the way back after Measure
	Problem                    bool
	Stale                      bool // a server of the folder shows only its last known state
	DiskOnly                   bool // a project folder that is on the disk of a server and in no Syncthing folder
	OnDisk                     bool // a server has the folder on its disk, not in Syncthing (a grey cell)
	Cells                      []gridCell
}

// measureTarget is a folder on the disk of one server that the ⟳ button
// next to its grey box asks the gate of that server to measure. Back is the
// way back to the row.
type measureTarget struct{ Server, ServerName, Path, Folder, Back string }

type gridGroup struct {
	Name           string
	Problems, Rows int
	Stale          int // rows with the last known state of a server
	OnDisk         int // rows with a folder on the disk of a server that is not in Syncthing
	Open           bool
	Items          []gridRow
}

// diskDir is a project folder on the disk of a server that no Syncthing
// folder holds (rules.NotInSyncthing), with its size from the gate.
type diskDir struct {
	share, path string
	size        *model.DirSize
	noSizes     bool // the server has no gate that measures sizes
}

type deviceItem struct {
	Folder string
	Cell   gridCell
}

type deviceList struct {
	Device   gridDevice
	Items    []deviceItem
	Problems int
}

type gridVM struct {
	Devices []gridDevice
	Groups  []gridGroup
	Lists   []deviceList
	Folders int
	Sites   []string // the choices of the site filter; nil with fewer than 2 sites
}

// groupName strips a leading "<server name>-" from a share name, so
// AKKA-LIVE-1SOURCE and SOL-LIVE-1SOURCE end up in the same group.
func groupName(server, share string) string {
	for _, sep := range []string{"-", "_", " "} {
		p := server + sep
		if len(share) > len(p) && strings.EqualFold(share[:len(p)], p) {
			return share[len(p):]
		}
	}
	return share
}

func pct(p float64) string {
	if p >= 100 {
		return "100%"
	}
	if p > 99 {
		return "99%"
	}
	return fmt.Sprintf("%.0f%%", p)
}

func serverCell(f *model.Folder) gridCell {
	have := progress(f.LocalPercent(), f.GlobalBytes, f.NeedBytes)
	switch f.FolderSync() {
	case model.SyncPaused:
		return gridCell{Class: "paused", Icon: "⏸", Text: "paused", Title: "Paused on the server. " + have, Problem: true}
	case model.SyncError:
		msg := f.Error
		if msg == "" {
			msg = plural(max(f.Errors, f.PullErrors), "failed item", "failed items")
		}
		return gridCell{Class: "err", Icon: "✖", Text: "error", Title: msg + ". " + have, Problem: true}
	case model.SyncWatchError:
		return gridCell{Class: "warn", Icon: "⚠", Text: "watcher error", Title: "file watcher error: " + f.WatchError + ". " + have, Problem: true}
	case model.SyncScanning:
		return gridCell{Class: "sync", Icon: "⟳", Text: rules.Size(haveBytes(f.GlobalBytes, f.NeedBytes)), Title: "Scanning. " + have}
	case model.SyncUpToDate:
		return gridCell{Class: "ok", Icon: "✔", Text: rules.Size(f.GlobalBytes), Title: "Up to date. " + progress(100, f.GlobalBytes, 0)}
	default:
		return gridCell{Class: "sync", Icon: "⟳", Text: rules.Size(haveBytes(f.GlobalBytes, f.NeedBytes)),
			Title: fmt.Sprintf("%s. %s, %s", f.State, have, plural(int(f.NeedItems), "remaining item", "remaining items"))}
	}
}

// haveBytes is what a device has of a folder of global bytes, with need
// bytes still to get.
func haveBytes(global, need int64) int64 { return max(global-need, 0) }

// progress writes the part of a folder that a device has, for a tooltip:
// "62%: 120 GB of 412 GB".
func progress(p float64, global, need int64) string {
	if need <= 0 {
		return pct(p) + ": " + rules.Size(global) // also 99%: only deletions remain
	}
	return fmt.Sprintf("%s: %s of %s", pct(p), rules.Size(haveBytes(global, need)), rules.Size(global))
}

// plural formats a count with its noun: plural(1, "folder", "folders") is
// "1 folder", plural(3, ...) is "3 folders".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// grid builds the Folders grid. A viewer (role other than admin) gets the
// device IDs of the column titles in their short form.
func (s *Server) grid(v *engine.View, role string) gridVM {
	snap := v.Snapshot
	// A disabled server is a server, but syncwatch does
	// not poll it: its column would have only "–" cells. The Overview shows
	// it as a grey "Disabled" tile instead.
	var devs []*model.DeviceView
	for _, d := range snap.Devices() {
		if !d.Disabled {
			devs = append(devs, d)
		}
	}
	cfg := s.cfg.Get()
	vm := gridVM{Sites: filterSites(cfg)}
	for _, d := range devs {
		id := d.ID
		if role != RoleAdmin {
			id = shortDeviceID(id)
		}
		gd := gridDevice{ID: id, Name: showControls(d.Name), Short: model.ShortID(d.ID), Server: d.IsServer, Online: d.Connected}
		if d.IsServer {
			gd.Site = cfg.SiteOf(d.ServerID)
		}
		vm.Devices = append(vm.Devices, gd)
	}
	cs := s.cfg.Structure()

	type folderInfo struct {
		id, label, group string
		on               map[string]*model.Folder // server ID → folder
		disk             map[string]diskDir       // server ID → the folder on the disk of the server, not in Syncthing
		diskOnly         bool                     // in no Syncthing folder of any server
		servers          []string
	}
	infos := map[string]*folderInfo{}
	var order []string
	for _, sv := range snap.Servers {
		st := pathx.StyleFor(sv.PathSep)
		var add, exclude []string
		if sc, ok := cfg.ServerByKey(sv.ID); ok {
			add, exclude = cfg.SharesFor(sc)
		}
		shares := rules.DetectShares(sv, add, exclude, cs)
		ids := make([]string, 0, len(sv.Folders))
		for id := range sv.Folders {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			f := sv.Folders[id]
			fi := infos[id]
			if fi == nil {
				fi = &folderInfo{id: id, label: showControls(f.DisplayName()), on: map[string]*model.Folder{}}
				if sh := rules.ShareOf(shares, f.Path, st); sh != nil {
					fi.group = showControls(groupName(sv.Name, sh.Name))
				} else {
					fi.group = "Other folders"
				}
				infos[id] = fi
				order = append(order, id)
			}
			fi.on[sv.ID] = f
			fi.servers = append(fi.servers, showControls(sv.Name))
		}
	}

	// The project folders on the disk of a server that are not in Syncthing
	// (S7): a grey cell in the column of the server, in the row of the
	// Syncthing folder with the same name in the same group, or in a row of
	// their own. They tell what still has to be added to Syncthing.
	byLabel := map[string]string{} // group and lower-case label → row ID
	for _, id := range order {
		fi := infos[id]
		if k := fi.group + "\x00" + strings.ToLower(fi.label); byLabel[k] == "" {
			byLabel[k] = id
		}
	}
	for _, sv := range snap.Servers {
		if sv.Structure == nil {
			continue
		}
		for _, sh := range sv.Structure.Shares {
			group := showControls(groupName(sv.Name, sh.Name))
			for _, d := range sh.Dirs {
				if !rules.NotInSyncthing(cs, d) {
					continue
				}
				label := showControls(d.Name)
				k := group + "\x00" + strings.ToLower(label)
				id := byLabel[k]
				if id == "" {
					id = "disk:" + k
					byLabel[k] = id
					infos[id] = &folderInfo{id: id, label: label, group: group, on: map[string]*model.Folder{}, diskOnly: true}
					order = append(order, id)
				}
				fi := infos[id]
				if _, ok := fi.on[sv.ID]; ok {
					continue // the server has the Syncthing folder, in another place
				}
				if fi.disk == nil {
					fi.disk = map[string]diskDir{}
				}
				dd := diskDir{share: showControls(sh.Name), path: d.Path, noSizes: sv.Structure.NoSizes}
				if sz := sv.Structure.Sizes[d.Path]; sz != nil {
					c := *sz
					dd.size = &c
				}
				if _, seen := fi.disk[sv.ID]; !seen {
					fi.disk[sv.ID] = dd
					if fi.diskOnly {
						fi.servers = append(fi.servers, showControls(sv.Name))
					}
				}
			}
		}
	}

	groups := map[string]*gridGroup{}
	var gorder []string
	lists := make([]deviceList, len(vm.Devices))
	for i, d := range vm.Devices {
		lists[i].Device = d
	}
	for _, id := range order {
		fi := infos[id]
		row := gridRow{ID: id, Label: fi.label, Search: strings.ToLower(fi.label + " " + id), Servers: strings.Join(fi.servers, ", "),
			Anchor: rowAnchor(id), DiskOnly: fi.diskOnly}
		if fi.diskOnly {
			row.Search = strings.ToLower(fi.label)
		}
		for i, d := range devs {
			var cell gridCell
			if d.IsServer {
				if dd, ok := fi.disk[d.ServerID]; ok {
					cell = s.diskCell(snap.Server(d.ServerID), dd, cfg)
					row.OnDisk = true
					// Not while the gate measures it: the box then shows ⟳ itself.
					if !dd.noSizes && (dd.size == nil || dd.size.State == "") {
						cell.Measure = &measureTarget{Server: d.ServerID, ServerName: showControls(d.Name), Path: dd.path, Folder: fi.label, Back: "/grid#" + row.Anchor}
					}
				} else if f, ok := fi.on[d.ServerID]; ok {
					cell = serverCell(f)
					if sv := snap.Server(d.ServerID); sv != nil && !folderStateKnown(sv) {
						// The last known state, in grey: not a current "✔ 100%".
						cell.Class, cell.Icon = "off", "○"
						cell.Title = fmt.Sprintf("%s %s, last known state %s: %s", sv.Name, staleWhy(sv), s.fmtTime(sv.LastContact), cell.Title)
						row.Stale = true
					}
				} else {
					cell = gridCell{Class: "na", Icon: "", Text: "–", Title: "Not on this server"}
				}
			} else {
				cell = s.pcCell(snap, fi.on, d)
			}
			cell.Title = showControls(cell.Title)
			if cell.Problem {
				row.Problem = true
				lists[i].Problems++
			}
			row.Cells = append(row.Cells, cell)
			if cell.Class != "na" {
				lists[i].Items = append(lists[i].Items, deviceItem{Folder: fi.label, Cell: cell})
			}
		}
		g := groups[fi.group]
		if g == nil {
			g = &gridGroup{Name: fi.group}
			groups[fi.group] = g
			gorder = append(gorder, fi.group)
		}
		g.Items = append(g.Items, row)
		if !row.DiskOnly {
			g.Rows++
			vm.Folders++
		}
		if row.OnDisk {
			g.OnDisk++
		}
		if row.Problem {
			g.Problems++
		}
		if row.Stale {
			g.Stale++
		}
	}
	for _, name := range gorder {
		g := groups[name]
		sort.SliceStable(g.Items, func(a, b int) bool {
			if g.Items[a].Problem != g.Items[b].Problem {
				return g.Items[a].Problem
			}
			return strings.ToLower(g.Items[a].Label) < strings.ToLower(g.Items[b].Label)
		})
		g.Open = g.Problems > 0
		vm.Groups = append(vm.Groups, *g)
	}
	sort.SliceStable(vm.Groups, func(a, b int) bool {
		if (vm.Groups[a].Problems > 0) != (vm.Groups[b].Problems > 0) {
			return vm.Groups[a].Problems > 0
		}
		return vm.Groups[a].Name < vm.Groups[b].Name
	})
	vm.Lists = lists
	return vm
}

// rowAnchor is the id of a row on the Folders page: short, and the same at
// each render, so that Measure can return to the row.
func rowAnchor(id string) string {
	sum := sha256.Sum256([]byte(id))
	return "r-" + hex.EncodeToString(sum[:6])
}

// diskCell shows a project folder on the disk of a server that is not in
// Syncthing: a grey box with its size, never a state of Syncthing.
func (s *Server) diskCell(sv *model.Server, dd diskDir, cfg *config.Config) gridCell {
	name := "the server"
	if sv != nil {
		name = sv.Name
	}
	c := gridCell{Class: "disk", Icon: "▢", Text: "no gate", Title: fmt.Sprintf("On the disk of %s in %s, not in Syncthing.", name, dd.share)}
	if dd.noSizes {
		c.Title += " The server has no syncwatch gate that measures sizes."
		return c
	}
	when := "The gate measures it each night at " + cfg.Collector.SizeTime + ", or select ⟳ to measure it now."
	if cfg.Collector.SizeTime == "off" {
		when = "The nightly measurement is off: select ⟳ to measure it now."
	}
	sz := dd.size
	if sz == nil {
		c.Text = "not measured"
		c.Title += " Not measured yet. " + when
		return c
	}
	if !sz.MeasuredAt.IsZero() {
		c.Text = rules.Size(sz.Bytes)
		if !sz.Complete {
			c.Text = "≥ " + c.Text
		}
		c.Title += fmt.Sprintf(" %s in %s, measured %s in %s.", c.Text, plural(int(sz.Files), "file", "files"), s.fmtTime(sz.MeasuredAt), sz.Took.Round(time.Second))
		if !sz.Complete {
			c.Title += " The measurement stopped early or could not read everything: the size is a lower limit."
		}
	} else {
		c.Text = "not measured"
	}
	switch sz.State {
	case stclient.SizeMeasuring:
		c.Icon = "⟳"
		c.Title += " The gate measures it now."
	case stclient.SizeQueued:
		c.Icon = "⟳"
		c.Title += " The gate measures it soon: it measures one folder at a time."
	}
	if sz.Error != "" {
		if sz.MeasuredAt.IsZero() {
			c.Text = "size unknown"
		}
		c.Title += " " + sz.Error + "."
	} else if sz.MeasuredAt.IsZero() && sz.State == "" {
		c.Title += " Not measured yet. " + when
	}
	return c
}

// pcCell shows a PC's state for a folder, using the best report from any server.
func (s *Server) pcCell(snap *model.Snapshot, on map[string]*model.Folder, d *model.DeviceView) gridCell {
	var best *model.Completion
	var bestConnected bool
	shared := false
	for _, sv := range snap.Servers {
		f, ok := on[sv.ID]
		if !ok || !f.SharedWithDevice(d.ID) {
			continue
		}
		shared = true
		c := f.Completion[d.ID]
		if c == nil {
			continue
		}
		cn := sv.Connections[d.ID]
		connected := cn != nil && cn.Connected && sv.Status == model.StatusUp
		if best == nil || (connected && !bestConnected) || (connected == bestConnected && c.UpdatedAt.After(best.UpdatedAt)) {
			best, bestConnected = c, connected
		}
	}
	if !shared {
		return gridCell{Class: "na", Text: "–", Title: "Not shared with this device"}
	}
	if best == nil {
		return gridCell{Class: "off", Icon: "?", Text: "unknown", Title: "No completion data for this device"}
	}
	switch {
	case best.RemoteState == "paused":
		return gridCell{Class: "paused", Icon: "⏸", Text: "paused", Title: "Paused on the device", Problem: true}
	case best.RemoteState == "notSharing":
		return gridCell{Class: "warn", Icon: "⊘", Text: "not shared back", Title: "The server shares the folder, but the device does not share it back", Problem: true}
	case !d.Connected && !serversKnown(snap, d.SeenBy):
		// A server that knows the device is not up: the device can be
		// connected to it. The same rule as the PCs tile.
		title := "A server of this device is not up: last known " + progress(best.Percent, best.GlobalBytes, best.NeedBytes)
		if !d.LastSeen.IsZero() {
			title += ", last seen " + s.fmtTime(d.LastSeen)
		}
		return gridCell{Class: "off", Icon: "?", Text: "unknown", Title: title}
	case !d.Connected:
		title := "Offline"
		if !d.LastSeen.IsZero() {
			title += ", last seen " + s.fmtTime(d.LastSeen)
		}
		title += ". Last known " + progress(best.Percent, best.GlobalBytes, best.NeedBytes)
		return gridCell{Class: "off", Icon: "○", Text: rules.Size(haveBytes(best.GlobalBytes, best.NeedBytes)), Title: title}
	case best.Percent >= 100:
		return gridCell{Class: "ok", Icon: "✔", Text: rules.Size(best.GlobalBytes), Title: "Up to date. " + progress(100, best.GlobalBytes, 0)}
	default:
		return gridCell{Class: "sync", Icon: "⟳", Text: rules.Size(haveBytes(best.GlobalBytes, best.NeedBytes)),
			Title: progress(best.Percent, best.GlobalBytes, best.NeedBytes) + ", " + plural(int(best.NeedItems), "remaining item", "remaining items")}
	}
}

// Problems

type problemFilter struct {
	Severity, Check, Server, Category string
	ShowSnoozed                       bool
}

func parseFilter(q url.Values) problemFilter {
	return problemFilter{
		Severity: q.Get("severity"), Check: q.Get("check"), Server: q.Get("server"),
		Category: q.Get("category"), ShowSnoozed: q.Get("snoozed") == "1",
	}
}

func (f problemFilter) query() string {
	q := url.Values{}
	for k, v := range map[string]string{"severity": f.Severity, "check": f.Check, "server": f.Server, "category": f.Category} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if f.ShowSnoozed {
		q.Set("snoozed", "1")
	}
	return q.Encode()
}

type historyRow struct {
	Icon, Check, Message, When string
}

type problemsVM struct {
	Rows             []problemRow
	Snoozed          []problemRow // hidden by a snooze or ignore, shown in their own list
	Hidden           int
	Filter           problemFilter
	Query            string
	Today            string // in the configured zone, for the snooze date field
	Checks           []config.CheckInfo
	Servers          []*model.Server
	Severities       []severityOption
	Errors, Warns    int
	Infos            int
	NewThisWeek      int
	ResolvedCount    int          // resolved in the last 7 days
	ResolvedThisWeek []historyRow // the latest of those, newest first
}

// severityOption is one choice of the severity filter: the value in the
// URL and the word shown.
type severityOption struct {
	Value, Label string
}

var severityOptions = []severityOption{
	{model.Error.Lower(), model.Error.Label()}, {model.Warn.Lower(), model.Warn.Label()}, {model.Info.Lower(), model.Info.Label()},
}

// problems builds the Findings page. A viewer (role other than admin) gets
// the short device IDs and no links to Syncthing.
func (s *Server) problems(v *engine.View, f problemFilter, role string) problemsVM {
	now := s.now()
	vm := problemsVM{Filter: f, Query: f.query(), Checks: config.Catalog, Servers: v.Snapshot.Servers, Severities: severityOptions,
		Today: now.In(s.cfg.Get().Location()).Format("2006-01-02")}
	var recs, hidden []*store.Record
	for _, r := range v.Open {
		_, snoozed := v.Snoozed(r.ID, now)
		if snoozed && !f.ShowSnoozed {
			hidden = append(hidden, r)
			continue
		}
		if f.Severity != "" && r.Severity.Lower() != f.Severity {
			continue
		}
		if f.Check != "" && !containsStr(r.Checks, f.Check) {
			continue
		}
		if f.Server != "" && r.Server != f.Server && !containsStr(r.Related, f.Server) {
			continue
		}
		if f.Category != "" && r.Category != f.Category {
			continue
		}
		switch r.Severity {
		case model.Error:
			vm.Errors++
		case model.Warn:
			vm.Warns++
		default:
			vm.Infos++
		}
		recs = append(recs, r)
	}
	admin := role == RoleAdmin
	vm.Rows = s.problemRows(v, recs, admin)
	vm.Snoozed = s.problemRows(v, hidden, admin)
	vm.Hidden = len(hidden)
	if hist, err := s.st.HistoryBetween(now.Add(-7*24*time.Hour), now.Add(time.Second)); err == nil {
		from := now.Add(-7 * 24 * time.Hour)
		var resolved []store.HistoryEntry
		for _, h := range hist {
			if !h.OpenedAt.Before(from) {
				vm.NewThisWeek++
			}
			if !h.ResolvedAt.IsZero() && !h.ResolvedAt.Before(from) {
				resolved = append(resolved, h)
			}
		}
		vm.ResolvedCount = len(resolved)
		sort.SliceStable(resolved, func(a, b int) bool { return resolved[a].ResolvedAt.After(resolved[b].ResolvedAt) })
		if len(resolved) > 30 {
			resolved = resolved[:30]
		}
		for _, h := range resolved {
			vm.ResolvedThisWeek = append(vm.ResolvedThisWeek, historyRow{Icon: "✔", Check: h.Check, Message: showControls(h.Message), When: s.fmtTime(h.ResolvedAt)})
		}
	}
	return vm
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Settings view helpers.

type serverSettings struct {
	config.Server
	HasKey         bool // a key entered for the current URL
	KeyForOtherURL bool // a key is stored, but it was entered for another URL
	KeyLost        bool // a key is stored, but the secret key cannot decrypt it
	Pin            string
	PinShort       string
	CertChange     *model.CertChange
	NewShort       string
	OldShort       string
	StatusLabel    string
	Status         string
	LastError      string
}

func (s *Server) serverSettings(v *engine.View) []serverSettings {
	var out []serverSettings
	for _, sc := range s.cfg.Get().Servers {
		ss := serverSettings{Server: sc, HasKey: settings.KeyUsable(s.st, sc)}
		if s.st.HasSecret(store.ServerAPIKeySecret(sc.ID)) && !ss.HasKey {
			// Stored but not usable: either syncwatch cannot decrypt it (the
			// secret key changed) or it was entered for another URL.
			_, _, err := s.st.ServerKey(sc.ID)
			ss.KeyLost = err != nil
			ss.KeyForOtherURL = err == nil
		}
		ss.Pin, _ = s.st.Pin(sc.ID)
		ss.PinShort = stclient.ShortFP(ss.Pin)
		if sv := v.Snapshot.Server(sc.ID); sv != nil {
			ss.StatusLabel, _, ss.Status = statusLabel(sv.Status)
			ss.LastError = sv.LastError
			if sv.CertChange != nil {
				cc := *sv.CertChange
				ss.CertChange = &cc
				ss.NewShort, ss.OldShort = stclient.ShortFP(cc.New), stclient.ShortFP(cc.Old)
			}
			if sv.Cert.CASigned && ss.PinShort != "" {
				ss.PinShort += " (CA-signed)"
			}
		} else if sc.Disabled {
			ss.StatusLabel, ss.Status = "Disabled", "wait"
		} else {
			// Added a moment ago: the view is older than the server.
			ss.StatusLabel, _, ss.Status = statusLabel(model.StatusUnknown)
		}
		out = append(out, ss)
	}
	return out
}
