package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/engine"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/rules"
	"github.com/ivarstudios/syncwatch/internal/store"
)

// Source is the part of the engine the notifier needs.
type Source interface {
	View() *engine.View
	MarkPinged(ids []string, at time.Time)
	MarkResolvedNotified(ids []string, at time.Time)
	MarkRepeated(ids []string, at time.Time)
}

// Settings keys for schedule markers.
const (
	keyOvernight = "notify.overnight_done"
	keyReminder  = "notify.last_reminder"
	keyWeekly    = "notify.last_weekly"
	keyAnnounced = "notify.announced"    // see announcedRun
	keyKept      = "notify.kept_notices" // see pendingNotice
)

// Notifier decides what to send and when.
type Notifier struct {
	src     Source
	st      *store.Store
	cfg     *config.Holder
	log     *slog.Logger
	senders func() []Sender
	// noticeSenders builds the channel for settings notices: the webhook
	// for settings notices, or nothing (then the notices go to senders).
	noticeSenders func() []Sender
	now           func() time.Time
	wake          chan struct{}
	started       time.Time // first tick, for the start-up grace

	mu        sync.Mutex
	httpProxy *http.Client
	runs      []announcedRun // under mu; read from the store at first use
	runsRead  bool

	dmu        sync.Mutex
	delivered  map[string]map[string]bool // message key → channels that already have it
	gen        int                        // bumped by ChannelsChanged: a failure from before it is an old channel's
	failures   int                        // failed ticks in a row, for the back-off
	retryAt    time.Time
	state      Delivery
	missed     time.Time         // first failure since the last catch-up; see missedBlips
	lastErr    map[string]string // channel → error already in the notification log
	notices    []pendingNotice   // settings notices that wait for a channel; see keptRead
	keptRead   bool              // notices holds the list stored before the start
	noticeSeq  int
	own        ownChannel // the state of the webhook for settings notices
	noticesErr bool       // the error of reading that webhook is in the log
	hold       *time.Timer
	holdUntil  time.Time // the end of the hold window of an urgent message; see holdUrgent
}

// ownChannel is the delivery state of the webhook for settings notices. Its
// failures have their own back-off and never show in Delivery, which is
// about the channel of the findings.
type ownChannel struct {
	failures int
	retryAt  time.Time
	since    time.Time // first failure in the current run of failures
}

// pendingNotice is a settings notice whose first delivery failed. It waits
// for a channel that accepts it: the channels configured at each retry.
// The list is stored in the settings (keyKept), thus a restart keeps it.
type pendingNotice struct {
	Key      string    `json:"key"`
	Title    string    `json:"title"`
	Summary  string    `json:"summary"`
	Security bool      `json:"security,omitempty"`
	At       time.Time `json:"at"` // the time of the change
	// GiveUp is when a notice that is not about security is dropped; zero
	// for a security notice, which is kept until a channel accepts it.
	GiveUp time.Time `json:"give_up,omitzero"`
}

// Settings notices that are not about security are retried for a day. At
// most maxNotices wait at a time: the oldest one that is not about security
// goes first.
const (
	noticeRetryFor = 24 * time.Hour
	maxNotices     = 50
)

// A kept security notice also goes to the channel of the findings when the
// webhook for settings notices fails for longer than this.
const noticeFallbackAfter = time.Hour

// Delivery is whether notifications currently reach their channels, for
// the dashboard banner and the status API.
type Delivery struct {
	Failing     bool
	Since       time.Time // first failure in the current run of failures
	Error       string    // the latest error
	NextTry     time.Time // while failing: the time of the next send; zero otherwise
	NoticesKept int       // settings notices that wait for a channel that accepts them
}

// Notice is a settings notice: a change of a setting that the channels must
// hear about. The Summary has lines of text without formatting; a name or
// another text from Syncthing in a line goes through OneLine first.
type Notice struct {
	Title, Summary string
	// Security marks a change of a secret (an API key, a webhook, an API
	// token), a pinned certificate, a password, the access settings or an
	// import.
	Security bool
}

// Delivery returns the current delivery state.
func (n *Notifier) Delivery() Delivery {
	n.dmu.Lock()
	defer n.dmu.Unlock()
	n.readKept()
	d := n.state
	d.NoticesKept = len(n.notices)
	return d
}

// ChannelsChanged is called when notification channels were replaced: the
// next tick tries again at once instead of waiting out the back-off, and an
// old channel's failure no longer shows in the banner. This also holds for
// the webhook for settings notices.
func (n *Notifier) ChannelsChanged() {
	n.dmu.Lock()
	n.gen++
	n.failures, n.retryAt = 0, time.Time{}
	n.state = Delivery{}
	n.lastErr = map[string]string{}
	n.own = ownChannel{}
	n.dmu.Unlock()
	n.Wake()
}

// generation is the channel generation a send starts in; see gen.
func (n *Notifier) generation() int {
	n.dmu.Lock()
	defer n.dmu.Unlock()
	return n.gen
}

// recordDelivery updates the delivery state after a send that started in
// generation gen. A failure of a channel replaced meanwhile is not the new
// channel's.
func (n *Notifier) recordDelivery(now time.Time, gen int, err error) {
	n.dmu.Lock()
	defer n.dmu.Unlock()
	if err == nil {
		n.state = Delivery{}
		n.failures, n.retryAt = 0, time.Time{}
		return
	}
	if gen != n.gen {
		return
	}
	if !n.state.Failing {
		n.state.Failing, n.state.Since = true, now
	}
	n.state.Error = err.Error()
	// Kept until the catch-up went out: a new webhook or a successful test
	// message clears the banner, not what the channel missed.
	if n.missed.IsZero() {
		n.missed = now
	}
}

// New creates a notifier that builds its senders from the stored secrets.
func New(src Source, st *store.Store, cfg *config.Holder, log *slog.Logger) *Notifier {
	if log == nil {
		log = slog.Default()
	}
	n := &Notifier{src: src, st: st, cfg: cfg, log: log, now: time.Now, wake: make(chan struct{}, 1),
		delivered: map[string]map[string]bool{}, lastErr: map[string]string{}}
	n.senders = n.defaultSenders
	n.noticeSenders = n.defaultNoticeSenders
	return n
}

// SetSenders replaces how senders are built (for tests).
func (n *Notifier) SetSenders(fn func() []Sender) { n.senders = fn }

