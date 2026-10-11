package notify

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/engine"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/store"
)

// With notifications.repeat_urgent, the urgent message goes out again with
// the role mention while an ERROR finding of a server stays open: every
// interval after the urgent message or the last repeat, one message for all
// findings that are due (W-050, admin-anna AA-11).
func TestUrgentMessageIsRepeated(t *testing.T) {
	start := local(2026, 10, 6, 10, 0)
	r := newRig(t, start, "AKKA", "SOL")
	setNotify(t, r, func(c *config.Config) { c.Notify.RepeatUrgent = config.Duration(2 * time.Hour) })
	r.setDown("SOL", true)
	r.at(start)
	pinged := start.Add(5*time.Minute + holdUrgent)
	r.at(pinged)
	if m := r.sender.take(); len(m) != 1 || m[0].Kind != KindUrgent {
		t.Fatalf("urgent message: %+v", m)
	}
	for at := pinged.Add(5 * time.Minute); at.Before(pinged.Add(2 * time.Hour)); at = at.Add(5 * time.Minute) {
		r.at(at)
	}
	if m := r.sender.take(); len(m) != 0 {
		t.Fatalf("before the interval: %s", m[0].Title)
	}
	r.at(pinged.Add(2 * time.Hour))
	msgs := r.sender.take()
	if len(msgs) != 1 {
		t.Fatalf("repeat: %+v", msgs)
	}
	m := msgs[0]
	if m.Kind != KindRepeat || !m.Mention || m.Title != "🚨 Still open after 2 hours: Server unreachable: SOL" || m.Color != ColorError {
		t.Fatalf("repeat: %+v", m)
	}
	if lines := m.Sections[0].Lines; m.Sections[0].Title != "Still open" || len(lines) != 1 ||
		!strings.Contains(lines[0], "Server SOL is unreachable: connection refused (open 2 hours) — [details](") ||
		!strings.Contains(lines[0], "\n↳ Possible causes: The server is off") {
		t.Fatalf("repeat lines: %+v", m.Sections)
	}
	r.at(pinged.Add(3 * time.Hour))
	if m := r.sender.take(); len(m) != 0 {
		t.Fatalf("1 hour after the repeat: %s", m[0].Title)
	}
	r.at(pinged.Add(4 * time.Hour))
	if m := r.sender.take(); len(m) != 1 || m[0].Title != "🚨 Still open after 4 hours: Server unreachable: SOL" {
		t.Fatalf("second repeat: %+v", m)
	}
	// A second server down: when both fall due, one message names both.
	r.now = pinged.Add(4*time.Hour + 5*time.Minute)
	r.setDown("AKKA", true)
	r.at(r.now)
	r.at(r.now.Add(5*time.Minute + holdUrgent))
	if m := r.sender.take(); len(m) != 1 || m[0].Kind != KindUrgent || !strings.Contains(m[0].Title, "AKKA") {
		t.Fatalf("urgent message for AKKA: %+v", m)
	}
	r.at(pinged.Add(6*time.Hour + 30*time.Minute))
	msgs = r.sender.take()
	if len(msgs) != 1 || msgs[0].Kind != KindRepeat || msgs[0].Title != "🚨 2 urgent findings are still open on AKKA, SOL: H1" || len(msgs[0].Sections[0].Lines) != 2 {
		t.Fatalf("repeat of 2 findings: %+v", msgs)
	}
}

// Which findings are repeated: only open urgent ERROR findings of one
// server that are announced, not stale, not snoozed and not ignored; none
// with the setting off.
func TestRepeatSelection(t *testing.T) {
	now := local(2026, 10, 6, 12, 0)
	cfg := config.Default()
	cfg.Notify.RepeatUrgent = config.Duration(2 * time.Hour)
	base := func(id string) *store.Record {
		return &store.Record{Finding: model.Finding{ID: id, Check: "H1", Checks: []string{"H1"}, Severity: model.Error, Server: "sol", Subject: "SOL", Urgent: true},
			OpenedAt: now.Add(-3 * time.Hour), PingedAt: now.Add(-2 * time.Hour)}
	}
	due := base("due")
	warning := base("warning")
	warning.Severity = model.Warn
	cross := base("cross")
	cross.Server, cross.Related = "", []string{"akka", "sol"}
	stale := base("stale")
	stale.Stale = true
	snoozed, ignored := base("snoozed"), base("ignored")
	quiet := base("not-announced")
	quiet.PingedAt = time.Time{}
	notUrgent := base("not-urgent")
	notUrgent.Urgent = false
	repeated := base("repeated")
	repeated.PingedAt, repeated.RepeatedAt = now.Add(-5*time.Hour), now.Add(-time.Hour)
	v := &engine.View{Open: []*store.Record{due, warning, cross, stale, snoozed, ignored, quiet, notUrgent, repeated}, Snoozes: map[string]store.Snooze{
		"snoozed": {FindingID: "snoozed", Kind: store.SnoozeKind, Until: now.Add(time.Hour)},
		"ignored": {FindingID: "ignored", Kind: store.IgnoreKind},
	}}
	if got := ids(repeatDue(cfg, v, now)); len(got) != 1 || got[0] != "due" {
		t.Fatalf("due: %v", got)
	}
	cfg.Notify.RepeatUrgent = 0
	if got := repeatDue(cfg, v, now); len(got) != 0 {
		t.Fatalf("with the setting off: %v", ids(got))
	}
}

