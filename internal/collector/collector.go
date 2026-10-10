// Package collector reads each Syncthing server through stclient and keeps
// its state in the hub: a live event loop, a periodic full re-check, an
// hourly structure scan and a polled fallback mode.
package collector

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/stclient"
	"github.com/ivarstudios/syncwatch/internal/store"
)

// Collector polls one server.
type Collector struct {
	id     string
	hub    *model.Hub
	st     *store.Store
	cfg    *config.Holder
	log    *slog.Logger
	client *stclient.Client

	wake        chan struct{}
	fullNow     chan struct{}
	lastEventID int64
	startTime   time.Time
	failures    int
	firstFail   time.Time
	certFails   int       // failed sessions in a row because the certificate changed
	certWarned  time.Time // when the certificate change was last logged at WARN
	lastFull    time.Time
	needFull    bool // a full check was started and hasn't completed
	lastScan    time.Time
	lastBase    time.Time
	scanMu      sync.Mutex
	scanning    bool

	trafficEvery time.Duration // how often traffic counters are read
}

func newCollector(sc config.Server, apiKey string, hub *model.Hub, st *store.Store, cfg *config.Holder, log *slog.Logger) (*Collector, error) {
	pin, err := st.Pin(sc.ID)
	if err != nil {
		return nil, err
	}
	c := &Collector{
		id: sc.ID, hub: hub, st: st, cfg: cfg, log: log.With("server", sc.Name),
		wake: make(chan struct{}, 1), fullNow: make(chan struct{}, 1), trafficEvery: trafficEvery,
	}
	c.client, err = stclient.New(stclient.Options{
		Name:    sc.Name,
		URL:     sc.URL,
		APIKey:  apiKey,
		Pin:     pin,
		Timeout: cfg.Get().Collector.RequestTimeout.D(),
		OnPin: func(fp string) {
			if err := st.SetPin(sc.ID, fp); err != nil {
				c.log.Error("saving pinned certificate", "err", err)
			}
			c.log.Info("trusting self-signed certificate on first use", "fingerprint", stclient.ShortFP(fp))
		},
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}

// Wake makes the collector retry now instead of waiting for its backoff.
func (c *Collector) Wake() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// FullCheckNow asks for a full re-check at the next opportunity.
func (c *Collector) FullCheckNow() {
	select {
	case c.fullNow <- struct{}{}:
	default:
	}
	c.Wake()
}

func (c *Collector) update(fn func(*model.Server)) { c.hub.Update(c.id, fn) }

// Backoff between failed sessions: backoffStep per failure in a row, up to
// backoffMax. After a certificate change nothing changes until an admin
// accepts it (which wakes the collector), so those retries slow down to
// one every certBackoffMax.
var (
	backoffStep    = 10 * time.Second
	backoffMax     = time.Minute
	certBackoffMax = 5 * time.Minute
)

// Run loops until ctx is cancelled.
func (c *Collector) Run(ctx context.Context) {
	defer c.client.Close()
	go c.trafficLoop(ctx)
	for ctx.Err() == nil {
		// The session's context also bounds the structure scan it starts,
		// so a scan stops when the server goes down.
		sctx, cancel := context.WithCancel(ctx)
		err := c.session(sctx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		c.failed(err)
		// A wake-up that arrived while the session was running (an
		// in-session FullCheckNow, a pin reset while the server was up) is
		// stale: it must not turn this backoff into an immediate retry.
		select {
		case <-c.wake:
		default:
		}
		select {
		case <-ctx.Done():
			return
		case <-c.wake:
		case <-time.After(c.backoff(err)):
		}
	}
}

// backoff is the wait before the next session after a failed one.
func (c *Collector) backoff(err error) time.Duration {
	n, limit := c.failures, backoffMax
	if _, ok := stclient.IsCertChanged(err); ok {
		n, limit = c.certFails, certBackoffMax
	}
	return min(max(time.Duration(n)*backoffStep, backoffStep), limit)
}

// session connects, runs a full check and then follows events (or polls)
// until something fails.
func (c *Collector) session(ctx context.Context) error {
	status, err := c.client.SystemStatus(ctx)
	if err != nil {
		return err
	}
	restarted := !status.StartTime.Equal(c.startTime)
	if restarted {
		if !c.startTime.IsZero() {
			c.log.Info("Syncthing restarted; re-reading everything", "startTime", status.StartTime)
		}
		c.startTime = status.StartTime
		c.lastEventID = 0
	}
	cfg := c.cfg.Get().Collector
	mode := cfg.Mode
	c.update(func(s *model.Server) { s.Mode = mode })
	// After a dropped connection (a VPN or firewall closing an idle
	// long-poll, a short outage) the state from the last full check is still
	// current if Syncthing didn't restart: Syncthing keeps buffering events
	// for the subscription, so the event stream can simply be resumed.
	// Re-reading everything on every reconnect would hammer the server with
	// db/status and db/completion calls while the link is flaky.
	recent := !restarted && !c.lastFull.IsZero() && !c.needFull

	if mode == config.ModeEvents {
		// Prime the event subscription first so nothing that happens during
		// the full check is missed.
		evs, err := c.client.Events(ctx, c.lastEventID, time.Second, stclient.EventMask)
		if err != nil {
			return err
		}
		if recent && time.Since(c.lastFull) < cfg.FullCheckInterval.D() && continues(evs, c.lastEventID) {
			c.log.Debug("reconnected; resuming events without a full re-check", "since", c.lastEventID)
			for _, e := range evs {
				if e.ID > c.lastEventID {
					c.lastEventID = e.ID
					c.apply(ctx, e)
				}
			}
			c.succeeded()
			c.maybeScan(ctx)
			return c.eventLoop(ctx)
		}
		for _, e := range evs {
			if e.ID > c.lastEventID {
				c.lastEventID = e.ID
			}
		}
	} else if recent && time.Since(c.lastFull) < cfg.PollInterval.D() {
		// Polled mode: the next full check is due at lastFull + poll_interval anyway.
		c.succeeded()
		return c.pollLoop(ctx)
	}
	if err := c.fullCheck(ctx); err != nil {
		return err
	}
	// Only a completed full check counts as "up": a server that answers
	// /rest/system/status but fails the re-check must not look healthy
	// while its data goes stale.
	c.succeeded()
	c.maybeScan(ctx)
	if mode == config.ModePolled {
		return c.pollLoop(ctx)
	}
	return c.eventLoop(ctx)
}

// continues reports whether evs (the answer to "events since last") follow on
// from last without a gap and without the IDs going backwards.
func continues(evs []stclient.Event, last int64) bool {
	if len(evs) == 0 {
		return true
	}
	return evs[0].ID <= last+1 && evs[len(evs)-1].ID >= last
}

func (c *Collector) succeeded() {
	c.failures = 0
	c.firstFail = time.Time{}
	c.certFails = 0
	c.certWarned = time.Time{}
	cert := c.client.LastCert()
	c.update(func(s *model.Server) {
		s.Status = model.StatusUp
		s.LastError = ""
		s.DownSince = time.Time{}
		s.CertChange = nil
		s.LastContact = time.Now()
		s.Cert = model.CertInfo{Fingerprint: cert.Fingerprint, NotAfter: cert.NotAfter, CASigned: cert.CASigned}
	})
}

func (c *Collector) failed(err error) {
	if err == nil {
		return
	}
	now := time.Now()
	if ce, ok := stclient.IsCertChanged(err); ok {
		c.certFails++
		fresh := false
		c.update(func(s *model.Server) {
			if s.CertChange == nil || s.CertChange.New != ce.New {
				s.CertChange = &model.CertChange{Old: ce.Old, New: ce.New, NotAfter: ce.NotAfter, SeenAt: now}
				fresh = true
			}
			s.Status = model.StatusCertChanged
			s.LastError = err.Error()
		})
		// Nothing changes until an admin accepts the certificate, so the
		// warning is repeated at most once an hour.
		level := slog.LevelDebug
		if fresh || now.Sub(c.certWarned) >= time.Hour {
			level, c.certWarned = slog.LevelWarn, now
		}
		c.log.Log(context.Background(), level, "certificate changed; not connecting until it is accepted", "old", stclient.ShortFP(ce.Old), "new", stclient.ShortFP(ce.New))
		return
	}
	if c.failures == 0 {
		c.firstFail = now
	}
	c.failures++
	msg := shortErr(err)
	c.log.Warn("request failed", "err", msg, "failures", c.failures)
	downAfter := c.cfg.Get().Collector.DownAfter
	c.update(func(s *model.Server) {
		s.LastError = msg
		if c.failures >= downAfter && s.Status != model.StatusDown {
			s.Status = model.StatusDown
			s.DownSince = c.firstFail
		}
	})
}

// shortErr turns an error into a short text for the dashboard, the API and
// notifications: HTTP errors as they are, the common network failures as a
// phrase, anything else trimmed.
func shortErr(err error) string {
	var se *stclient.StatusError
	if errors.As(err, &se) {
		return se.Error()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timed out"
	}
	s := err.Error()
	for _, p := range netErrs {
		if strings.Contains(s, p.match) {
			return p.text
		}
	}
	// The client strips the "Get <url>" prefix of transport errors, which
	// can leave a bare ": EOF".
	s = strings.TrimLeft(s, ": ")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// netErrs maps the wording of Go's network errors (Linux and Windows) to
// short phrases. The first match wins.
var netErrs = []struct{ match, text string }{
	{"connection refused", "connection refused"},
	{"actively refused", "connection refused"},
	{"No connection could be made", "connection refused"},
	{"no route to host", "no route to host"},
	{"host is unreachable", "no route to host"},
	{"network is unreachable", "network unreachable"},
	{"TLS handshake timeout", "timed out"},
	{"i/o timeout", "timed out"},
	{"timed out", "timed out"},
	{"did not properly respond", "timed out"},
	{"connection reset", "connection reset"},
	{"forcibly closed", "connection reset"},
	{"broken pipe", "connection closed early"},
	{"EOF", "connection closed early"},
	{"no such host", "host name not found"},
	{"server misbehaving", "host name lookup failed"},
}

func (c *Collector) eventLoop(ctx context.Context) error {
	cfg := c.cfg.Get().Collector
	for ctx.Err() == nil {
		if time.Since(c.lastFull) >= cfg.FullCheckInterval.D() {
			if err := c.fullCheck(ctx); err != nil {
				return err
			}
		}
		select {
		case <-c.fullNow:
			if err := c.fullCheck(ctx); err != nil {
				return err
			}
		default:
		}
		c.maybeScan(ctx)

		evs, err := c.client.Events(ctx, c.lastEventID, cfg.EventTimeout.D(), stclient.EventMask)
		if err != nil {
			return err
		}
		now := time.Now()
		c.update(func(s *model.Server) { s.LastContact = now })
		if len(evs) == 0 {
			continue
		}
		if evs[0].ID <= c.lastEventID && c.lastEventID > 0 && evs[len(evs)-1].ID < c.lastEventID {
			// Event IDs went backwards: Syncthing restarted under us.
			c.log.Info("event IDs restarted; running a full re-check")
			c.lastEventID = 0
			if err := c.fullCheck(ctx); err != nil {
				return err
			}
			continue
		}
		if c.lastEventID > 0 && evs[0].ID > c.lastEventID+1 {
			c.log.Info("missed events; running a full re-check", "from", c.lastEventID, "to", evs[0].ID)
			c.FullCheckNow()
		}
		for _, e := range evs {
			if e.ID <= c.lastEventID {
				continue
			}
			c.lastEventID = e.ID
			c.apply(ctx, e)
		}
		c.update(func(s *model.Server) { s.LastEvent = now })
	}
	return ctx.Err()
}

func (c *Collector) pollLoop(ctx context.Context) error {
	for ctx.Err() == nil {
		cfg := c.cfg.Get().Collector
		forced := false
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.fullNow:
			forced = true
		case <-time.After(c.pollWait(cfg)):
		}
		if forced || time.Since(c.lastFull) >= cfg.PollInterval.D() {
			if err := c.fullCheck(ctx); err != nil {
				return err
			}
		}
		c.maybeScan(ctx)
	}
	return ctx.Err()
}

// pollWait is how long the polled mode sleeps: until the next full check or
// the next structure scan, whichever comes first, but at least a second. A
// scan that is still running when the next one is due doesn't count,
// because maybeScan does nothing while one runs: the loop waits for the
// next poll instead of spinning until the scan ends.
func (c *Collector) pollWait(cfg config.Collector) time.Duration {
	wait := time.Until(c.lastFull.Add(cfg.PollInterval.D()))
	c.scanMu.Lock()
	scanning := c.scanning
	c.scanMu.Unlock()
	if w := time.Until(c.lastScan.Add(cfg.StructureInterval.D())); !scanning && w < wait {
		wait = w
	}
	return max(wait, time.Second)
}

// fullCheck reads everything from the server and replaces its state.
func (c *Collector) fullCheck(ctx context.Context) error {
	start := time.Now()
	cfg := c.cfg.Get().Collector
	cl := c.client
	// Cleared only when the check completes, so one that fails half-way
	// (e.g. the one asked for after missed events) runs again on reconnect.
	c.needFull = true

	status, err := cl.SystemStatus(ctx)
	if err != nil {
		return err
	}
	version, err := cl.SystemVersion(ctx)
	if err != nil {
		return err
	}
	folders, err := cl.ConfigFolders(ctx)
	if err != nil {
		return err
	}
	devices, err := cl.ConfigDevices(ctx)
	if err != nil {
		return err
	}
	conns, err := cl.Connections(ctx)
	if err != nil {
		return err
	}
	dstats, err := cl.DeviceStats(ctx)
	if err != nil {
		return err
	}
	fstats, err := cl.FolderStats(ctx)
	if err != nil {
		return err
	}
	pdev, err := cl.PendingDevices(ctx)
	if err != nil {
		return err
	}
	pfold, err := cl.PendingFolders(ctx)
	if err != nil {
		return err
	}

	newFolders := map[string]*model.Folder{}
	for _, fc := range folders {
		newFolders[fc.ID] = folderFromConfig(fc, status.MyID)
	}

	// Per-folder status and per-device completion, throttled. The first
	// error that isn't about a single folder ends the check: the server is
	// probably gone, and working through every queued request (each with
	// its own timeout and retries) would keep it "up" for as long as hours.
	fctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, max(1, cfg.Concurrency))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	run := func(fn func() error) {
		select {
		case sem <- struct{}{}:
		case <-fctx.Done():
			return
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := fn(); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				cancel()
			}
		}()
	}
	// A per-folder request answered with an HTTP error (other than 401/403,
	// which the key gets everywhere) is that folder's problem, not the
	// server's: the folder shows the error and the check goes on. The 404 a
	// paused folder answers is normal and only logged. Transport errors and
	// timeouts fail the check as above.
	problems := map[string]string{} // by folder ID; applied after the parallel phase
	folderErr := func(f *model.Folder, dev, what string, err error) error {
		id := f.ID
		if dev != "" {
			id += "/" + model.ShortID(dev)
		}
		if serverLevel(err) {
			return fmt.Errorf("%s %s: %w", what, id, err)
		}
		msg := shortErr(err)
		if f.Paused || paused404(err) {
			c.log.Debug(what+" unavailable", "folder", id, "err", msg)
			return nil
		}
		c.log.Warn(what+" failed; the folder shows the error", "folder", id, "err", msg)
		if dev != "" {
			msg += " (device " + model.ShortID(dev) + ")"
		}
		mu.Lock()
		if _, ok := problems[f.ID]; !ok {
			problems[f.ID] = msg
		}
		mu.Unlock()
		return nil
	}
	for _, f := range newFolders {
		f := f
		run(func() error {
			ds, err := cl.DBStatus(fctx, f.ID)
			if err != nil {
				return folderErr(f, "", "db/status", err)
			}
			mu.Lock()
			applyDBStatus(f, ds)
			if st, ok := fstats[f.ID]; ok {
				f.LastScan = st.LastScan
				if !st.LastFile.At.IsZero() && st.LastFile.At.Year() > 1970 {
					f.LastFileAt, f.LastFileName = st.LastFile.At, st.LastFile.Filename
				}
			}
			mu.Unlock()
			if ds.Errors > 0 || ds.PullErrors > 0 {
				fe, err := cl.FolderErrors(fctx, f.ID, 5)
				if err != nil {
					if serverLevel(err) {
						return fmt.Errorf("folder/errors %s: %w", f.ID, err)
					}
					c.log.Debug("folder/errors unavailable", "folder", f.ID, "err", shortErr(err))
					return nil
				}
				mu.Lock()
				f.FileErrors = fileErrors(fe.Errors)
				mu.Unlock()
			}
			return nil
		})
		if f.Paused {
			// Syncthing answers db/completion for a paused folder with 404.
			continue
		}
		for _, dev := range f.SharedWith {
			f, dev := f, dev
			run(func() error {
				comp, err := cl.Completion(fctx, f.ID, dev)
				if err != nil {
					return folderErr(f, dev, "completion", err)
				}
				mu.Lock()
				f.Completion[dev] = completionFrom(comp, time.Now())
				mu.Unlock()
				return nil
			})
		}
	}
	wg.Wait()
	if firstErr == nil {
		firstErr = ctx.Err() // stopped before every request was made
	}
	if firstErr != nil {
		return firstErr
	}
	for id, msg := range problems {
		f := newFolders[id]
		if f.State == "" {
			f.State = "unknown" // db/status didn't answer
		}
		if f.Error == "" {
			f.Error = msg
		}
	}

	conflicts, err := cl.ConflictCounters(ctx)
	if err != nil {
		c.log.Warn("reading conflict counters from /metrics", "err", shortErr(err))
	} else if err := c.st.RecordConflicts(c.id, status.StartTime.UTC().Format(time.RFC3339), conflicts, time.Now()); err != nil {
		c.log.Warn("recording conflict counters", "err", err)
	}

	newDevices := map[string]*model.Device{}
	for _, d := range devices {
		md := &model.Device{ID: d.DeviceID, Name: d.Name, Paused: d.Paused, Addresses: d.Addresses}
		if st, ok := dstats[d.DeviceID]; ok && st.LastSeen.Year() > 1970 {
			md.LastSeen = st.LastSeen
		}
		newDevices[d.DeviceID] = md
	}
	newConns := map[string]*model.Connection{}
	for id, cn := range conns {
		newConns[id] = &model.Connection{Connected: cn.Connected, Paused: cn.Paused, Address: cn.Address, Type: cn.Type, ClientVersion: cn.ClientVersion, StartedAt: cn.StartedAt}
		if cn.Connected {
			if d := newDevices[id]; d != nil {
				d.LastSeen = time.Now()
			}
		}
	}
	var pendingDevices []model.PendingDevice
	for id, p := range pdev {
		pendingDevices = append(pendingDevices, model.PendingDevice{DeviceID: id, Name: p.Name, Address: p.Address, Time: p.Time})
	}
	var pendingFolders []model.PendingFolder
	for id, p := range pfold {
		for by, o := range p.OfferedBy {
			pendingFolders = append(pendingFolders, model.PendingFolder{FolderID: id, Label: o.Label, OfferedBy: by, Time: o.Time})
		}
	}
	now := time.Now()
	c.update(func(s *model.Server) {
		s.MyID = status.MyID
		s.StartTime = status.StartTime
		s.PathSep = status.PathSeparator
		s.Version = version.Version
		s.OS = version.OS
		s.Folders = newFolders
		s.Devices = newDevices
		s.Connections = newConns
		s.PendingDevices = pendingDevices
		s.PendingFolders = pendingFolders
		if conflicts != nil {
			s.Conflicts = conflicts
		}
		s.Loaded = true
		s.LastFullCheck = now
		s.LastContact = now
	})
	c.lastFull = now
	c.needFull = false
	c.log.Debug("full re-check done", "folders", len(newFolders), "devices", len(newDevices), "took", time.Since(start).Round(time.Millisecond))
	return nil
}

// perFolder reports whether an error concerns only one folder or device
// (e.g. 404 "folder is paused", or a folder removed since the config was
// read), so the rest of the full check can go on.
func perFolder(err error) bool {
	var se *stclient.StatusError
	return errors.As(err, &se) && se.Code >= 400 && se.Code < 500 && se.Code != 401 && se.Code != 403
}

// serverLevel reports whether an error from a per-folder request means the
// server itself is in trouble: a transport error or timeout, or 401/403,
// which the key gets on every request. Any other HTTP status concerns only
// that folder or device.
func serverLevel(err error) bool {
	var se *stclient.StatusError
	return !errors.As(err, &se) || se.Code == 401 || se.Code == 403
}

// paused404 reports whether err is the 404 "folder is paused" Syncthing
// answers for a folder that was paused after the config was read.
func paused404(err error) bool {
	var se *stclient.StatusError
	return errors.As(err, &se) && se.Code == 404 && strings.Contains(se.Body, "paused")
}

func folderFromConfig(fc stclient.FolderConfig, self string) *model.Folder {
	f := &model.Folder{ID: fc.ID, Label: fc.Label, Path: fc.Path, Type: fc.Type, Paused: fc.Paused, Completion: map[string]*model.Completion{}}
	for _, d := range fc.Devices {
		if d.DeviceID != self {
			f.SharedWith = append(f.SharedWith, d.DeviceID)
		}
	}
	return f
}

func applyDBStatus(f *model.Folder, ds stclient.DBStatus) {
	f.State = ds.State
	if !ds.StateChanged.IsZero() {
		f.StateChanged = ds.StateChanged
	}
	f.Error = ds.Error
	f.WatchError = ds.WatchError
	f.Errors = ds.Errors
	f.PullErrors = ds.PullErrors
	f.GlobalBytes, f.GlobalItems = ds.GlobalBytes, ds.GlobalTotalItems
	f.NeedBytes, f.NeedItems = ds.NeedBytes, ds.NeedTotalItems
	if ds.Errors == 0 && ds.PullErrors == 0 {
		f.FileErrors = nil
	}
}

func fileErrors(in []stclient.FileError) []model.FileError {
	var out []model.FileError
	for i, e := range in {
		if i == 5 {
			break
		}
		out = append(out, model.FileError{Path: e.Path, Error: e.Error})
	}
	return out
}

func completionFrom(c stclient.Completion, at time.Time) *model.Completion {
	return &model.Completion{Percent: c.Completion, NeedBytes: c.NeedBytes, NeedItems: c.NeedItems, RemoteState: c.RemoteState, UpdatedAt: at}
}
