package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ivarstudios/syncwatch/internal/collector"
	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/engine"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/pathx"
)

// "Scan now": an administrator starts a structure scan of a server
// at once, from a structure finding on the Findings page (the server of the
// finding) or from Settings → Structure (each enabled server). The scan is
// the timed scan of the collector (collector.ScanNow): it only reads, one
// scan of a server runs at a time, and an administrator starts at most 1
// scan of a server in 30 seconds. Each structure finding shows the end of
// the last scan that listed its share (S3 to S7), or of the last scan of its
// server (S1, S2). The Overview tile of the server shows the end of its last
// scan.

// scanWait is how long the answer to "Scan now" waits for the scans and for
// the evaluation of their result. The page after a complete scan thus shows
// its findings; the live updates show the result of a longer scan.
var scanWait = 5 * time.Second

func (s *Server) handleScanNow(w http.ResponseWriter, r *http.Request, info requestInfo) {
	back := backTo(r)
	cfg := s.cfg.Get()
	var targets []config.Server
	if id := r.FormValue("server"); id != "" {
		sc, ok := cfg.ServerByKey(id)
		if !ok || sc.Disabled {
			s.redirect(w, r, back, "syncwatch cannot scan this server. The server is not in Settings → Servers, or it is disabled.", true)
			return
		}
		targets = append(targets, sc)
	} else {
		for _, sc := range cfg.Servers {
			if !sc.Disabled {
				targets = append(targets, sc)
			}
		}
	}
	if len(targets) == 0 {
		s.redirect(w, r, back, "There are no enabled servers. Add a server in Settings → Servers.", true)
		return
	}
	before := openStructure(s.eng.View())
	login := "password"
	if cfg.Access.Mode == config.AccessProxy {
		login = "proxy"
	}
	starts := make([]collector.ScanStart, len(targets))
	for i, sc := range targets {
		starts[i] = collector.ScanStart{State: collector.ScanNotUp}
		if s.hooks.ScanNow != nil {
			starts[i] = s.hooks.ScanNow(sc.ID)
		}
		if starts[i].State == collector.ScanStarted {
			s.log.Info("administrator started a structure scan", "server", sc.Name, "role", info.Role, "login", login, "ip", clientIP(r))
		}
	}

	// The scans, then an evaluation of their result. A scan counts as
	// ended only when its channel is closed: the wait can end first. A scan
	// that stopped before the end wrote no result: it needs no evaluation.
	ctx, cancel := context.WithTimeout(r.Context(), scanWait)
	defer cancel()
	ended := make([]bool, len(starts))
	settle := false
	for i, st := range starts {
		if st.Done == nil {
			continue
		}
		select {
		case <-st.Done:
		case <-ctx.Done():
		}
		select {
		case <-st.Done:
			ended[i] = true
			settle = settle || !scanStopped(st)
		default:
		}
	}
	if settle {
		s.eng.Settle(ctx, scanWait)
	}
	v := s.eng.View()
	after := openStructure(v)

	// The result of a scan shows on the Findings page: its list updates
	// live. Another page (Settings → Structure) names the Findings page.
	follows, is := "The result follows on this page.", "The result is on this page."
	if u, err := url.Parse(back); err != nil || u.Path != "/problems" {
		follows, is = "The Findings page shows the result after the scan.", "The Findings page shows the result."
	}
	var ok, refused []string
	for i, sc := range targets {
		st := starts[i]
		name := showControls(sc.Name)
		switch {
		case ended[i] && scanStopped(st):
			// The scan wrote no result, and the collector does not count
			// it: the next press can start a scan.
			refused = append(refused, scanStoppedText(name, st.End.Stopped))
		case st.State == collector.ScanStarted, st.State == collector.ScanRunning:
			// A scan that ended here is complete (End). It can have shares
			// that it could not list (scanCompleteText).
			sv := v.Snapshot.Server(sc.ID)
			if ended[i] && sv != nil {
				done, failed := scanCompleteText(name, sv, before[sc.ID], after[sc.ID])
				if done != "" {
					ok = append(ok, done)
				}
				refused = append(refused, failed...)
			} else {
				ok = append(ok, fmt.Sprintf("The structure scan of server %s started at %s. %s", name, s.fmtClockSeconds(st.Started), follows))
			}
		case st.State == collector.ScanTooSoon:
			result := is
			if st.Done != nil && !ended[i] {
				result = follows
			}
			refused = append(refused, fmt.Sprintf("The last structure scan of server %s started at %s. %s You can start the next scan at %s.",
				name, s.fmtClockSeconds(st.Started), result, s.fmtClockSeconds(st.Next)))
		default:
			refused = append(refused, notUpText(name, v.Snapshot.Server(sc.ID)))
		}
	}
	setFlash(w, "sw_flash_err", strings.Join(refused, "\n"))
	s.redirect(w, r, back, strings.Join(ok, "\n"), false)
}