// A repeat never goes out in the quiet hours: one that falls due there goes
// out at the first tick after them.
func TestRepeatWaitsForTheEndOfQuietHours(t *testing.T) {
	start := local(2026, 10, 6, 21, 0)
	r := newRig(t, start, "AKKA", "SOL")
	setNotify(t, r, func(c *config.Config) { c.Notify.RepeatUrgent = config.Duration(time.Hour) })
	r.setDown("SOL", true)
	r.at(start)
	r.at(start.Add(5*time.Minute + holdUrgent))
	if m := r.sender.take(); len(m) != 1 || m[0].Kind != KindUrgent {
		t.Fatalf("urgent message: %+v", m)
	}
	for at := local(2026, 10, 6, 21, 30); at.Before(local(2026, 10, 7, 7, 0)); at = at.Add(30 * time.Minute) {
		r.at(at)
		for _, m := range r.sender.take() {
			if m.Kind == KindRepeat && !at.Before(local(2026, 10, 6, 22, 0)) {
				t.Fatalf("repeat in the quiet hours at %s", at.Format("15:04"))
			}
		}
	}
	r.at(local(2026, 10, 7, 7, 0))
	if m := r.sender.take(); len(m) != 1 || m[0].Kind != KindRepeat || m[0].Title != "🚨 Still open after 10 hours: Server unreachable: SOL" {
		t.Fatalf("at the end of the quiet hours: %+v", m)
	}
}

// The daily reminder leaves out a finding with an urgent message or a repeat
// in the hour before it.
func TestReminderSkipsARecentRepeat(t *testing.T) {
	for _, repeat := range []bool{true, false} {
		start := local(2026, 10, 6, 5, 20)
		r := newRig(t, start, "AKKA", "SOL")
		setNotify(t, r, func(c *config.Config) {
			c.Notify.QuietHours, c.Notify.WeeklyDay = "off", "off"
			if repeat {
				c.Notify.RepeatUrgent = config.Duration(time.Hour)
			}
		})
		r.setDown("SOL", true)
		r.at(start)
		var kinds []string
		for at := start.Add(5*time.Minute + holdUrgent); at.Before(local(2026, 10, 6, 8, 1)); at = at.Add(time.Minute) {
			r.at(at) // the repeats at 06:25 and 07:25, the reminder at 08:00
			for _, m := range r.sender.take() {
				kinds = append(kinds, m.Kind+" "+at.Format("15:04"))
			}
		}
		want := "urgent 05:25 · repeat 06:25 · repeat 07:25"
		if !repeat {
			want = "urgent 05:25 · reminder 08:00"
		}
		if got := strings.Join(kinds, " · "); got != want {
			t.Errorf("repeat %v: %s, want %s", repeat, got, want)
		}
	}
}

// Urgent findings that fall due seconds apart share one message: the
// notifier waits until 10 seconds after the first one fell due, and a timer
// wakes it then. A finding due later than that gets its own message (RT-3,
// the AKKA and SOL findings of one directory 2 seconds apart).
func TestUrgentFindingsSecondsApartShareOneMessage(t *testing.T) {
	start := local(2026, 10, 6, 10, 0)
	r := newRig(t, start, "AKKA", "SOL", "SIRIUS")
	r.setDown("AKKA", true)
	r.at(start)
	r.now = start.Add(2 * time.Second)
	r.setDown("SOL", true)
	r.at(r.now)
	r.now = start.Add(15 * time.Second)
	r.setDown("SIRIUS", true)
	r.at(r.now)
	due := start.Add(5 * time.Minute)
	r.at(due) // AKKA due
	r.n.dmu.Lock()
	until, armed := r.n.holdUntil, r.n.hold != nil
	r.n.dmu.Unlock()
	if !armed || !until.Equal(due.Add(holdUrgent)) {
		t.Fatalf("hold until %s, timer %v", until, armed)
	}
	r.at(due.Add(2 * time.Second)) // SOL due
	if m := r.sender.take(); len(m) != 0 {
		t.Fatalf("sent in the hold window: %s", m[0].Title)
	}
	r.at(due.Add(holdUrgent)) // the timer
	msgs := r.sender.take()
	if len(msgs) != 1 || msgs[0].Title != "🚨 2 new urgent findings on AKKA, SOL: H1" || len(msgs[0].Sections[0].Lines) != 2 {
		t.Fatalf("one message for AKKA and SOL: %+v", msgs)
	}
	r.at(due.Add(15 * time.Second)) // SIRIUS due, 15 seconds after AKKA
	if m := r.sender.take(); len(m) != 0 {
		t.Fatalf("sent in the second hold window: %s", m[0].Title)
	}
	r.at(due.Add(25 * time.Second))
	if m := r.sender.take(); len(m) != 1 || m[0].Title != "🚨 Server unreachable: SIRIUS" {
		t.Fatalf("second message: %+v", m)
	}
}