// SetNoticeSenders replaces how the channel for settings notices is built
// (for tests). A function that returns nothing sends the notices to the
// senders of SetSenders.
func (n *Notifier) SetNoticeSenders(fn func() []Sender) { n.noticeSenders = fn }

// SetHTTPClient sets the HTTP client used for Discord (for tests).
func (n *Notifier) SetHTTPClient(c *http.Client) { n.httpProxy = c }

func (n *Notifier) defaultSenders() []Sender {
	var out []Sender
	cfg := n.cfg.Get()
	if u, err := n.st.Secret(store.SecretDiscordWebhook); err == nil && u != "" {
		out = append(out, &Discord{URL: u, RoleID: cfg.Notify.DiscordRoleID, HTTP: n.httpProxy})
	} else if err != nil {
		n.log.Error("cannot read the Discord webhook", "err", err)
		// Fails like a dead webhook, so the banner, the notification log
		// and the test button all say why.
		out = append(out, &brokenChannel{err: errUndecryptable})
	}
	return out
}

// defaultNoticeSenders builds the channel of the webhook for settings
// notices, when one is set. It never mentions the role.
func (n *Notifier) defaultNoticeSenders() []Sender {
	u, err := n.st.Secret(store.SecretNoticesWebhook)
	n.dmu.Lock()
	logged := n.noticesErr
	n.noticesErr = err != nil
	n.dmu.Unlock()
	if err != nil {
		if !logged { // the kept notices try again each minute
			n.log.Error("cannot read the Discord webhook for settings notices", "err", err)
		}
		return []Sender{&brokenChannel{name: channelNotices, err: errNoticesUndecryptable}}
	}
	if u == "" {
		return nil
	}
	return []Sender{&Discord{URL: u, HTTP: n.httpProxy, Channel: channelNotices}}
}

// errUndecryptable is the failure of every send while the stored webhook
// cannot be decrypted.
var errUndecryptable error = unusableError("syncwatch cannot decrypt the Discord webhook: check the secret key")

// errNoticesUndecryptable is the same for the webhook for settings notices.
var errNoticesUndecryptable error = unusableError("syncwatch cannot decrypt the Discord webhook for settings notices: check the secret key")

// brokenChannel stands in for a Discord channel while its webhook can't be
// read. Every send fails with the reason.
type brokenChannel struct {
	name string // "" is discord
	err  error
}

func (b *brokenChannel) Name() string {
	if b.name != "" {
		return b.name
	}
	return channelDiscord
}

func (b *brokenChannel) Send(context.Context, Message) error { return b.err }

// Wake asks for a tick soon (after an evaluation).
func (n *Notifier) Wake() {
	select {
	case n.wake <- struct{}{}:
	default:
	}
}

// Run ticks every 30 s and whenever woken.
func (n *Notifier) Run(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-n.wake:
		}
		n.Tick(ctx, n.now())
	}
}

// Tick does everything due at time now.
func (n *Notifier) Tick(ctx context.Context, now time.Time) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.started.IsZero() {
		n.started = now
	}
	// Settings notices go out at once, also during quiet hours. With their
	// own webhook they have their own back-off: a failure there does not
	// hold up the messages about findings.
	var own []Sender
	if len(n.keptNotices()) > 0 {
		if own = n.noticeSenders(); len(own) > 0 {
			n.retryOwnNotices(ctx, now, own)
		}
	}
	n.dmu.Lock()
	wait, gen := now.Before(n.retryAt), n.gen
	n.dmu.Unlock()
	if wait {
		return
	}
	// Without their own webhook, a notice that can't be sent doesn't hold
	// up the urgent messages: its failure counts once the rest of the tick
	// is done.
	var nerr error
	if len(own) == 0 {
		nerr = n.retryNotices(ctx, now)
	}
	cfg := n.cfg.Get()
	loc := cfg.Location()
	local := now.In(loc)
	q := parseQuiet(cfg.Notify.QuietHours)
	quiet := q.in(local)
	v := n.src.View()

	if !quiet && q.enabled {
		if err := n.overnight(ctx, cfg, v, q, now); err != nil {
			n.fail(now, gen, err)
			return
		}
		v = n.src.View()
	}
	if !quiet {
		if err := n.urgent(ctx, cfg, v, now); err != nil {
			n.fail(now, gen, err)
			return
		}
		if err := n.repeat(ctx, cfg, n.src.View(), now); err != nil {
			n.fail(now, gen, err)
			return
		}
		if err := n.resolved(ctx, cfg, now); err != nil {
			n.fail(now, gen, err)
			return
		}
		if err := n.reminder(ctx, cfg, n.src.View(), local, now); err != nil {
			n.fail(now, gen, err)
			return
		}
		if err := n.weekly(ctx, cfg, n.src.View(), local, now); err != nil {
			n.fail(now, gen, err)
			return
		}
	}
	if nerr != nil {
		n.fail(now, gen, nerr)
	}
}

// fail backs off after a failed send in a tick that started in generation
// gen. If the channels were replaced since, the new ones are tried at once.
func (n *Notifier) fail(now time.Time, gen int, err error) {
	if errors.Is(err, errNoChannel) {
		// Nothing is marked as sent; it all goes out once a channel exists.
		return
	}
	// Back off while a channel keeps failing: 1, 2, 4, 5, 5 … minutes while
	// Discord or the network is down, 1, 2, 4 … 30 minutes while Discord
	// refuses the webhook or syncwatch cannot use it (only a new webhook
	// helps, and saving one tries again at once).
	n.dmu.Lock()
	if gen != n.gen {
		n.dmu.Unlock()
		return
	}
	n.failures++
	d := backoff(n.failures, err)
	n.retryAt = now.Add(d)
	if n.state.Failing {
		n.state.NextTry = n.retryAt
	}
	n.dmu.Unlock()
	n.log.Warn("cannot send the notification", "err", err, "retry_in", d)
}

// backoff is the wait after the given number of failed tries in a row.
func backoff(failures int, err error) time.Duration {
	limit := 30 * time.Minute
	if transient(err) {
		limit = 5 * time.Minute
	}
	return min(time.Minute<<min(failures-1, 5), limit)
}

// debounce is the shortest debounce of a record's urgent checks.
func debounce(cfg *config.Config, r *store.Record) time.Duration {
	d := time.Duration(-1)
	for _, c := range r.Checks {
		if !cfg.Urgent(c) {
			continue
		}
		if x := cfg.Debounce(c); d < 0 || x < d {
			d = x
		}
	}
	if d < 0 {
		d = cfg.Debounce(r.Check)
	}
	return d
}

