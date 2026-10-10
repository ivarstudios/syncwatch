package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/engine"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/store"
)

type fakeSender struct {
	mu       sync.Mutex
	msgs     []Message
	err      error
	name     string // "" is discord
	attempts int
}

func (f *fakeSender) Name() string {
	if f.name != "" {
		return f.name
	}
	return "discord"
}

func (f *fakeSender) fail(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

func (f *fakeSender) Send(_ context.Context, m Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	if f.err != nil {
		return f.err
	}
	f.msgs = append(f.msgs, m)
	return nil
}
func (f *fakeSender) take() []Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.msgs
	f.msgs = nil
	return out
}

type rig struct {
	t      *testing.T
	now    time.Time
	hub    *model.Hub
	eng    *engine.Engine
	n      *Notifier
	sender *fakeSender
	st     *store.Store
}

var stockholm, _ = time.LoadLocation("Europe/Stockholm")

func newRig(t *testing.T, start time.Time, servers ...string) *rig {
	t.Helper()
	r := &rig{t: t, now: start, sender: &fakeSender{}}
	key := make([]byte, 32)
	st, err := store.OpenMemory(key)
	if err != nil {
		t.Fatal(err)
	}
	r.st = st
	var cs []config.Server
	var hs []*model.Server
	for _, s := range servers {
		cs = append(cs, config.Server{ID: strings.ToLower(s), Name: s, URL: "https://" + s + ":8384"})
		hs = append(hs, &model.Server{ID: strings.ToLower(s), Name: s, Status: model.StatusUp, Loaded: true, MyID: s + "-ID"})
	}
	cfg := &config.Config{Servers: cs, Timezone: "Europe/Stockholm", PublicURL: "http://sw.local:8080"}
	h, err := config.NewHolder(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r.hub = model.NewHub(func() time.Time { return r.now })
	r.hub.SetServers(hs)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	r.eng, err = engine.New(st, r.hub, h, func() time.Time { return r.now }, quiet)
	if err != nil {
		t.Fatal(err)
	}
	r.n = New(r.eng, st, h, quiet)
	r.n.SetSenders(func() []Sender { return []Sender{r.sender} })
	return r
}

func (r *rig) at(t time.Time) {
	r.now = t
	r.eng.Evaluate()
	r.n.Tick(context.Background(), t)
}

func (r *rig) setDown(server string, down bool) {
	r.hub.Update(strings.ToLower(server), func(s *model.Server) {
		if down {
			s.Status, s.DownSince, s.LastError = model.StatusDown, r.now, "connection refused"
		} else {
			s.Status = model.StatusUp
		}
	})
}

func local(y int, m time.Month, d, h, min int) time.Time {
	return time.Date(y, m, d, h, min, 0, 0, stockholm)
}

func TestDebounceBatchResolve(t *testing.T) {
	r := newRig(t, local(2026, 10, 6, 10, 0), "AKKA", "SOL", "SIRIUS")
	r.setDown("SOL", true)
	r.setDown("SIRIUS", true)
	r.at(r.now)
	r.at(r.now.Add(2 * time.Minute))
	if m := r.sender.take(); len(m) != 0 {
		t.Fatalf("sent before debounce: %v", m[0].Title)
	}
	r.at(local(2026, 10, 6, 10, 5))
	msgs := r.sender.take()
	if len(msgs) != 1 {
		t.Fatalf("want one batched message, got %d", len(msgs))
	}
	m := msgs[0]
	if !m.Mention || m.Kind != KindUrgent || len(m.Sections[0].Lines) != 2 {
		t.Fatalf("urgent message: %+v", m)
	}
	r.at(local(2026, 10, 6, 10, 6))
	if len(r.sender.take()) != 0 {
		t.Fatal("pinged twice")
	}
	r.setDown("SOL", false)
	r.at(local(2026, 10, 6, 10, 20))
	if m := r.sender.take(); len(m) != 0 {
		t.Fatalf("resolved sent before it settled: %v", m[0].Title)
	}
	r.at(local(2026, 10, 6, 10, 25))
	msgs = r.sender.take()
	if len(msgs) != 1 || msgs[0].Kind != KindResolved || msgs[0].Mention || !strings.Contains(msgs[0].Title, "SOL") {
		t.Fatalf("resolved: %+v", msgs)
	}
	r.at(local(2026, 10, 6, 10, 26))
	if len(r.sender.take()) != 0 {
		t.Fatal("resolved sent twice")
	}
}

// Urgent findings that open before any channel is configured are announced
// once one is, and findings never announced get no "resolved" message.
func TestNothingMarkedSentWithoutChannel(t *testing.T) {
	r := newRig(t, local(2026, 10, 6, 10, 0), "AKKA", "SOL")
	configured := false
	r.n.SetSenders(func() []Sender {
		if !configured {
			return nil
		}
		return []Sender{r.sender}
	})
	r.setDown("AKKA", true)
	r.setDown("SOL", true)
	r.at(r.now)
	r.at(local(2026, 10, 6, 10, 10))
	for _, rec := range r.eng.View().Open {
		if !rec.PingedAt.IsZero() {
			t.Fatalf("%s marked as pinged without a channel", rec.Subject)
		}
	}
	// SOL comes back before anyone could have been told about it.
	r.setDown("SOL", false)
	r.at(local(2026, 10, 6, 10, 15))

	configured = true
	r.at(local(2026, 10, 6, 10, 20))
	msgs := r.sender.take()
	if len(msgs) != 1 || msgs[0].Kind != KindUrgent || !strings.Contains(msgs[0].Title, "AKKA") {
		t.Fatalf("after adding a channel: %+v", msgs)
	}
	r.at(local(2026, 10, 6, 10, 21))
	if m := r.sender.take(); len(m) != 0 {
		t.Fatalf("unexpected follow-up (resolved for the never-announced SOL?): %s", m[0].Title)
	}
}

// While Discord fails, retries must not send the message to the other
// channel again.
func TestRetryOnlyResendsToFailedChannels(t *testing.T) {
	r := newRig(t, local(2026, 10, 6, 10, 0), "AKKA")
	other := &fakeSender{name: "second"}
	r.n.SetSenders(func() []Sender { return []Sender{r.sender, other} })
	r.sender.fail(errors.New("discord: HTTP 404"))
	r.setDown("AKKA", true)
	r.at(r.now)
	for i := 0; i < 5; i++ {
		r.at(local(2026, 10, 6, 10, 10).Add(time.Duration(i) * time.Hour))
	}
	if n := len(other.take()); n != 1 {
		t.Fatalf("second channel got the urgent message %d times while Discord failed", n)
	}
	r.sender.fail(nil)
	r.at(local(2026, 10, 6, 16, 0))
	if m := r.sender.take(); len(m) != 1 || m[0].Kind != KindUrgent {
		t.Fatalf("Discord after recovering: %+v", m)
	}
	if n := len(other.take()); n != 0 {
		t.Fatalf("second channel got %d more messages after Discord recovered", n)
	}
	r.at(local(2026, 10, 6, 16, 5))
	if len(r.sender.take()) != 0 {
		t.Fatal("urgent message sent twice to Discord")
	}
}

// A dead webhook is retried at growing intervals, logged once, and reported
// as failing until a send succeeds.
func TestFailingChannelBacksOff(t *testing.T) {
	start := local(2026, 10, 6, 10, 0)
	r := newRig(t, start, "AKKA")
	r.sender.fail(errors.New("discord: HTTP 404: Unknown Webhook"))
	r.setDown("AKKA", true)
	r.at(start)
	// The urgent message is due at 10:05. Tick every minute for an hour:
	// attempts at 10:05, :06, :08, :12, :20 and :36 (back-off 1, 2, 4, 8, 16, 30 min).
	for m := 1; m <= 65; m++ {
		r.at(start.Add(time.Duration(m) * time.Minute))
	}
	r.sender.mu.Lock()
	attempts := r.sender.attempts
	r.sender.mu.Unlock()
	if attempts != 6 {
		t.Fatalf("%d attempts in an hour; want 6", attempts)
	}
	d := r.n.Delivery()
	if !d.Failing || !d.Since.Equal(local(2026, 10, 6, 10, 5)) || !strings.Contains(d.Error, "404") {
		t.Fatalf("delivery state: %+v", d)
	}
	// Replacing the channel clears the old channel's failure from the banner.
	r.n.ChannelsChanged()
	if d := r.n.Delivery(); d.Failing {
		t.Fatalf("still failing after the channel was replaced: %+v", d)
	}
	log, _ := r.st.Notifications(100)
	if len(log) != 1 || log[0].OK {
		t.Fatalf("notification log has %d rows for one repeated failure", len(log))
	}

	// A new webhook is tried at once, not after the back-off.
	r.sender.fail(nil)
	r.n.ChannelsChanged()
	r.at(local(2026, 10, 6, 11, 6))
	if m := r.sender.take(); len(m) != 1 || m[0].Kind != KindUrgent {
		t.Fatalf("after the channel was fixed: %+v", m)
	}
	if d := r.n.Delivery(); d.Failing {
		t.Fatalf("still failing after a successful send: %+v", d)
	}
	if log, _ := r.st.Notifications(100); len(log) != 2 || !log[0].OK {
		t.Fatalf("recovery not logged: %+v", log)
	}
}

func TestShortBlipNotSent(t *testing.T) {
	r := newRig(t, local(2026, 10, 6, 10, 0), "AKKA")
	r.setDown("AKKA", true)
	r.at(r.now)
	r.setDown("AKKA", false)
	r.at(r.now.Add(3 * time.Minute))
	r.at(r.now.Add(10 * time.Minute))
	if m := r.sender.take(); len(m) != 0 {
		t.Fatalf("blip shorter than debounce was sent: %v", m[0].Title)
	}
}

func TestQuietHoursAndReminder(t *testing.T) {
	r := newRig(t, local(2026, 10, 6, 23, 0), "AKKA", "SOL", "SIRIUS")
	r.setDown("AKKA", true)
	r.at(r.now)
	r.at(local(2026, 10, 6, 23, 30))
	// SIRIUS is down 01:00–02:00 and comes back during the night.
	r.now = local(2026, 10, 7, 1, 0)
	r.setDown("SIRIUS", true)
	r.at(r.now)
	r.at(local(2026, 10, 7, 1, 30))
	r.setDown("SIRIUS", false)
	r.at(local(2026, 10, 7, 2, 0))
	r.at(local(2026, 10, 7, 6, 59))
	if m := r.sender.take(); len(m) != 0 {
		t.Fatalf("sent during quiet hours: %s", m[0].Title)
	}
	r.at(local(2026, 10, 7, 7, 0))
	msgs := r.sender.take()
	if len(msgs) != 1 || msgs[0].Kind != KindOvernight {
		t.Fatalf("overnight batch: %+v", msgs)
	}
	m := msgs[0]
	if !m.Mention || len(m.Sections[0].Lines) != 1 || !strings.Contains(m.Sections[0].Lines[0], "AKKA") {
		t.Fatalf("still open section: %+v", m.Sections[0])
	}
	if len(m.Sections[2].Lines) != 1 || !strings.Contains(m.Sections[2].Lines[0], "SIRIUS") {
		t.Fatalf("came-and-went section: %+v", m.Sections[2])
	}
	r.at(local(2026, 10, 7, 7, 5))
	if len(r.sender.take()) != 0 {
		t.Fatal("AKKA pinged again after the batch")
	}
	r.at(local(2026, 10, 7, 8, 0))
	msgs = r.sender.take()
	if len(msgs) != 1 || msgs[0].Kind != KindReminder || msgs[0].Mention {
		t.Fatalf("reminder: %+v", msgs)
	}
	r.at(local(2026, 10, 7, 9, 0))
	if len(r.sender.take()) != 0 {
		t.Fatal("reminder sent twice")
	}
}

func TestResolvedHeldDuringQuiet(t *testing.T) {
	r := newRig(t, local(2026, 10, 6, 21, 0), "AKKA")
	r.setDown("AKKA", true)
	r.at(r.now)
	r.at(local(2026, 10, 6, 21, 10))
	if m := r.sender.take(); len(m) != 1 {
		t.Fatal("expected ping before quiet hours")
	}
	r.setDown("AKKA", false)
	r.at(local(2026, 10, 6, 23, 0))
	if len(r.sender.take()) != 0 {
		t.Fatal("resolved sent during quiet hours")
	}
	r.at(local(2026, 10, 7, 7, 0))
	msgs := r.sender.take()
	if len(msgs) != 1 || msgs[0].Mention || len(msgs[0].Sections[1].Lines) != 1 {
		t.Fatalf("overnight resolved: %+v", msgs)
	}
}

// messageText is a message's title, summary and section lines.
func messageText(m Message) string {
	parts := []string{m.Title, m.Summary}
	for _, s := range m.Sections {
		parts = append(parts, s.Lines...)
	}
	return strings.Join(parts, "\n")
}

func TestFlapKeepsSinglePing(t *testing.T) {
	r := newRig(t, local(2026, 10, 6, 21, 0), "AKKA")
	r.setDown("AKKA", true)
	r.at(r.now)
	r.at(local(2026, 10, 6, 21, 10))
	r.sender.take()
	r.setDown("AKKA", false)
	r.at(local(2026, 10, 6, 23, 0)) // resolved, message held
	r.setDown("AKKA", true)
	r.at(local(2026, 10, 6, 23, 30)) // back down before the resolved message went out
	r.at(local(2026, 10, 7, 7, 0))
	for _, m := range r.sender.take() {
		if text := messageText(m); len(m.Sections) > 0 && strings.Contains(text, "AKKA") {
			t.Fatalf("flap produced a message: %s\n%s", m.Title, text)
		}
	}
}

func TestSnoozedNeverPings(t *testing.T) {
	r := newRig(t, local(2026, 10, 6, 10, 0), "AKKA")
	r.setDown("AKKA", true)
	r.at(r.now)
	v := r.eng.View()
	if len(v.Open) != 1 {
		t.Fatal("expected H1")
	}
	if err := r.eng.Snooze(v.Open[0].ID, r.now.Add(24*time.Hour), "maintenance", "admin"); err != nil {
		t.Fatal(err)
	}
	r.at(local(2026, 10, 6, 10, 30))
	if len(r.sender.take()) != 0 {
		t.Fatal("snoozed finding pinged")
	}
	r.at(local(2026, 10, 7, 10, 30)) // snooze expired
	if len(r.sender.take()) != 1 {
		t.Fatal("expected ping after snooze expiry")
	}
}

func TestWeeklyReport(t *testing.T) {
	r := newRig(t, local(2026, 10, 5, 7, 30), "AKKA", "SOL") // Monday
	r.setDown("SOL", true)
	r.at(r.now)
	r.at(local(2026, 10, 5, 7, 40))
	r.sender.take()
	r.at(local(2026, 10, 5, 8, 0))
	var weekly *Message
	for _, m := range r.sender.take() {
		if m.Kind == KindWeekly {
			mm := m
			weekly = &mm
		}
	}
	if weekly == nil {
		t.Fatal("no weekly report")
	}
	if !strings.Contains(weekly.Summary, `1 of 2 servers up \(SOL down\)`) || !strings.Contains(weekly.Summary, "ERROR 1 ·") {
		t.Fatalf("health line: %s", weekly.Summary)
	}
	if len(weekly.Sections[2].Lines) != 1 {
		t.Fatalf("top problems: %+v", weekly.Sections[2])
	}
	r.at(local(2026, 10, 5, 9, 0))
	for _, m := range r.sender.take() {
		if m.Kind == KindWeekly {
			t.Fatal("weekly sent twice")
		}
	}
}

// Names an unknown device or a remote folder offer can choose must not turn
// into links or formatting in Discord.
func TestDiscordEscapesNames(t *testing.T) {
	hostile := "[Open the dashboard](https://evil.example) <@&1> # Heading"
	rec := &store.Record{Finding: model.Finding{ID: "x", Check: "H9", Checks: []string{"H9"}, Severity: model.Warn,
		Subject: hostile + " on AKKA", Message: "Unknown device " + hostile + " wants to connect to AKKA"}}
	cfg := &config.Config{PublicURL: "http://sw.local:8080"}
	cfg.ApplyDefaults()
	d := &Discord{RoleID: "42"}
	p := d.Payload(UrgentMessage(cfg, []*store.Record{rec}))
	desc := p.Embeds[0].Description
	for where, s := range map[string]string{"content": p.Content, "description": desc} {
		if strings.Contains(s, "[Open the dashboard](") || strings.Contains(s, "<@&1>") || strings.Contains(s, "\n# ") {
			t.Errorf("%s keeps markdown from a name: %s", where, s)
		}
	}
	if !strings.Contains(p.Content, `\[Open the dashboard\]\(https:`+zwsp+`//evil.example\)`) {
		t.Errorf("content: %s", p.Content)
	}
	// syncwatch's own link survives.
	if !strings.Contains(desc, "[details](http://sw.local:8080/problems#f-x)") {
		t.Errorf("details link lost: %s", desc)
	}
	// A Windows path keeps its backslashes, escaped for Discord.
	rec.Message = `Sync stopped for X on SOL: D:\Share\LIVE-*1*`
	if desc := d.Payload(ResolvedMessage(cfg, []*store.Record{rec})).Embeds[0].Description; !strings.Contains(desc, `D:\\Share\\LIVE-\*1\*`) {
		t.Errorf("Windows path in the description: %s", desc)
	}
}

// A change that replaces a channel is announced to the channel it replaces.
func TestNoticeLaterGoesToTheOldChannels(t *testing.T) {
	r := newRig(t, local(2026, 10, 6, 10, 0), "AKKA")
	oldCh, newCh := &fakeSender{}, &fakeSender{}
	current := oldCh
	var mu sync.Mutex
	r.n.SetSenders(func() []Sender {
		mu.Lock()
		defer mu.Unlock()
		return []Sender{current}
	})
	notice := r.n.NoticeLater()
	mu.Lock()
	current = newCh
	mu.Unlock()
	notice("Discord webhook replaced", "x")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		oldCh.mu.Lock()
		n := len(oldCh.msgs)
		oldCh.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(oldCh.take()) != 1 || len(newCh.take()) != 0 {
		t.Fatal("the notice must go to the channels configured before the change")
	}
}

// A settings notice that can't be sent is held back and sent with the next
// notifications, to the channels configured then, saying when the change
// happened. A dead old channel doesn't hold up the new one.
func TestFailedNoticeIsRetried(t *testing.T) {
	r := newRig(t, local(2026, 10, 6, 23, 0), "AKKA") // quiet hours: notices still go out
	r.n.now = func() time.Time { return r.now }
	dead := &fakeSender{}
	dead.fail(errors.New("discord: HTTP 404: Unknown Webhook"))
	current := []Sender{dead}
	var mu sync.Mutex
	r.n.SetSenders(func() []Sender {
		mu.Lock()
		defer mu.Unlock()
		return current
	})
	pending := func() int {
		r.n.dmu.Lock()
		defer r.n.dmu.Unlock()
		return len(r.n.notices)
	}
	notice := r.n.NoticeLater() // captures the dead webhook
	mu.Lock()
	current = []Sender{r.sender} // replaced by a working one
	mu.Unlock()
	notice("Discord webhook replaced", "From 10.0.0.9.")
	deadline := time.Now().Add(5 * time.Second)
	for pending() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if pending() != 1 {
		t.Fatal("failed notice not held back")
	}
	if d := r.n.Delivery(); d.Failing {
		t.Fatalf("the replaced channel's failure shows as the current channels': %+v", d)
	}
	r.at(local(2026, 10, 6, 23, 1))
	msgs := r.sender.take()
	if len(msgs) != 1 || msgs[0].Kind != KindNotice || !strings.Contains(msgs[0].Summary, "Changed at 2026-10-06 23:00") {
		t.Fatalf("retried notice: %+v", msgs)
	}
	if pending() != 0 {
		t.Fatal("notice still pending after it was sent")
	}

	// Give up after a day.
	mu.Lock()
	current = []Sender{dead}
	mu.Unlock()
	r.n.Notice("Server SOL removed", "x")
	for pending() == 0 && time.Now().Before(deadline.Add(5*time.Second)) {
		time.Sleep(10 * time.Millisecond)
	}
	r.n.ChannelsChanged()
	r.at(local(2026, 10, 8, 0, 0))
	if pending() != 0 {
		t.Fatal("notice kept for more than a day")
	}
}

// Replies from a host that isn't Discord's aren't relayed to the settings
// page.
func TestDiscordErrorBodyOnlyFromDiscord(t *testing.T) {
	allowAnyWebhook(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal admin page: secret=hunter2", http.StatusInternalServerError)
	}))
	defer srv.Close()
	err := (&Discord{URL: srv.URL + "/api/webhooks/1/x"}).Send(context.Background(), Message{Title: "t"})
	if err == nil || strings.Contains(err.Error(), "hunter2") || !strings.Contains(err.Error(), "500") {
		t.Fatalf("error: %v", err)
	}
}

