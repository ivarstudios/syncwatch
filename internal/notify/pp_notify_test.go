package notify

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/store"
)

// The tests of the pre-release review (prepub) of the notifier.

// Device IDs of 2 PCs with the same name, EDIT-01: one at the office, one at
// site B. Syncthing does not make device names unique.
const (
	officePC = "OFFICE2-AAAAAAA-BBBBBBB-CCCCCCC-DDDDDDD-EEEEEEE-FFFFFFF-GGGGGGG"
	siteBPC  = "SITEB22-AAAAAAA-BBBBBBB-CCCCCCC-DDDDDDD-EEEEEEE-FFFFFFF-GGGGGGG"
)

// Two PCs with the same name are 2 devices: their findings have a line each
// in every message. The views of 1 PC from both servers stay 1 line (text-3).
func TestTwoPCsWithTheSameNameHaveTwoLines(t *testing.T) {
	cfg := twoNAS()
	now := local(2026, 10, 6, 12, 0)
	rec := func(id, check, server, subject, message, device string) *store.Record {
		return &store.Record{Finding: model.Finding{ID: id, Check: check, Checks: []string{check}, Severity: model.Warn, Server: server, Urgent: true,
			Subject: subject, Message: message, Details: []model.Detail{{Key: "Folder ID", Value: "live-x"}, {Key: "Device ID", Value: device}}},
			OpenedAt: now.Add(-25 * time.Hour), PingedAt: now.Add(-24 * time.Hour), ResolvedAt: now}
	}
	h5 := func(id, server, name, device, pct string) *store.Record {
		return rec(id, "H5", server, "LIVE-X on EDIT-01",
			fmt.Sprintf("Device EDIT-01 is stuck at %s%% of folder LIVE-X for more than 24 hours (120 remaining items, seen from %s)", pct, name), device)
	}
	h9 := func(id, server, name, device string) *store.Record {
		return rec(id, "H9", server, "EDIT-01 on "+name, "Unknown device EDIT-01 requests a connection to "+name, device)
	}
	h11 := func(id, server, name, device string) *store.Record {
		return rec(id, "H11", server, name+" ↔ EDIT-01", "Device EDIT-01 is connected to "+name+" through a relay, which is slow", device)
	}
	for _, c := range []struct {
		what string
		rs   []*store.Record
		want int
	}{
		{"H5 of 2 PCs", []*store.Record{h5("a", "akka", "AKKA", officePC, "40"), h5("b", "sol", "SOL", siteBPC, "12")}, 2},
		{"H5 of 1 PC from both servers", []*store.Record{h5("a", "akka", "AKKA", officePC, "40"), h5("b", "sol", "SOL", officePC, "12")}, 1},
		{"H9 of 2 devices", []*store.Record{h9("c", "akka", "AKKA", officePC), h9("d", "sol", "SOL", siteBPC)}, 2},
		{"H9 of 1 device on both servers", []*store.Record{h9("c", "akka", "AKKA", officePC), h9("d", "sol", "SOL", officePC)}, 1},
		{"H11 of 2 PCs", []*store.Record{h11("e", "akka", "AKKA", officePC), h11("f", "sol", "SOL", siteBPC)}, 2},
	} {
		for kind, m := range map[string]Message{
			"urgent":   UrgentMessage(cfg, c.rs),
			"repeat":   RepeatMessage(cfg, c.rs, now),
			"reminder": ReminderMessage(cfg, c.rs, now),
			"resolved": ResolvedMessage(cfg, c.rs, nil),
		} {
			if lines := m.Sections[0].Lines; len(lines) != c.want {
				t.Errorf("%s, %s message: %d lines, want %d: %q", c.what, kind, len(lines), c.want, lines)
			}
		}
		night := OvernightMessage(cfg, c.rs, nil, nil, nil, nil)
		if want := fmt.Sprintf("New and still open (%d)", c.want); night.Sections[0].Title != want {
			t.Errorf("%s, overnight summary: %q, want %q", c.what, night.Sections[0].Title, want)
		}
	}
	// The line of the PC at site B has its own state and link.
	lines := strings.Join(UrgentMessage(cfg, []*store.Record{h5("a", "akka", "AKKA", officePC, "40"), h5("b", "sol", "SOL", siteBPC, "12")}).Sections[0].Lines, "\n")
	if !strings.Contains(lines, "stuck at 12% of folder LIVE-X") || !strings.Contains(lines, "/problems#f-b)") || strings.Contains(lines, "also seen from") {
		t.Errorf("urgent lines:\n%s", lines)
	}
}