// openStructure gives the IDs of the open structure findings of each server.
func openStructure(v *engine.View) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, rec := range v.Open {
		if rec.Category != model.CatStructure || rec.Server == "" {
			continue
		}
		if out[rec.Server] == nil {
			out[rec.Server] = map[string]bool{}
		}
		out[rec.Server][rec.ID] = true
	}
	return out
}

// scanCompleteText is the answer for a complete scan: the findings that it
// resolved and opened, and the open structure findings of the server. Each
// share that the scan could not list gets a red sentence: its findings show
// the last known state. A scan that listed no share gets no green answer.
func scanCompleteText(name string, sv *model.Server, before, after map[string]bool) (done string, failed []string) {
	shares := 0
	if sv.Structure != nil {
		shares = len(sv.Structure.Shares)
		for _, sh := range sv.Structure.Shares {
			if sh.Error != "" {
				failed = append(failed, fmt.Sprintf("The findings of share %s on server %s show the last known state. syncwatch cannot list the share: %s.",
					showControls(sh.Name), name, strings.TrimSuffix(showControls(sh.Error), ".")))
			}
		}
	}
	if shares > 0 && len(failed) == shares {
		return "", failed
	}
	resolved, opened := 0, 0
	for id := range before {
		if !after[id] {
			resolved++
		}
	}
	for id := range after {
		if !before[id] {
			opened++
		}
	}
	out := []string{"The structure scan of server " + name + " is complete."}
	if len(failed) > 0 {
		out[0] = fmt.Sprintf("The structure scan of server %s is complete for %d of %d shares.", name, shares-len(failed), shares)
	}
	if resolved > 0 {
		out = append(out, "syncwatch resolved "+plural(resolved, "finding", "findings")+".")
	}
	if opened > 0 {
		out = append(out, "syncwatch opened "+plural(opened, "new finding", "new findings")+".")
	}
	switch n := len(after); n {
	case 0:
		out = append(out, "The server has no open structure findings.")
	case 1:
		out = append(out, "1 structure finding of the server is open.")
	default:
		out = append(out, fmt.Sprintf("%d structure findings of the server are open.", n))
	}
	return strings.Join(out, " "), failed
}

// scanStopped reports whether the scan of st stopped before the end. Read
// it only after st.Done is closed.
func scanStopped(st collector.ScanStart) bool {
	return st.End != nil && st.End.Stopped != ""
}

// scanStoppedText is the answer for a scan that stopped before the end
// (collector.ScanEnd): the scan wrote no result.
func scanStoppedText(name, why string) string {
	reason := "the connection to the server ended"
	if why == collector.ScanStopNotLoaded {
		reason = "the first full check of the server is not complete"
	}
	return fmt.Sprintf("The structure scan of server %s stopped before the end: %s. Try again in 1 minute.", name, reason)
}

