package notify

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// noticeIcon starts the title of a settings notice.
const noticeIcon = "🔧 "

// Settings notices: each change of a setting that the channels must hear
// about. A notice goes to the webhook for settings notices when one is set,
// else to the channel of the findings. A notice whose first send fails is
// kept (pendingNotice) and sent again at the next ticks: a security notice
// until a channel accepts it, any other notice for a day.

// Notice announces a settings change (a server's URL, API key or
// certificate pin, the access settings). It is sent at once, also during
// quiet hours, without a mention, so a change nobody expected is noticed.
func (n *Notifier) Notice(nt Notice) {
	gen := n.generation()
	senders, own := n.noticeTargets()
	n.notice(senders, own, gen, true, nt)
}

// NoticeLater captures the channels configured now and returns a function
// that sends a notice to them. A change that replaces or removes a channel
// is announced with it, so the channel that loses messages hears about it.
func (n *Notifier) NoticeLater() func(Notice) {
	gen := n.generation()
	senders, own := n.noticeTargets()
	// Those channels may be gone by then; their failures aren't the
	// current channels' and don't count towards the banner.
	return func(nt Notice) { n.notice(senders, own, gen, false, nt) }
}

// noticeTargets returns the channels for settings notices now: the webhook
// for settings notices (own), or else the channel of the findings.
func (n *Notifier) noticeTargets() (senders []Sender, own bool) {
	if s := n.noticeSenders(); len(s) > 0 {
		return s, true
	}
	return n.senders(), false
}

// errNoNoticesChannel means no webhook for settings notices is configured.
var errNoNoticesChannel = errors.New("No Discord webhook for settings notices is configured.")

// SendNoticesTest sends a test notice to the webhook for settings notices.
// A success there also ends the wait of the kept notices.
func (n *Notifier) SendNoticesTest(ctx context.Context) error {
	senders := n.noticeSenders()
	if len(senders) == 0 {
		return errNoNoticesChannel
	}
	m := Message{Kind: KindTest, Title: "syncwatch test settings notice", Summary: "Settings notices from syncwatch reach this channel. They mention no role.",
		Color: ColorNotice, URL: n.cfg.Get().PublicURL}
	gen, now := n.generation(), n.now()
	var errs []error
	for _, s := range senders {
		err := s.Send(ctx, m)
		n.logSend(now, m, s.Name(), err)
		if err != nil {
			errs = append(errs, err)
		}
	}
	err := errors.Join(errs...)
	n.ownResult(now, gen, err)
	if err == nil {
		n.Wake() // the kept notices can go out now
	}
	return err
}