// Through the rules and the engine: 2 unknown devices with the same name
// request a connection, one to AKKA and one to SOL (H9 urgent). The urgent
// message has 2 lines (text-3).
func TestTwoUnknownDevicesWithTheSameNameInTheUrgentMessage(t *testing.T) {
	start := local(2026, 10, 6, 10, 0)
	r := newRig(t, start, "AKKA", "SOL")
	setNotify(t, r, func(c *config.Config) {
		urgent, debounce := true, config.Duration(time.Minute)
		c.Checks = map[string]config.Check{"H9": {Urgent: &urgent, Debounce: &debounce}}
	})
	for server, id := range map[string]string{"akka": officePC, "sol": siteBPC} {
		r.hub.Update(server, func(s *model.Server) {
			s.PendingDevices = []model.PendingDevice{{DeviceID: id, Name: "EDIT-01", Address: "10.0.0.9:22000", Time: start}}
		})
	}
	r.at(start)
	r.at(start.Add(time.Minute + holdUrgent))
	msgs := r.sender.take()
	if len(msgs) != 1 || msgs[0].Kind != KindUrgent {
		t.Fatalf("urgent message: %+v", msgs)
	}
	if lines := msgs[0].Sections[0].Lines; len(lines) != 2 || strings.Contains(strings.Join(lines, "\n"), "also on") {
		t.Errorf("%s: %q", msgs[0].Title, lines)
	}
}

// probeSender is a fake channel that can also check itself without a
// message, as the Discord sender does (Probe).
type probeSender struct {
	*fakeSender
	probes int
}

func (p *probeSender) Probe(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.probes++
	return p.err
}

// unhealthyFrom says whether /healthz answers 503 at now for the state of
// the notifications (the rule of web/health.go with the default limit of 15
// minutes).
func unhealthyFrom(d Delivery, now time.Time) bool {
	return d.Failing && !d.Since.IsZero() && now.Sub(d.Since) > 15*time.Minute
}

// A test message fails during a short Discord outage, and no message is due
// after it. syncwatch checks the webhook without a message and ends the
// failure: /healthz does not answer 503 for days (notify-2, exposure-3).
func TestFailedTestMessageEndsWhenDiscordWorksAgain(t *testing.T) {
	start := local(2026, 10, 12, 9, 59) // a Monday, nothing is due
	r := newRig(t, start, "AKKA")
	r.n.now = func() time.Time { return r.now }
	p := &probeSender{fakeSender: r.sender}
	r.n.SetSenders(func() []Sender { return []Sender{p} })
	r.at(start) // the weekly report of this Monday: the channel works
	r.sender.take()
	r.now = local(2026, 10, 12, 10, 0)
	r.sender.fail(&httpError{code: 503})
	if err := r.n.SendTest(context.Background()); err == nil {
		t.Fatal("the test message did not fail")
	}
	if d := r.n.Delivery(); !d.Failing {
		t.Fatalf("after the failed test message: %+v", d)
	}
	var cleared time.Time
	for at := r.now.Add(30 * time.Second); !at.After(local(2026, 10, 13, 10, 0)); at = at.Add(30 * time.Second) {
		if at.Equal(local(2026, 10, 12, 10, 1)) {
			r.sender.fail(nil) // Discord works again
		}
		r.at(at)
		d := r.n.Delivery()
		if unhealthyFrom(d, at) {
			t.Fatalf("%s: /healthz answers 503: %+v", at.Format("01-02 15:04:05"), d)
		}
		if !d.Failing && cleared.IsZero() {
			cleared = at
		}
	}
	if cleared.IsZero() || cleared.After(local(2026, 10, 12, 10, 3)) {
		t.Errorf("the failure ended at %v, want by 10:03", cleared)
	}
	if m := r.sender.take(); len(m) != 0 {
		t.Errorf("the check posted %d messages: %q", len(m), m[0].Title)
	}
	if p.probes == 0 || p.probes > 5 {
		t.Errorf("%d checks of the webhook", p.probes)
	}
}

