package web

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/notify"
	"github.com/ivarstudios/syncwatch/internal/store"
)

type apiServer struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	Status      string     `json:"status"` // up, down, cert-changed, unknown (not contacted), disabled
	Version     string     `json:"version,omitempty"`
	UptimeS     *int64     `json:"uptime_s"` // null while the server is not up
	LastContact *time.Time `json:"last_contact,omitempty"`
	CertExpires *time.Time `json:"cert_expires,omitempty"`
	Error       string     `json:"error,omitempty"`
	Folders     struct {
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
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Server    bool       `json:"server"`
	Connected bool       `json:"connected"`
	LastSeen  *time.Time `json:"last_seen,omitempty"`
	Version   string     `json:"version,omitempty"`
}

type apiFinding struct {
	ID       string         `json:"id"`
	Check    string         `json:"check"`
	Checks   []string       `json:"checks"`
	Severity string         `json:"severity"`
	Category string         `json:"category"`
	Server   string         `json:"server,omitempty"`
	Subject  string         `json:"subject"`
	Message  string         `json:"message"`
	Urgent   bool           `json:"urgent"`
	Stale    bool           `json:"stale"`
	Snoozed  bool           `json:"snoozed"` // snoozed or ignored
	Ignored  bool           `json:"ignored"`
	OpenedAt time.Time      `json:"opened_at"`
	Since    time.Time      `json:"since"`
	Details  []model.Detail `json:"details,omitempty"`
	Paths    []string       `json:"paths,omitempty"`
}

type apiStatus struct {
	GeneratedAt   time.Time        `json:"generated_at"`
	Version       string           `json:"version"`
	Health        string           `json:"health"`
	HealthCounts  apiHealthCounts  `json:"health_counts"`
	Counts        map[string]int   `json:"counts"`
	Notifications apiNotifications `json:"notifications"`
	Servers       []apiServer      `json:"servers"`
	Devices       []apiDevice      `json:"devices"`
	Findings      []apiFinding     `json:"findings"`
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
	Failing bool       `json:"failing"`
	Since   *time.Time `json:"since,omitempty"`
	Error   string     `json:"error,omitempty"`
}

func tptr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// apiAuthorized lets a bearer token or a logged-in viewer through to the
// JSON API, and answers 401 otherwise.
func (s *Server) apiAuthorized(w http.ResponseWriter, r *http.Request) bool {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		want, err := s.st.Secret(store.SecretAPIToken)
		got := strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
		if err == nil && want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1 {
			return true
		}
		w.Header().Set("WWW-Authenticate", `Bearer realm="syncwatch"`)
		http.Error(w, `{"error":"invalid token"}`, http.StatusUnauthorized)
		return false
	}
	if roleAtLeast(s.resolve(r).Role, RoleViewer) {
		return true
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="syncwatch"`)
	http.Error(w, `{"error":"token required"}`, http.StatusUnauthorized)
	return false
}

// handleAPIStatus serves /api/v1/status to a bearer token, or to a logged-in viewer.
func (s *Server) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	if !s.apiAuthorized(w, r) {
		return
	}
	v := s.eng.View()
	now := s.now()
	cfg := s.cfg.Get()
	out := apiStatus{GeneratedAt: now.UTC(), Version: s.version, Health: notify.HealthLine(cfg, v, now), Counts: map[string]int{}}
	if s.hooks.Delivery != nil {
		if d := s.hooks.Delivery(); d.Failing {
			out.Notifications = apiNotifications{Failing: true, Since: tptr(d.Since.UTC())}
			// The error text can quote the channel's answer; like the
			// banner, only admins see it.
			if s.resolve(r).Role == RoleAdmin {
				out.Notifications.Error = d.Error
			}
		}
	}
	for _, sv := range v.Snapshot.Servers {
		a := apiServer{ID: sv.ID, Name: sv.Name, Status: string(sv.Status), Version: sv.Version, LastContact: tptr(sv.LastContact), CertExpires: tptr(sv.Cert.NotAfter), Error: sv.LastError}
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
			a := apiServer{ID: sc.ID, Name: sc.Name, Status: "disabled"}
			a.Folders.Stale = true
			out.Servers = append(out.Servers, a)
			out.HealthCounts.ServersDisabled++
		}
	}
	out.HealthCounts.Servers = len(out.Servers)
	pcs := countPCs(v.Snapshot, cfg.Thresholds.PCOffline.D(), now)
	out.HealthCounts.PCs, out.HealthCounts.PCsOnline, out.HealthCounts.PCsOffline, out.HealthCounts.PCsUnknown = pcs.Total, pcs.Online, pcs.Offline, pcs.Unknown
	for _, d := range v.Snapshot.Devices() {
		out.Devices = append(out.Devices, apiDevice{ID: d.ID, Name: d.Name, Server: d.IsServer, Connected: d.Connected, LastSeen: tptr(d.LastSeen), Version: d.Version})
	}
	for _, r := range v.Open {
		z, snoozed := v.Snoozed(r.ID, now)
		ignored := snoozed && z.Kind == store.IgnoreKind
		out.Findings = append(out.Findings, apiFinding{
			ID: r.ID, Check: r.Check, Checks: r.Checks, Severity: r.Severity.String(), Category: r.Category, Server: r.Server,
			Subject: r.Subject, Message: r.Message, Urgent: r.Urgent, Stale: r.Stale, Snoozed: snoozed, Ignored: ignored,
			OpenedAt: r.OpenedAt.UTC(), Since: r.Since.UTC(), Details: r.Details, Paths: r.Paths,
		})
		switch {
		case snoozed:
			out.Counts["snoozed"]++ // ignored findings included, as before
			if ignored {
				out.Counts["ignored"]++
			}
		default:
			out.Counts[strings.ToLower(r.Severity.String())]++
			if r.Urgent {
				out.Counts["urgent"]++
			}
		}
	}
	for _, k := range []string{"error", "warn", "info", "snoozed", "ignored", "urgent"} {
		out.Counts[k] += 0
	}
	if out.Findings == nil {
		out.Findings = []apiFinding{}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}