func activeUrgent(v *engine.View, r *store.Record, now time.Time) bool {
	if !r.Urgent || r.Stale || !r.Open() {
		return false
	}
	_, snoozed := v.Snoozed(r.ID, now)
	return !snoozed
}

func dueRecords(cfg *config.Config, v *engine.View, now time.Time) []*store.Record {
	var out []*store.Record
	for _, r := range v.Open {
		if activeUrgent(v, r, now) && r.PingedAt.IsZero() && now.Sub(r.UrgentFrom()) >= debounce(cfg, r) {
			out = append(out, r)
		}
	}
	return out
}

// A problem must stay resolved for this long (or its debounce, if shorter)
// before "resolved" goes out. A re-open inside that window carries on as
// the same incident (the engine keeps its PingedAt), so a flapping link
// gets one ping, not one per cycle.
const resolveSettle = 5 * time.Minute

// settled keeps the resolved findings whose resolution has lasted.
func settled(cfg *config.Config, rs []*store.Record, now time.Time) []*store.Record {
	var out []*store.Record
	for _, r := range rs {
		if now.Sub(r.ResolvedAt) >= min(debounce(cfg, r), resolveSettle) {
			out = append(out, r)
		}
	}
	return out
}

func ids(rs []*store.Record) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.ID
	}
	return out
}

// holdUrgent is how long an urgent message waits after the first of its
// findings fell due, so that findings that fall due seconds apart (the
// AKKA and SOL findings of one directory) share one message and one
// mention. A timer wakes the notifier at the end of the wait.
const holdUrgent = 10 * time.Second

// dueAt is when the urgent message of a finding falls due.
func dueAt(cfg *config.Config, r *store.Record) time.Time {
	return r.UrgentFrom().Add(debounce(cfg, r))
}

// holding reports whether the urgent message for due waits for more
// findings, and arms the timer that ends the wait.
func (n *Notifier) holding(cfg *config.Config, due []*store.Record, now time.Time) bool {
	if len(due) == 0 {
		return false
	}
	first := dueAt(cfg, due[0])
	for _, r := range due[1:] {
		if t := dueAt(cfg, r); t.Before(first) {
			first = t
		}
	}
	until := first.Add(holdUrgent)
	if !now.Before(until) {
		return false
	}
	n.dmu.Lock()
	defer n.dmu.Unlock()
	if n.hold != nil {
		n.hold.Stop()
	}
	n.holdUntil = until
	n.hold = time.AfterFunc(until.Sub(now), n.Wake)
	return true
}

func (n *Notifier) urgent(ctx context.Context, cfg *config.Config, v *engine.View, now time.Time) error {
	due := dueRecords(cfg, v, now)
	if n.holding(cfg, due, now) {
		return nil // the catch-up goes with them
	}
	n.dmu.Lock()
	since, failing := n.missed, n.state.Failing
	n.dmu.Unlock()
	var missed []store.HistoryEntry
	if !since.IsZero() {
		var err error
		if missed, err = n.missedBlips(cfg, v, since, now); err != nil {
			return err
		}
		if len(missed) == 0 && !failing {
			n.clearMissed(since) // delivery works and nothing was missed
		}
	}
	if len(due) == 0 && len(missed) == 0 {
		return nil
	}
	// The catch-up goes with the urgent findings, or alone: then it is also
	// the next try of a failing channel.
	m := MissedMessage(cfg, due, missed, since, now)
	m.Key = "urgent:" + strings.Join(ids(due), ",")
	for _, h := range missed {
		m.Key += fmt.Sprintf(";%s@%d", h.FindingID, h.ResolvedAt.UnixMilli())
	}
	if err := n.send(ctx, now, m); err != nil {
		return err
	}
	n.src.MarkPinged(ids(due), now)
	if !since.IsZero() {
		n.remember(now, listedRuns(missed))
		n.clearMissed(since)
	}
	return nil
}

// missedBlips are the urgent findings that resolved after notifications
// started to fail (since), were due before they resolved and were never
// announced: nobody heard of them. Snoozed and ignored ones are left out,
// as in the overnight summary, and so are the ones it listed.
func (n *Notifier) missedBlips(cfg *config.Config, v *engine.View, since, now time.Time) ([]store.HistoryEntry, error) {
	hist, err := n.st.HistoryBetween(since, now.Add(time.Second))
	if err != nil {
		return nil, err
	}
	var out []store.HistoryEntry
	for _, h := range hist {
		if !h.Urgent || h.ResolvedAt.IsZero() || h.ResolvedAt.Before(since) {
			continue
		}
		if h.ResolvedAt.Before(h.OpenedAt.Add(cfg.Debounce(h.Check))) || n.announced(h) || hidden(v, h, now) {
			continue
		}
		out = append(out, h)
	}
	return out, nil
}

// clearMissed ends the catch-up period that started at since.
func (n *Notifier) clearMissed(since time.Time) {
	n.dmu.Lock()
	defer n.dmu.Unlock()
	if n.missed.Equal(since) {
		n.missed = time.Time{}
	}
}

func (n *Notifier) resolved(ctx context.Context, cfg *config.Config, now time.Time) error {
	pend, err := n.st.PendingResolved()
	if err != nil {
		return err
	}
	pend = settled(cfg, pend, now)
	if len(pend) == 0 {
		return nil
	}
	engine.SortRecords(pend)
	m := ResolvedMessage(cfg, pend, n.gaps())
	m.Key = "resolved:" + strings.Join(ids(pend), ",")
	if err := n.send(ctx, now, m); err != nil {
		return err
	}
	n.src.MarkResolvedNotified(ids(pend), now)
	n.remember(now, resolvedRuns(pend))
	return nil
}

// repeat sends the urgent message again for the urgent ERROR findings of a
// server that stay open (notifications.repeat_urgent; 0 is off): every
// interval after the urgent message or the last repeat. Snoozed, ignored
// and stale findings are not repeated, and nothing goes out in the quiet
// hours: a repeat that falls due there goes out at their end.
func (n *Notifier) repeat(ctx context.Context, cfg *config.Config, v *engine.View, now time.Time) error {
	due := repeatDue(cfg, v, now)
	if len(due) == 0 {
		return nil
	}
	m := RepeatMessage(cfg, due, now)
	m.Key = "repeat:" + strings.Join(ids(due), ",")
	if err := n.send(ctx, now, m); err != nil {
		return err
	}
	n.src.MarkRepeated(ids(due), now)
	return nil
}

