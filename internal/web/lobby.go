package web

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/engine"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/notify"
	"github.com/ivarstudios/syncwatch/internal/rules"
	"github.com/ivarstudios/syncwatch/internal/store"
)

// The lobby page (W-052, W-057): a read-only full-screen view for a screen in
// the office. It shows the health line, the connections between the sites,
// the servers of each site and the open urgent and ERROR findings. A viewer
// can open it (with open access without a login); no API token goes to the
// browser. The page has no forms, no navigation, no links and no device IDs,
// and it refreshes through the event stream like the other pages.

// lobbyMaxFindings is the most findings that the lobby lists.
const lobbyMaxFindings = 10

// lobbyMaxMessage is the most characters of a finding message in the list,
// thus 10 findings fit on a screen. The Findings page has the full message.
const lobbyMaxMessage = 160

// lobbyOther is the name of the group of the servers without a site, when
// other servers have a site.
const lobbyOther = "Other servers"

type lobbyVM struct {
	Site     string // the site of ?site=, as shown; "" for all sites
	Query    string // the query of the fragment URL ("site=…"), "" for all sites
	Health   string
	Changed  string // the time of the last change
	Failing  string // the time from which notifications fail, "" while they work
	Links    []lobbyLink
	Groups   []lobbyGroup
	Findings []lobbyFinding
	More     int      // open urgent and ERROR findings that are not in the list
	NoSite   bool     // no server has the site of ?site=
	Sites    []string // the sites of the servers, for the text when NoSite is set
}

// lobbyLink is one row of the connections: between 2 sites, or between 2
// servers when fewer than 2 sites are set (C11).
type lobbyLink struct {
	Text  string // "IVAR Office ↔ Branch office: connected from 08:12"
	State string // model.LinkConnected, LinkDisconnected or LinkUnreachable
	Icon  string
}

// lobbyGroup is the servers of one site.
type lobbyGroup struct {
	Name    string // the site, lobbyOther, or "" when no server has a site (no heading)
	PCs     string // the PCs connected to the servers of the group now: "4", "4 or more" or "unknown"; "" without a polled server
	Servers []lobbyServer
}

type lobbyServer struct {
	Name, Status, StatusLabel, StatusIcon string
	Disabled                              bool // syncwatch does not poll the server
	NoContact                             bool // never contacted: no folder state at all
	Stale                                 bool // the folder counts are the last known state
	LastContact                           string
	Folders                               folderCounts
	Errors, Warnings                      int // open ERROR and WARNING findings of the server, not snoozed or ignored
}

type lobbyFinding struct {
	Severity, SeverityLabel, Icon, Checks, Message, OpenFor string
	Urgent, Stale                                           bool
}

// handleLobby serves the lobby page. ?site=<name> shows one site, ?theme=dark
// or ?theme=light sets the colors (else the preference of the browser).
func (s *Server) handleLobby(w http.ResponseWriter, r *http.Request, info requestInfo) {
	q := r.URL.Query()
	vm := s.lobby(s.eng.View(), q.Get("site"))
	title := "Lobby"
	if vm.Site != "" {
		title = vm.Site + " · Lobby"
	}
	s.render(w, r, http.StatusOK, "lobby", info, map[string]any{"Title": title, "VM": vm, "Theme": lobbyTheme(q.Get("theme"))})
}

// fragLobby serves the body of the lobby page for live updates.
func (s *Server) fragLobby(w http.ResponseWriter, r *http.Request, info requestInfo) {
	s.fragment(w, "lobby-body", info, map[string]any{"VM": s.lobby(s.eng.View(), r.URL.Query().Get("site"))})
}

// lobbyTheme returns the color theme of ?theme=: "dark", "light", or "" for
// the preference of the browser (prefers-color-scheme).
func lobbyTheme(t string) string {
	switch t {
	case "dark", "light":
		return t
	}
	return ""
}

// lobbyTime writes a time for the lobby: the clock time when it is today in
// the configured time zone, else the date and the time.
func (s *Server) lobbyTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	loc := s.cfg.Get().Location()
	lt, now := t.In(loc), s.now().In(loc)
	if lt.Year() == now.Year() && lt.YearDay() == now.YearDay() {
		return lt.Format("15:04")
	}
	return s.fmtTime(t)
}

// lobbyCard is a server of the lobby before the grouping.
type lobbyCard struct {
	id    string
	order int           // the position in the configuration
	sv    *model.Server // nil for a disabled server
	srv   lobbyServer
}

