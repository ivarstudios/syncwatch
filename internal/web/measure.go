package web

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/ivarstudios/syncwatch/internal/stclient"
)

// maxMeasure caps the folders of one Measure request: one row of the
// Folders page has a folder on at most each server.
const maxMeasure = 20

// handleMeasure asks the gates to measure the size of a project folder that
// is not in Syncthing, for an administrator: the ⟳ button of a row on the
// Folders page. The gates only read; the size shows on the page when the
// measurement is complete.
func (s *Server) handleMeasure(w http.ResponseWriter, r *http.Request, info requestInfo) {
	back := "/grid"
	if b := r.FormValue("back"); strings.HasPrefix(b, "/grid") {
		back = safeNext(b)
	}
	_ = r.ParseForm()
	servers, paths := r.Form["server"], r.Form["path"]
	if len(servers) == 0 || len(servers) != len(paths) || len(servers) > maxMeasure {
		s.redirect(w, r, back, "The request is not complete. Reload the page and try again.", true)
		return
	}
	cfg := s.cfg.Get()
	var started, failed []string
	for i, id := range servers {
		sc, ok := cfg.ServerByKey(id)
		if !ok || sc.Disabled {
			failed = append(failed, "syncwatch cannot measure on a server that is not in Settings → Servers, or that is disabled")
			continue
		}
		err := errors.New("syncwatch cannot measure now")
		if s.hooks.MeasureSize != nil {
			err = s.hooks.MeasureSize(sc.ID, paths[i])
		}
		if err != nil {
			if errors.Is(err, stclient.ErrNoSize) {
				err = errors.New("its gate measures no sizes: update the gate to the version of syncwatch")
			}
			failed = append(failed, fmt.Sprintf("%s: %v", sc.Name, err))
			continue
		}
		started = append(started, sc.Name)
		s.log.Info("administrator started a folder size measurement", "server", sc.Name, "path", paths[i], "role", info.Role, "ip", clientIP(r))
	}
	name := paths[0]
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	msg := ""
	if len(started) > 0 {
		msg = fmt.Sprintf("The gate of %s measures %s now. The size shows on this page when the measurement is complete.", strings.Join(started, " and "), name)
	}
	if len(failed) > 0 {
		msg = strings.TrimSpace(msg + " syncwatch cannot measure " + name + ": " + strings.Join(failed, "; ") + ".")
	}
	s.redirect(w, r, back, msg, len(started) == 0)
}