// repeatDue returns the findings whose urgent message falls due again: open
// urgent ERROR findings of one server that are not stale, snoozed or
// ignored, announced at least one interval ago.
func repeatDue(cfg *config.Config, v *engine.View, now time.Time) []*store.Record {
	every := cfg.Notify.RepeatUrgent.D()
	if every <= 0 {
		return nil
	}
	var due []*store.Record
	for _, r := range v.Open {
		if !r.Urgent || r.Severity != model.Error || r.Server == "" || r.Stale || !r.Open() || r.PingedAt.IsZero() {
			continue
		}
		if _, z := v.Snoozed(r.ID, now); z {
			continue
		}
		if now.Sub(announcedAt(r)) >= every {
			due = append(due, r)
		}
	}
	return due
}

// announcedAt is the time of the last message with the role mention for a
// finding: its urgent message or the last repeat.
func announcedAt(r *store.Record) time.Time {
	if r.RepeatedAt.After(r.PingedAt) {
		return r.RepeatedAt
	}
	return r.PingedAt
}

func (n *Notifier) overnight(ctx context.Context, cfg *config.Config, v *engine.View, q quietHours, now time.Time) error {
	end := q.lastEnd(now.In(cfg.Location()))
	if now.Sub(end) > 12*time.Hour {
		return nil
	}
	marker := end.UTC().Format(time.RFC3339)
	if done, _ := n.st.Setting(keyOvernight); done >= marker {
		return nil
	}
	start := q.startBefore(end)
	// Findings that became due before quiet hours ended are the night's;
	// later ones (a summary held back by a failing channel) are labelled.
	var due, day []*store.Record
	for _, r := range dueRecords(cfg, v, now) {
		if r.OpenedAt.Add(debounce(cfg, r)).Before(end) {
			due = append(due, r)
		} else {
			day = append(day, r)
		}
	}
	pend, err := n.st.PendingResolved()
	if err != nil {
		return err
	}
	pend = settled(cfg, pend, now)
	engine.SortRecords(pend)
	// Resolved findings that were due (open for their debounce) at some
	// moment of the night and were never announced: also ones that opened
	// before quiet hours, and ones that resolved after them but before this
	// summary went out.
	hist, err := n.st.HistoryBetween(start, now.Add(time.Second))
	if err != nil {
		return err
	}
	var blips []store.HistoryEntry
	for _, h := range hist {
		if !h.Urgent || h.ResolvedAt.IsZero() {
			continue
		}
		dueAt := h.OpenedAt.Add(cfg.Debounce(h.Check))
		if !dueAt.Before(end) || h.ResolvedAt.Before(dueAt) {
			continue
		}
		if n.announced(h) || hidden(v, h, now) {
			continue
		}
		blips = append(blips, h)
	}
	if len(due) > 0 || len(day) > 0 || len(pend) > 0 || len(blips) > 0 {
		m := OvernightMessage(cfg, due, pend, blips, day, n.gaps())
		m.Key = "overnight:" + marker
		if err := n.send(ctx, now, m); err != nil {
			return err
		}
		n.src.MarkPinged(append(ids(due), ids(day)...), now)
		n.src.MarkResolvedNotified(ids(pend), now)
		n.remember(now, append(resolvedRuns(pend), listedRuns(blips)...))
	}
	return n.st.SetSetting(keyOvernight, marker)
}

// hidden reports whether an occurrence is ignored or snoozed, now or when
// it resolved.
func hidden(v *engine.View, h store.HistoryEntry, now time.Time) bool {
	if _, z := v.Snoozed(h.FindingID, now); z {
		return true
	}
	_, z := v.Snoozed(h.FindingID, h.ResolvedAt)
	return z
}

// announced reports whether an occurrence of a finding was announced: an
// urgent message went out for it before it resolved (a re-open that
// carried PingedAt over is the same, already announced problem), or a
// summary listed it. The record knows this only for its latest run of
// occurrences: a re-open after the "resolved" message starts a new record,
// so older occurrences are looked up in the announced runs.
func (n *Notifier) announced(h store.HistoryEntry) bool {
	at := h.ResolvedAt.UnixMilli()
	for _, run := range n.announcedRuns() {
		if run.ID == h.FindingID && run.From <= at && at <= run.To {
			return true
		}
	}
	r, err := n.st.Record(h.FindingID)
	if err != nil || r == nil || r.PingedAt.IsZero() {
		return false
	}
	return !r.PingedAt.After(h.ResolvedAt)
}

// announcedRun is a run of occurrences of one finding that a notification
// announced, by their resolve times (Unix ms): from the urgent message to
// the resolve that the "resolved" message reported, or one occurrence that
// a summary listed.
type announcedRun struct {
	ID   string `json:"id"`
	From int64  `json:"from"`
	To   int64  `json:"to"`
}

// announcedKeep is how long announced runs are kept: longer than the
// overnight summary looks back.
const announcedKeep = 3 * 24 * time.Hour

// resolvedRuns are the runs that resolved messages for rs report.
func resolvedRuns(rs []*store.Record) []announcedRun {
	var out []announcedRun
	for _, r := range rs {
		out = append(out, announcedRun{ID: r.ID, From: r.PingedAt.UnixMilli(), To: r.ResolvedAt.UnixMilli()})
	}
	return out
}

// listedRuns are the runs of occurrences that a summary lists.
func listedRuns(hs []store.HistoryEntry) []announcedRun {
	var out []announcedRun
	for _, h := range hs {
		out = append(out, announcedRun{ID: h.FindingID, From: h.ResolvedAt.UnixMilli(), To: h.ResolvedAt.UnixMilli()})
	}
	return out
}

// announcedRuns returns the announced runs, read from the store once.
func (n *Notifier) announcedRuns() []announcedRun {
	if !n.runsRead {
		if _, err := n.st.JSONSetting(keyAnnounced, &n.runs); err != nil {
			n.log.Warn("cannot read the list of announced findings", "err", err)
		}
		n.runsRead = true
	}
	return n.runs
}

