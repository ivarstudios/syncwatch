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
	Urgent, Stale, Snoozed                                     bool
	SnoozeLabel, OpenFor, Since, OpenedAt, Pinged              string
	Details                                                    []model.Detail
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
			SeverityLabel: r.Severity.String(), Icon: notify.Icon(r.Severity), Category: r.Category,
			Server: r.Server, Subject: r.Subject, Message: r.Message, Urgent: r.Urgent, Stale: r.Stale,
			Details: r.Details, Paths: r.Paths, Link: r.Link,
			Title: config.CatalogByID[r.Check].Title,
		}
		if sv := v.Snapshot.Server(r.Server); sv != nil {
			row.ServerName = sv.Name
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
				row.SnoozeLabel = "ignored: " + z.Reason
			} else {
				row.SnoozeLabel = "snoozed until " + s.fmtTime(z.Until)
				if z.Reason != "" {
					row.SnoozeLabel += " (" + z.Reason + ")"
				}
			}
		}
		out = append(out, row)
	}
	return out
}

// Overview

type serverTile struct {
	ID, Name, Status, StatusLabel, StatusIcon, Version, Uptime, Link string
	Folders, Idle, Syncing, Errors, Paused, Problems                 int
	CertExpires                                                      string
	CertWarn                                                         bool
	LastContact, Mode, LastError                                     string
	ShareErrors                                                      []string
	Shares                                                           int
	LastScan, ScanCost                                               string
}

type overviewVM struct {
	Health                   string
	Servers                  []serverTile
	PCTotal, PCOnline, PCOff int
	PCOffLabel               string
	Top                      []problemRow
	OpenCount, UrgentOpen    int
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
	vm := overviewVM{Health: notify.HealthLine(cfg, v, now), Evaluated: s.fmtTime(v.Changed)}
	perServer := map[string]int{}
	var visible []*store.Record
	for _, r := range v.Open {
		if _, ok := v.Snoozed(r.ID, now); ok {
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
		t := serverTile{ID: sv.ID, Name: sv.Name, Status: class, StatusLabel: label, StatusIcon: icon,
			Version: sv.Version, Link: sv.GUIURL, Problems: perServer[sv.ID], Mode: sv.Mode,
			LastContact: s.fmtTime(sv.LastContact), LastError: sv.LastError}
		if !sv.StartTime.IsZero() && sv.Status == model.StatusUp {
			t.Uptime = rules.Ago(now.Sub(sv.StartTime))
		}
		for _, f := range sv.Folders {
			t.Folders++
			switch {
			case f.Paused:
				t.Paused++
			case f.State == "error" || f.Error != "" || f.Errors > 0 || f.PullErrors > 0:
				t.Errors++
			case f.State == "idle" && f.NeedItems == 0:
				t.Idle++
			default:
				t.Syncing++
			}
		}
		if !sv.Cert.NotAfter.IsZero() {
			t.CertExpires = sv.Cert.NotAfter.In(cfg.Location()).Format("2006-01-02")
			t.CertWarn = sv.Cert.NotAfter.Sub(now) < 30*24*time.Hour
		}
		if sv.Structure != nil {
			t.Shares = len(sv.Structure.Shares)
			t.LastScan = s.fmtTime(sv.Structure.ScannedAt)
			if n := sv.Structure.ScanListings; n > 0 {
				t.ScanCost = fmt.Sprintf("%d directory listings in %s", n, sv.Structure.ScanTook.Round(100*time.Millisecond))
			}
			for _, sh := range sv.Structure.Shares {
				if sh.Error != "" {
					t.ShareErrors = append(t.ShareErrors, sh.Name+": "+sh.Error)
				}
			}
		}
		vm.Servers = append(vm.Servers, t)
	}
	for _, d := range v.Snapshot.Devices() {
		if d.IsServer {
			continue
		}
		vm.PCTotal++
		if d.Connected {
			vm.PCOnline++
		} else if d.LastSeen.IsZero() || now.Sub(d.LastSeen) > cfg.Thresholds.PCOffline.D() {
			vm.PCOff++
		}
	}
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
	Cells                      []gridCell
}

type gridGroup struct {
	Name           string
	Problems, Rows int
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
	switch {
	case f.Paused:
		return gridCell{Class: "paused", Icon: "⏸", Text: "paused", Title: "Paused on the server", Problem: true}
	case f.State == "error" || f.Error != "" || f.Errors > 0 || f.PullErrors > 0 || f.WatchError != "":
		msg := f.Error
		if msg == "" {
			msg = fmt.Sprintf("%d failing items", max(f.Errors, f.PullErrors))
		}
		if f.WatchError != "" && f.Error == "" {
			msg = "watcher: " + f.WatchError
		}
		return gridCell{Class: "err", Icon: "✖", Text: "error", Title: msg, Problem: true}
	case f.State == "idle" && f.NeedItems == 0:
		return gridCell{Class: "ok", Icon: "✔", Text: "100%", Title: "Up to date"}
	default:
		return gridCell{Class: "sync", Icon: "⟳", Text: pct(f.LocalPercent()), Title: fmt.Sprintf("%s, %d items left", f.State, f.NeedItems)}
	}
}

func (s *Server) grid(v *engine.View) gridVM {
	snap := v.Snapshot
	devs := snap.Devices()
	vm := gridVM{}
	for _, d := range devs {
		vm.Devices = append(vm.Devices, gridDevice{ID: d.ID, Name: d.Name, Short: model.ShortID(d.ID), Server: d.IsServer, Online: d.Connected})
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
				fi = &folderInfo{id: id, label: f.DisplayName(), on: map[string]*model.Folder{}}
				if sh := rules.ShareOf(shares, f.Path, st); sh != nil {
					fi.group = groupName(sv.Name, sh.Name)
				} else {
					fi.group = "Other folders"
				}
				infos[id] = fi
				order = append(order, id)
			}
			fi.on[sv.ID] = f
			fi.servers = append(fi.servers, sv.Name)
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
				} else {
					cell = gridCell{Class: "na", Icon: "", Text: "–", Title: "Not on this server"}
				}
			} else {
				cell = s.pcCell(snap, fi.on, d)
			}
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
		return gridCell{Class: "off", Icon: "?", Text: "unknown", Title: "No completion reported yet"}
	}
	switch {
	case best.RemoteState == "paused":
		return gridCell{Class: "paused", Icon: "⏸", Text: "paused", Title: "Paused on the device", Problem: true}
	case best.RemoteState == "notSharing":
		return gridCell{Class: "warn", Icon: "⊘", Text: "not shared", Title: "Shared by the server, but the device doesn't share it back", Problem: true}
	case !d.Connected:
		title := "Offline"
		if !d.LastSeen.IsZero() {
			title += ", last seen " + s.fmtTime(d.LastSeen)
		}
		return gridCell{Class: "off", Icon: "○", Text: "offline " + pct(best.Percent), Title: title}
	case best.Percent >= 100:
		return gridCell{Class: "ok", Icon: "✔", Text: "100%", Title: "Up to date"}
	default:
		return gridCell{Class: "sync", Icon: "⟳", Text: pct(best.Percent), Title: fmt.Sprintf("%d items left", best.NeedItems)}
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
	Hidden           int
	Filter           problemFilter
	Query            string
	Checks           []config.CheckInfo
	Servers          []*model.Server
	Errors, Warns    int
	Infos            int
	NewThisWeek      int
	ResolvedThisWeek []historyRow
}

