// Package collector reads each Syncthing server through stclient and keeps
// its state in the hub: a live event loop, a periodic full re-check, an
// hourly structure scan and a polled fallback mode.
package collector

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
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

	wake          chan struct{}
	fullNow       chan struct{}
	lastEventID   int64
	startTime     time.Time
	failures      int
	firstFail     time.Time
	certFails     int       // failed sessions in a row because the certificate changed
	certWarned    time.Time // when the certificate change was last logged at WARN
	lastFull      time.Time
	needFull      bool      // the next session starts with a full check: one did not complete, or see dropClears
	configDue     bool      // a ConfigSaved event in the current batch; see applyBatch
	pendingDue    bool      // a Pending*Changed event in the current batch
	pendingWarned bool      // the pending re-read failed and was logged this session
	lastScan      time.Time // the start of the last structure scan; it and the fields to baseChoice need scanMu
	lastBase      time.Time
	scanMu        sync.Mutex
	scanning      bool
	scanDone      chan struct{}   // closed when the running scan is complete; nil while no scan runs
	scanEnd       *ScanEnd        // how the running scan ended, written before scanDone closes; nil while no scan runs
	sessCtx       context.Context // the context of the session that called maybeScan last; see maybeScan and ScanNow
	manualAt      time.Time       // when the last scan that an administrator started began; see ScanNow
	baseTook      time.Duration   // how long the last completed baseline scan took; see baselineEvery
	baseKnown     bool            // a baseline scan completed since the start
	baseChoice    string          // the baseline interval that noteBaseline logged last
	gaps          gapState
	clearDue      map[string]time.Time // folders whose error events cleared, with when; see confirmClears
	copies        copySet              // conflict copies counted from events; see conflictCopy
	needRefused   atomic.Bool          // the gate refused /rest/db/remoteneed in this session; see needList
	localSaved    map[string]time.Time // by folder: when its last local change was written to the store; see saveLocalChange
	localDue      map[string]bool      // folders whose newest local change waits for the store
	relayOK       map[string]time.Time // by relay listener URI: the first sample without an error after a failure; see relayListeners
	sizeMu        sync.Mutex           // guards the fields to sizeNone; see sizes.go
	sizeFetched   map[string]time.Time // by path: when the size was last read from the gate
	sizeNight     map[string]string    // by path: the night whose measurement was requested
	sizeNone      bool                 // the server answered that it measures no sizes, in this session

	trafficEvery time.Duration // how often traffic counters are read
}

// rootCAs are the certificate authorities that a CA-signed certificate of a
// server must chain to: nil is the system pool. Tests set a test CA.
var rootCAs *x509.CertPool

// pinQuiet is how long a certificate that the collector pinned is no news:
// when the server gives it again in this time, the collector pins it again
// without a settings notice. 2 front ends with their own CA-signed
// certificate for the same host name (DNS round robin, a load balancer)
// make the client pin A, B, A, B ... at each new connection: they give 2
// notices, not 1 for each connection.
const pinQuiet = 24 * time.Hour

// pinNews reports whether the pin of fp at now is news: the collector did
// not pin fp in the pinQuiet before now. pinned has the fingerprints that
// the collector pinned, with the time of the last pin; pinNews records fp
// and forgets the fingerprints of before pinQuiet.
func pinNews(pinned map[string]time.Time, fp string, now time.Time) bool {
	for k, at := range pinned {
		if now.Sub(at) >= pinQuiet {
			delete(pinned, k)
		}
	}
	_, seen := pinned[fp]
	pinned[fp] = now
	return !seen
}