// remember adds announced runs, removes old ones and saves the list, so a
// restart does not announce an occurrence again.
func (n *Notifier) remember(now time.Time, add []announcedRun) {
	if len(add) == 0 {
		return
	}
	keep := []announcedRun{}
	for _, run := range n.announcedRuns() {
		if run.To >= now.Add(-announcedKeep).UnixMilli() {
			keep = append(keep, run)
		}
	}
	n.runs = append(keep, add...)
	if err := n.st.SetJSONSetting(keyAnnounced, n.runs); err != nil {
		n.log.Warn("cannot save the list of announced findings", "err", err)
	}
}

func (n *Notifier) reminder(ctx context.Context, cfg *config.Config, v *engine.View, local, now time.Time) error {
	if cfg.Notify.ReminderTime == "" || cfg.Notify.ReminderTime == "off" {
		return nil
	}
	mins, err := config.ParseClock(cfg.Notify.ReminderTime)
	if err != nil {
		return nil
	}
	at := time.Date(local.Year(), local.Month(), local.Day(), mins/60, mins%60, 0, 0, local.Location())
	if local.Before(at) || local.Sub(at) > 12*time.Hour {
		return nil
	}
	day := at.Format("2006-01-02")
	if last, _ := n.st.Setting(keyReminder); last == day {
		return nil
	}
	if !n.checked(cfg, v, now) {
		return nil
	}
	var open []*store.Record
	for _, r := range v.Open {
		// Only findings announced more than an hour before the reminder
		// time; one pinged or repeated just now (after a snooze, say) or
		// listed in the overnight summary at quiet end doesn't need a
		// reminder yet.
		if !r.Urgent || r.PingedAt.IsZero() || announcedAt(r).After(at.Add(-time.Hour)) {
			continue
		}
		if _, snoozed := v.Snoozed(r.ID, now); snoozed {
			continue
		}
		open = append(open, r)
	}
	if len(open) > 0 {
		m := ReminderMessage(cfg, open, now)
		m.Key = "reminder:" + day
		if err := n.send(ctx, now, m); err != nil {
			return err
		}
	}
	return n.st.SetSetting(keyReminder, day)
}

func (n *Notifier) weekly(ctx context.Context, cfg *config.Config, v *engine.View, local, now time.Time) error {
	if cfg.Notify.WeeklyDay == "" || cfg.Notify.WeeklyDay == "off" {
		return nil
	}
	wd, err := config.ParseWeekday(cfg.Notify.WeeklyDay)
	if err != nil {
		return nil
	}
	mins, err := config.ParseClock(cfg.Notify.WeeklyTime)
	if err != nil {
		return nil
	}
	back := (int(local.Weekday()) - int(wd) + 7) % 7
	d := local.AddDate(0, 0, -back)
	at := time.Date(d.Year(), d.Month(), d.Day(), mins/60, mins%60, 0, 0, local.Location())
	if local.Before(at) {
		at = at.AddDate(0, 0, -7)
	}
	if local.Sub(at) > 24*time.Hour {
		return nil
	}
	marker := at.UTC().Format(time.RFC3339)
	if last, _ := n.st.Setting(keyWeekly); last >= marker {
		return nil
	}
	if !n.checked(cfg, v, now) {
		return nil
	}
	hist, err := n.st.HistoryBetween(now.Add(-7*24*time.Hour), now.Add(time.Second))
	if err != nil {
		return err
	}
	m := WeeklyMessage(cfg, v, hist, now)
	m.Key = "weekly:" + marker
	if err := n.send(ctx, now, m); err != nil {
		return err
	}
	return n.st.SetSetting(keyWeekly, marker)
}

// Scheduled messages wait this long after start for every server's first
// result, so a report doesn't call servers down before they were contacted.
const startupGrace = 5 * time.Minute

// checked reports whether every enabled server has had a first result, or
// the start-up grace is over.
func (n *Notifier) checked(cfg *config.Config, v *engine.View, now time.Time) bool {
	if now.Sub(n.started) >= startupGrace {
		return true
	}
	if v.Snapshot == nil {
		return false
	}
	for _, s := range cfg.Servers {
		if s.Disabled {
			continue
		}
		if sv := v.Snapshot.Server(s.ID); sv == nil || sv.Status == model.StatusUnknown {
			return false
		}
	}
	return true
}

// SendTest sends a test message to every configured channel.
func (n *Notifier) SendTest(ctx context.Context) error {
	cfg := n.cfg.Get()
	// With a role, the test mentions it like an urgent message does, so a
	// wrong role ID shows now and not at the first incident.
	summary := "Notifications from syncwatch reach this channel. Urgent messages mention no role."
	if cfg.Notify.DiscordRoleID != "" {
		summary = "Notifications from syncwatch reach this channel. This test message mentions the role, the same as urgent messages. If your phone did not notify you, check the role ID and that you have the role."
	}
	m := Message{Kind: KindTest, Mention: cfg.Notify.DiscordRoleID != "", Title: "syncwatch test message", Summary: summary, Color: ColorInfo, URL: cfg.PublicURL}
	gen := n.generation()
	senders := n.senders()
	if len(senders) == 0 {
		return errNoChannel
	}
	var errs []error
	results := map[string]error{}
	for _, s := range senders {
		err := s.Send(ctx, m)
		n.logSend(n.now(), m, s.Name(), err)
		results[s.Name()] = err
		if err != nil {
			errs = append(errs, err)
		}
	}
	delivered := deliveryErr(results)
	n.recordDelivery(n.now(), gen, delivered)
	if delivered == nil {
		n.Wake() // anything held back by a failing channel can go out now
	}
	return errors.Join(errs...)
}

// errNoChannel means no notification channel is configured. Callers mark
// nothing as sent, so urgent findings stay due until a channel is added.
var errNoChannel = errors.New("No Discord webhook is configured.")

// send delivers to every channel. It succeeds if Discord succeeds, or if
// there is no Discord channel and any other channel succeeds. When a keyed
// message is retried, channels that got it on an earlier attempt are skipped.
// A message about findings first names the times when syncwatch did not
// run that no message reported yet.
func (n *Notifier) send(ctx context.Context, now time.Time, m Message) error {
	var upTo time.Time
	if gapKinds[m.Kind] {
		var lines []string
		if lines, upTo = n.gapLines(now); len(lines) > 0 {
			if m.Summary != "" {
				lines = append(lines, m.Summary)
			}
			m.Summary = strings.Join(lines, "\n")
		}
	}
	gen := n.generation()
	err := n.sendTo(ctx, now, m, n.senders(), true, gen)
	if err == nil && !upTo.IsZero() {
		if err := n.st.MarkGapsReported(upTo); err != nil {
			n.log.Warn("cannot mark the reported times when syncwatch did not run", "err", err)
		}
	}
	return err
}

