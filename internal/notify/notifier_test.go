package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	msgs = r.sender.take()
	if len(msgs) != 1 || msgs[0].Kind != KindResolved || msgs[0].Mention || !strings.Contains(msgs[0].Title, "SOL") {
		t.Fatalf("resolved: %+v", msgs)
	}
	r.at(local(2026, 10, 6, 10, 21))
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
	if !strings.Contains(weekly.Summary, "1/2 servers OK (SOL down)") || !strings.Contains(weekly.Summary, "1 errors") {
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
	if !strings.Contains(p.Content, `\[Open the dashboard\]\(https://evil.example\)`) {
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