// notUpText is the answer for a server that syncwatch cannot scan now: its
// status, a first full check that is not complete, or no connection while
// the dashboard still shows the server as up (before down_after_failures).
func notUpText(name string, sv *model.Server) string {
	switch {
	case sv == nil:
	case sv.Status == model.StatusDown:
		why := ""
		if e := showControls(sv.LastError); e != "" {
			why = ": " + e
		}
		return fmt.Sprintf("Server %s is unreachable%s. syncwatch can scan it when it is up again.", name, why)
	case sv.Status == model.StatusCertChanged:
		return fmt.Sprintf("The certificate of server %s changed. Accept the certificate in Settings → Servers. Then start the scan again.", name)
	case sv.Status == model.StatusUp && !sv.Loaded:
		return fmt.Sprintf("The first full check of server %s is not complete. Try again in 1 minute.", name)
	}
	return fmt.Sprintf("syncwatch is not connected to server %s now. Try again in 1 minute.", name)
}

// fmtClockSeconds writes a time with seconds: 21:52:07 today (in the
// configured zone), else with the date, as fmtClock does.
func (s *Server) fmtClockSeconds(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	loc := s.cfg.Get().Location()
	if lt := t.In(loc); lt.Format("2006-01-02") == s.now().In(loc).Format("2006-01-02") {
		return lt.Format("15:04:05")
	}
	return t.In(loc).Format("2006-01-02 15:04:05")
}

// agoWords says how long ago something was, in one unit: "less than 1
// minute ago", "3 minutes ago", "2 hours ago", "1 day ago". app.js has the
// same words (agoWords), and counts on while a page is open.
func agoWords(d time.Duration) string {
	m := int(max(d, 0) / time.Minute)
	switch {
	case m < 1:
		return "less than 1 minute ago"
	case m < 60:
		return plural(m, "minute", "minutes") + " ago"
	case m < 24*60:
		return plural(m/60, "hour", "hours") + " ago"
	default:
		return plural(m/(24*60), "day", "days") + " ago"
	}
}

// lastScan is the time of the last structure scan of a server, or of the
// share of a structure finding, as the dashboard shows it: the clock time
// (with the date when it is not today), how long ago, and that age in
// seconds for app.js.
type lastScan struct {
	Server   string // the name of the server (Settings → Structure)
	At, Ago  string
	AgeS     int64
	Complete bool   // a scan (that listed the share) ended after the start of syncwatch
	Share    bool   // the time is that of the share of the finding
	Error    string // why syncwatch cannot list the share now: the time is that of its last listing
}

// lastScanOf gives the last structure scan of a server (f nil), or of a
// structure finding: the end of the last scan that listed the share of the
// finding (S3 to S7), else of the server (S1, S2). The findings of a share
// that the scan cannot list keep the last known state (the engine shows
// them as stale): they show the time of its last listing and the error.
func (s *Server) lastScanOf(sv *model.Server, f *model.Finding) lastScan {
	ls := lastScan{Server: showControls(sv.Name)}
	t := sv.LastStructureScan
	if sh := findingShare(sv, f); sh != nil {
		t, ls.Share, ls.Error = sh.ListedAt, true, showControls(sh.Error)
	}
	if !t.IsZero() {
		age := max(s.now().Sub(t), 0)
		ls.At, ls.Ago, ls.AgeS, ls.Complete = s.fmtClock(t), agoWords(age), int64(age/time.Second), true
	}
	return ls
}

// findingShare gives the share that has the path of a structure finding
// (its Path detail) inside it, as the engine finds the findings of a share
// that the scan cannot list. Nil for f nil, and for a finding without such
// a path (S1, S2).
func findingShare(sv *model.Server, f *model.Finding) *model.Share {
	if f == nil || sv.Structure == nil {
		return nil
	}
	p := f.DetailValue("Path")
	if p == "" {
		return nil
	}
	st := pathx.StyleFor(sv.PathSep)
	var best *model.Share
	for _, sh := range sv.Structure.Shares {
		if st.Within(p, sh.Path) && (best == nil || len(sh.Path) > len(best.Path)) {
			best = sh
		}
	}
	return best
}

// lastScans gives the last structure scan of each server that syncwatch
// polls, for Settings → Structure.
func (s *Server) lastScans(v *engine.View) []lastScan {
	var out []lastScan
	if v == nil || v.Snapshot == nil {
		return nil
	}
	for _, sv := range v.Snapshot.Servers {
		out = append(out, s.lastScanOf(sv, nil))
	}
	return out
}
