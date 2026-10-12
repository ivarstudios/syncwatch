package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/engine"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/notify"
	"github.com/ivarstudios/syncwatch/internal/store"
)

// apiTime is a time in /api/v1: UTC with whole seconds, for example
// "2026-10-11T15:29:17Z". A *apiTime is null when the time is not
// known.
type apiTime time.Time

func (t apiTime) MarshalJSON() ([]byte, error) {
	return []byte(`"` + time.Time(t).UTC().Format(time.RFC3339) + `"`), nil
}

// atime returns t for the JSON API, or nil (null) when t is zero.
func atime(t time.Time) *apiTime {
	if t.IsZero() {
		return nil
	}
	a := apiTime(t)
	return &a
}

type apiServer struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Site        string   `json:"site"`   // servers[].site, "" when not set
	Status      string   `json:"status"` // up, down, cert-changed, unknown (not contacted), disabled
	Version     string   `json:"version,omitempty"`
	UptimeS     *int64   `json:"uptime_s"` // null while the server is not up
	LastContact *apiTime `json:"last_contact"`
	CertExpires *apiTime `json:"cert_expires"`
	// The end of the last structure scan that listed a share of the server,
	// or that found no share; null before the first such scan after
	// the start of syncwatch, and for a disabled server.
	LastStructureScan *apiTime `json:"last_structure_scan"`
	Error             string   `json:"error,omitempty"`
	Folders           struct {
		Total      int  `json:"total"`
		Idle       int  `json:"idle"`
		Syncing    int  `json:"syncing"`
		Scanning   int  `json:"scanning"`
		Error      int  `json:"error"`
		WatchError int  `json:"watch_error"`
		Paused     int  `json:"paused"`
		Stale      bool `json:"stale"` // the counts are the last known state: the server is not up or not checked yet
	} `json:"folders"`
}

type apiDevice struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Server    bool     `json:"server"`
	Connected bool     `json:"connected"`
	LastSeen  *apiTime `json:"last_seen"`
	Version   string   `json:"version,omitempty"`
}

type apiFinding struct {
	ID           string         `json:"id"`
	Check        string         `json:"check"`
	Checks       []string       `json:"checks"`
	Severity     string         `json:"severity"`
	Category     string         `json:"category"`
	Server       string         `json:"server,omitempty"`
	Subject      string         `json:"subject"`
	Message      string         `json:"message"`
	Urgent       bool           `json:"urgent"`
	Stale        bool           `json:"stale"`
	Snoozed      bool           `json:"snoozed"` // snoozed or ignored
	Ignored      bool           `json:"ignored"`
	SnoozeUntil  *apiTime       `json:"snooze_until"`  // the end of an active snooze; null for an ignore or without a snooze
	SnoozeReason string         `json:"snooze_reason"` // the reason of the snooze or the ignore, "" when none
	OpenedAt     *apiTime       `json:"opened_at"`
	Since        *apiTime       `json:"since"`
	Details      []model.Detail `json:"details,omitempty"`
	Paths        []string       `json:"paths,omitempty"`
}

// apiLink is a connection between 2 servers (Snapshot.Links).
type apiLink struct {
	A     string   `json:"a"`
	B     string   `json:"b"`
	SiteA string   `json:"site_a"`
	SiteB string   `json:"site_b"`
	State string   `json:"state"` // connected, disconnected or unreachable
	Since *apiTime `json:"since"`
}

// apiSiteLink is a connection between 2 sites (Snapshot.SiteLinks).
type apiSiteLink struct {
	A     string   `json:"a"`
	B     string   `json:"b"`
	State string   `json:"state"` // connected, disconnected or unreachable
	Since *apiTime `json:"since"`
}

type apiStatus struct {
	GeneratedAt   apiTime          `json:"generated_at"`
	Version       string           `json:"version"`
	Health        string           `json:"health"`
	HealthCounts  apiHealthCounts  `json:"health_counts"`
	Counts        map[string]int   `json:"counts"`
	Notifications apiNotifications `json:"notifications"`
	Servers       []apiServer      `json:"servers"`
	Devices       []apiDevice      `json:"devices"`
	Findings      []apiFinding     `json:"findings"`
	Links         []apiLink        `json:"links"`
	SiteLinks     []apiSiteLink    `json:"site_links"`
}

// apiHealthCounts are the numbers of the health line as stable machine
// values: the health line is text for people and can change.
type apiHealthCounts struct {
	Servers         int `json:"servers"` // every entry of servers[], disabled ones included
	ServersUp       int `json:"servers_up"`
	ServersDisabled int `json:"servers_disabled"`
	PCs             int `json:"pcs"`
	PCsOnline       int `json:"pcs_online"`
	PCsOffline      int `json:"pcs_offline"` // offline longer than pc_offline, seen by servers that are up
	PCsUnknown      int `json:"pcs_unknown"` // not connected, but a server that knows the PC is not up
}

