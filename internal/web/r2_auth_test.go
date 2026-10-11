package web

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/engine"
	"github.com/ivarstudios/syncwatch/internal/notify"
)

// healthz returns the status and the body of /healthz.
func (e *env) healthz() (int, string) {
	e.t.Helper()
	resp, body := e.get("/healthz")
	return resp.StatusCode, body
}

// /healthz answers 503 when the notifications fail for more than the limit
// (SYNCWATCH_HEALTH_NOTIFY_LIMIT), with the time and without the error text
// of Discord: the address needs no login. A shorter failure is healthy.
func TestHealthzNotificationFailure(t *testing.T) {
	e := newEnv(t, twoServers)
	e.web.SetHealth(HealthOptions{Started: time.Now().Add(-time.Hour), NotifyLimit: 15 * time.Minute, EvalLimit: 5 * time.Minute})
	if code, body := e.healthz(); code != http.StatusOK || body != "ok\n" {
		t.Fatalf("healthy: %d %q", code, body)
	}
	since := time.Now().Add(-16 * time.Minute)
	e.setDelivery(notify.Delivery{Failing: true, Since: since, Error: "Discord: HTTP 404 Unknown Webhook (secret-hook)"})
	code, body := e.healthz()
	if code != http.StatusServiceUnavailable || body != "notifications fail from "+e.web.fmtTime(since)+"\n" {
		t.Fatalf("16 minutes of failed notifications: %d %q", code, body)
	}
	if strings.Contains(body, "Discord") || strings.Contains(body, "secret-hook") {
		t.Fatalf("the error text of Discord is in the answer: %q", body)
	}
	e.setDelivery(notify.Delivery{Failing: true, Since: time.Now().Add(-14 * time.Minute), Error: "Discord: HTTP 500"})
	if code, body := e.healthz(); code != http.StatusOK {
		t.Fatalf("14 minutes of failed notifications: %d %q", code, body)
	}
	// The rule is off without a limit (tests and older callers).
	e.web.SetHealth(HealthOptions{})
	e.setDelivery(notify.Delivery{Failing: true, Since: time.Now().Add(-48 * time.Hour)})
	if code, body := e.healthz(); code != http.StatusOK {
		t.Fatalf("without the rule: %d %q", code, body)
	}
}

// /healthz answers 503 when no evaluation of the checks completed for more
// than the limit: counted from the last evaluation, and from the start until
// the first one.
func TestHealthzEvaluations(t *testing.T) {
	e := newEnv(t, twoServers)
	evaluated := e.eng.View().Evaluated
	if evaluated.IsZero() {
		t.Fatal("the test engine did not evaluate")
	}
	e.web.SetHealth(HealthOptions{Started: evaluated.Add(-time.Minute), NotifyLimit: 15 * time.Minute, EvalLimit: 5 * time.Minute})
	e.web.now = func() time.Time { return evaluated.Add(4 * time.Minute) }
	if code, body := e.healthz(); code != http.StatusOK {
		t.Fatalf("4 minutes after the last evaluation: %d %q", code, body)
	}
	e.web.now = func() time.Time { return evaluated.Add(6 * time.Minute) }
	if code, body := e.healthz(); code != http.StatusServiceUnavailable || body != "no evaluation of the checks from "+e.web.fmtTime(evaluated)+"\n" {
		t.Fatalf("6 minutes after the last evaluation: %d %q", code, body)
	}

	// An engine that did not evaluate yet: counted from the start.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng, err := engine.New(e.st, e.hub, e.cfg, nil, log)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(e.cfg, e.st, eng, Hooks{}, "test", log)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	s.SetHealth(HealthOptions{Started: started, EvalLimit: 5 * time.Minute})
	ctx := t.Context()
	s.now = func() time.Time { return started.Add(4 * time.Minute) }
	if r := s.Ready(ctx); len(r) != 0 {
		t.Fatalf("4 minutes after the start without an evaluation: %q", r)
	}
	s.now = func() time.Time { return started.Add(6 * time.Minute) }
	if r := s.Ready(ctx); len(r) != 1 || r[0] != "no evaluation of the checks from "+s.fmtTime(started) {
		t.Fatalf("6 minutes after the start without an evaluation: %q", r)
	}
}

// A database that does not answer makes /healthz answer 503, with one line
// for each reason.
func TestHealthzDatabaseAndReasons(t *testing.T) {
	e := newEnv(t, twoServers)
	evaluated := e.eng.View().Evaluated
	e.web.SetHealth(HealthOptions{Started: evaluated, NotifyLimit: 15 * time.Minute, EvalLimit: 5 * time.Minute})
	e.web.now = func() time.Time { return evaluated.Add(20 * time.Minute) }
	e.setDelivery(notify.Delivery{Failing: true, Since: evaluated.Add(time.Minute)})
	if err := e.st.Close(); err != nil {
		t.Fatal(err)
	}
	code, body := e.healthz()
	want := "database not available\nno evaluation of the checks from " + e.web.fmtTime(evaluated) + "\nnotifications fail from " + e.web.fmtTime(evaluated.Add(time.Minute)) + "\n"
	if code != http.StatusServiceUnavailable || body != want {
		t.Fatalf("closed database: %d %q, want %q", code, body, want)
	}
}