// gapKinds are the messages that report the times when syncwatch did not
// run: the messages about findings (the catch-up is an urgent or a
// resolved message). Test messages and settings notices do not.
var gapKinds = map[string]bool{KindUrgent: true, KindRepeat: true, KindResolved: true, KindOvernight: true, KindReminder: true, KindWeekly: true}

// maxGapLines is the number of gaps a message names; "…and N more" stands
// for the older ones.
const maxGapLines = 5

// gapLines returns a line for each time when syncwatch did not run that no
// message reported yet, "syncwatch did not run from 21:04 to 21:24.", with
// the date for a time that is not on the day of the message, and the end
// of the newest of them.
func (n *Notifier) gapLines(now time.Time) ([]string, time.Time) {
	var lines []string
	var upTo time.Time
	loc := n.cfg.Get().Location()
	for _, g := range n.gaps() {
		if g.Reported {
			continue
		}
		lines = append(lines, "syncwatch did not run from "+clockOn(g.From, now, loc)+" to "+clockOn(g.To, now, loc)+".")
		if g.To.After(upTo) {
			upTo = g.To
		}
	}
	if k := len(lines) - maxGapLines; k > 0 {
		lines = append(lines[k:], fmt.Sprintf("…and %s when syncwatch did not run.", plural(k, "more time")))
	}
	return lines, upTo
}

// gaps returns the times when syncwatch did not run (the last 30 days),
// oldest first.
func (n *Notifier) gaps() []store.Gap {
	gaps, err := n.st.Gaps(time.Time{})
	if err != nil {
		n.log.Warn("cannot read the times when syncwatch did not run", "err", err)
	}
	return gaps
}

// clockOn writes a time in loc as "21:04", with the date when it is not on
// the day of ref: "2026-10-05 21:04".
func clockOn(t, ref time.Time, loc *time.Location) string {
	t, ref = t.In(loc), ref.In(loc)
	if t.Format("2006-01-02") != ref.Format("2006-01-02") {
		return t.Format("2006-01-02 15:04")
	}
	return t.Format("15:04")
}