func TestDiscordErrorMessage(t *testing.T) {
	if got := discordError([]byte(`{"message": "Unknown Webhook", "code": 10015}`)); got != "Unknown Webhook" {
		t.Errorf("JSON reply: %q", got)
	}
	if got := discordError([]byte("  upstream connect error  ")); got != "upstream connect error" {
		t.Errorf("plain reply: %q", got)
	}
}

func TestDiscordPayload(t *testing.T) {
	allowAnyWebhook(t)
	var got []DiscordPayload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var p DiscordPayload
		if err := json.Unmarshal(b, &p); err != nil {
			t.Error(err)
		}
		got = append(got, p)
		if len(got) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"retry_after": 0.05}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	d := &Discord{URL: srv.URL, RoleID: "42"}
	var lines []string
	for i := 0; i < 400; i++ {
		lines = append(lines, strings.Repeat("x", 40)+" line")
	}
	m := Message{Title: "t", Mention: true, Sections: []Section{{Title: "Many", Lines: lines}}, Color: ColorError}
	if err := d.Send(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected a retry after 429, got %d posts", len(got))
	}
	p := got[1]
	if !strings.HasPrefix(p.Content, "<@&42>") || p.AllowedMentions.Roles[0] != "42" {
		t.Fatalf("mention: %+v", p)
	}
	if len(p.Embeds[0].Description) > maxDescription || !strings.Contains(p.Embeds[0].Description, "more") {
		t.Fatalf("description not truncated: %d", len(p.Embeds[0].Description))
	}
	// Without mention no role may be pinged.
	p2 := d.Payload(Message{Title: "x"})
	if len(p2.AllowedMentions.Roles) != 0 || strings.Contains(p2.Content, "<@&") {
		t.Fatal("unexpected mention")
	}
	if escapeMD("a*b_@everyone") != `a\*b\_@`+"​"+`everyone` {
		t.Fatal(escapeMD("a*b_@everyone"))
	}
}