// An urgent message fails just before the quiet hours, and Discord works
// again in them. Nothing is sent in the quiet hours, but the check ends the
// failure: no 503 for the night. The overnight summary still announces the
// finding (exposure-3).
func TestFailureBeforeQuietHoursEndsInThem(t *testing.T) {
	start := local(2026, 10, 6, 21, 50)
	r := newRig(t, start, "AKKA", "SOL")
	setNotify(t, r, func(c *config.Config) { c.Notify.QuietHours = "22:00-07:00" })
	p := &probeSender{fakeSender: r.sender}
	r.n.SetSenders(func() []Sender { return []Sender{p} })
	r.setDown("SOL", true)
	r.sender.fail(&httpError{code: 503})
	var failedAt, cleared time.Time
	for at := start; at.Before(local(2026, 10, 7, 7, 0)); at = at.Add(30 * time.Second) {
		if at.Equal(local(2026, 10, 6, 22, 3)) {
			r.sender.fail(nil)
		}
		r.at(at)
		d := r.n.Delivery()
		if d.Failing && failedAt.IsZero() {
			failedAt = at
		}
		if !failedAt.IsZero() && !d.Failing && cleared.IsZero() {
			cleared = at
		}
		if unhealthyFrom(d, at) {
			t.Fatalf("%s: /healthz answers 503: %+v", at.Format("15:04:05"), d)
		}
	}
	if failedAt.IsZero() || !failedAt.Before(local(2026, 10, 6, 22, 0)) {
		t.Fatalf("the urgent message did not fail before the quiet hours: %v", failedAt)
	}
	if cleared.IsZero() || cleared.After(local(2026, 10, 6, 22, 10)) {
		t.Errorf("the failure ended at %v, want by 22:10", cleared)
	}
	if m := r.sender.take(); len(m) != 0 {
		t.Fatalf("sent in the quiet hours: %q", m[0].Title)
	}
	r.at(local(2026, 10, 7, 7, 0))
	m := r.sender.take()
	if len(m) != 1 || m[0].Kind != KindOvernight || !strings.Contains(messageText(m[0]), "SOL") {
		t.Fatalf("at the end of the quiet hours: %+v", m)
	}
}

// The urgent message fails and the administrator snoozes the finding before
// the next try: nothing is due, but the check ends the failure (notify-2).
func TestFailureEndsAfterTheFindingIsSnoozed(t *testing.T) {
	start := local(2026, 10, 6, 10, 0)
	r := newRig(t, start, "AKKA")
	p := &probeSender{fakeSender: r.sender}
	r.n.SetSenders(func() []Sender { return []Sender{p} })
	r.setDown("AKKA", true)
	r.sender.fail(&httpError{code: 503})
	at := start
	for ; !r.n.Delivery().Failing; at = at.Add(30 * time.Second) {
		r.at(at)
	}
	if err := r.eng.Snooze(r.eng.View().Open[0].ID, at.Add(24*time.Hour), "maintenance", "admin"); err != nil {
		t.Fatal(err)
	}
	r.sender.fail(nil)
	end := at.Add(10 * time.Minute)
	for ; !at.After(end); at = at.Add(30 * time.Second) {
		r.at(at)
	}
	if d := r.n.Delivery(); d.Failing {
		t.Fatalf("10 minutes after the snooze, with Discord working: %+v", d)
	}
	if m := r.sender.take(); len(m) != 0 {
		t.Fatalf("sent: %q", m[0].Title)
	}
}

// A webhook that Discord deleted stays failing: the checks back off as the
// sends do, and the banner shows the time of the next check.
func TestCheckOfADeletedWebhookKeepsTheFailure(t *testing.T) {
	start := local(2026, 10, 6, 10, 0)
	r := newRig(t, start, "AKKA")
	r.n.now = func() time.Time { return r.now }
	p := &probeSender{fakeSender: r.sender}
	r.n.SetSenders(func() []Sender { return []Sender{p} })
	r.sender.fail(&httpError{code: 404, text: "Unknown Webhook"})
	r.now = start
	_ = r.n.SendTest(context.Background())
	for m := 0; m <= 60; m++ {
		r.at(start.Add(time.Duration(m) * time.Minute))
	}
	d := r.n.Delivery()
	if !d.Failing || !d.Since.Equal(start) || !strings.Contains(d.Error, "404") || !d.NextTry.After(start.Add(time.Hour)) {
		t.Fatalf("after an hour: %+v", d)
	}
	// 10:00, :01, :03, :07, :15, :31 (1, 2, 4, 8, 16, 30 minutes).
	if p.probes != 6 {
		t.Errorf("%d checks in an hour, want 6", p.probes)
	}
}

