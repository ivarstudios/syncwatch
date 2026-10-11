package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The dead-man URL (SYNCWATCH_DEADMAN_URL) is the address of an outside
// monitor service, for example healthchecks.io or a push monitor of Uptime
// Kuma. After the evaluations of the checks, at most every deadmanEvery,
// syncwatch calls it while syncwatch is healthy (/healthz). When the calls
// stop, the service sends its own alarm: also when syncwatch is stopped or
// the host is off.
const (
	deadmanEvery     = 30 * time.Second
	deadmanTimeout   = 10 * time.Second
	deadmanBody      = 1 << 10 // syncwatch reads at most this much of an answer
	deadmanWarnEvery = time.Hour
)

// errDeadmanURL is the problem of a dead-man URL that syncwatch cannot use.
// It does not repeat the value: the path and the query can contain the
// secret of the monitor service.
var errDeadmanURL = errors.New("the value must be an http or https URL with a host name and without a user name or password.")

// ParseDeadmanURL checks a dead-man URL: http or https, a host name, no user
// name or password.
func ParseDeadmanURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" || u.User != nil || u.Hostname() == "" {
		return nil, errDeadmanURL
	}
	return u, nil
}

// deadman calls the dead-man URL.
type deadman struct {
	u      *url.URL
	client *http.Client
	ready  func(context.Context) []string // the reasons why syncwatch is not healthy (web.Server.Ready)
	log    *slog.Logger
	agent  string

	every, warnEvery time.Duration
	now              func() time.Time
	wake             chan struct{}

	// The state of the last tries, for the log (only run's goroutine).
	unready  bool      // the last try found syncwatch not healthy
	failing  bool      // the last call failed
	failures int       // failed calls after the last WARN line
	warnedAt time.Time // time of the last WARN line
}

func newDeadman(raw string, ready func(context.Context) []string, version string, log *slog.Logger) (*deadman, error) {
	u, err := ParseDeadmanURL(raw)
	if err != nil {
		return nil, fmt.Errorf("SYNCWATCH_DEADMAN_URL: %w", err)
	}
	return &deadman{
		u: u, ready: ready, log: log, agent: "syncwatch/" + version,
		client: &http.Client{
			Timeout:   deadmanTimeout,
			Transport: http.DefaultTransport.(*http.Transport).Clone(),
			// A redirect goes to an address that nobody configured.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		every: deadmanEvery, warnEvery: deadmanWarnEvery, now: time.Now,
		wake: make(chan struct{}, 1),
	}, nil
}

// target is the dead-man URL for the log: the scheme and the host, without
// the path and the query.
func (d *deadman) target() string { return d.u.Scheme + "://" + d.u.Host }

// Wake tells the pusher that an evaluation completed (engine.OnEvaluate).
// It does not wait.
func (d *deadman) Wake() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// run calls the URL after the evaluations until ctx ends: at the first
// evaluation after a try that is at least every old, or when that time comes
// for an evaluation that had to wait.
func (d *deadman) run(ctx context.Context) {
	var last time.Time // the last try
	pending := false   // an evaluation completed after the last try
	var due <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.wake:
			pending = true
		case <-due:
			due = nil
		}
		if !pending || due != nil {
			continue
		}
		if wait := d.every - d.now().Sub(last); !last.IsZero() && wait > 0 {
			due = time.After(wait)
			continue
		}
		pending, last = false, d.now()
		d.try(ctx)
	}
}

// try calls the URL when syncwatch is healthy, and logs the changes.
func (d *deadman) try(ctx context.Context) {
	reasons := d.ready(ctx)
	if ctx.Err() != nil {
		return // syncwatch stops: the database ping of ready failed for that reason
	}
	if len(reasons) > 0 {
		if !d.unready {
			d.unready = true
			d.log.Info("dead-man URL not called: syncwatch is not healthy", "reasons", strings.Join(reasons, ", "))
		}
		return
	}
	if d.unready {
		d.unready = false
		d.log.Info("dead-man URL called again: syncwatch is healthy")
	}
	if err := d.call(ctx); err != nil {
		if ctx.Err() != nil {
			return // syncwatch stops
		}
		d.failing = true
		d.failures++
		if d.warnedAt.IsZero() || d.now().Sub(d.warnedAt) >= d.warnEvery {
			d.log.Warn("cannot call the dead-man URL", "host", d.u.Host, "err", d.redact(err), "failures", d.failures)
			d.warnedAt, d.failures = d.now(), 0
		}
		return
	}
	if d.failing {
		d.failing = false
		d.log.Info("the dead-man URL answers again", "host", d.u.Host)
	}
}

// call sends GET to the URL. Only a 2xx answer counts.
func (d *deadman) call(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.u.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", d.agent)
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, deadmanBody))
	switch {
	case resp.StatusCode/100 == 2:
		return nil
	case resp.StatusCode/100 == 3:
		return fmt.Errorf("HTTP %d: syncwatch does not follow a redirect", resp.StatusCode)
	}
	return fmt.Errorf("HTTP %d", resp.StatusCode)
}

// redact returns the text of err without the URL, its path and its query:
// they can contain the secret of the monitor service.
func (d *deadman) redact(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err // the error without the URL
	}
	msg := err.Error()
	for _, s := range []string{d.u.String(), d.u.RequestURI(), d.u.EscapedPath(), d.u.Path, d.u.RawQuery} {
		if len(s) > 1 {
			msg = strings.ReplaceAll(msg, s, "…")
		}
	}
	return msg
}
