package web

import (
	"math"
	"net/http"
	"slices"
	"time"

	"github.com/ivarstudios/syncwatch/internal/engine"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/store"
)

// haServer is one server's values for Home Assistant's RESTful sensors:
// flat, in GB and MB/s, keyed by server ID so templates stay short
// (value_json.servers.akka.status).
type haServer struct {
	Name           string   `json:"name"`
	Site           string   `json:"site"` // servers[].site, "" when not set
	Running        bool     `json:"running"`
	Status         string   `json:"status"` // one of the ha* status values
	LastContact    *apiTime `json:"last_contact"`
	FindingsOpen   int      `json:"findings_open"`   // open findings of the server that are not snoozed or ignored
	FindingsUrgent int      `json:"findings_urgent"` // the urgent ones of them
	Folders        int      `json:"folders"`
	FoldersSyncing int      `json:"folders_syncing"`
	FoldersScan    int      `json:"folders_scanning"`
	FoldersError   int      `json:"folders_error"`
	FoldersWatch   int      `json:"folders_watch_error"`
	FoldersPaused  int      `json:"folders_paused"`
	Received24hGB  float64  `json:"received_24h_gb"`
	Sent24hGB      float64  `json:"sent_24h_gb"`
	Moved24hGB     float64  `json:"moved_24h_gb"`
	Received7dGB   float64  `json:"received_7d_gb"`
	Sent7dGB       float64  `json:"sent_7d_gb"`
	Moved7dGB      float64  `json:"moved_7d_gb"`
	DownloadMBs    float64  `json:"download_mb_s"`
	UploadMBs      float64  `json:"upload_mb_s"`
}

type haStatus struct {
	GeneratedAt apiTime `json:"generated_at"`
	// The counts of the open findings that are not snoozed or ignored: the
	// numbers of counts in /api/v1/status.
	FindingsOpen              int                 `json:"findings_open"`
	FindingsUrgent            int                 `json:"findings_urgent"`
	FindingsError             int                 `json:"findings_error"`
	FindingsWarn              int                 `json:"findings_warn"`
	FindingsInfo              int                 `json:"findings_info"`
	NotificationsFailing      bool                `json:"notifications_failing"`
	NotificationsFailingSince *apiTime            `json:"notifications_failing_since"`
	Links                     []apiLink           `json:"links"`
	SiteLinks                 []apiSiteLink       `json:"site_links"`
	Servers                   map[string]haServer `json:"servers"`
}

// The status values of /api/v1/ha (W-049): machine values without spaces. A
// new text on the dashboard does not change them.
const (
	haUpToDate           = "up_to_date"
	haSyncing            = "syncing"
	haScanning           = "scanning"
	haWatchError         = "watch_error"
	haError              = "error" // only while an H4 finding with severity ERROR is open (W-054)
	haPaused             = "paused"
	haUnreachable        = "unreachable"         // status down
	haCertificateChanged = "certificate_changed" // status cert-changed
	haNotContacted       = "not_contacted"       // no full check after the start of syncwatch
	haDisabled           = "disabled"
)

func gb(bytes int64) float64           { return math.Round(float64(bytes)/1e6) / 1e3 }
func mbPerS(bytesPerS float64) float64 { return math.Round(bytesPerS/1e4) / 1e2 }

// handleAPIHA serves /api/v1/ha: per server whether Syncthing runs, its
// overall sync status, its open findings, the data moved in the last 24
// hours and 7 days, and the current transfer rate; and the counts of the
// findings, the state of the notifications and the links. Every API token
// can read it.
func (s *Server) handleAPIHA(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.apiAuthorized(w, r, store.ScopeHA); !ok {
		return
	}
	now := s.now()
	day, err := s.st.TrafficSince(now.Add(-24 * time.Hour))
	if err != nil {
		apiError(w, http.StatusInternalServerError, "reading traffic")
		return
	}
	week, err := s.st.TrafficSince(now.Add(-7 * 24 * time.Hour))
	if err != nil {
		apiError(w, http.StatusInternalServerError, "reading traffic")
		return
	}
	writeAPI(w, s.haStatusOf(s.eng.View(), day, week))
}