func (s *Server) lobby(v *engine.View, site string) lobbyVM {
	cfg := s.cfg.Get()
	now := s.now()
	snap := v.Snapshot
	if snap == nil {
		snap = &model.Snapshot{}
	}
	site = strings.TrimSpace(site)
	vm := lobbyVM{Health: showControls(notify.HealthLine(cfg, v, now)), Changed: s.lobbyTime(v.Changed)}
	if site != "" {
		vm.Query = url.Values{"site": {site}}.Encode()
		vm.Site = showControls(site)
		if r := []rune(site); len(r) > config.MaxSiteLen {
			vm.Site = showControls(string(r[:config.MaxSiteLen])) + "…"
		}
	}
	if s.hooks.Delivery != nil {
		if d := s.hooks.Delivery(); d.Failing {
			vm.Failing = s.lobbyTime(d.Since)
		}
	}

	// The findings that a person still has to see: not snoozed or ignored.
	var open []*store.Record
	for _, r := range v.Open {
		if _, ok := v.Snoozed(r.ID, now); !ok {
			open = append(open, r)
		}
	}

	// The servers in configuration order: the polled ones from the snapshot,
	// and a grey card for each disabled server (as on the Overview).
	order := map[string]int{}
	for i, sc := range cfg.Servers {
		order[sc.ID] = i
	}
	pos := func(id string) int {
		if i, ok := order[id]; ok {
			return i
		}
		return len(cfg.Servers)
	}
	var cards []lobbyCard
	for _, sv := range snap.Servers {
		label, icon, class := statusLabel(sv.Status)
		srv := lobbyServer{Name: showControls(sv.Name), Status: class, StatusLabel: label, StatusIcon: icon,
			Folders: countFolders(sv), Stale: !folderStateKnown(sv), LastContact: s.lobbyTime(sv.LastContact)}
		srv.NoContact = srv.Stale && sv.LastContact.IsZero()
		// A finding counts for its server and for each related server (the
		// rule of the per-server counts of /api/v1/ha).
		for _, r := range open {
			if r.Server != sv.ID && !slices.Contains(r.Related, sv.ID) {
				continue
			}
			switch r.Severity {
			case model.Error:
				srv.Errors++
			case model.Warn:
				srv.Warnings++
			}
		}
		cards = append(cards, lobbyCard{id: sv.ID, order: pos(sv.ID), sv: sv, srv: srv})
	}
	for _, sc := range cfg.Servers {
		if sc.Disabled && snap.Server(sc.ID) == nil {
			cards = append(cards, lobbyCard{id: sc.ID, order: pos(sc.ID),
				srv: lobbyServer{Name: showControls(sc.Name), Status: "wait", StatusLabel: "Disabled", StatusIcon: "○", Disabled: true}})
		}
	}
	sort.SliceStable(cards, func(a, b int) bool { return cards[a].order < cards[b].order })

	// One group for each site, in the order of their first server, and the
	// servers without a site last. Without any site: one group, no heading.
	anySite := false
	for _, c := range cards {
		if cfg.SiteOf(c.id) != "" {
			anySite = true
			break
		}
	}
	var keys []string
	members := map[string][]lobbyCard{}
	for _, c := range cards {
		k := cfg.SiteOf(c.id)
		if _, ok := members[k]; !ok && k != "" {
			keys = append(keys, k)
		}
		members[k] = append(members[k], c)
	}
	for _, k := range keys {
		vm.Sites = append(vm.Sites, showControls(k))
	}
	if _, ok := members[""]; ok {
		keys = append(keys, "")
	}
	siteServers := map[string]bool{} // the servers of ?site=
	for _, k := range keys {
		if site != "" && k != site {
			continue
		}
		g := lobbyGroup{Name: showControls(k)}
		if k == "" && anySite {
			g.Name = lobbyOther
		}
		var polled []*model.Server
		for _, c := range members[k] {
			g.Servers = append(g.Servers, c.srv)
			siteServers[c.id] = true
			if c.sv != nil {
				polled = append(polled, c.sv)
			}
		}
		g.PCs = lobbyPCs(snap, polled)
		vm.Groups = append(vm.Groups, g)
	}
	if site != "" && len(vm.Groups) == 0 {
		vm.NoSite = true
		return vm
	}

	vm.Links = s.lobbyLinks(cfg, snap, site)

	var list []*store.Record
	for _, r := range open {
		if !r.Urgent && r.Severity != model.Error {
			continue
		}
		if site != "" && !lobbyTouches(r, siteServers) {
			continue
		}
		list = append(list, r)
	}
	if len(list) > lobbyMaxFindings {
		vm.More = len(list) - lobbyMaxFindings
		list = list[:lobbyMaxFindings]
	}
	for _, r := range list {
		since := r.Since
		if since.IsZero() || since.After(r.OpenedAt) {
			since = r.OpenedAt
		}
		checks := strings.Join(r.Checks, "+")
		if checks == "" {
			checks = r.Check
		}
		vm.Findings = append(vm.Findings, lobbyFinding{Severity: r.Severity.Lower(), SeverityLabel: r.Severity.Label(), Icon: notify.Icon(r.Severity),
			Checks: checks, Message: showControls(lobbyCut(hideDeviceIDs(r.Message))), OpenFor: rules.Ago(now.Sub(since)), Urgent: r.Urgent, Stale: r.Stale})
	}
	return vm
}

// lobbyCut cuts a message after lobbyMaxMessage characters and adds "…".
func lobbyCut(s string) string {
	r := []rune(s)
	if len(r) <= lobbyMaxMessage {
		return s
	}
	return strings.TrimRight(string(r[:lobbyMaxMessage]), " ") + "…"
}

