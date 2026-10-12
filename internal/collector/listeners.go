package collector

import (
	"context"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/stclient"
)

// The relay listeners of a server (H12): a NAS that reaches remote
// devices only through a relay loses them when its relay session stops.
// Syncthing reports each listener in /rest/system/status, which the
// collector reads at the start of a session, at each full check and each
// minute with the traffic counters.

// setRelayListeners records the relay listeners of a /rest/system/status
// answer that the collector read at now.
func (c *Collector) setRelayListeners(status stclient.SystemStatus, now time.Time) {
	c.update(func(s *model.Server) {
		// The hub runs this function under its lock: one sample at a time
		// changes relayOK (the session and the traffic loop both sample).
		if c.relayOK == nil {
			c.relayOK = map[string]time.Time{}
		}
		s.RelayListeners = relayListeners(s.RelayListeners, c.relayOK, status.ConnectionServiceStatus, now)
	})
}

// relayConfirm is how long a relay listener that was not connected must
// report no error before it counts as connected again: the first sample
// without an error and a later one at least relayConfirm after it, with no
// error between them. While the relay is off or cannot be reached, Syncthing
// clears the error of the listener for some seconds at each new try to
// connect (approximately every 70 seconds with Syncthing 2.1.5 and 2.1.6 in
// a test); without this rule a sample in that time ended H12 and started
// relay_lost again. The samples of the traffic loop are 1 minute apart.
const relayConfirm = 50 * time.Second

// sampleListeners reads the listeners again while the server is up. A
// failure is left to the session, which reports a server that cannot be
// reached.
func (c *Collector) sampleListeners(ctx context.Context) {
	up := false
	c.hub.View(c.id, func(s *model.Server) { up = s.Status == model.StatusUp && s.Loaded })
	if !up {
		return
	}
	status, err := c.client.SystemStatus(ctx)
	if err != nil {
		return
	}
	c.setRelayListeners(status, time.Now())
}

// relayListeners returns the relay listeners in status, ordered by URI:
// the listeners whose scheme is relay or starts with dynamic+. A listener
// is connected when Syncthing reports a WAN address and no error for it.
// Since stays as in prev while the listener stays connected or not
// connected; a new listener or a change gets now. A listener that was not
// connected counts as connected again only after relayConfirm without an
// error (see relayConfirm); okSince keeps the first sample without an error
// of each such listener, and Since is then the time of that sample.
func relayListeners(prev []model.RelayListener, okSince map[string]time.Time, status map[string]stclient.ListenerStatus, now time.Time) []model.RelayListener {
	var out []model.RelayListener
	seen := map[string]bool{}
	for addr, ls := range status {
		uri, ok := relayURI(addr)
		if !ok {
			continue
		}
		seen[uri] = true
		l := model.RelayListener{URI: uri, Since: now}
		if ls.Error != nil {
			l.Error = *ls.Error
		}
		l.Connected = len(ls.WANAddresses) > 0 && l.Error == ""
		var p *model.RelayListener
		for i := range prev {
			if prev[i].URI == uri {
				p = &prev[i]
			}
		}
		switch {
		case p != nil && !p.Connected && l.Connected:
			first, waiting := okSince[uri]
			if !waiting {
				first = now
				okSince[uri] = now
			}
			if now.Sub(first) < relayConfirm {
				// Not confirmed yet: maybe a new try to connect.
				l.Connected, l.Error, l.Since = false, p.Error, p.Since
			} else {
				l.Since = first
				delete(okSince, uri)
			}
		default:
			delete(okSince, uri) // an error ends a confirmation
			if p != nil && p.Connected == l.Connected && !p.Since.IsZero() {
				l.Since = p.Since
			}
		}
		out = append(out, l)
	}
	for uri := range okSince {
		if !seen[uri] {
			delete(okSince, uri)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].URI < out[j].URI })
	return out
}

// relayURI returns the listen address of a relay listener without its
// query, which can hold the token of a private relay, and without its user
// information, which can hold the password of a relay pool (Syncthing sends
// it to the pool as basic authentication):
// relay://10.115.1.60:22067/?id=…&token=… gives relay://10.115.1.60:22067,
// dynamic+https://user:password@relays.example/endpoint gives
// dynamic+https://relays.example/endpoint. H12 shows the address to viewers.
// ok is false for the other listeners (tcp, quic).
func relayURI(addr string) (uri string, ok bool) {
	scheme, rest, found := strings.Cut(addr, "://")
	if !found || (scheme != "relay" && !strings.HasPrefix(scheme, "dynamic+")) {
		return "", false
	}
	u, err := url.Parse(addr)
	if err != nil {
		// Not a URL: keep what is before the query, and from that what is
		// after the last @ (a password can have a / or an @).
		rest, _, _ = strings.Cut(rest, "?")
		rest, _, _ = strings.Cut(rest, "#")
		if i := strings.LastIndex(rest, "@"); i >= 0 {
			rest = rest[i+1:]
		}
		return strings.TrimSuffix(scheme+"://"+rest, "/"), true
	}
	u.User = nil
	u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = "", false, "", ""
	if u.Path == "/" {
		u.Path, u.RawPath = "", ""
	}
	return u.String(), true
}