// The next message about findings names the times when syncwatch did not
// run, once: after a successful send the gaps are reported. A test message
// and a settings notice do not take them (W-044d, admin-anna AA-24).
func TestNextMessageNamesTheGap(t *testing.T) {
	start := local(2026, 10, 6, 10, 0)
	r := newRig(t, start, "AKKA", "SOL")
	r.n.now = func() time.Time { return r.now }
	for _, g := range []store.Gap{
		{From: local(2026, 10, 5, 21, 4), To: local(2026, 10, 6, 7, 10)},
		{From: local(2026, 10, 6, 9, 4), To: local(2026, 10, 6, 9, 24)},
	} {
		if err := r.st.AddGap(g); err != nil {
			t.Fatal(err)
		}
	}
	unreported := func() int {
		gaps, err := r.st.Gaps(time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, g := range gaps {
			if !g.Reported {
				n++
			}
		}
		return n
	}
	if err := r.n.SendTest(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.n.Notice(Notice{Title: "Checks changed", Summary: "x"})
	waitFor(t, func() bool { return r.sender.sent() == 2 })
	for _, m := range r.sender.take() {
		if strings.Contains(m.Summary, "did not run") {
			t.Fatalf("%s message names the gap: %s", m.Kind, m.Summary)
		}
	}
	if unreported() != 2 {
		t.Fatal("a test message or a notice marked the gaps as reported")
	}
	r.sender.fail(&httpError{code: 503})
	r.setDown("SOL", true)
	r.at(start)
	r.at(start.Add(5*time.Minute + holdUrgent))
	if unreported() != 2 {
		t.Fatal("a failed send marked the gaps as reported")
	}
	r.sender.fail(nil)
	r.at(start.Add(7 * time.Minute))
	msgs := r.sender.take()
	if len(msgs) != 1 || msgs[0].Kind != KindUrgent ||
		msgs[0].Summary != "syncwatch did not run from 2026-10-05 21:04 to 07:10.\nsyncwatch did not run from 09:04 to 09:24." {
		t.Fatalf("urgent message: %+v", msgs)
	}
	if unreported() != 0 {
		t.Fatal("gaps not reported after the send")
	}
	r.setDown("SOL", false)
	r.at(start.Add(8 * time.Minute))
	r.at(start.Add(14 * time.Minute))
	if msgs := r.sender.take(); len(msgs) != 1 || msgs[0].Kind != KindResolved || strings.Contains(msgs[0].Summary, "did not run") {
		t.Fatalf("resolved message: %+v", msgs)
	}
}

// While notifications fail, Delivery has the time of the next try (the
// banner shows it); after a success it is zero (W-040).
func TestDeliveryNextTry(t *testing.T) {
	start := local(2026, 10, 6, 10, 0)
	r := newRig(t, start, "SOL")
	r.sender.fail(&httpError{code: 500})
	r.setDown("SOL", true)
	r.at(start)
	if d := r.n.Delivery(); d.Failing || !d.NextTry.IsZero() {
		t.Fatalf("before a send: %+v", d)
	}
	first := start.Add(5*time.Minute + holdUrgent)
	r.at(first)
	if d := r.n.Delivery(); !d.Failing || !d.NextTry.Equal(first.Add(time.Minute)) {
		t.Fatalf("after a 500: %+v", d)
	}
	r.at(first.Add(time.Minute))
	if d := r.n.Delivery(); !d.NextTry.Equal(first.Add(3 * time.Minute)) {
		t.Fatalf("after the second 500: %+v", d)
	}
	r.sender.fail(nil)
	r.at(first.Add(3 * time.Minute))
	if d := r.n.Delivery(); d.Failing || !d.NextTry.IsZero() {
		t.Fatalf("after a success: %+v", d)
	}
}