// lobbyTouches reports whether a finding belongs to one of the servers: its
// server or a related server. A finding of no server shows for every site.
func lobbyTouches(r *store.Record, servers map[string]bool) bool {
	if r.Server == "" && len(r.Related) == 0 {
		return true
	}
	if servers[r.Server] {
		return true
	}
	for _, id := range r.Related {
		if servers[id] {
			return true
		}
	}
	return false
}

// lobbyPCs counts the PCs that are connected to the servers now. When a
// server is not up or did not finish a full check, its PCs are not known:
// the count is a minimum, or unknown when it is 0.
func lobbyPCs(snap *model.Snapshot, servers []*model.Server) string {
	if len(servers) == 0 {
		return ""
	}
	pcs := map[string]bool{}
	known := true
	for _, sv := range servers {
		if !folderStateKnown(sv) {
			known = false
		}
		if sv.Status != model.StatusUp {
			continue
		}
		for id, cn := range sv.Connections {
			if cn.Connected && id != sv.MyID && !snap.IsServerDevice(id) {
				pcs[id] = true
			}
		}
	}
	switch {
	case known:
		return strconv.Itoa(len(pcs))
	case len(pcs) > 0:
		return fmt.Sprintf("%d or more", len(pcs))
	}
	return "unknown"
}

// lobbyLinks gives the rows of the connections (C11): one row for each link
// between 2 sites when 2 or more sites are set, else one row for each link
// between 2 servers. With a site, only the rows that touch the site.
func (s *Server) lobbyLinks(cfg *config.Config, snap *model.Snapshot, site string) []lobbyLink {
	// The sites of the polled servers: a disabled server is in no link.
	sites := map[string]bool{}
	for _, sc := range cfg.Servers {
		if !sc.Disabled && sc.Site != "" {
			sites[sc.Site] = true
		}
	}
	var out []lobbyLink
	if len(sites) >= 2 {
		for _, l := range snap.SiteLinks {
			if site != "" && l.A != site && l.B != site {
				continue
			}
			var ids []string
			for _, p := range l.Servers {
				ids = append(ids, p[0], p[1])
			}
			out = append(out, s.lobbyLink(snap, l.A, l.B, l.State, l.Since, ids))
		}
		return out
	}
	name := func(id string) string {
		if sv := snap.Server(id); sv != nil {
			return sv.Name
		}
		if sc, ok := cfg.ServerByKey(id); ok {
			return sc.Name
		}
		return id
	}
	for _, l := range snap.Links {
		if site != "" && cfg.SiteOf(l.A) != site && cfg.SiteOf(l.B) != site {
			continue
		}
		out = append(out, s.lobbyLink(snap, name(l.A), name(l.B), l.State, l.Since, []string{l.A, l.B}))
	}
	return out
}

// lobbyLink writes one row: "IVAR Office ↔ Branch office: connected from
// 08:12", "…: not connected from 21:04" or "…: state unknown: SOL is
// unreachable". ids are the servers of the link.
func (s *Server) lobbyLink(snap *model.Snapshot, a, b, state string, since time.Time, ids []string) lobbyLink {
	row := lobbyLink{State: state}
	text := showControls(a) + " ↔ " + showControls(b) + ": "
	switch state {
	case model.LinkConnected:
		row.Icon = "✔"
		text += "connected"
	case model.LinkDisconnected:
		row.Icon = "✖"
		text += "not connected"
	default:
		row.Icon, row.State = "?", model.LinkUnreachable
		text += "state unknown"
		if why := lobbyUnknownWhy(snap, ids); why != "" {
			text += ": " + why
		}
		since = time.Time{}
	}
	if !since.IsZero() {
		text += " from " + s.lobbyTime(since)
	}
	row.Text = text
	return row
}

// lobbyUnknownWhy says why syncwatch cannot tell the state of a link: the
// servers of the link that are not up or did not finish a full check.
func lobbyUnknownWhy(snap *model.Snapshot, ids []string) string {
	var why []string
	seen := map[string]bool{}
	for _, id := range ids {
		sv := snap.Server(id)
		if seen[id] || sv == nil || folderStateKnown(sv) {
			continue
		}
		seen[id] = true
		name := showControls(sv.Name)
		switch sv.Status {
		case model.StatusDown:
			why = append(why, name+" is unreachable")
		case model.StatusCertChanged:
			why = append(why, "the certificate of "+name+" changed")
		case model.StatusUp:
			why = append(why, "the full check of "+name+" is not complete")
		default:
			why = append(why, "syncwatch has no contact with "+name)
		}
	}
	return strings.Join(why, ", ")
}

var deviceIDText = regexp.MustCompile(`[A-Z2-7]{7}(?:-[A-Z2-7]{7}){7}`)

// hideDeviceIDs shortens each complete device ID in a text as the pages of a
// viewer show them (C12). The finding messages name devices by their names,
// but the name that a device sends can be any text.
func hideDeviceIDs(s string) string {
	return deviceIDText.ReplaceAllStringFunc(s, func(id string) string { return model.ShortID(id) + "…" })
}