// notice sends a settings notice to senders, built in channel generation
// built. current: they are the channels configured now. own: they are the
// webhook for settings notices, whose result never shows in Delivery.
func (n *Notifier) notice(senders []Sender, own bool, built int, current bool, nt Notice) {
	cfg := n.cfg.Get()
	now := n.now()
	n.dmu.Lock()
	n.noticeSeq++
	key := fmt.Sprintf("notice:%d:%d", now.UnixNano(), n.noticeSeq)
	gen := n.gen
	n.dmu.Unlock()
	title := nt.Title
	m := Message{Kind: KindNotice, Title: noticeIcon + title, Summary: nt.Summary, Color: ColorNotice, URL: cfg.PublicURL, Key: key}
	p := pendingNotice{Key: key, Title: m.Title, Summary: m.Summary, Security: nt.Security, At: now}
	if nt.Security {
		// Kept before the first send: a stop of syncwatch while Discord does
		// not answer (up to a minute) does not lose it. When the call
		// returns, the notice is stored (the command line deletes its own
		// copy then). The retries skip it until the first send ends.
		kept := p
		kept.sending = true
		n.queueNotice(kept)
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		failed, err := n.sendNotice(ctx, now, m, senders, current && !own, gen)
		if own && built == n.generation() {
			n.ownResult(now, gen, err)
		}
		if err != nil && own && nt.Security && !invalidMessage(err) && n.ownFailingFor(p, now) > noticeFallbackAfter {
			// The webhook for settings notices fails for more than an hour:
			// the channel of the findings too, as for a kept notice.
			more, aerr := n.sendNotice(ctx, now, m, n.senders(), false, gen)
			if aerr == nil {
				err = nil
			} else {
				failed, err = append(failed, more...), errors.Join(err, aerr)
			}
		}
		switch {
		case err == nil:
		case errors.Is(err, errNoChannel):
			n.log.Info("settings notice not sent: no Discord webhook is configured", "title", title)
			n.logNotice(now, m.Title, channelDiscord, err.Error())
		case nt.Security && invalidMessage(err):
			// Discord refuses the message itself: no channel takes it.
			n.log.Warn("settings notice discarded, Discord refused it", "title", title, "err", err)
			n.logFailures(now, m.Title, failed, "discarded: ")
		case nt.Security:
			n.log.Warn("settings notice kept until a channel accepts it", "title", title, "err", err)
			n.logFailures(now, m.Title, failed, "kept until a channel accepts it: ")
			n.firstSendFailed(key)
			return
		case rejected(err) && built == n.generation():
			// The retry would go to the same channels, which refuse it:
			// one decision, one log line.
			n.log.Warn("settings notice discarded, Discord refused it", "title", title, "err", err)
			n.logFailures(now, m.Title, failed, "discarded: ")
		default:
			n.log.Warn("cannot send the settings notice, syncwatch retries it with the next notifications", "title", title, "err", err)
			n.logFailures(now, m.Title, failed, "kept for 1 day: ")
			p.GiveUp = now.Add(noticeRetryFor)
			n.queueNotice(p)
		}
		if nt.Security {
			n.dropNotice(key) // sent, or no channel takes it
		}
	}()
}

// firstSendFailed lets the retries send a security notice whose first send
// failed. The stored list does not change.
func (n *Notifier) firstSendFailed(key string) {
	n.dmu.Lock()
	for i := range n.notices {
		if n.notices[i].Key == key {
			n.notices[i].sending = false
		}
	}
	n.dmu.Unlock()
	n.Wake()
}

// chanErr is the failure of one channel.
type chanErr struct {
	channel string
	err     error
}

