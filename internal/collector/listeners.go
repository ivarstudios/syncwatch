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

// The relay listeners of a server (H12, W-047): a NAS that reaches remote
// devices only through a relay loses them when its relay session stops.
// Syncthing reports each listener in /rest/system/status, which the
// collector reads at the start of a session, at each full check and each
// minute with the traffic counters.

// setRelayListeners records the relay listeners of a /rest/system/status
// answer that the collector read at now.
func (c *Collector) setRelayListeners(status stclient.SystemStatus, now time.Time) {
	c.update(func(s *model.Server) {
		s.RelayListeners = relayListeners(s.RelayListeners, status.ConnectionServiceStatus, now)
	})
}

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
// connected; a new listener or a change gets now.
func relayListeners(prev []model.RelayListener, status map[string]stclient.ListenerStatus, now time.Time) []model.RelayListener {
	var out []model.RelayListener
	for addr, ls := range status {
		uri, ok := relayURI(addr)
		if !ok {
			continue
		}
		l := model.RelayListener{URI: uri, Since: now}
		if ls.Error != nil {
			l.Error = *ls.Error
		}
		l.Connected = len(ls.WANAddresses) > 0 && l.Error == ""
		for _, p := range prev {
			if p.URI == uri && p.Connected == l.Connected && !p.Since.IsZero() {
				l.Since = p.Since
			}
		}
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].URI < out[j].URI })
	return out
}

// relayURI returns the listen address of a relay listener without its
// query, which can hold the token of a private relay:
// relay://10.115.1.60:22067/?id=…&token=… gives relay://10.115.1.60:22067.
// ok is false for the other listeners (tcp, quic).
func relayURI(addr string) (uri string, ok bool) {
	scheme, _, found := strings.Cut(addr, "://")
	if !found || (scheme != "relay" && !strings.HasPrefix(scheme, "dynamic+")) {
		return "", false
	}
	u, err := url.Parse(addr)
	if err != nil {
		// Not a URL: keep what is before the query.
		uri, _, _ = strings.Cut(addr, "?")
		uri, _, _ = strings.Cut(uri, "#")
		return strings.TrimSuffix(uri, "/"), true
	}
	u.RawQuery, u.ForceQuery, u.Fragment, u.RawFragment = "", false, "", ""
	if u.Path == "/" {
		u.Path, u.RawPath = "", ""
	}
	return u.String(), true
}
