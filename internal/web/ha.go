package web

import (
	"encoding/json"
	"math"
	"net/http"
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
)

// haServer is one server's values for Home Assistant's RESTful sensors:
// flat, in GB and MB/s, keyed by server ID so templates stay short
// (value_json.servers.akka.status).
type haServer struct {
	Name           string  `json:"name"`
	Running        bool    `json:"running"`
	Status         string  `json:"status"` // up to date, syncing, scanning, error, paused, unknown, disabled
	Folders        int     `json:"folders"`
	FoldersSyncing int     `json:"folders_syncing"`
	FoldersError   int     `json:"folders_error"`
	FoldersPaused  int     `json:"folders_paused"`
	Received24hGB  float64 `json:"received_24h_gb"`
	Sent24hGB      float64 `json:"sent_24h_gb"`
	Moved24hGB     float64 `json:"moved_24h_gb"`
	Received7dGB   float64 `json:"received_7d_gb"`
	Sent7dGB       float64 `json:"sent_7d_gb"`
	Moved7dGB      float64 `json:"moved_7d_gb"`
	DownloadMBs    float64 `json:"download_mb_s"`
	UploadMBs      float64 `json:"upload_mb_s"`
}

type haStatus struct {
	GeneratedAt time.Time           `json:"generated_at"`
	Servers     map[string]haServer `json:"servers"`
}

func gb(bytes int64) float64           { return math.Round(float64(bytes)/1e6) / 1e3 }
func mbPerS(bytesPerS float64) float64 { return math.Round(bytesPerS/1e4) / 1e2 }

// handleAPIHA serves /api/v1/ha: per server whether Syncthing runs, its
// overall sync status, the data moved in the last 24 hours and 7 days, and
// the current transfer rate. Same access as /api/v1/status.
func (s *Server) handleAPIHA(w http.ResponseWriter, r *http.Request) {
	if !s.apiAuthorized(w, r) {
		return
	}
	now := s.now()
	day, err := s.st.TrafficSince(now.Add(-24 * time.Hour))
	if err != nil {
		http.Error(w, `{"error":"reading traffic"}`, http.StatusInternalServerError)
		return
	}
	week, err := s.st.TrafficSince(now.Add(-7 * 24 * time.Hour))
	if err != nil {
		http.Error(w, `{"error":"reading traffic"}`, http.StatusInternalServerError)
		return
	}
	out := haStatus{GeneratedAt: now.UTC(), Servers: map[string]haServer{}}
	// Every configured server is listed, disabled ones too, so the
	// templates in Home Assistant always find their key.
	for _, sc := range s.cfg.Get().Servers {
		if sc.Disabled {
			out.Servers[sc.ID] = haServer{Name: sc.Name, Status: "disabled",
				Received24hGB: gb(day[sc.ID].In), Sent24hGB: gb(day[sc.ID].Out), Moved24hGB: gb(day[sc.ID].In + day[sc.ID].Out),
				Received7dGB: gb(week[sc.ID].In), Sent7dGB: gb(week[sc.ID].Out), Moved7dGB: gb(week[sc.ID].In + week[sc.ID].Out)}
		}
	}
	for _, sv := range s.eng.View().Snapshot.Servers {
		running := sv.Status == model.StatusUp
		h := haServer{Name: sv.Name, Running: running, Status: sv.SyncStatus(), Folders: len(sv.Folders),
			Received24hGB: gb(day[sv.ID].In), Sent24hGB: gb(day[sv.ID].Out), Moved24hGB: gb(day[sv.ID].In + day[sv.ID].Out),
			Received7dGB: gb(week[sv.ID].In), Sent7dGB: gb(week[sv.ID].Out), Moved7dGB: gb(week[sv.ID].In + week[sv.ID].Out)}
		for _, f := range sv.Folders {
			switch f.FolderSync() {
			case model.SyncSyncing:
				h.FoldersSyncing++
			case model.SyncError:
				h.FoldersError++
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
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
}