// haStatusOf builds the answer of /api/v1/ha from a view of the engine and
// the traffic of the last 24 hours and 7 days.
func (s *Server) haStatusOf(v *engine.View, day, week map[string]store.Traffic) haStatus {
	now := s.now()
	cfg := s.cfg.Get()
	counts := apiCounts(v, now)
	out := haStatus{GeneratedAt: apiTime(now), Servers: map[string]haServer{},
		FindingsOpen: counts["error"] + counts["warn"] + counts["info"], FindingsUrgent: counts["urgent"],
		FindingsError: counts["error"], FindingsWarn: counts["warn"], FindingsInfo: counts["info"]}
	if s.hooks.Delivery != nil {
		if d := s.hooks.Delivery(); d.Failing {
			out.NotificationsFailing, out.NotificationsFailingSince = true, atime(d.Since)
		}
	}
	out.Links, out.SiteLinks = apiLinks(v.Snapshot, cfg)
	open, urgent, h4 := haFindings(v, now)
	traffic := func(h *haServer, id string) {
		h.Received24hGB, h.Sent24hGB, h.Moved24hGB = gb(day[id].In), gb(day[id].Out), gb(day[id].In+day[id].Out)
		h.Received7dGB, h.Sent7dGB, h.Moved7dGB = gb(week[id].In), gb(week[id].Out), gb(week[id].In+week[id].Out)
	}
	// Every configured server is listed, disabled ones too, so the
	// templates in Home Assistant always find their key.
	for _, sc := range cfg.Servers {
		if sc.Disabled {
			h := haServer{Name: sc.Name, Site: cfg.SiteOf(sc.ID), Status: haDisabled, FindingsOpen: open[sc.ID], FindingsUrgent: urgent[sc.ID]}
			traffic(&h, sc.ID)
			out.Servers[sc.ID] = h
		}
	}
	for _, sv := range v.Snapshot.Servers {
		running := sv.Status == model.StatusUp
		h := haServer{Name: sv.Name, Site: cfg.SiteOf(sv.ID), Running: running, Status: haServerStatus(sv, h4[sv.ID]),
			LastContact: atime(sv.LastContact), FindingsOpen: open[sv.ID], FindingsUrgent: urgent[sv.ID], Folders: len(sv.Folders)}
		traffic(&h, sv.ID)
		for _, f := range sv.Folders {
			switch f.FolderSync() { // the same states as the dashboard
			case model.SyncSyncing:
				h.FoldersSyncing++
			case model.SyncScanning:
				h.FoldersScan++
			case model.SyncError:
				h.FoldersError++
			case model.SyncWatchError:
				h.FoldersWatch++
			case model.SyncPaused:
				h.FoldersPaused++
			}
		}
		// A rate older than two readings is stale (the server stopped answering).
		if running && now.Sub(sv.Rate.At) < 3*time.Minute {
			h.DownloadMBs, h.UploadMBs = mbPerS(sv.Rate.In), mbPerS(sv.Rate.Out)
		}
		out.Servers[sv.ID] = h
	}
	return out
}

// haFindings counts, by server ID, the open findings that are not snoozed
// or ignored and the urgent ones of them. A finding counts for its server and
// for each server in its related list (a PC that 2 servers know, for
// example). h4 has the servers with an open H4 finding of severity ERROR,
// snoozed or not.
func haFindings(v *engine.View, now time.Time) (open, urgent map[string]int, h4 map[string]bool) {
	open, urgent, h4 = map[string]int{}, map[string]int{}, map[string]bool{}
	for _, r := range v.Open {
		if r.Server != "" && r.Severity == model.Error && (r.Check == "H4" || slices.Contains(r.Checks, "H4")) {
			h4[r.Server] = true
		}
		if _, snoozed := v.Snoozed(r.ID, now); snoozed {
			continue
		}
		var ids []string
		for _, id := range append([]string{r.Server}, r.Related...) {
			if id != "" && !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
		}
		for _, id := range ids {
			open[id]++
			if r.Urgent {
				urgent[id]++
			}
		}
	}
	return open, urgent, h4
}

// haServerStatus is the status value of a polled server. A server that is
// up and checked has the worst state of its folders (error, watch_error,
// syncing, scanning, up_to_date), or paused when every folder is paused.
// h4 tells whether an H4 finding with severity ERROR is open for the server
// (W-054): only then a folder in error makes the status error. Syncthing
// repairs many failed items itself, and H4 waits for that before it opens.
// Without H4, such a folder counts as syncing (watch_error when its file
// watcher also failed); folders_error keeps the count of folders in error.
func haServerStatus(sv *model.Server, h4 bool) string {
	switch {
	case sv.Status == model.StatusDown:
		return haUnreachable
	case sv.Status == model.StatusCertChanged:
		return haCertificateChanged
	case sv.Status != model.StatusUp || !sv.Loaded:
		return haNotContacted
	}
	rank := map[string]int{haUpToDate: 0, haScanning: 1, haSyncing: 2, haWatchError: 3, haError: 4}
	worst, active := haUpToDate, 0
	for _, f := range sv.Folders {
		st := haFolderState(f, h4)
		if st == haPaused {
			continue
		}
		active++
		if rank[st] > rank[worst] {
			worst = st
		}
	}
	if active == 0 && len(sv.Folders) > 0 {
		return haPaused
	}
	return worst
}

// haFolderState is the state of a folder for the status of its server: the
// state of the dashboard (model.FolderSync), but an error counts only when
// h4 is true.
func haFolderState(f *model.Folder, h4 bool) string {
	switch f.FolderSync() {
	case model.SyncPaused:
		return haPaused
	case model.SyncError:
		switch {
		case h4:
			return haError
		case f.WatchError != "":
			return haWatchError
		}
		return haSyncing
	case model.SyncWatchError:
		return haWatchError
	case model.SyncScanning:
		return haScanning
	case model.SyncUpToDate:
		return haUpToDate
	}
	return haSyncing
}