// Discord.Probe asks for the webhook with GET and posts nothing. It does not
// follow a redirect, and a deleted webhook is refused (not transient).
func TestDiscordProbe(t *testing.T) {
	was := config.AllowAnyWebhook
	config.AllowAnyWebhook = true
	t.Cleanup(func() { config.AllowAnyWebhook = was })
	status, posts := http.StatusOK, 0
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method != http.MethodGet {
			posts++
		}
		if status == http.StatusMovedPermanently {
			http.Redirect(w, r, "http://127.0.0.1:1/elsewhere", status)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"type":1,"id":"1","token":"secret-token"}`))
	}))
	defer srv.Close()
	d := &Discord{URL: srv.URL + "/api/webhooks/1/secret-token"}
	set := func(s int) { mu.Lock(); status = s; mu.Unlock() }
	if err := d.Probe(context.Background()); err != nil {
		t.Fatalf("200: %v", err)
	}
	set(http.StatusNotFound)
	if err := d.Probe(context.Background()); !rejected(err) || transient(err) {
		t.Errorf("404: %v", err)
	}
	set(http.StatusServiceUnavailable)
	if err := d.Probe(context.Background()); err == nil || !transient(err) {
		t.Errorf("503: %v", err)
	}
	set(http.StatusMovedPermanently)
	if err := d.Probe(context.Background()); err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Errorf("301: %v", err)
	}
	if posts != 0 {
		t.Errorf("%d posts", posts)
	}
	srv.Close()
	if err := d.Probe(context.Background()); err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Errorf("no connection: %v", err)
	}
}

// syncwatch restarts while Discord refuses every message. The new process
// continues the failure from its start, thus /healthz answers 503 at once
// and the dead-man URL is not called (exposure-1). A send that works and a
// new webhook end the failure, also for the next start.
func TestRestartKeepsTheFailure(t *testing.T) {
	start := local(2026, 10, 6, 10, 0)
	r := newRig(t, start, "AKKA")
	r.sender.fail(&httpError{code: 404, text: "Unknown Webhook"})
	r.setDown("AKKA", true)
	for m := 0; m <= 30; m++ {
		r.at(start.Add(time.Duration(m) * time.Minute))
	}
	before := r.n.Delivery()
	if !before.Failing || !unhealthyFrom(before, start.Add(30*time.Minute)) {
		t.Fatalf("before the restart: %+v", before)
	}
	restart := func() *Notifier {
		n := New(r.eng, r.st, r.n.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
		n.now = func() time.Time { return r.now }
		n.SetSenders(func() []Sender { return []Sender{r.sender} })
		return n
	}
	r.n = restart()
	if d := r.n.Delivery(); !d.Failing || !d.Since.Equal(before.Since) || d.Error != before.Error || !unhealthyFrom(d, start.Add(31*time.Minute)) {
		t.Fatalf("after the restart: %+v, before it: %+v", d, before)
	}

	// Discord works again: the urgent message goes out and ends the failure.
	r.sender.fail(nil)
	r.at(start.Add(31 * time.Minute))
	if m := r.sender.take(); len(m) != 1 || m[0].Kind != KindUrgent {
		t.Fatalf("after the restart, with Discord working: %+v", m)
	}
	if r.n.Delivery().Failing || restart().Delivery().Failing {
		t.Fatalf("the failure did not end: %+v", r.n.Delivery())
	}

	// A new webhook ends the failure too.
	r.sender.fail(&httpError{code: 404, text: "Unknown Webhook"})
	_ = r.n.SendTest(context.Background())
	if !restart().Delivery().Failing {
		t.Fatal("the failure of the test message is not kept")
	}
	r.n.ChannelsChanged()
	if d := restart().Delivery(); d.Failing {
		t.Fatalf("after a new webhook and a restart: %+v", d)
	}
}

// The webhook for settings notices fails with an error that keeps a notice
// that is not about security for a day (HTTP 503, a redirect). A security
// notice behind such an older notice still goes to the channel of the
// findings after 1 hour, not after the day of the older notice (notify-3).
func TestFallbackIsNotBlockedByAnOlderNotice(t *testing.T) {
	for _, ownErr := range []error{&httpError{code: 503}, &httpError{code: 301, text: "redirect"}, errNoticesUndecryptable} {
		start := local(2026, 10, 6, 10, 0)
		r := newRig(t, start, "AKKA")
		r.n.now = func() time.Time { return r.now }
		own := &fakeSender{name: "discord-notices"}
		own.fail(ownErr)
		r.n.SetNoticeSenders(func() []Sender { return []Sender{own} })
		r.n.Notice(Notice{Title: "Checks changed", Summary: "H7 is off."})
		waitFor(t, func() bool { return own.tries() == 1 && pendingNotices(r) == 1 })
		r.now = start.Add(5 * time.Minute)
		r.n.Notice(Notice{Title: "Server AKKA changed", Summary: "The API key is replaced.", Security: true})
		waitFor(t, func() bool { return own.tries() == 2 && pendingNotices(r) == 2 })
		var arrived time.Time
		for at := r.now; at.Before(start.Add(3 * time.Hour)); at = at.Add(30 * time.Second) {
			r.at(at)
			for _, m := range r.sender.take() {
				if m.Title != "🔧 Server AKKA changed" {
					t.Fatalf("%v: the channel of the findings got %q", ownErr, m.Title)
				}
				arrived = at
			}
		}
		t.Logf("%v: the security notice reached the channel of the findings at %s", ownErr, arrived.Format("15:04:05"))
		if arrived.IsZero() || arrived.After(start.Add(time.Hour+11*time.Minute)) {
			t.Errorf("%v: the security notice reached the channel of the findings at %v, want by 11:11", ownErr, arrived)
		}
		if pendingNotices(r) != 1 {
			t.Errorf("%v: %d notices kept, want 1 (Checks changed)", ownErr, pendingNotices(r))
		}
	}
}