// sendTo delivers to the given channels. record says whether they are the
// current channels, whose result is the delivery state the banner shows;
// gen is the channel generation they were built in.
func (n *Notifier) sendTo(ctx context.Context, now time.Time, m Message, senders []Sender, record bool, gen int) error {
	if len(senders) == 0 {
		return errNoChannel
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	results := map[string]error{}
	for _, s := range senders {
		var err error
		if !n.alreadySent(m.Key, s.Name()) {
			err = s.Send(ctx, m)
			n.logSend(now, m, s.Name(), err)
			if err == nil {
				n.markSent(m.Key, s.Name())
			}
		}
		results[s.Name()] = err
	}
	err := deliveryErr(results)
	if err == nil {
		n.forgetSent(m.Key)
	}
	if record {
		n.recordDelivery(now, gen, err)
	}
	return err
}

// deliveryErr decides whether a message counts as delivered, from each
// channel's result: Discord took it, or there is no Discord channel and
// another channel took it.
func deliveryErr(results map[string]error) error {
	if err, ok := results["discord"]; ok {
		return err
	}
	var last error
	for _, err := range results {
		if err == nil {
			return nil
		}
		last = err
	}
	return last
}

func (n *Notifier) alreadySent(key, channel string) bool {
	n.dmu.Lock()
	defer n.dmu.Unlock()
	return key != "" && n.delivered[key][channel]
}

func (n *Notifier) markSent(key, channel string) {
	if key == "" {
		return
	}
	n.dmu.Lock()
	defer n.dmu.Unlock()
	if n.delivered[key] == nil {
		if len(n.delivered) >= 32 {
			// Messages whose content moved on while a channel was failing.
			n.delivered = map[string]map[string]bool{}
		}
		n.delivered[key] = map[string]bool{}
	}
	n.delivered[key][channel] = true
}

func (n *Notifier) forgetSent(key string) {
	n.dmu.Lock()
	defer n.dmu.Unlock()
	delete(n.delivered, key)
}

// logSend writes a send to the notification log. A failure that repeats the
// channel's previous error isn't written again, so retries against a dead
// webhook don't push the real history out of the log.
func (n *Notifier) logSend(now time.Time, m Message, channel string, err error) {
	errText := ""
	if err != nil {
		errText = err.Error()
	}
	n.dmu.Lock()
	repeat := err != nil && m.Kind != KindTest && n.lastErr[channel] == errText
	if err != nil {
		n.lastErr[channel] = errText
	} else {
		delete(n.lastErr, channel)
	}
	n.dmu.Unlock()
	if repeat {
		return
	}
	if e := n.st.LogNotification(now, m.Kind, channel, err == nil, errText, m.Title); e != nil {
		n.log.Warn("cannot write the notification log", "err", e)
	}
	if err == nil {
		n.log.Info("notification sent", "kind", m.Kind, "channel", channel, "title", m.Title)
	}
}

// quietHours is a daily window in local minutes; it may cross midnight.
type quietHours struct {
	enabled    bool
	start, end int
}

func parseQuiet(s string) quietHours {
	if s == "" || s == "off" {
		return quietHours{}
	}
	a, b, err := config.ParseRange(s)
	if err != nil || a == b {
		return quietHours{}
	}
	return quietHours{enabled: true, start: a, end: b}
}

func (q quietHours) in(t time.Time) bool {
	if !q.enabled {
		return false
	}
	m := t.Hour()*60 + t.Minute()
	if q.start < q.end {
		return m >= q.start && m < q.end
	}
	return m >= q.start || m < q.end
}

// lastEnd is the most recent end of the quiet window at or before t.
func (q quietHours) lastEnd(t time.Time) time.Time {
	e := time.Date(t.Year(), t.Month(), t.Day(), q.end/60, q.end%60, 0, 0, t.Location())
	if e.After(t) {
		e = e.AddDate(0, 0, -1)
	}
	return e
}

// startBefore is the start of the quiet window that ends at end.
func (q quietHours) startBefore(end time.Time) time.Time {
	s := time.Date(end.Year(), end.Month(), end.Day(), q.start/60, q.start%60, 0, 0, end.Location())
	if !s.Before(end) {
		s = s.AddDate(0, 0, -1)
	}
	return s
}

// Message builders.

func dashboard(cfg *config.Config) string { return strings.TrimRight(cfg.PublicURL, "/") }

// UrgentMessage announces new urgent findings.
func UrgentMessage(cfg *config.Config, rs []*store.Record) Message {
	title := fmt.Sprintf("🚨 %d new urgent findings", len(rs))
	if len(rs) == 1 {
		title = "🚨 " + config.CatalogByID[rs[0].Check].Title + ": " + rs[0].Subject
	} else {
		// A phone notification shows only the title.
		title += where(cfg, rs)
	}
	return Message{Kind: KindUrgent, Mention: true, Title: title, Color: colorFor(rs), URL: dashboard(cfg),
		Sections: []Section{{Title: "New findings", Lines: limitLines(recordLines(cfg, rs, time.Time{}, withAdvice), 25)}}}
}

// RepeatMessage sends the urgent message again for urgent findings that
// stay open (notifications.repeat_urgent), with the role mention.
func RepeatMessage(cfg *config.Config, rs []*store.Record, now time.Time) Message {
	title := fmt.Sprintf("🚨 %d urgent findings are still open", len(rs)) + where(cfg, rs)
	if len(rs) == 1 {
		r := rs[0]
		title = "🚨 Still open after " + rules.Ago(now.Sub(started(r))) + ": " + config.CatalogByID[r.Check].Title + ": " + r.Subject
	}
	return Message{Kind: KindRepeat, Mention: true, Title: title, Color: colorFor(rs), URL: dashboard(cfg),
		Sections: []Section{{Title: "Still open", Lines: limitLines(recordLines(cfg, rs, now, withOpen|withAdvice), 25)}}}
}

// where names the servers (in configuration order) and the checks of
// findings: " on AKKA, SOL: H1, S3".
func where(cfg *config.Config, rs []*store.Record) string {
	servers, checks := map[string]bool{}, map[string]bool{}
	for _, r := range rs {
		for _, id := range append([]string{r.Server}, r.Related...) {
			servers[id] = id != ""
		}
		for _, c := range r.Checks {
			checks[c] = true
		}
	}
	var names, gone, ids []string
	for _, s := range cfg.Servers {
		if servers[s.ID] {
			names = append(names, s.Name)
			delete(servers, s.ID)
		}
	}
	for id, ok := range servers {
		if ok {
			gone = append(gone, id) // removed from the configuration since
		}
	}
	sort.Strings(gone)
	names = append(names, gone...)
	for _, c := range config.Catalog {
		if checks[c.ID] {
			ids = append(ids, c.ID)
		}
	}
	out := ""
	if len(names) > 0 {
		out += " on " + strings.Join(names, ", ")
	}
	if len(ids) > 0 {
		out += ": " + strings.Join(ids, ", ")
	}
	return out
}

// serverName is the configured name of a server, or its ID.
func serverName(cfg *config.Config, id string) string {
	for _, s := range cfg.Servers {
		if s.ID == id {
			return s.Name
		}
	}
	return id
}

// andList joins names: "SOL", "SOL and SIRIUS", "AKKA, SOL and SIRIUS".
func andList(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// ResolvedMessage announces that pinged findings are resolved. gaps are the
// times when syncwatch did not run: a finding can resolve in one.
func ResolvedMessage(cfg *config.Config, rs []*store.Record, gaps []store.Gap) Message {
	title := fmt.Sprintf("✅ %d findings resolved", len(rs))
	if len(rs) == 1 {
		title = "✅ Resolved: " + config.CatalogByID[rs[0].Check].Title + ": " + rs[0].Subject
	}
	return Message{Kind: KindResolved, Title: title, Color: ColorResolved, URL: dashboard(cfg),
		Sections: []Section{{Title: "Resolved findings", Lines: limitLines(resolvedLines(cfg, rs, gaps), 25)}}}
}

// ReminderMessage lists urgent findings that are still open.
func ReminderMessage(cfg *config.Config, rs []*store.Record, now time.Time) Message {
	return Message{Kind: KindReminder, Title: "⏰ Still open: " + plural(len(rs), "urgent finding"), Color: colorFor(rs), URL: dashboard(cfg),
		Sections: []Section{{Title: "Still open", Lines: limitLines(recordLines(cfg, rs, now, withOpen|withAdvice), 25)}}}
}

// OvernightMessage is the batch sent when quiet hours end. day holds the
// findings that became due after they ended (a summary that went out late).
// The counts in the section titles count the lines.
func OvernightMessage(cfg *config.Config, open, resolved []*store.Record, blips []store.HistoryEntry, day []*store.Record, gaps []store.Gap) Message {
	openL := recordLines(cfg, open, time.Time{}, 0)
	resL := resolvedLines(cfg, resolved, gaps)
	blipL := blipLines(cfg, blips)
	dayL := recordLines(cfg, day, time.Time{}, 0)
	var all []*store.Record
	all = append(all, open...)
	all = append(all, day...)
	color := ColorResolved
	if len(all) > 0 {
		color = colorFor(all)
	}
	return Message{Kind: KindOvernight, Mention: len(all) > 0, Title: "🌅 Overnight summary", Color: color, URL: dashboard(cfg),
		Sections: []Section{
			{Title: fmt.Sprintf("New and still open (%d)", len(openL)), Lines: limitLines(openL, 20)},
			{Title: fmt.Sprintf("Resolved during the night (%d)", len(resL)), Lines: limitLines(resL, 20)},
			{Title: fmt.Sprintf("Opened and resolved during the night (%d)", len(blipL)), Lines: limitLines(blipL, 20)},
			{Title: fmt.Sprintf("New after the end of quiet hours (%d)", len(dayL)), Lines: limitLines(dayL, 20)},
		}}
}

// MissedMessage reports the urgent findings that opened and resolved while
// notifications failed, from since to now, without a mention. With new
// urgent findings rs it is their urgent message with one more section.
func MissedMessage(cfg *config.Config, rs []*store.Record, missed []store.HistoryEntry, since, now time.Time) Message {
	var m Message
	switch {
	case len(rs) > 0:
		m = UrgentMessage(cfg, rs)
	case len(missed) == 1:
		m = Message{Kind: KindResolved, Title: "✅ Resolved while the notifications failed: " + config.CatalogByID[missed[0].Check].Title + ": " + missed[0].Subject}
	default:
		m = Message{Kind: KindResolved, Title: fmt.Sprintf("✅ %d urgent findings resolved while the notifications failed", len(missed))}
	}
	if len(rs) == 0 {
		m.Color, m.URL = ColorResolved, dashboard(cfg)
	}
	if len(missed) == 0 {
		return m
	}
	from, to := since.In(cfg.Location()), now.In(cfg.Location())
	span := from.Format("15:04") + "–" + to.Format("15:04")
	if from.Format("2006-01-02") != to.Format("2006-01-02") {
		span = from.Format("2006-01-02 15:04") + "–" + to.Format("15:04")
	}
	m.Sections = append(m.Sections, Section{Title: "Opened and resolved while the notifications failed (" + span + ")", Lines: limitLines(blipLines(cfg, missed), 20)})
	return m
}

// HealthLine summarises the deployment in one line.
func HealthLine(cfg *config.Config, v *engine.View, now time.Time) string {
	var parts []string
	snap := v.Snapshot
	if snap != nil {
		// Disabled servers are not polled and not in the snapshot.
		disabled := map[string]bool{}
		var off []string
		for _, s := range cfg.Servers {
			if s.Disabled {
				disabled[s.ID] = true
				off = append(off, s.Name)
			}
		}
		up, total, loaded := 0, len(off), false
		known := map[string]bool{} // up and loaded: its view of the PCs is current
		var down, cert, unknown []string
		for _, sv := range snap.Servers {
			if disabled[sv.ID] {
				continue
			}
			total++
			loaded = loaded || sv.Loaded
			switch sv.Status {
			case model.StatusUp:
				up++
				known[sv.ID] = sv.Loaded
			case model.StatusUnknown:
				unknown = append(unknown, sv.Name)
			case model.StatusCertChanged:
				cert = append(cert, sv.Name)
			default:
				down = append(down, sv.Name)
			}
		}
		if up == total {
			parts = append(parts, plural(total, "server")+" up")
		} else {
			var why []string
			for _, g := range []struct {
				names []string
				state string
			}{{down, "down"}, {cert, "certificate changed"}, {unknown, "not contacted"}, {off, "disabled"}} {
				if len(g.names) > 0 {
					why = append(why, strings.Join(g.names, ", ")+" "+g.state)
				}
			}
			parts = append(parts, fmt.Sprintf("%d of %d servers up (%s)", up, total, strings.Join(why, ", ")))
		}
		if !loaded {
			// No server answered a full check since the start: the PCs are
			// not known, which is not "0 PCs".
			parts = append(parts, "PCs unknown")
		} else {
			pcs, offline, unsure := 0, 0, 0
			for _, d := range snap.Devices() {
				if d.IsServer {
					continue
				}
				pcs++
				if d.Connected || (!d.LastSeen.IsZero() && now.Sub(d.LastSeen) <= cfg.Thresholds.PCOffline.D()) {
					continue
				}
				// Offline only when every server that has the PC configured
				// can say so; else the PC can be connected to one of the others.
				if allKnown(known, d.SeenBy) {
					offline++
				} else {
					unsure++
				}
			}
			pc := fmt.Sprintf("%s, %d offline more than %s", plural(pcs, "PC"), offline, rules.Ago(cfg.Thresholds.PCOffline.D()))
			if unsure > 0 {
				pc += fmt.Sprintf(", %d unknown", unsure)
			}
			parts = append(parts, pc)
		}
	}
	var errs, warns, infos, snoozed, ignored int
	for _, r := range v.Open {
		if z, ok := v.Snoozed(r.ID, now); ok {
			if z.Kind == store.IgnoreKind {
				ignored++
			} else {
				snoozed++
			}
			continue
		}
		switch r.Severity {
		case model.Error:
			errs++
		case model.Warn:
			warns++
		default:
			infos++
		}
	}
	parts = append(parts, fmt.Sprintf("ERROR %d · WARNING %d · INFO %d", errs, warns, infos))
	parts = append(parts, fmt.Sprintf("%d snoozed", snoozed))
	if ignored > 0 {
		parts = append(parts, fmt.Sprintf("%d ignored", ignored))
	}
	return strings.Join(parts, " · ")
}

// allKnown reports whether every one of the servers is up and loaded.
func allKnown(known map[string]bool, ids []string) bool {
	for _, id := range ids {
		if !known[id] {
			return false
		}
	}
	return true
}

// plural writes a count with its noun: "1 server", "2 servers".
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// WeeklyMessage is the weekly report.
func WeeklyMessage(cfg *config.Config, v *engine.View, hist []store.HistoryEntry, now time.Time) Message {
	from := now.Add(-7 * 24 * time.Hour)
	var opened, resolved []store.HistoryEntry
	for _, h := range hist {
		// ERROR and WARNING findings only, and not the snoozed or ignored
		// ones (as in the overnight summary); the counts are of the lines.
		if h.Severity < model.Warn {
			continue
		}
		if _, z := v.Snoozed(h.FindingID, now); z {
			continue
		}
		if !h.OpenedAt.Before(from) {
			opened = append(opened, h)
		}
		if !h.ResolvedAt.IsZero() && !h.ResolvedAt.Before(from) {
			resolved = append(resolved, h)
		}
	}
	// The resolved lines keep the message of the history, which has no
	// resolved form.
	newL, resL := historyLines(cfg, opened, false), historyLines(cfg, resolved, true)
	var open []*store.Record
	for _, r := range v.Open {
		if _, ok := v.Snoozed(r.ID, now); !ok {
			open = append(open, r)
		}
	}
	top := recordLines(cfg, open, now, withOpen)
	if k := cfg.Notify.WeeklyTop; k > 0 && len(top) > k {
		top = top[:k]
	}
	color := ColorResolved
	for _, r := range open {
		if r.Severity >= model.Warn {
			color = ColorWarn
			if r.Severity == model.Error {
				color = ColorError
				break
			}
		}
	}
	summary := escapeMD(HealthLine(cfg, v, now)) // server names
	if cfg.PublicURL != "" {
		summary += "\n[Open the dashboard](" + dashboard(cfg) + ")"
	}
	return Message{Kind: KindWeekly, Title: "📋 Weekly Syncthing report", Summary: summary, Color: color, URL: dashboard(cfg),
		Sections: []Section{
			{Title: fmt.Sprintf("New this week (%d)", len(newL)), Lines: limitLines(newL, 8)},
			{Title: fmt.Sprintf("Resolved this week (%d)", len(resL)), Lines: limitLines(resL, 8)},
			{Title: "Most important open findings", Lines: top},
		}}
}
