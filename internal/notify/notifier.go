package notify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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
}

// Settings keys for schedule markers.
const (
	keyOvernight = "notify.overnight_done"
	keyReminder  = "notify.last_reminder"
	keyWeekly    = "notify.last_weekly"
)

// Notifier decides what to send and when.
type Notifier struct {
	src     Source
	st      *store.Store
	cfg     *config.Holder
	log     *slog.Logger
	senders func() []Sender
	now     func() time.Time
	wake    chan struct{}

	mu        sync.Mutex
	httpProxy *http.Client

	dmu       sync.Mutex
	delivered map[string]map[string]bool // message key → channels that already have it
	failures  int                        // failed ticks in a row, for the back-off
	retryAt   time.Time
	state     Delivery
	lastErr   map[string]string // channel → error already in the notification log
	notices   []pendingNotice   // settings notices that couldn't be delivered yet
	noticeSeq int
}

// pendingNotice is a settings notice whose first delivery failed. It is
// retried with the regular messages, to the channels configured then.
type pendingNotice struct {
	m      Message
	giveUp time.Time
}

// Settings notices are retried for a day, at most this many at a time.
const (
	noticeRetryFor = 24 * time.Hour
	maxNotices     = 50
)

// Delivery is whether notifications currently reach their channels, for
// the dashboard banner and the status API.
type Delivery struct {
	Failing bool
	Since   time.Time // first failure in the current run of failures
	Error   string    // the latest error
}

// Delivery returns the current delivery state.
func (n *Notifier) Delivery() Delivery {
	n.dmu.Lock()
	defer n.dmu.Unlock()
	return n.state
}

// ChannelsChanged is called when notification channels were replaced: the
// next tick tries again at once instead of waiting out the back-off, and an
// old channel's failure no longer shows in the banner.
func (n *Notifier) ChannelsChanged() {
	n.dmu.Lock()
	n.failures, n.retryAt = 0, time.Time{}
	n.state = Delivery{}
	n.dmu.Unlock()
	n.Wake()
}

// recordDelivery updates the delivery state after a send.
func (n *Notifier) recordDelivery(now time.Time, err error) {
	n.dmu.Lock()
	defer n.dmu.Unlock()
	if err == nil {
		n.state = Delivery{}
		n.failures, n.retryAt = 0, time.Time{}
		return
	}
	if !n.state.Failing {
		n.state.Failing, n.state.Since = true, now
	}
	n.state.Error = err.Error()
}

// New creates a notifier that builds its senders from the stored secrets.
func New(src Source, st *store.Store, cfg *config.Holder, log *slog.Logger) *Notifier {
	if log == nil {
		log = slog.Default()
	}
	n := &Notifier{src: src, st: st, cfg: cfg, log: log, now: time.Now, wake: make(chan struct{}, 1),
		delivered: map[string]map[string]bool{}, lastErr: map[string]string{}}
	n.senders = n.defaultSenders
	return n
}

// SetSenders replaces how senders are built (for tests).
func (n *Notifier) SetSenders(fn func() []Sender) { n.senders = fn }

// SetHTTPClient sets the HTTP client used for Discord (for tests).
func (n *Notifier) SetHTTPClient(c *http.Client) { n.httpProxy = c }