// kindSender fails messages of one kind with a given error and records the
// rest, like Discord rejecting one notice but nothing else.
type kindSender struct {
	mu       sync.Mutex
	failKind string
	err      error
	msgs     []Message
	attempts map[string]int
}

func (s *kindSender) Name() string { return "discord" }

func (s *kindSender) Send(_ context.Context, m Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attempts == nil {
		s.attempts = map[string]int{}
	}
	s.attempts[m.Kind]++
	if m.Kind == s.failKind {
		return s.err
	}
	s.msgs = append(s.msgs, m)
	return nil
}

func (s *kindSender) take() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.msgs
	s.msgs = nil
	return out
}

func (s *kindSender) tried(kind string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts[kind]
}

// lockedLog is a log sink that can be read while notice goroutines write.
type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func pendingNotices(r *rig) int {
	r.n.dmu.Lock()
	defer r.n.dmu.Unlock()
	return len(r.n.notices)
}

// queueNoticeTo sends a notice through n to the given sender and waits for
// it to be held back.
func queueNoticeTo(t *testing.T, r *rig, title string) {
	t.Helper()
	before := pendingNotices(r)
	r.n.Notice(title, strings.Repeat("x", 100))
	deadline := time.Now().Add(5 * time.Second)
	for pendingNotices(r) == before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if pendingNotices(r) != before+1 {
		t.Fatal("failed notice not held back")
	}
}