type apiNotifications struct {
	Failing bool     `json:"failing"`
	Since   *apiTime `json:"since"`    // the first failure; null while notifications work
	NextTry *apiTime `json:"next_try"` // the next send while notifications fail; null otherwise
	Error   string   `json:"error,omitempty"`
}

// apiError answers an API request with a JSON error body.
func apiError(w http.ResponseWriter, code int, msg string) {
	b, _ := json.Marshal(map[string]string{"error": msg})
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_, _ = w.Write(append(b, '\n'))
}

// writeAPI answers an API request with v as indented JSON.
func writeAPI(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

// apiCaller is the caller of an API request that apiAuthorized let through.
type apiCaller struct {
	token *store.APIToken // the API token of the request; nil for a session or proxy mode
	admin bool            // the administrator role: the answer can have notifications.error
}

// scopeCovers reports whether an API token with the scope have can read an
// address that needs the scope need: full reads all of /api/v1, ha only
// /api/v1/ha.
func scopeCovers(have, need string) bool {
	return have == store.ScopeFull || (have == store.ScopeHA && need == store.ScopeHA)
}

// apiAuthorized lets a request through to the JSON API, or answers 401 or
// 403. need is the scope that the address needs: store.ScopeFull for
// /api/v1/status, store.ScopeHA for /api/v1/ha. A request with an API token
// passes when the scope of the API token covers need. A request without one
// passes only with a logged-in session (administrator or viewer) or in proxy
// mode: open access shows the dashboard pages, not the API. The
// scheme name is not case sensitive (RFC 7235): "bearer" is the same as
// "Bearer".
func (s *Server) apiAuthorized(w http.ResponseWriter, r *http.Request, need string) (apiCaller, bool) {
	info := s.resolve(r)
	caller := apiCaller{admin: info.Role == RoleAdmin}
	if scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " "); ok && strings.EqualFold(scheme, "Bearer") {
		tok = strings.TrimSpace(tok)
		t, err := s.st.APITokenByValue(tok)
		if err != nil {
			s.log.Error("cannot read the API tokens", "err", err)
			apiError(w, http.StatusInternalServerError, "internal error")
			return caller, false
		}
		if t == nil {
			ip := clientIP(r)
			if n := s.logs.count("token "+ip, s.now()); n > 0 {
				s.log.Warn("API request with an unknown token", "ip", ip, "path", r.URL.Path, "count", n)
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="syncwatch"`)
			apiError(w, http.StatusUnauthorized, "invalid token")
			return caller, false
		}
		// The API token is in use, also when its scope refuses the address.
		s.touchAPIToken(t, tok)
		if !scopeCovers(t.Scope, need) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="syncwatch", error="insufficient_scope", scope="`+need+`"`)
			apiError(w, http.StatusForbidden, "the API token permits only /api/v1/ha")
			return caller, false
		}
		caller.token = t
		return caller, true
	}
	if s.cfg.Get().Access.Mode == config.AccessProxy {
		return caller, true // the reverse proxy controls access
	}
	if info.Session != nil && roleAtLeast(info.Role, RoleViewer) {
		return caller, true
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="syncwatch"`)
	apiError(w, http.StatusUnauthorized, "token required")
	return caller, false
}

// handleAPIStatus serves /api/v1/status to an API token with the scope full,
// a logged-in session or proxy mode.
func (s *Server) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	c, ok := s.apiAuthorized(w, r, store.ScopeFull)
	if !ok {
		return
	}
	writeAPI(w, s.apiStatusOf(s.eng.View(), c.admin))
}

// apiStatusOf builds the answer of /api/v1/status from a view of the engine.
// admin adds the error text of a notification failure.
func (s *Server) apiStatusOf(v *engine.View, admin bool) apiStatus {
	now := s.now()
	cfg := s.cfg.Get()
	out := apiStatus{GeneratedAt: apiTime(now), Version: s.version, Health: notify.HealthLine(cfg, v, now), Counts: apiCounts(v, now),
		Servers: []apiServer{}, Devices: []apiDevice{}, Findings: []apiFinding{}}
	if s.hooks.Delivery != nil {
		if d := s.hooks.Delivery(); d.Failing {
			out.Notifications = apiNotifications{Failing: true, Since: atime(d.Since), NextTry: atime(d.NextTry)}
			// The error text can quote the channel's answer; like the
			// banner, only admins see it.
			if admin {
				out.Notifications.Error = d.Error
			}
		}
	}
	for _, sv := range v.Snapshot.Servers {
		a := apiServer{ID: sv.ID, Name: sv.Name, Site: cfg.SiteOf(sv.ID), Status: string(sv.Status), Version: sv.Version,
			LastContact: atime(sv.LastContact), CertExpires: atime(sv.Cert.NotAfter), LastStructureScan: atime(sv.LastStructureScan), Error: sv.LastError}
		if sv.Status == model.StatusUp {
			out.HealthCounts.ServersUp++
			if !sv.StartTime.IsZero() {
				up := int64(now.Sub(sv.StartTime).Seconds())
				a.UptimeS = &up
			}
		}
		c := countFolders(sv) // the same states as the dashboard
		a.Folders.Total, a.Folders.Idle, a.Folders.Syncing, a.Folders.Scanning = c.Total, c.Idle, c.Syncing, c.Scanning
		a.Folders.Error, a.Folders.WatchError, a.Folders.Paused = c.Errors, c.WatchErrors, c.Paused
		a.Folders.Stale = !folderStateKnown(sv)
		out.Servers = append(out.Servers, a)
	}
	// Disabled servers are listed too, as in /api/v1/ha, so they do not
	// just disappear from the list.
	for _, sc := range cfg.Servers {
		if sc.Disabled && v.Snapshot.Server(sc.ID) == nil {
			a := apiServer{ID: sc.ID, Name: sc.Name, Site: cfg.SiteOf(sc.ID), Status: "disabled"}
			a.Folders.Stale = true
			out.Servers = append(out.Servers, a)
			out.HealthCounts.ServersDisabled++
		}
	}
	out.HealthCounts.Servers = len(out.Servers)
	pcs := countPCs(v.Snapshot, cfg.Thresholds.PCOffline.D(), now)
	out.HealthCounts.PCs, out.HealthCounts.PCsOnline, out.HealthCounts.PCsOffline, out.HealthCounts.PCsUnknown = pcs.Total, pcs.Online, pcs.Offline, pcs.Unknown
	for _, d := range v.Snapshot.Devices() {
		out.Devices = append(out.Devices, apiDevice{ID: d.ID, Name: d.Name, Server: d.IsServer, Connected: d.Connected, LastSeen: atime(d.LastSeen), Version: d.Version})
	}
	for _, r := range v.Open {
		f := apiFinding{
			ID: r.ID, Check: r.Check, Checks: r.Checks, Severity: r.Severity.String(), Category: r.Category, Server: r.Server,
			Subject: r.Subject, Message: r.Message, Urgent: r.Urgent, Stale: r.Stale,
			OpenedAt: atime(r.OpenedAt), Since: atime(r.Since), Details: r.Details, Paths: r.Paths,
		}
		if z, snoozed := v.Snoozed(r.ID, now); snoozed {
			f.Snoozed, f.Ignored, f.SnoozeReason = true, z.Kind == store.IgnoreKind, z.Reason
			if !f.Ignored {
				f.SnoozeUntil = atime(z.Until)
			}
		}
		out.Findings = append(out.Findings, f)
	}
	out.Links, out.SiteLinks = apiLinks(v.Snapshot, cfg)
	return out
}

// apiCounts counts the open findings for counts of /api/v1/status and the
// findings_* values of /api/v1/ha: error, warn, info and urgent for the
// findings that are not snoozed or ignored; snoozed for the snoozed and the
// ignored findings, and ignored for the ignored findings alone.
func apiCounts(v *engine.View, now time.Time) map[string]int {
	out := map[string]int{"error": 0, "warn": 0, "info": 0, "snoozed": 0, "ignored": 0, "urgent": 0}
	for _, r := range v.Open {
		if z, snoozed := v.Snoozed(r.ID, now); snoozed {
			out["snoozed"]++ // ignored findings included, as before
			if z.Kind == store.IgnoreKind {
				out["ignored"]++
			}
			continue
		}
		out[strings.ToLower(r.Severity.String())]++
		if r.Urgent {
			out["urgent"]++
		}
	}
	return out
}

// apiLinks returns the links between the servers and between the sites of
// a snapshot, with the site of each server. Both lists are empty, not null,
// when the snapshot has none.
func apiLinks(snap *model.Snapshot, cfg *config.Config) ([]apiLink, []apiSiteLink) {
	links, sites := []apiLink{}, []apiSiteLink{}
	if snap == nil {
		return links, sites
	}
	for _, l := range snap.Links {
		links = append(links, apiLink{A: l.A, B: l.B, SiteA: cfg.SiteOf(l.A), SiteB: cfg.SiteOf(l.B), State: l.State, Since: atime(l.Since)})
	}
	for _, l := range snap.SiteLinks {
		sites = append(sites, apiSiteLink{A: l.A, B: l.B, State: l.State, Since: atime(l.Since)})
	}
	return links, sites
}