// sendNotice delivers a settings notice to senders. It counts as sent when
// one of the channels accepts it; a channel that got it on an earlier try
// is skipped. Each success goes to the notification log. The failures are
// the caller's: one line for each decision, not one for each try. record:
// the senders are the current channels of the findings, whose result is the
// delivery state of the banner.
func (n *Notifier) sendNotice(ctx context.Context, now time.Time, m Message, senders []Sender, record bool, gen int) ([]chanErr, error) {
	if len(senders) == 0 {
		return nil, errNoChannel
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	var failed []chanErr
	sent := false
	for _, s := range senders {
		if n.alreadySent(m.Key, s.Name()) {
			sent = true
			continue
		}
		if err := s.Send(ctx, m); err != nil {
			failed = append(failed, chanErr{s.Name(), err})
			continue
		}
		n.logSend(now, m, s.Name(), nil)
		n.markSent(m.Key, s.Name())
		sent = true
	}
	var err error
	if sent {
		n.forgetSent(m.Key)
	} else if len(failed) == 1 {
		err = failed[0].err
	} else {
		errs := make([]error, len(failed))
		for i, f := range failed {
			errs[i] = f.err
		}
		err = errors.Join(errs...)
	}
	if record {
		n.recordDelivery(now, gen, err)
	}
	return failed, err
}

// logFailures writes one line for each channel that refused a settings
// notice, with the decision about it ("kept …", "discarded").
func (n *Notifier) logFailures(now time.Time, title string, failed []chanErr, decision string) {
	for _, f := range failed {
		n.logNotice(now, title, f.channel, decision+f.err.Error())
	}
}

// logNotice writes a failed settings notice to the notification log.
func (n *Notifier) logNotice(now time.Time, title, channel, text string) {
	if e := n.st.LogNotification(now, KindNotice, channel, false, text, title); e != nil {
		n.log.Warn("cannot write the notification log", "err", e)
	}
}

// ownResult records the result of a send to the webhook for settings
// notices that started in channel generation gen: a success ends its
// back-off, a failure makes it longer (1, 2, 4 … minutes as for the
// channel of the findings). A refused message (HTTP 400) says nothing
// about the webhook.
func (n *Notifier) ownResult(now time.Time, gen int, err error) {
	if err != nil && invalidMessage(err) {
		return
	}
	n.dmu.Lock()
	defer n.dmu.Unlock()
	if gen != n.gen {
		return
	}
	if err == nil {
		n.own = ownChannel{}
		return
	}
	n.own.failures++
	n.own.retryAt = now.Add(backoff(n.own.failures, err))
	if n.own.since.IsZero() {
		n.own.since = now
	}
}

// ownFailingFor is how long the webhook for settings notices has failed, as
// far as a kept notice p knows: from its first failure in this run, or from
// the change of p (its first send failed then; this survives a restart).
func (n *Notifier) ownFailingFor(p pendingNotice, now time.Time) time.Duration {
	n.dmu.Lock()
	since := n.own.since
	n.dmu.Unlock()
	if since.IsZero() || p.At.Before(since) {
		since = p.At
	}
	return now.Sub(since)
}

// queueNotice keeps a notice whose first delivery failed, so a change made
// while a channel is down still gets announced.
func (n *Notifier) queueNotice(p pendingNotice) {
	p.Summary += "\nChanged at " + p.At.In(n.cfg.Get().Location()).Format("2006-01-02 15:04") + ". syncwatch kept this settings notice because the first send failed."
	n.dmu.Lock()
	n.readKept()
	n.notices = append(n.notices, p)
	for len(n.notices) > maxNotices {
		// The oldest notice that is not about security goes first.
		i := max(slices.IndexFunc(n.notices, func(p pendingNotice) bool { return !p.Security }), 0)
		old := n.notices[i]
		n.log.Warn("oldest delayed settings notice discarded", "title", strings.TrimPrefix(old.Title, noticeIcon))
		n.logNotice(p.At, old.Title, "", fmt.Sprintf("discarded: more than %d settings notices wait for a channel", maxNotices))
		n.notices = slices.Delete(n.notices, i, i+1)
	}
	n.saveKept()
	n.dmu.Unlock()
	n.Wake()
}

// readKept reads the notices that were kept before the start, one time.
// The caller holds dmu.
func (n *Notifier) readKept() {
	if n.keptRead {
		return
	}
	n.keptRead = true
	var kept []pendingNotice
	if _, err := n.st.JSONSetting(keyKept, &kept); err != nil {
		n.log.Warn("cannot read the kept settings notices", "err", err)
		return
	}
	if len(kept) > 0 {
		n.notices = append(kept, n.notices...)
		n.log.Info("kept settings notices loaded", "count", len(kept))
	}
}

// saveKept stores the kept notices. The caller holds dmu.
func (n *Notifier) saveKept() {
	list := n.notices
	if list == nil {
		list = []pendingNotice{}
	}
	if err := n.st.SetJSONSetting(keyKept, list); err != nil {
		n.log.Warn("cannot save the kept settings notices", "err", err)
	}
}

// keptNotices returns a copy of the kept notices, oldest first, without the
// ones whose first send runs.
func (n *Notifier) keptNotices() []pendingNotice {
	n.dmu.Lock()
	defer n.dmu.Unlock()
	n.readKept()
	return slices.DeleteFunc(slices.Clone(n.notices), func(p pendingNotice) bool { return p.sending })
}

// keptMessage is the message of a kept notice.
func (n *Notifier) keptMessage(p pendingNotice) Message {
	return Message{Kind: KindNotice, Title: p.Title, Summary: p.Summary, Color: ColorNotice, URL: n.cfg.Get().PublicURL, Key: p.Key}
}

func (n *Notifier) dropNotice(key string) {
	n.dmu.Lock()
	defer n.dmu.Unlock()
	for i, p := range n.notices {
		if p.Key == key {
			n.notices = slices.Delete(n.notices, i, i+1)
			n.saveKept()
			return
		}
	}
}

// expired drops a kept notice that is not about security a day after the
// change. A security notice never expires.
func (n *Notifier) expired(p pendingNotice, now time.Time) bool {
	if p.Security {
		return false
	}
	giveUp := p.GiveUp
	if giveUp.IsZero() {
		giveUp = p.At.Add(noticeRetryFor)
	}
	if !now.After(giveUp) {
		return false
	}
	n.log.Warn("settings notice discarded after 1 day of send failures", "title", strings.TrimPrefix(p.Title, noticeIcon))
	n.logNotice(now, p.Title, "", "discarded after 1 day of send failures")
	n.dropNotice(p.Key)
	return true
}

// discardKept drops a kept notice that Discord refused for good.
func (n *Notifier) discardKept(now time.Time, p pendingNotice, failed []chanErr, err error) {
	n.log.Warn("settings notice discarded, Discord refused it", "title", strings.TrimPrefix(p.Title, noticeIcon), "err", err)
	n.logFailures(now, p.Title, failed, "discarded: ")
	n.dropNotice(p.Key)
}

// retryNotices sends the kept notices to the channels of the findings
// configured now (the ones a notice first went to may be gone; a dead old
// webhook mustn't hold up urgent messages). It is used when no webhook for
// settings notices is set. A notice that Discord refuses as not valid
// (HTTP 400) is dropped; a security notice is kept through any other
// failure; any other notice is dropped when Discord refuses it, and a day
// after the change. It stops at the first failure, so a notice that can't
// be sent costs each tick one attempt. Retries add no line to the
// notification log until the notice is sent or dropped.
func (n *Notifier) retryNotices(ctx context.Context, now time.Time) error {
	for _, p := range n.keptNotices() {
		if n.expired(p, now) {
			continue
		}
		failed, err := n.sendNotice(ctx, now, n.keptMessage(p), n.senders(), true, n.generation())
		switch {
		case err == nil:
			n.dropNotice(p.Key)
		case invalidMessage(err) || (!p.Security && rejected(err)):
			n.discardKept(now, p, failed, err)
		default:
			return err
		}
	}
	return nil
}

// retryOwnNotices sends the kept notices to the webhook for settings
// notices, with its own back-off: its failures never hold up the messages
// about findings and never show in Delivery. A kept security notice also
// goes to the channel of the findings while that webhook fails for more
// than noticeFallbackAfter; the first channel that accepts it ends the wait.
// After the first failure of the webhook in a pass, it gets nothing more in
// that pass, but the security notices behind the failed one still get the
// fallback: an older notice that the webhook does not take must not hold
// them up for its day.
func (n *Notifier) retryOwnNotices(ctx context.Context, now time.Time, own []Sender) {
	n.dmu.Lock()
	wait := now.Before(n.own.retryAt)
	n.dmu.Unlock()
	if wait {
		return
	}
	gen := n.generation()
	var last error // the result of the last send to the webhook
	tried := false
	defer func() {
		if tried {
			n.ownResult(now, gen, last)
		}
	}()
	down := false // the webhook failed in this pass
	for _, p := range n.keptNotices() {
		if n.expired(p, now) {
			continue
		}
		fallback := p.Security && n.ownFailingFor(p, now) > noticeFallbackAfter
		if down && !fallback {
			continue
		}
		m := n.keptMessage(p)
		var failed []chanErr
		var err error
		if !down {
			failed, err = n.sendNotice(ctx, now, m, own, false, gen)
			tried, last = true, err
		}
		if fallback && (down || err != nil && !invalidMessage(err)) {
			more, aerr := n.sendNotice(ctx, now, m, n.senders(), false, gen)
			if aerr == nil {
				err = nil
			} else {
				failed, err = append(failed, more...), errors.Join(err, aerr)
			}
		}
		switch {
		case err == nil:
			n.dropNotice(p.Key)
		case invalidMessage(err) || (!p.Security && rejected(err)):
			n.discardKept(now, p, failed, err)
		case down:
			return // the channel of the findings fails too
		default:
			down = true
		}
	}
}