// A notice Discord rejects for good (HTTP 400) is dropped with one warning
// instead of blocking every other notification for a day.
func TestRejectedNoticeIsDropped(t *testing.T) {
	start := local(2026, 10, 6, 10, 0)
	r := newRig(t, start, "AKKA")
	r.n.now = func() time.Time { return r.now }
	logs := &lockedLog{}
	r.n.log = slog.New(slog.NewTextHandler(logs, nil))
	sel := &kindSender{failKind: KindNotice, err: &httpError{code: 400, text: "Invalid Form Body"}}
	r.n.SetSenders(func() []Sender { return []Sender{sel} })
	queueNoticeTo(t, r, "Configuration imported")
	r.setDown("AKKA", true)
	r.at(start)
	r.at(local(2026, 10, 6, 10, 5))
	if msgs := sel.take(); len(msgs) != 1 || msgs[0].Kind != KindUrgent {
		t.Fatalf("urgent ping held up by a rejected notice: %+v", msgs)
	}
	if pendingNotices(r) != 0 {
		t.Fatal("rejected notice kept in the queue")
	}
	if d := r.n.Delivery(); d.Failing {
		t.Fatalf("the rejected notice counts as a failing channel: %+v", d)
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 2 { // once held back, once dropped
		t.Fatalf("expected two warnings, got %d:\n%s", n, logs.String())
	}
	for m := 6; m <= 60; m++ {
		r.at(start.Add(time.Duration(m) * time.Minute))
	}
	if n := sel.tried(KindNotice); n != 2 { // the first attempt and the one retry
		t.Fatalf("rejected notice tried %d times", n)
	}
}

// A notice that fails for other reasons is retried, but costs each tick one
// attempt at most: the urgent messages go out behind it.
func TestFailingNoticeDoesNotHoldUpUrgent(t *testing.T) {
	start := local(2026, 10, 6, 10, 0)
	r := newRig(t, start, "AKKA")
	r.n.now = func() time.Time { return r.now }
	sel := &kindSender{failKind: KindNotice, err: &httpError{code: 502}}
	r.n.SetSenders(func() []Sender { return []Sender{sel} })
	queueNoticeTo(t, r, "Server SOL removed")
	queueNoticeTo(t, r, "Access settings changed")
	r.setDown("AKKA", true)
	r.at(start)
	if n := sel.tried(KindNotice); n != 3 { // two first attempts, one retry
		t.Fatalf("notices tried %d times in the tick; want one retry", n)
	}
	r.at(local(2026, 10, 6, 10, 5))
	if msgs := sel.take(); len(msgs) != 1 || msgs[0].Kind != KindUrgent {
		t.Fatalf("urgent ping held up by a failing notice: %+v", msgs)
	}
	if n := sel.tried(KindNotice); n != 4 {
		t.Fatalf("notices tried %d times by now; want one retry per tick", n)
	}
	if pendingNotices(r) != 2 {
		t.Fatalf("%d notices pending; both should be kept", pendingNotices(r))
	}
	// The failure still backs off the next attempt, and the notices go out
	// once the channel takes them.
	sel.mu.Lock()
	sel.failKind = ""
	sel.mu.Unlock()
	r.at(local(2026, 10, 6, 10, 5).Add(30 * time.Second))
	if n := sel.tried(KindNotice); n != 4 {
		t.Fatalf("retried within the back-off: %d attempts", n)
	}
	r.at(local(2026, 10, 6, 10, 6))
	if msgs := sel.take(); len(msgs) != 2 || msgs[0].Kind != KindNotice || msgs[1].Kind != KindNotice {
		t.Fatalf("after the back-off: %+v", msgs)
	}
	if pendingNotices(r) != 0 {
		t.Fatal("notices still pending after they were sent")
	}
}

// A webhook that can't be decrypted (the secret key changed) must show up
// in the test button, the banner and the notification log, not vanish.
func TestUndecryptableWebhookIsReported(t *testing.T) {
	dir := t.TempDir()
	keyA, keyB := make([]byte, 32), make([]byte, 32)
	keyB[0] = 1
	st, err := store.Open(filepath.Join(dir, "db.sqlite"), keyA)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSecret(store.SecretDiscordWebhook, "https://discord.com/api/webhooks/1/x"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	st2, err := store.Open(filepath.Join(dir, "db.sqlite"), keyB)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	h, _ := config.NewHolder(config.Default())
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng, err := engine.New(st2, model.NewHub(nil), h, nil, quiet)
	if err != nil {
		t.Fatal(err)
	}
	n := New(eng, st2, h, quiet)
	const want = "syncwatch cannot decrypt the Discord webhook: check the secret key"
	if err := n.SendTest(context.Background()); err == nil || err.Error() != want {
		t.Fatalf("SendTest: %v", err)
	}
	if d := n.Delivery(); !d.Failing || d.Error != want {
		t.Fatalf("delivery state: %+v", d)
	}
	if log, _ := st2.Notifications(10); len(log) != 1 || log[0].OK || log[0].Error != want || log[0].Channel != "discord" {
		t.Fatalf("notification log: %+v", log)
	}
}

// blockingSender holds a send until the test releases it with a result.
type blockingSender struct {
	started chan struct{}
	release chan error
}

func (b *blockingSender) Name() string { return "discord" }

func (b *blockingSender) Send(context.Context, Message) error {
	b.started <- struct{}{}
	return <-b.release
}

// A failure of the old webhook that lands after the webhook was replaced
// must not set the banner or the back-off for the new one: "retry at once"
// has to mean that.
func TestStaleFailureDoesNotUndoChannelChange(t *testing.T) {
	start := local(2026, 10, 6, 10, 0)
	r := newRig(t, start, "AKKA")
	slow := &blockingSender{started: make(chan struct{}), release: make(chan error)}
	r.n.SetSenders(func() []Sender { return []Sender{slow} })
	r.setDown("AKKA", true)
	r.at(start)
	r.now = local(2026, 10, 6, 10, 5)
	r.eng.Evaluate()
	done := make(chan struct{})
	go func() {
		r.n.Tick(context.Background(), r.now)
		close(done)
	}()
	<-slow.started
	r.n.SetSenders(func() []Sender { return []Sender{r.sender} })
	r.n.ChannelsChanged() // the admin saved a new webhook meanwhile
	slow.release <- errors.New("discord: HTTP 404: Unknown Webhook")
	<-done
	if d := r.n.Delivery(); d.Failing {
		t.Fatalf("old channel's failure shown for the new channel: %+v", d)
	}
	r.at(r.now.Add(30 * time.Second))
	if m := r.sender.take(); len(m) != 1 || m[0].Kind != KindUrgent {
		t.Fatalf("new webhook not tried at once: %+v", m)
	}
}

// An urgent finding that opens just before quiet hours, becomes due inside
// them and resolves before they end was never pinged: the overnight summary
// must list it.
func TestOvernightListsDueBeforeQuietResolvedDuring(t *testing.T) {
	r := newRig(t, local(2026, 10, 6, 21, 58), "AKKA")
	r.setDown("AKKA", true)
	r.at(r.now)
	r.at(local(2026, 10, 6, 21, 59))
	r.at(local(2026, 10, 6, 22, 10)) // due at 22:03, inside quiet hours
	r.setDown("AKKA", false)
	r.at(local(2026, 10, 6, 23, 0))
	if m := r.sender.take(); len(m) != 0 {
		t.Fatalf("sent during quiet hours: %s", m[0].Title)
	}
	r.at(local(2026, 10, 7, 7, 0))
	msgs := r.sender.take()
	if len(msgs) != 1 || msgs[0].Kind != KindOvernight || msgs[0].Mention {
		t.Fatalf("overnight summary: %+v", msgs)
	}
	lines := msgs[0].Sections[2].Lines
	if len(lines) != 1 || !strings.Contains(lines[0], "AKKA") || !strings.Contains(lines[0], "(21:58–23:00)") {
		t.Fatalf("opened-and-resolved section: %+v", msgs[0].Sections)
	}
	r.at(local(2026, 10, 7, 8, 0))
	if m := r.sender.take(); len(m) != 0 {
		t.Fatalf("follow-up for a resolved finding: %s", m[0].Title)
	}
}

// A link that drops for 6 minutes every 10 minutes is one incident: one ping
// and no "resolved" until it has stayed up for min(debounce, 5 min).
func TestResolveHysteresisDuringFlapping(t *testing.T) {
	start := local(2026, 10, 6, 10, 0)
	r := newRig(t, start, "AKKA")
	counts := map[string]int{}
	for m := 0; m < 120; m++ {
		r.now = start.Add(time.Duration(m) * time.Minute)
		r.setDown("AKKA", m%10 < 6)
		r.at(r.now)
		for _, msg := range r.sender.take() {
			counts[msg.Kind]++
		}
	}
	if counts[KindUrgent] != 1 || counts[KindResolved] != 0 {
		t.Fatalf("2 h of flapping: %v, want one urgent ping and no resolved message", counts)
	}
	// Up since 11:56; "resolved" goes out once it has stayed up for 5 minutes.
	r.at(local(2026, 10, 6, 12, 0))
	if m := r.sender.take(); len(m) != 0 {
		t.Fatalf("resolved sent after 4 minutes up: %s", m[0].Title)
	}
	r.at(local(2026, 10, 6, 12, 1))
	if msgs := r.sender.take(); len(msgs) != 1 || msgs[0].Kind != KindResolved {
		t.Fatalf("resolved after staying up for 5 minutes: %+v", msgs)
	}
	// An outage after that is a new problem and gets a new ping.
	r.now = local(2026, 10, 6, 12, 10)
	r.setDown("AKKA", true)
	r.at(r.now)
	r.at(local(2026, 10, 6, 12, 15))
	if msgs := r.sender.take(); len(msgs) != 1 || msgs[0].Kind != KindUrgent {
		t.Fatalf("new outage after a settled resolution: %+v", msgs)
	}
}

// The health line reads correctly with one of something.
func TestHealthLineWording(t *testing.T) {
	now := local(2026, 10, 6, 10, 0)
	r := newRig(t, now, "AKKA")
	r.at(now)
	want := "1 server up · 0 PCs, 0 offline more than 7 days · ERROR 0 · WARNING 0 · INFO 0 · 0 snoozed"
	if got := HealthLine(r.n.cfg.Get(), r.eng.View(), now); got != want {
		t.Fatalf("health line:\n got %s\nwant %s", got, want)
	}
	r = newRig(t, now, "AKKA", "SOL")
	r.hub.Update("akka", func(s *model.Server) {
		s.Devices = map[string]*model.Device{"PC1": {ID: "PC1", Name: "pc1", LastSeen: now.Add(-10 * 24 * time.Hour)}}
	})
	r.setDown("SOL", true)
	r.at(now)
	// The PC that has been away for 10 days is also a warning finding.
	want = "1 of 2 servers up (SOL down) · 1 PC, 1 offline more than 7 days · ERROR 1 · WARNING 1 · INFO 0 · 0 snoozed"
	if got := HealthLine(r.n.cfg.Get(), r.eng.View(), now); got != want {
		t.Fatalf("health line:\n got %s\nwant %s", got, want)
	}
}

// Scheduled messages wait for every enabled server's first result, or 5
// minutes after start, so a report sent at startup doesn't call servers
// down that simply haven't been contacted yet.
func TestScheduledMessagesWaitForFirstResults(t *testing.T) {
	start := local(2026, 10, 5, 10, 0) // Monday, inside the weekly window
	r := newRig(t, start, "AKKA", "SOL")
	unknown := func(id string) {
		r.hub.Update(id, func(s *model.Server) { s.Status, s.Loaded = model.StatusUnknown, false })
	}
	unknown("akka")
	unknown("sol")
	// A disabled server never reports and must not hold the report back.
	c := r.n.cfg.Get().Clone()
	c.Servers = append(c.Servers, config.Server{ID: "old", Name: "OLD", URL: "https://old:8384", Disabled: true})
	if err := r.n.cfg.Set(c); err != nil {
		t.Fatal(err)
	}
	r.at(start)
	r.hub.Update("akka", func(s *model.Server) { s.Status, s.Loaded = model.StatusUp, true })
	r.at(start.Add(time.Minute))
	if m := r.sender.take(); len(m) != 0 {
		t.Fatalf("sent before SOL had a result: %s | %s", m[0].Title, m[0].Summary)
	}
	r.setDown("SOL", true)
	r.at(start.Add(2 * time.Minute))
	msgs := r.sender.take()
	if len(msgs) != 1 || msgs[0].Kind != KindWeekly || !strings.Contains(msgs[0].Summary, `1 of 2 servers up \(SOL down\)`) { // parentheses escaped for Discord
		t.Fatalf("weekly report once every server reported: %+v", msgs)
	}

	// Servers that never answer: the report goes out 5 minutes after start,
	// and says they haven't been checked rather than that they are down.
	r = newRig(t, start, "AKKA", "SOL")
	unknown("akka")
	unknown("sol")
	r.at(start)
	r.at(start.Add(4 * time.Minute))
	if m := r.sender.take(); len(m) != 0 {
		t.Fatalf("sent before the start-up grace: %s | %s", m[0].Title, m[0].Summary)
	}
	r.at(start.Add(5 * time.Minute))
	msgs = r.sender.take()
	if len(msgs) != 1 || msgs[0].Kind != KindWeekly || !strings.Contains(msgs[0].Summary, `0 of 2 servers up \(AKKA, SOL not contacted\)`) || strings.Contains(msgs[0].Summary, "down") {
		t.Fatalf("weekly report after the grace period: %+v", msgs)
	}
}

// A reminder time inside quiet hours that could never be sent is rejected:
// the evening part of a window that crosses midnight (the reminder is only
// looked for until 12 hours after its time). A time the notifier can send
// at quiet end is still accepted.
func TestReminderInsideQuietHoursRejected(t *testing.T) {
	const inside = "is inside the quiet hours"
	for _, tc := range []struct {
		quiet, reminder string
		want            string // "" means accepted
	}{
		{"22:00-07:00", "08:00", ""},
		{"22:00-07:00", "23:00", inside}, // quiet until the next day
		{"22:00-07:00", "22:00", inside},
		{"22:00-07:00", "00:30", ""}, // sent at 07:00
		{"22:00-07:00", "07:00", ""},
		{"09:00-17:00", "12:00", ""},     // sent at 17:00
		{"20:00-14:00", "01:00", inside}, // 14:00 is more than 12 hours later
		{"off", "23:00", ""},
		// Equal ends used to mean off; since the config validation of r0-g
		// they are refused as such, before the reminder is looked at.
		{"07:00-07:00", "23:00", "the start and the end are the same"},
	} {
		c := config.Default()
		c.Notify.QuietHours, c.Notify.ReminderTime = tc.quiet, tc.reminder
		err := c.Validate()
		if tc.want == "" && err != nil {
			t.Errorf("quiet %s, reminder %s: unexpected error %v", tc.quiet, tc.reminder, err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("quiet %s, reminder %s: want %q, got %v", tc.quiet, tc.reminder, tc.want, err)
		}
	}
}

// Findings that are ignored or snoozed don't show up in the overnight
// "opened and resolved" list.
func TestOvernightSkipsIgnoredAndSnoozed(t *testing.T) {
	r := newRig(t, local(2026, 10, 6, 23, 0), "AKKA", "SOL", "SIRIUS")
	r.at(r.now)
	r.now = local(2026, 10, 7, 1, 0)
	for _, s := range []string{"AKKA", "SOL", "SIRIUS"} {
		r.setDown(s, true)
	}
	r.at(r.now)
	for _, rec := range r.eng.View().Open {
		var err error
		switch rec.Server {
		case "akka":
			err = r.eng.Ignore(rec.ID, "known flaky", "admin")
		case "sol":
			err = r.eng.Snooze(rec.ID, local(2026, 10, 8, 1, 0), "maintenance", "admin")
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	r.at(local(2026, 10, 7, 1, 30))
	for _, s := range []string{"AKKA", "SOL", "SIRIUS"} {
		r.setDown(s, false)
	}
	r.at(local(2026, 10, 7, 2, 0))
	r.at(local(2026, 10, 7, 7, 0))
	msgs := r.sender.take()
	if len(msgs) != 1 || msgs[0].Kind != KindOvernight {
		t.Fatalf("overnight summary: %+v", msgs)
	}
	if lines := msgs[0].Sections[2].Lines; len(lines) != 1 || !strings.Contains(lines[0], "SIRIUS") {
		t.Fatalf("opened-and-resolved section should list only SIRIUS: %+v", lines)
	}
}

// The daily reminder doesn't repeat what was announced less than an hour
// before its time: an urgent ping at 07:59, or the overnight summary at
// 07:00.
func TestReminderSkipsRecentlyAnnounced(t *testing.T) {
	r := newRig(t, local(2026, 10, 6, 7, 54), "AKKA", "SOL") // Tuesday
	c := r.n.cfg.Get().Clone()
	c.Notify.WeeklyDay = "off" // Monday's report would still be caught up
	if err := r.n.cfg.Set(c); err != nil {
		t.Fatal(err)
	}
	r.setDown("AKKA", true)
	r.at(r.now)
	r.at(local(2026, 10, 6, 7, 59))
	if m := r.sender.take(); len(m) != 1 || m[0].Kind != KindUrgent {
		t.Fatalf("ping at 07:59: %+v", m)
	}
	r.at(local(2026, 10, 6, 8, 0))
	if m := r.sender.take(); len(m) != 0 {
		t.Fatalf("reminder a minute after the ping: %s", m[0].Title)
	}
	// Next morning: AKKA (pinged yesterday) is reminded; SOL, announced by
	// the overnight summary at 07:00, isn't.
	r.now = local(2026, 10, 6, 23, 0)
	r.setDown("SOL", true)
	r.at(r.now)
	r.at(local(2026, 10, 7, 7, 0).Add(20 * time.Second))
	if m := r.sender.take(); len(m) != 1 || m[0].Kind != KindOvernight || !strings.Contains(messageText(m[0]), "SOL") {
		t.Fatalf("overnight summary: %+v", m)
	}
	r.at(local(2026, 10, 7, 8, 0))
	msgs := r.sender.take()
	if len(msgs) != 1 || msgs[0].Kind != KindReminder {
		t.Fatalf("reminder: %+v", msgs)
	}
	if text := messageText(msgs[0]); !strings.Contains(text, "AKKA") || strings.Contains(text, "SOL") {
		t.Fatalf("reminder should list AKKA only:\n%s", text)
	}
}

// An overnight summary that goes out late (the channel was failing at quiet
// end) labels findings by when they became due: during the night, or since
// quiet hours ended. A finding due during the night and resolved before the
// late summary is still listed.
func TestLateOvernightLabelsDaytimeFindings(t *testing.T) {
	r := newRig(t, local(2026, 10, 6, 23, 0), "AKKA", "SOL", "SIRIUS")
	r.setDown("AKKA", true)
	r.at(r.now)
	r.now = local(2026, 10, 7, 1, 0)
	r.setDown("SIRIUS", true)
	r.at(r.now)
	r.sender.fail(errors.New("discord: HTTP 502"))
	r.at(local(2026, 10, 7, 7, 0))
	r.now = local(2026, 10, 7, 7, 30)
	r.setDown("SIRIUS", false) // resolved after quiet end, never pinged
	r.at(r.now)
	r.now = local(2026, 10, 7, 8, 0)
	r.setDown("SOL", true) // due at 08:05, after quiet end
	r.at(r.now)
	r.at(local(2026, 10, 7, 8, 30))
	if n := len(r.sender.take()); n != 0 {
		t.Fatalf("%d messages got through a failing channel", n)
	}
	r.sender.fail(nil)
	r.n.ChannelsChanged()
	r.at(local(2026, 10, 7, 10, 1))
	msgs := r.sender.take()
	if len(msgs) != 1 || msgs[0].Kind != KindOvernight || !msgs[0].Mention {
		t.Fatalf("late overnight summary: %+v", msgs)
	}
	m := msgs[0]
	if lines := m.Sections[0].Lines; len(lines) != 1 || !strings.Contains(lines[0], "AKKA") {
		t.Fatalf("new and still open: %+v", m.Sections[0])
	}
	if lines := m.Sections[2].Lines; len(lines) != 1 || !strings.Contains(lines[0], "SIRIUS") || !strings.Contains(lines[0], "(01:00–07:30)") {
		t.Fatalf("opened and resolved: %+v", m.Sections[2])
	}
	if len(m.Sections) != 4 || !strings.Contains(m.Sections[3].Title, "after the end of quiet hours") || len(m.Sections[3].Lines) != 1 || !strings.Contains(m.Sections[3].Lines[0], "SOL") {
		t.Fatalf("since quiet hours ended: %+v", m.Sections)
	}
	r.at(local(2026, 10, 7, 10, 2))
	if m := r.sender.take(); len(m) != 0 {
		t.Fatalf("pinged again after the summary: %s", m[0].Title)
	}
}
