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
	Status      string     `json:"status"`
	Version     string     `json:"version,omitempty"`
	UptimeS     int64      `json:"uptime_s,omitempty"`
	LastContact *time.Time `json:"last_contact,omitempty"`
	CertExpires *time.Time `json:"cert_expires,omitempty"`
	Error       string     `json:"error,omitempty"`
	Folders     struct {
		Total   int `json:"total"`
		Idle    int `json:"idle"`
		Syncing int `json:"syncing"`
		Error   int `json:"error"`
		Paused  int `json:"paused"`
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
	Snoozed  bool           `json:"snoozed"`
	OpenedAt time.Time      `json:"opened_at"`
	Since    time.Time      `json:"since"`
	Details  []model.Detail `json:"details,omitempty"`
	Paths    []string       `json:"paths,omitempty"`
}

type apiStatus struct {
	GeneratedAt   time.Time        `json:"generated_at"`
	Version       string           `json:"version"`
	Health        string           `json:"health"`
	Counts        map[string]int   `json:"counts"`
	Notifications apiNotifications `json:"notifications"`
	Servers       []apiServer      `json:"servers"`
	Devices       []apiDevice      `json:"devices"`
	Findings      []apiFinding     `json:"findings"`
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
			out.Notifications = apiNotifications{Failing: true, Since: tptr(d.Since.UTC()), Error: d.Error}
		}
	}
	for _, sv := range v.Snapshot.Servers {
		a := apiServer{ID: sv.ID, Name: sv.Name, Status: string(sv.Status), Version: sv.Version, LastContact: tptr(sv.LastContact), CertExpires: tptr(sv.Cert.NotAfter), Error: sv.LastError}
		if sv.Status == model.StatusUp && !sv.StartTime.IsZero() {
			a.UptimeS = int64(now.Sub(sv.StartTime).Seconds())
		}
		for _, f := range sv.Folders {
			a.Folders.Total++
			switch {
			case f.Paused:
				a.Folders.Paused++
			case f.State == "error" || f.Error != "" || f.Errors > 0 || f.PullErrors > 0:
				a.Folders.Error++
			case f.State == "idle" && f.NeedItems == 0:
				a.Folders.Idle++
			default:
				a.Folders.Syncing++
			}
		}
		out.Servers = append(out.Servers, a)
	}
	for _, d := range v.Snapshot.Devices() {
		out.Devices = append(out.Devices, apiDevice{ID: d.ID, Name: d.Name, Server: d.IsServer, Connected: d.Connected, LastSeen: tptr(d.LastSeen), Version: d.Version})
	}
	for _, r := range v.Open {
		_, snoozed := v.Snoozed(r.ID, now)
		out.Findings = append(out.Findings, apiFinding{
			ID: r.ID, Check: r.Check, Checks: r.Checks, Severity: r.Severity.String(), Category: r.Category, Server: r.Server,
			Subject: r.Subject, Message: r.Message, Urgent: r.Urgent, Stale: r.Stale, Snoozed: snoozed,
			OpenedAt: r.OpenedAt.UTC(), Since: r.Since.UTC(), Details: r.Details, Paths: r.Paths,
		})
		switch {
		case snoozed:
			out.Counts["snoozed"]++
		default:
			out.Counts[strings.ToLower(r.Severity.String())]++
			if r.Urgent {
				out.Counts["urgent"]++
			}
		}
	}
	for _, k := range []string{"error", "warn", "info", "snoozed", "urgent"} {
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