// newCollector makes the collector of one server. onPin, when not nil, is
// called after the collector pinned a certificate of the server without an
// administrator (see Manager.OnPin), at most once in pinQuiet for each
// certificate.
func newCollector(sc config.Server, apiKey string, hub *model.Hub, st *store.Store, cfg *config.Holder, log *slog.Logger, onPin func(serverID, fingerprint string, renewal bool)) (*Collector, error) {
	pin, err := st.Pin(sc.ID)
	if err != nil {
		return nil, err
	}
	c := &Collector{
		id: sc.ID, hub: hub, st: st, cfg: cfg, log: log.With("server", sc.Name),
		wake: make(chan struct{}, 1), fullNow: make(chan struct{}, 1), trafficEvery: trafficEvery,
	}
	pinned := map[string]time.Time{} // see pinNews; only OnPin uses it, under the lock of the client
	c.client, err = stclient.New(stclient.Options{
		Name:    sc.Name,
		URL:     sc.URL,
		APIKey:  apiKey,
		Pin:     pin,
		Timeout: cfg.Get().Collector.RequestTimeout.D(),
		RootCAs: rootCAs,
		Log:     log,
		OnPin: func(fp string) {
			// The pin before this one tells a first contact from a renewal.
			old, err := st.Pin(sc.ID)
			if err != nil {
				c.log.Warn("cannot read the pinned certificate", "err", err)
			}
			if err := st.SetPin(sc.ID, fp); err != nil {
				c.log.Error("cannot save the pinned certificate", "err", err)
			}
			if news := pinNews(pinned, fp, time.Now()); news && onPin != nil && old != fp {
				// Not here: the client calls this in the TLS handshake and
				// holds its lock.
				go onPin(sc.ID, fp, old != "")
			}
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
	// The local changes that saveLocalChange held back go to the store
	// before the store closes (the application waits for the collectors).
	defer c.flushLocalChanges(true)
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
		c.dropClears()
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
	c.setRelayListeners(status, time.Now())
	c.needRefused.Store(false) // the gate may be a newer one now
	restarted := !status.StartTime.Equal(c.startTime)
	if restarted {
		if !c.startTime.IsZero() {
			c.log.Info("Syncthing restarted, full check started", "startTime", status.StartTime)
		}
		c.startTime = status.StartTime
		c.lastEventID = 0
	}
	cfg := c.cfg.Get().Collector
	mode := cfg.Mode
	c.update(func(s *model.Server) { s.Mode = mode })
	c.pendingWarned = false
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
			c.log.Debug("reconnected, events resume without a full check", "since", c.lastEventID)
			c.applyBatch(ctx, evs)
			c.readConflicts(ctx, status.StartTime)
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
		c.readConflicts(ctx, status.StartTime)
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
		c.log.Log(context.Background(), level, "certificate changed, syncwatch does not connect until an administrator accepts it", "old", stclient.ShortFP(ce.Old), "new", stclient.ShortFP(ce.New))
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
		return "timeout"
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
	{"TLS handshake timeout", "timeout"},
	{"i/o timeout", "timeout"},
	{"timed out", "timeout"},
	{"did not properly respond", "timeout"},
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

		timeout := cfg.EventTimeout.D()
		if w, ok := c.clearWait(); ok && w < timeout {
			timeout = w
		}
		evs, err := c.client.Events(ctx, c.lastEventID, timeout, stclient.EventMask)
		if err != nil {
			return err
		}
		now := time.Now()
		c.update(func(s *model.Server) { s.LastContact = now })
		if len(evs) == 0 {
			c.confirmClears(now)
			c.flushLocalChanges(false)
			continue
		}
		if evs[0].ID <= c.lastEventID && c.lastEventID > 0 && evs[len(evs)-1].ID < c.lastEventID {
			// Event IDs went backwards: Syncthing restarted under us.
			c.log.Info("event IDs restarted, full check started")
			c.lastEventID = 0
			if err := c.fullCheck(ctx); err != nil {
				return err
			}
			continue
		}
		if c.lastEventID > 0 && evs[0].ID > c.lastEventID+1 {
			c.missedEvents(ctx, c.lastEventID, evs[0].ID, evs)
		}
		c.applyBatch(ctx, evs)
		c.confirmClears(now)
		c.flushLocalChanges(false)
		c.update(func(s *model.Server) { s.LastEvent = now })
	}
	return ctx.Err()
}

// applyBatch applies the events of one long-poll answer. The re-reads some
// of them ask for (ConfigSaved, Pending*Changed) are done once per batch
// afterwards, not once per event: five ConfigSaved in a batch are one
// config re-read, while no long-poll is open.
func (c *Collector) applyBatch(ctx context.Context, evs []stclient.Event) {
	for _, e := range evs {
		if e.ID <= c.lastEventID {
			continue
		}
		c.lastEventID = e.ID
		c.apply(ctx, e)
	}
	if c.configDue {
		c.configDue = false
		if err := c.refreshConfig(ctx); err != nil {
			c.log.Warn("cannot read the configuration again after ConfigSaved", "err", shortErr(err))
			c.FullCheckNow()
		}
	}
	if c.pendingDue {
		c.pendingDue = false
		if err := c.refreshPending(ctx); err != nil && !c.pendingWarned {
			// Once per session: the next full check reads the lists anyway.
			c.pendingWarned = true
			c.log.Warn("cannot read the pending devices and folders again", "err", shortErr(err))
		}
	}
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
// next poll instead of spinning until the scan ends. lastScan is read under
// scanMu: ScanNow sets it from a request of the dashboard.
func (c *Collector) pollWait(cfg config.Collector) time.Duration {
	wait := time.Until(c.lastFull.Add(cfg.PollInterval.D()))
	c.scanMu.Lock()
	scanning, lastScan := c.scanning, c.lastScan
	c.scanMu.Unlock()
	if w := time.Until(lastScan.Add(cfg.StructureInterval.D())); !scanning && w < wait {
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
	c.setRelayListeners(status, time.Now())
	if !status.StartTime.Equal(c.startTime) {
		// Syncthing restarted between two requests without breaking one:
		// its event IDs start over, so the stream is followed from 0 again.
		if !c.startTime.IsZero() {
			c.log.Info("Syncthing restarted, full check started", "startTime", status.StartTime)
		}
		c.startTime = status.StartTime
		c.lastEventID = 0
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
	statusRead := map[string]bool{}                     // folders whose db/status answered
	needLists := map[string]map[string]model.NeedList{} // see needList

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
		c.log.Warn(what+" failed, the folder shows the error", "folder", id, "err", msg)
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
			statusRead[f.ID] = true
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
					c.log.Debug("folder/errors not available", "folder", f.ID, "err", shortErr(err))
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
				at := time.Now()
				mu.Lock()
				f.Completion[dev] = completionFrom(comp, at)
				mu.Unlock()
				// The names for H5, in the same slot of the throttle.
				if cn, ok := conns[dev]; ok && cn.Connected && comp.NeedItems > 0 {
					if nl, ok := c.needList(fctx, f.ID, dev, comp.NeedItems, at); ok {
						mu.Lock()
						if needLists[f.ID] == nil {
							needLists[f.ID] = map[string]model.NeedList{}
						}
						needLists[f.ID][dev] = nl
						mu.Unlock()
					}
				}
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

	c.readConflicts(ctx, status.StartTime)

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
	// What the store keeps across restarts: the local changes that
	// wait for the store go there first, so that it has the newest ones.
	c.flushLocalChanges(true)
	ids := make([]string, 0, len(newFolders))
	for id := range newFolders {
		ids = append(ids, id)
	}
	act := c.folderActivity(ids, now)
	c.update(func(s *model.Server) {
		s.MyID = status.MyID
		s.StartTime = status.StartTime
		s.PathSep = status.PathSeparator
		s.Version = version.Version
		s.OS = version.OS
		for id, f := range newFolders {
			f.FirstSeen, f.LastLocalChange = act[id].FirstSeen, act[id].LastLocalChange
			if old := s.Folders[id]; old != nil {
				if f.FirstSeen.IsZero() {
					f.FirstSeen = old.FirstSeen // the store did not answer
				}
				if old.LastLocalChange.After(f.LastLocalChange) {
					// Only events know it, and the store did not get it.
					f.LastLocalChange = old.LastLocalChange
				}
			}
			// Progress samples for H5: the server's own copy and
			// each device.
			if statusRead[id] {
				s.AddProgress(id, "", model.ProgressSample{At: now, NeedBytes: f.NeedBytes, GlobalBytes: f.GlobalBytes})
			}
			for dev, comp := range f.Completion {
				s.AddProgress(id, dev, model.ProgressSample{At: comp.UpdatedAt, NeedBytes: comp.NeedBytes, GlobalBytes: comp.GlobalBytes})
			}
		}
		pruneProgress(s, newFolders)
		s.NeedLists = needLists
		s.Folders = newFolders
		s.Devices = newDevices
		s.Connections = newConns
		s.PendingDevices = pendingDevices
		s.PendingFolders = pendingFolders
		s.Loaded = true
		s.LastFullCheck = now
		s.LastContact = now
	})
	c.lastFull = now
	c.needFull = false
	c.clearDue = nil // every folder's error was read again
	if err := c.st.SetServerDeviceID(c.id, status.MyID); err != nil {
		c.log.Warn("cannot save the device ID of the server", "err", err)
	}
	c.log.Debug("full check done", "folders", len(newFolders), "devices", len(newDevices), "took", time.Since(start).Round(time.Millisecond))
	return nil
}

// readConflicts reads the conflict counters from /metrics and records their
// increase since the last reading (H10). Besides every full check it runs
// at each session start, so a counter reset by a Syncthing restart is seen
// sooner; the whole new value counts as new conflicts then. A failure only
// costs this reading.
func (c *Collector) readConflicts(ctx context.Context, startTime time.Time) {
	conflicts, err := c.client.ConflictCounters(ctx)
	if err != nil {
		c.log.Warn("cannot read the conflict counters from /metrics", "err", shortErr(err))
		return
	}
	if err := c.st.RecordConflicts(c.id, startTime.UTC().Format(time.RFC3339), conflicts, time.Now()); err != nil {
		c.log.Warn("cannot save the conflict counters", "err", err)
	}
	c.update(func(s *model.Server) { s.Conflicts = conflicts })
}

// perFolder reports whether an error concerns only one folder or device
// (e.g. 404 "folder is paused", or a folder removed since the config was
// read), so the rest of the full check can go on.
func perFolder(err error) bool {
	var se *stclient.StatusError
	return errors.As(err, &se) && se.Code >= 400 && se.Code < 500 && se.Code != 401 && se.Code != 403
}

// serverLevel reports whether an error from a per-folder request means the
// server itself is in trouble: a transport error or timeout, 401/403, which
// the key gets on every request, or a gate failure (see gateFailure). Any
// other HTTP status concerns only that folder or device.
func serverLevel(err error) bool {
	var se *stclient.StatusError
	return !errors.As(err, &se) || se.Code == 401 || se.Code == 403 || gateFailure(se)
}

// gateFailure reports whether a read-only gate, not Syncthing, gave the
// answer because of a problem of the whole server: Syncthing is unreachable
// or refused the gate's API key (5xx), or this client has too many parallel
// requests (429). Such an answer says nothing about the folder in the
// request. A 5xx from Syncthing itself has no gate prefix and stays a folder
// problem.
func gateFailure(se *stclient.StatusError) bool {
	return strings.HasPrefix(se.Body, "syncwatch gate:") && (se.Code == 429 || se.Code >= 500)
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
	return &model.Completion{Percent: c.Completion, NeedBytes: c.NeedBytes, NeedItems: c.NeedItems, GlobalBytes: c.GlobalBytes, RemoteState: c.RemoteState, UpdatedAt: at}
}
