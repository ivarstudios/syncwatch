package web

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// HealthOptions are the rules of the health address /healthz.
type HealthOptions struct {
	Started     time.Time     // start of syncwatch
	NotifyLimit time.Duration // how long notifications can fail; 0 = this rule is off
	EvalLimit   time.Duration // how long syncwatch can be without an evaluation; 0 = this rule is off
}

// SetHealth sets the rules of /healthz. Call it before serving.
func (s *Server) SetHealth(o HealthOptions) { s.health = o }

// Ready returns the reasons why syncwatch is not healthy now, one line each.
// It returns none when syncwatch is healthy. syncwatch is not healthy when
// the database does not answer, when no evaluation of the checks completed
// for more than EvalLimit (counted from the start until the first one), and
// when the notifications fail for more than NotifyLimit. A notification
// failure shows only when syncwatch tries to send. The reasons contain no
// error text of Discord or of a server: /healthz needs no login.
func (s *Server) Ready(ctx context.Context) []string {
	var out []string
	if err := s.st.Ping(ctx); err != nil {
		out = append(out, "database not available")
	}
	now := s.now()
	if lim := s.health.EvalLimit; lim > 0 && s.eng != nil {
		last := s.eng.View().Evaluated
		if last.IsZero() {
			last = s.health.Started
		}
		if !last.IsZero() && now.Sub(last) > lim {
			out = append(out, "no evaluation of the checks from "+s.fmtTime(last))
		}
	}
	if lim := s.health.NotifyLimit; lim > 0 && s.hooks.Delivery != nil {
		if d := s.hooks.Delivery(); d.Failing && !d.Since.IsZero() && now.Sub(d.Since) > lim {
			out = append(out, "notifications fail from "+s.fmtTime(d.Since))
		}
	}
	return out
}

// handleHealthz answers the health check: 200 "ok", or 503 with the reasons.
// The address needs no login. The image healthcheck (syncwatch healthcheck)
// and outside checks read it.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	// An answer from a cache would hide a change of the state.
	w.Header().Set("Cache-Control", "no-store")
	if reasons := s.Ready(r.Context()); len(reasons) > 0 {
		http.Error(w, strings.Join(reasons, "\n"), http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}
