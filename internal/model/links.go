package model

import (
	"sort"
	"time"
)

// live reports whether syncwatch reads a server now: up and loaded.
func (s *Server) live() bool { return s.Status == StatusUp && s.Loaded }

// ServerLinks returns the links between the polled servers that have each
// other configured as devices (the pairs that H2 checks), A before B in
// configuration order. A link is connected when one side has a connection
// to the other, disconnected when both servers are up and loaded and have
// none, and unreachable when a server of the pair is not up or not loaded.
func (s *Snapshot) ServerLinks() []ServerLink {
	var out []ServerLink
	for i := 0; i < len(s.Servers); i++ {
		for j := i + 1; j < len(s.Servers); j++ {
			a, b := s.Servers[i], s.Servers[j]
			if a.MyID == "" || b.MyID == "" {
				continue
			}
			da, okA := a.Devices[b.MyID]
			db, okB := b.Devices[a.MyID]
			if !okA || !okB {
				continue
			}
			l := ServerLink{A: a.ID, B: b.ID, State: LinkUnreachable}
			if a.live() && b.live() {
				l.State = LinkDisconnected
				for _, c := range []*Connection{a.Connections[b.MyID], b.Connections[a.MyID]} {
					if c == nil || !c.Connected {
						continue
					}
					// The start of the connection: the earlier of the 2 sides.
					if l.State != LinkConnected || (!c.StartedAt.IsZero() && (l.Since.IsZero() || c.StartedAt.Before(l.Since))) {
						l.Since = c.StartedAt
					}
					l.State = LinkConnected
				}
				if l.State == LinkDisconnected {
					// The last time they saw each other.
					l.Since = da.LastSeen
					if db.LastSeen.After(l.Since) {
						l.Since = db.LastSeen
					}
				}
			}
			out = append(out, l)
		}
	}
	return out
}

// SiteLinks returns the links between the sites of the servers: one for each
// pair of sites with 1 or more server links between them. siteOf gives the
// site of a server ID ("" for none); servers without a site and links inside
// one site are not included. Site A comes before site B in the order of their
// first server in serverOrder (the configuration). A site link is connected
// when 1 of its server links is connected, else unreachable when 1 is
// unreachable, else disconnected.
func SiteLinks(links []ServerLink, siteOf func(serverID string) string, serverOrder []string) []SiteLink {
	rank := map[string]int{}
	for i, id := range serverOrder {
		if site := siteOf(id); site != "" {
			if _, ok := rank[site]; !ok {
				rank[site] = i
			}
		}
	}
	before := func(x, y string) bool {
		rx, okX := rank[x]
		ry, okY := rank[y]
		switch {
		case okX && okY && rx != ry:
			return rx < ry
		case okX != okY:
			return okX
		}
		return x < y
	}
	type pair struct{ a, b string }
	byPair := map[pair]*SiteLink{}
	var out []*SiteLink
	for _, l := range links {
		sa, sb := siteOf(l.A), siteOf(l.B)
		if sa == "" || sb == "" || sa == sb {
			continue
		}
		if before(sb, sa) {
			sa, sb = sb, sa
		}
		sl := byPair[pair{sa, sb}]
		if sl == nil {
			sl = &SiteLink{A: sa, B: sb}
			byPair[pair{sa, sb}] = sl
			out = append(out, sl)
		}
		sl.Servers = append(sl.Servers, [2]string{l.A, l.B})
	}
	if len(out) == 0 {
		return nil
	}
	state := map[[2]string]ServerLink{}
	for _, l := range links {
		state[[2]string{l.A, l.B}] = l
	}
	result := make([]SiteLink, 0, len(out))
	for _, sl := range out {
		var connected, unreachable bool
		var first, last time.Time
		for _, p := range sl.Servers {
			l := state[p]
			switch l.State {
			case LinkConnected:
				if !connected || (!l.Since.IsZero() && (first.IsZero() || l.Since.Before(first))) {
					first = l.Since
				}
				connected = true
			case LinkUnreachable:
				unreachable = true
			}
			if l.Since.After(last) {
				last = l.Since
			}
		}
		switch {
		case connected:
			sl.State, sl.Since = LinkConnected, first
		case unreachable:
			sl.State = LinkUnreachable
		default:
			sl.State, sl.Since = LinkDisconnected, last
		}
		result = append(result, *sl)
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].A != result[j].A {
			return before(result[i].A, result[j].A)
		}
		return before(result[i].B, result[j].B)
	})
	return result
}