func (s *Server) problems(v *engine.View, f problemFilter) problemsVM {
	now := s.now()
	vm := problemsVM{Filter: f, Query: f.query(), Checks: config.Catalog, Servers: v.Snapshot.Servers}
	var recs []*store.Record
	for _, r := range v.Open {
		_, snoozed := v.Snoozed(r.ID, now)
		if snoozed && !f.ShowSnoozed {
			vm.Hidden++
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
	if hist, err := s.st.HistoryBetween(now.Add(-7*24*time.Hour), now.Add(time.Second)); err == nil {
		from := now.Add(-7 * 24 * time.Hour)
		for i := len(hist) - 1; i >= 0; i-- {
			h := hist[i]
			if !h.OpenedAt.Before(from) {
				vm.NewThisWeek++
			}
			if !h.ResolvedAt.IsZero() && !h.ResolvedAt.Before(from) && len(vm.ResolvedThisWeek) < 30 {
				vm.ResolvedThisWeek = append(vm.ResolvedThisWeek, historyRow{Icon: "✔", Check: h.Check, Message: h.Message, When: s.fmtTime(h.ResolvedAt)})
			}
		}
		sort.SliceStable(vm.ResolvedThisWeek, func(a, b int) bool { return vm.ResolvedThisWeek[a].When > vm.ResolvedThisWeek[b].When })
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
		ss.KeyForOtherURL = !ss.HasKey && s.st.HasSecret(store.ServerAPIKeySecret(sc.ID))
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
			if sv.Cert.CASigned {
				ss.PinShort = "CA-signed (not pinned)"
			}
		} else {
			ss.StatusLabel, ss.Status = "Disabled", "wait"
		}
		out = append(out, ss)
	}
	return out
}