func (n *Notifier) defaultSenders() []Sender {
	var out []Sender
	cfg := n.cfg.Get()
	if u, err := n.st.Secret(store.SecretDiscordWebhook); err == nil && u != "" {
		out = append(out, &Discord{URL: u, RoleID: cfg.Notify.DiscordRoleID, HTTP: n.httpProxy})
	} else if err != nil {
		n.log.Error("reading Discord webhook", "err", err)
	}
	return out
}

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
	n.dmu.Lock()
	wait := now.Before(n.retryAt)
	n.dmu.Unlock()
	if wait {
		return
	}
	// Settings notices go out at once, also during quiet hours.
	if err := n.retryNotices(ctx, now); err != nil {
		n.fail(now, err)
		return
	}
	cfg := n.cfg.Get()
	loc := cfg.Location()
	local := now.In(loc)
	q := parseQuiet(cfg.Notify.QuietHours)
	quiet := q.in(local)
	v := n.src.View()

	if !quiet && q.enabled {
		if err := n.overnight(ctx, cfg, v, q, now); err != nil {
			n.fail(now, err)
			return
		}
		v = n.src.View()
	}
	if !quiet {
		if err := n.urgent(ctx, cfg, v, now); err != nil {
			n.fail(now, err)
			return
		}
		if err := n.resolved(ctx, cfg, now); err != nil {
			n.fail(now, err)
			return
		}
		if err := n.reminder(ctx, cfg, n.src.View(), local, now); err != nil {
			n.fail(now, err)
			return
		}
		if err := n.weekly(ctx, cfg, n.src.View(), local, now); err != nil {
			n.fail(now, err)
			return
		}
	}
}

