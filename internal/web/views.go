package web

import (
	"fmt"
	"net/url"
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
}

func (s *Server) fmtTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.In(s.cfg.Get().Location()).Format("2006-01-02 15:04")
}

func (s *Server) problemRows(v *engine.View, recs []*store.Record) []problemRow {
	now := s.now()
	var out []problemRow
	for _, r := range recs {
		row := problemRow{
			ID: r.ID, Check: r.Check, Checks: strings.Join(r.Checks, "+"), Severity: r.Severity.Lower(),
			SeverityLabel: r.Severity.Label(), Icon: notify.Icon(r.Severity), Category: r.Category,
			Server: r.Server, Subject: showControls(r.Subject), Message: showControls(r.Message), Urgent: r.Urgent, Stale: r.Stale,
			Details: s.detailRows(r.Details), Paths: showAll(r.Paths), Link: r.Link,
			Title: config.CatalogByID[r.Check].Title,
		}
		if sv := v.Snapshot.Server(r.Server); sv != nil {
			row.ServerName = showControls(sv.Name)
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
	FoldersStale                                                            bool // the folder counts are the last known state
	NoContact                                                               bool // never contacted: no folder state at all
	Disabled                                                                bool // in the configuration, but syncwatch does not poll it
}

type overviewVM struct {
	Health                   string
	Servers                  []serverTile
	PCTotal, PCOnline, PCOff int
	PCUnknown                int // not connected, but a server that knows the PC is not up: maybe online
	PCOffLabel               string
	Top                      []problemRow
	OpenCount, UrgentOpen    int
	SnoozedCount             int // open findings hidden by a snooze or ignore
	Evaluated                string
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

func (s *Server) overview(v *engine.View) overviewVM {
	cfg := s.cfg.Get()
	now := s.now()
	vm := overviewVM{Health: showControls(notify.HealthLine(cfg, v, now)), Evaluated: s.fmtTime(v.Changed)}
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
	if len(visible) > 8 {
		visible = visible[:8]
	}
	vm.Top = s.problemRows(v, visible)
	for _, sv := range v.Snapshot.Servers {
		label, icon, class := statusLabel(sv.Status)
		t := serverTile{ID: sv.ID, Name: showControls(sv.Name), Status: class, StatusLabel: label, StatusIcon: icon,
			Version: sv.Version, Link: sv.GUIURL, Problems: perServer[sv.ID], Mode: sv.Mode,
			LastContact: s.fmtTime(sv.LastContact), LastError: showControls(sv.LastError)}
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
			t.LastScan = s.fmtTime(sv.Structure.ScannedAt)
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
			vm.Servers = append(vm.Servers, serverTile{ID: sc.ID, Name: showControls(sc.Name), Status: "wait", StatusLabel: "Disabled", StatusIcon: "○", Disabled: true})
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
}

type gridCell struct {
	Class, Icon, Text, Title string
	Problem                  bool
}

type gridRow struct {
	ID, Label, Search, Servers string
	Problem                    bool
	Stale                      bool // a server of the folder shows only its last known state
	Cells                      []gridCell
}

type gridGroup struct {
	Name           string
	Problems, Rows int
	Stale          int // rows with the last known state of a server
	Open           bool
	Items          []gridRow
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
	switch f.FolderSync() {
	case model.SyncPaused:
		return gridCell{Class: "paused", Icon: "⏸", Text: "paused", Title: "Paused on the server", Problem: true}
	case model.SyncError:
		msg := f.Error
		if msg == "" {
			msg = plural(max(f.Errors, f.PullErrors), "failed item", "failed items")
		}
		return gridCell{Class: "err", Icon: "✖", Text: "error", Title: msg, Problem: true}
	case model.SyncWatchError:
		return gridCell{Class: "warn", Icon: "⚠", Text: "file watcher error", Title: "file watcher error: " + f.WatchError, Problem: true}
	case model.SyncScanning:
		return gridCell{Class: "sync", Icon: "⟳", Text: "scanning", Title: "Scanning"}
	case model.SyncUpToDate:
		return gridCell{Class: "ok", Icon: "✔", Text: "100%", Title: "Up to date"}
	default:
		return gridCell{Class: "sync", Icon: "⟳", Text: pct(f.LocalPercent()), Title: fmt.Sprintf("%s, %s", f.State, plural(int(f.NeedItems), "remaining item", "remaining items"))}
	}
}

// plural formats a count with its noun: plural(1, "folder", "folders") is
// "1 folder", plural(3, ...) is "3 folders".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func (s *Server) grid(v *engine.View) gridVM {
	snap := v.Snapshot
	devs := snap.Devices()
	vm := gridVM{}
	for _, d := range devs {
		vm.Devices = append(vm.Devices, gridDevice{ID: d.ID, Name: showControls(d.Name), Short: model.ShortID(d.ID), Server: d.IsServer, Online: d.Connected})
	}
	cs := s.cfg.Structure()
	cfg := s.cfg.Get()

	type folderInfo struct {
		id, label, group string
		on               map[string]*model.Folder // server ID → folder
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

	groups := map[string]*gridGroup{}
	var gorder []string
	lists := make([]deviceList, len(vm.Devices))
	for i, d := range vm.Devices {
		lists[i].Device = d
	}
	for _, id := range order {
		fi := infos[id]
		row := gridRow{ID: id, Label: fi.label, Search: strings.ToLower(fi.label + " " + id), Servers: strings.Join(fi.servers, ", ")}
		for i, d := range devs {
			var cell gridCell
			if d.IsServer {
				if f, ok := fi.on[d.ServerID]; ok {
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
		g.Rows++
		if row.Problem {
			g.Problems++
		}
		if row.Stale {
			g.Stale++
		}
		vm.Folders++
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
		title := "A server of this device is not up: last known " + pct(best.Percent)
		if !d.LastSeen.IsZero() {
			title += ", last seen " + s.fmtTime(d.LastSeen)
		}
		return gridCell{Class: "off", Icon: "?", Text: "unknown", Title: title}
	case !d.Connected:
		title := "Offline"
		if !d.LastSeen.IsZero() {
			title += ", last seen " + s.fmtTime(d.LastSeen)
		}
		return gridCell{Class: "off", Icon: "○", Text: "offline " + pct(best.Percent), Title: title}
	case best.Percent >= 100:
		return gridCell{Class: "ok", Icon: "✔", Text: "100%", Title: "Up to date"}
	default:
		return gridCell{Class: "sync", Icon: "⟳", Text: pct(best.Percent), Title: plural(int(best.NeedItems), "remaining item", "remaining items")}
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

func (s *Server) problems(v *engine.View, f problemFilter) problemsVM {
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
	vm.Rows = s.problemRows(v, recs)
	vm.Snoozed = s.problemRows(v, hidden)
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
			// secret key changed, W-030) or it was entered for another URL.
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
		} else {
			ss.StatusLabel, ss.Status = "Disabled", "wait"
		}
		out = append(out, ss)
	}
	return out
}
