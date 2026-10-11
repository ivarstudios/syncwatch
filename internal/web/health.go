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
// It returns none when syncwatch is healthy.
func (s *Server) Ready(ctx context.Context) []string {
	if err := s.st.Ping(ctx); err != nil {
		return []string{"database not available"}
	}
	return nil
}

// handleHealthz answers the health check: 200 "ok", or 503 with the reasons.
// The address needs no login.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if reasons := s.Ready(r.Context()); len(reasons) > 0 {
		http.Error(w, strings.Join(reasons, "\n"), http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}