func (n *Notifier) fail(now time.Time, err error) {
	if errors.Is(err, errNoChannel) {
		// Nothing is marked as sent; it all goes out once a channel exists.
		return
	}
	// Back off while a channel keeps failing: 1, 2, 4 … 30 minutes.
	n.dmu.Lock()
	n.failures++
	d := min(time.Minute<<min(n.failures-1, 5), 30*time.Minute)
	n.retryAt = now.Add(d)
	n.dmu.Unlock()
	n.log.Warn("sending notification failed", "err", err, "retry_in", d)
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
		if activeUrgent(v, r, now) && r.PingedAt.IsZero() && now.Sub(r.OpenedAt) >= debounce(cfg, r) {
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

func (n *Notifier) urgent(ctx context.Context, cfg *config.Config, v *engine.View, now time.Time) error {
	due := dueRecords(cfg, v, now)
	if len(due) == 0 {
		return nil
	}
	m := UrgentMessage(cfg, due)
	m.Key = "urgent:" + strings.Join(ids(due), ",")
	if err := n.send(ctx, now, m); err != nil {
		return err
	}
	n.src.MarkPinged(ids(due), now)
	return nil
}

func (n *Notifier) resolved(ctx context.Context, cfg *config.Config, now time.Time) error {
	pend, err := n.st.PendingResolved()
	if err != nil || len(pend) == 0 {
		return err
	}
	engine.SortRecords(pend)
	m := ResolvedMessage(cfg, pend)
	m.Key = "resolved:" + strings.Join(ids(pend), ",")
	if err := n.send(ctx, now, m); err != nil {
		return err
	}
	n.src.MarkResolvedNotified(ids(pend), now)
	return nil
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
	due := dueRecords(cfg, v, now)
	pend, err := n.st.PendingResolved()
	if err != nil {
		return err
	}
	engine.SortRecords(pend)
	hist, err := n.st.HistoryBetween(start, end)
	if err != nil {
		return err
	}
	pendIDs := map[string]bool{}
	for _, r := range pend {
		pendIDs[r.ID] = true
	}
	var blips []store.HistoryEntry
	for _, h := range hist {
		if !h.Urgent || h.ResolvedAt.IsZero() || pendIDs[h.FindingID] {
			continue
		}
		if h.OpenedAt.Before(start) || !h.ResolvedAt.Before(end.Add(time.Second)) {
			continue
		}
		if h.ResolvedAt.Sub(h.OpenedAt) < cfg.Debounce(h.Check) {
			continue
		}
		blips = append(blips, h)
	}
	if len(due) > 0 || len(pend) > 0 || len(blips) > 0 {
		m := OvernightMessage(cfg, due, pend, blips)
		m.Key = "overnight:" + marker
		if err := n.send(ctx, now, m); err != nil {
			return err
		}
		n.src.MarkPinged(ids(due), now)
		n.src.MarkResolvedNotified(ids(pend), now)
	}
	return n.st.SetSetting(keyOvernight, marker)
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
	var open []*store.Record
	for _, r := range v.Open {
		// Only findings announced before the reminder time; one pinged just
		// now (e.g. after quiet hours or a snooze) doesn't need a reminder.
		if !r.Urgent || r.PingedAt.IsZero() || !r.PingedAt.Before(at) {
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

// SendTest sends a test message to every configured channel.
func (n *Notifier) SendTest(ctx context.Context) error {
	cfg := n.cfg.Get()
	m := Message{Kind: KindTest, Title: "syncwatch test message", Summary: "Notifications from syncwatch reach this channel.", Color: ColorInfo, URL: cfg.PublicURL}
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
	n.recordDelivery(n.now(), delivered)
	if delivered == nil {
		n.Wake() // anything held back by a failing channel can go out now
	}
	return errors.Join(errs...)
}

// Notice announces a security-relevant settings change (a server's URL, API
// key or certificate pin, access settings). It is sent at once, also during
// quiet hours, without a mention, so a change nobody expected is noticed.
func (n *Notifier) Notice(title, summary string) {
	n.notice(n.senders(), true, title, summary)
}

// NoticeLater captures the channels configured now and returns a function
// that sends a notice to them. A change that replaces or removes a channel
// is announced with it, so the channel that loses messages hears about it.
func (n *Notifier) NoticeLater() func(title, summary string) {
	senders := n.senders()
	// Those channels may be gone by then; their failures aren't the
	// current channels' and don't count towards the banner.
	return func(title, summary string) { n.notice(senders, false, title, summary) }
}

func (n *Notifier) notice(senders []Sender, current bool, title, summary string) {
	cfg := n.cfg.Get()
	now := n.now()
	n.dmu.Lock()
	n.noticeSeq++
	key := fmt.Sprintf("notice:%d:%d", now.UnixNano(), n.noticeSeq)
	n.dmu.Unlock()
	m := Message{Kind: KindNotice, Title: "🔧 " + title, Summary: summary, Color: ColorWarn, URL: cfg.PublicURL, Key: key}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := n.sendTo(ctx, now, m, senders, current); errors.Is(err, errNoChannel) {
			n.log.Info("settings notice not sent: no notification channel is configured", "title", title)
		} else if err != nil {
			n.log.Warn("sending settings notice failed; retrying with the next notifications", "title", title, "err", err)
			n.queueNotice(m, now)
		}
	}()
}

// queueNotice keeps a notice whose first delivery failed, so a change made
// while a channel is down still gets announced.
func (n *Notifier) queueNotice(m Message, at time.Time) {
	m.Summary += "\nChanged at " + at.In(n.cfg.Get().Location()).Format("2006-01-02 15:04") + "; this notice was held back because sending failed."
	n.dmu.Lock()
	n.notices = append(n.notices, pendingNotice{m: m, giveUp: at.Add(noticeRetryFor)})
	if len(n.notices) > maxNotices {
		n.log.Warn("dropping the oldest held-back settings notice", "title", n.notices[0].m.Title)
		n.notices = n.notices[1:]
	}
	n.dmu.Unlock()
	n.Wake()
}

// retryNotices sends held-back notices to the channels configured now (the
// ones a notice first went to may be gone; a dead old webhook mustn't hold
// up urgent messages). Notices older than noticeRetryFor are dropped.
func (n *Notifier) retryNotices(ctx context.Context, now time.Time) error {
	n.dmu.Lock()
	pending := append([]pendingNotice(nil), n.notices...)
	n.dmu.Unlock()
	for _, p := range pending {
		if now.After(p.giveUp) {
			n.log.Warn("giving up on a settings notice after a day of failed sends", "title", p.m.Title)
			n.dropNotice(p.m.Key)
			continue
		}
		if err := n.send(ctx, now, p.m); err != nil {
			return err
		}
		n.dropNotice(p.m.Key)
	}
	return nil
}

func (n *Notifier) dropNotice(key string) {
	n.dmu.Lock()
	defer n.dmu.Unlock()
	for i, p := range n.notices {
		if p.m.Key == key {
			n.notices = append(n.notices[:i], n.notices[i+1:]...)
			return
		}
	}
}

// errNoChannel means no notification channel is configured. Callers mark
// nothing as sent, so urgent findings stay due until a channel is added.
var errNoChannel = errors.New("no notification channel is configured")

// send delivers to every channel. It succeeds if Discord succeeds, or if
// there is no Discord channel and any other channel succeeds. When a keyed
// message is retried, channels that got it on an earlier attempt are skipped.
func (n *Notifier) send(ctx context.Context, now time.Time, m Message) error {
	return n.sendTo(ctx, now, m, n.senders(), true)
}

// sendTo delivers to the given channels. record says whether they are the
// current channels, whose result is the delivery state the banner shows.
func (n *Notifier) sendTo(ctx context.Context, now time.Time, m Message, senders []Sender, record bool) error {
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
		n.recordDelivery(now, err)
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
		n.log.Warn("writing notification log", "err", e)
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
	title := fmt.Sprintf("🚨 %d new urgent problems", len(rs))
	if len(rs) == 1 {
		title = "🚨 " + config.CatalogByID[rs[0].Check].Title + ": " + rs[0].Subject
	}
	var lines []string
	for _, r := range rs {
		lines = append(lines, findingLine(r, dashboard(cfg)))
	}
	return Message{Kind: KindUrgent, Mention: true, Title: title, Color: colorFor(rs), URL: dashboard(cfg),
		Sections: []Section{{Title: "New", Lines: limitLines(lines, 25)}}}
}

// ResolvedMessage announces that pinged findings are resolved.
func ResolvedMessage(cfg *config.Config, rs []*store.Record) Message {
	title := fmt.Sprintf("✅ %d problems resolved", len(rs))
	if len(rs) == 1 {
		title = "✅ Resolved: " + config.CatalogByID[rs[0].Check].Title + ": " + rs[0].Subject
	}
	var lines []string
	for _, r := range rs {
		lines = append(lines, resolvedLine(r)+fmt.Sprintf(" (open %s)", rules.Ago(r.ResolvedAt.Sub(r.OpenedAt))))
	}
	return Message{Kind: KindResolved, Title: title, Color: ColorResolved, URL: dashboard(cfg),
		Sections: []Section{{Title: "Resolved", Lines: limitLines(lines, 25)}}}
}

// ReminderMessage lists urgent findings that are still open.
func ReminderMessage(cfg *config.Config, rs []*store.Record, now time.Time) Message {
	var lines []string
	for _, r := range rs {
		lines = append(lines, findingLine(r, dashboard(cfg))+fmt.Sprintf(" (open %s)", rules.Ago(now.Sub(r.OpenedAt))))
	}
	return Message{Kind: KindReminder, Title: fmt.Sprintf("⏰ Still open: %d urgent problem(s)", len(rs)), Color: colorFor(rs), URL: dashboard(cfg),
		Sections: []Section{{Title: "Still open", Lines: limitLines(lines, 25)}}}
}

// OvernightMessage is the batch sent when quiet hours end.
func OvernightMessage(cfg *config.Config, open, resolved []*store.Record, blips []store.HistoryEntry) Message {
	var openL, resL, blipL []string
	for _, r := range open {
		openL = append(openL, findingLine(r, dashboard(cfg)))
	}
	for _, r := range resolved {
		resL = append(resL, resolvedLine(r))
	}
	for _, h := range blips {
		blipL = append(blipL, historyLine(h, true)+fmt.Sprintf(" (%s–%s)", h.OpenedAt.In(cfg.Location()).Format("15:04"), h.ResolvedAt.In(cfg.Location()).Format("15:04")))
	}
	color := ColorResolved
	if len(open) > 0 {
		color = colorFor(open)
	}
	return Message{Kind: KindOvernight, Mention: len(open) > 0, Title: "🌅 Overnight summary", Color: color, URL: dashboard(cfg),
		Sections: []Section{
			{Title: fmt.Sprintf("New and still open (%d)", len(open)), Lines: limitLines(openL, 20)},
			{Title: fmt.Sprintf("Resolved during the night (%d)", len(resolved)), Lines: limitLines(resL, 20)},
			{Title: fmt.Sprintf("Opened and resolved during the night (%d)", len(blips)), Lines: limitLines(blipL, 20)},
		}}
}

// HealthLine summarises the deployment in one line.
func HealthLine(cfg *config.Config, v *engine.View, now time.Time) string {
	var parts []string
	snap := v.Snapshot
	if snap != nil {
		up, total := 0, 0
		var down []string
		for _, sv := range snap.Servers {
			total++
			if sv.Status == model.StatusUp {
				up++
			} else {
				down = append(down, sv.Name)
			}
		}
		if up == total {
			parts = append(parts, fmt.Sprintf("%d servers OK", total))
		} else {
			parts = append(parts, fmt.Sprintf("%d/%d servers OK (%s down)", up, total, strings.Join(down, ", ")))
		}
		pcs, offline := 0, 0
		for _, d := range snap.Devices() {
			if d.IsServer {
				continue
			}
			pcs++
			if !d.Connected && (d.LastSeen.IsZero() || now.Sub(d.LastSeen) > cfg.Thresholds.PCOffline.D()) {
				offline++
			}
		}
		parts = append(parts, fmt.Sprintf("%d PCs, %d offline > %s", pcs, offline, rules.Ago(cfg.Thresholds.PCOffline.D())))
	}
	var errs, warns, infos, snoozed int
	for _, r := range v.Open {
		if _, ok := v.Snoozed(r.ID, now); ok {
			snoozed++
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
	parts = append(parts, fmt.Sprintf("%d errors, %d warnings, %d suggestions", errs, warns, infos))
	parts = append(parts, fmt.Sprintf("%d snoozed", snoozed))
	return strings.Join(parts, " · ")
}

// WeeklyMessage is the weekly report.
func WeeklyMessage(cfg *config.Config, v *engine.View, hist []store.HistoryEntry, now time.Time) Message {
	from := now.Add(-7 * 24 * time.Hour)
	var newL, resL []string
	nNew, nRes := 0, 0
	for _, h := range hist {
		if !h.OpenedAt.Before(from) {
			nNew++
			if h.Severity >= model.Warn {
				newL = append(newL, historyLine(h, false))
			}
		}
		if !h.ResolvedAt.IsZero() && !h.ResolvedAt.Before(from) {
			nRes++
			if h.Severity >= model.Warn {
				resL = append(resL, historyLine(h, true))
			}
		}
	}
	var top []string
	for _, r := range v.Open {
		if _, ok := v.Snoozed(r.ID, now); ok {
			continue
		}
		top = append(top, findingLine(r, dashboard(cfg))+fmt.Sprintf(" (open %s)", rules.Ago(now.Sub(r.OpenedAt))))
		if len(top) == cfg.Notify.WeeklyTop {
			break
		}
	}
	color := ColorResolved
	for _, r := range v.Open {
		if _, ok := v.Snoozed(r.ID, now); !ok && r.Severity >= model.Warn {
			color = ColorWarn
			if r.Severity == model.Error {
				color = ColorError
				break
			}
		}
	}
	summary := HealthLine(cfg, v, now)
	if cfg.PublicURL != "" {
		summary += "\n[Open the dashboard](" + dashboard(cfg) + ")"
	}
	return Message{Kind: KindWeekly, Title: "📋 Weekly Syncthing report", Summary: summary, Color: color, URL: dashboard(cfg),
		Sections: []Section{
			{Title: fmt.Sprintf("New this week (%d)", nNew), Lines: limitLines(newL, 8)},
			{Title: fmt.Sprintf("Resolved this week (%d)", nRes), Lines: limitLines(resL, 8)},
			{Title: "Top open problems", Lines: top},
		}}
}
