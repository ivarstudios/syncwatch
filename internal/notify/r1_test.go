package notify

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/ivarstudios/syncwatch/internal/engine"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/store"
)

// utc is a time on 2026-10-06 (a Tuesday) in UTC.
func utc(h, m int) time.Time { return time.Date(2026, 10, 6, h, m, 0, 0, time.UTC) }

// setNotify changes the notification settings of a rig.
func setNotify(t *testing.T, r *rig, fn func(c *config.Config)) {
	t.Helper()
	c := r.n.cfg.Get().Clone()
	fn(c)
	if err := r.n.cfg.Set(c); err != nil {
		t.Fatal(err)
	}
}

// restart replaces the rig's notifier with a new one on the same database,
// like a restart of syncwatch.
func (r *rig) restart() {
	r.n = New(r.eng, r.st, r.n.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r.n.SetSenders(func() []Sender { return []Sender{r.sender} })
}

// An occurrence that got its urgent and its resolved message is not listed
// again in the overnight summary when the same finding opens again later
// and the new occurrence resets the record (ops-chaos OC-10: quiet hours
// set while the first occurrence was inside the window). A restart between
// the two does not change this.
func TestOvernightSkipsEarlierAnnouncedOccurrence(t *testing.T) {
	r := newRig(t, utc(19, 0), "AKKA")
	r.setDown("AKKA", true)
	r.at(utc(19, 0))
	r.at(utc(19, 5))
	if m := r.sender.take(); len(m) != 1 || m[0].Kind != KindUrgent {
		t.Fatalf("urgent message: %+v", m)
	}
	r.setDown("AKKA", false)
	r.at(utc(19, 6))
	r.at(utc(19, 11))
	if m := r.sender.take(); len(m) != 1 || m[0].Kind != KindResolved {
		t.Fatalf("resolved message: %+v", m)
	}
	setNotify(t, r, func(c *config.Config) { c.Timezone, c.Notify.QuietHours = "UTC", "19:05-19:40" })
	r.now = utc(19, 15)
	r.setDown("AKKA", true) // the same finding opens again, inside quiet hours
	r.at(utc(19, 15))
	r.at(utc(19, 21))
	r.setDown("AKKA", false)
	r.at(utc(19, 25))
	r.restart()
	r.at(utc(19, 39))
	if m := r.sender.take(); len(m) != 0 {
		t.Fatalf("sent during quiet hours: %s", m[0].Title)
	}
	r.at(utc(19, 40))
	msgs := r.sender.take()
	if len(msgs) != 1 || msgs[0].Kind != KindOvernight {
		t.Fatalf("overnight summary: %+v", msgs)
	}
	if lines := msgs[0].Sections[2].Lines; len(lines) != 1 || !strings.Contains(lines[0], "(19:15–19:25)") {
		t.Fatalf("opened and resolved during the night: want only the second occurrence, got %q", lines)
	}
}

// The test message mentions the configured role, the same as an urgent
// message, so a wrong role ID shows at the test and not at the first
// incident. Without a role it mentions nobody.
func TestTestMessageMentionsTheRole(t *testing.T) {
	allowAnyWebhook(t)
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	for _, role := range []string{"424242424242424242", ""} {
		r := newRig(t, local(2026, 10, 6, 10, 0), "AKKA")
		c := r.n.cfg.Get().Clone()
		c.Notify.DiscordRoleID = role
		if err := r.n.cfg.Set(c); err != nil {
			t.Fatal(err)
		}
		r.n.SetSenders(func() []Sender { return []Sender{&Discord{URL: srv.URL + "/api/webhooks/1/x", RoleID: role}} })
		if err := r.n.SendTest(context.Background()); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		body := bodies[len(bodies)-1]
		mu.Unlock()
		var p DiscordPayload
		if err := json.Unmarshal([]byte(body), &p); err != nil {
			t.Fatal(err)
		}
		desc := p.Embeds[0].Description
		if role != "" {
			if !strings.HasPrefix(p.Content, "<@&"+role+"> ") || len(p.AllowedMentions.Roles) != 1 || p.AllowedMentions.Roles[0] != role {
				t.Errorf("role %s: no mention: %s", role, body)
			}
			if !strings.Contains(desc, "mentions the role") || !strings.Contains(desc, "check the role ID") {
				t.Errorf("role %s: summary: %s", role, desc)
			}
			continue
		}
		if strings.Contains(p.Content, "<@") || len(p.AllowedMentions.Roles) != 0 || !strings.Contains(body, `"parse":[]`) {
			t.Errorf("no role: mention in %s", body)
		}
		if !strings.Contains(desc, "Urgent messages mention no role.") {
			t.Errorf("no role: summary: %s", desc)
		}
	}
}

// Urgent findings that became due and resolved while the notifications failed
// are reported once the channel works again, in one message without a
// mention; a finding shorter than its debounce is not (ops-chaos OC-03).
func TestCatchUpAfterFailure(t *testing.T) {
	r := newRig(t, local(2026, 10, 6, 10, 0), "AKKA", "SOL")
	r.sender.fail(&httpError{code: 502})
	r.setDown("AKKA", true)
	r.at(local(2026, 10, 6, 10, 0))
	r.at(local(2026, 10, 6, 10, 5)) // due: the urgent message fails
	r.now = local(2026, 10, 6, 10, 20)
	r.setDown("AKKA", false) // resolved before anybody heard of it
	r.at(r.now)
	r.now = local(2026, 10, 6, 10, 30)
	r.setDown("SOL", true)
	r.at(r.now)
	r.now = local(2026, 10, 6, 10, 32)
	r.setDown("SOL", false) // shorter than its debounce
	r.at(r.now)
	r.sender.fail(nil)
	r.at(local(2026, 10, 6, 10, 45))
	msgs := r.sender.take()
	if len(msgs) != 1 || msgs[0].Mention || msgs[0].Kind != KindResolved || !strings.Contains(msgs[0].Title, "Server unreachable: AKKA") {
		t.Fatalf("catch-up message: %+v", msgs)
	}
	s := msgs[0].Sections
	if len(s) != 1 || s[0].Title != "Opened and resolved while the notifications failed (10:05–10:45)" {
		t.Fatalf("catch-up section: %+v", s)
	}
	if len(s[0].Lines) != 1 || !strings.Contains(s[0].Lines[0], "AKKA") || !strings.Contains(s[0].Lines[0], "(10:00–10:20)") {
		t.Fatalf("catch-up lines: %q", s[0].Lines)
	}
	if d := r.n.Delivery(); d.Failing {
		t.Fatalf("still failing after the catch-up went out: %+v", d)
	}
	r.at(local(2026, 10, 6, 11, 0))
	if m := r.sender.take(); len(m) != 0 {
		t.Fatalf("catch-up sent twice: %s", m[0].Title)
	}
}

// A new webhook gets the catch-up too, in the same message as an urgent
// finding that is due now (with the mention for that finding).
func TestCatchUpWithUrgentAfterNewWebhook(t *testing.T) {
	r := newRig(t, local(2026, 10, 6, 10, 0), "AKKA", "SOL")
	dead := &fakeSender{}
	dead.fail(&httpError{code: 404, text: "Unknown Webhook"})
	r.n.SetSenders(func() []Sender { return []Sender{dead} })
	r.setDown("AKKA", true)
	r.at(local(2026, 10, 6, 10, 0))
	r.at(local(2026, 10, 6, 10, 5))
	r.now = local(2026, 10, 6, 10, 10)
	r.setDown("AKKA", false)
	r.setDown("SOL", true)
	r.at(r.now)
	r.at(local(2026, 10, 6, 10, 16)) // SOL due, still failing
	r.n.SetSenders(func() []Sender { return []Sender{r.sender} })
	r.n.ChannelsChanged() // the administrator saved a new webhook
	r.at(local(2026, 10, 6, 10, 17))
	msgs := r.sender.take()
	if len(msgs) != 1 || !msgs[0].Mention || msgs[0].Kind != KindUrgent || !strings.Contains(msgs[0].Title, "SOL") {
		t.Fatalf("urgent message with the catch-up: %+v", msgs)
	}
	s := msgs[0].Sections
	if len(s) != 2 || !strings.HasPrefix(s[1].Title, "Opened and resolved while the notifications failed (10:05–") ||
		len(s[1].Lines) != 1 || !strings.Contains(s[1].Lines[0], "AKKA") {
		t.Fatalf("sections: %+v", s)
	}
}

// When a failure lasts through quiet hours, the overnight summary lists the
// night's occurrences and the catch-up only the others: none twice.
func TestCatchUpAndOvernightListEachOccurrenceOnce(t *testing.T) {
	r := newRig(t, local(2026, 10, 6, 21, 0), "AKKA", "SOL")
	r.sender.fail(&httpError{code: 500})
	r.setDown("AKKA", true)
	r.at(local(2026, 10, 6, 21, 0))
	r.at(local(2026, 10, 6, 21, 5)) // due: fails
	r.now = local(2026, 10, 6, 21, 30)
	r.setDown("AKKA", false)
	r.at(r.now)
	r.now = local(2026, 10, 6, 23, 0)
	r.setDown("SOL", true) // inside quiet hours
	r.at(r.now)
	r.at(local(2026, 10, 6, 23, 10))
	r.now = local(2026, 10, 6, 23, 30)
	r.setDown("SOL", false)
	r.at(r.now)
	r.sender.fail(nil) // Discord works again during the night
	r.at(local(2026, 10, 7, 6, 0))
	if m := r.sender.take(); len(m) != 0 {
		t.Fatalf("sent during quiet hours: %s", m[0].Title)
	}
	r.at(local(2026, 10, 7, 7, 0))
	msgs := r.sender.take()
	if len(msgs) != 2 || msgs[0].Kind != KindOvernight || msgs[1].Kind != KindResolved {
		t.Fatalf("overnight summary and catch-up: %+v", msgs)
	}
	if text := messageText(msgs[0]); !strings.Contains(text, "SOL") || strings.Contains(text, "AKKA") {
		t.Fatalf("overnight summary lists:\n%s", text)
	}
	if text := messageText(msgs[1]); !strings.Contains(text, "AKKA") || strings.Contains(text, "SOL") {
		t.Fatalf("catch-up lists:\n%s", text)
	}
	if title := msgs[1].Sections[0].Title; title != "Opened and resolved while the notifications failed (2026-10-06 21:05–07:00)" {
		t.Fatalf("catch-up section title: %s", title)
	}
}

// Which failures can end without a change of the webhook (short back-off).
func TestTransientFailures(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&httpError{code: 500}, true},
		{&httpError{code: 502}, true},
		{&httpError{code: 429}, true},
		{errors.New("Discord: too many requests (HTTP 429) after 3 tries"), true},
		{errors.New(`Discord: Post "<webhook>": dial tcp: connection refused`), true},
		{context.DeadlineExceeded, true},
		{&httpError{code: 400, text: "Invalid Form Body"}, false},
		{&httpError{code: 401}, false},
		{&httpError{code: 403}, false},
		{&httpError{code: 404, text: "Unknown Webhook"}, false},
		{&httpError{code: 307, text: "the webhook answers with a redirect, which syncwatch does not follow"}, false},
		{(&brokenChannel{err: errUndecryptable}).Send(context.Background(), Message{}), false},
		{(&Discord{URL: "https://example.invalid/api/webhooks/1/x"}).usable(), false},
	} {
		if got := transient(tc.err); got != tc.want {
			t.Errorf("transient(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// The weekly report counts what it lists: ERROR and WARNING occurrences of
// findings that are not snoozed or ignored (INFO findings stay on the
// dashboard), and a cut list ends with "…and N more" (admin-anna AA-17).
func TestWeeklyCountsWhatItLists(t *testing.T) {
	now := local(2026, 10, 5, 8, 0)
	cfg := &config.Config{PublicURL: "http://sw.local:8080"}
	cfg.ApplyDefaults()
	var hist []store.HistoryEntry
	add := func(id, check string, sev model.Severity, opened, resolved time.Duration) {
		h := store.HistoryEntry{FindingID: id, Check: check, Severity: sev, Message: id + " message", OpenedAt: now.Add(-opened)}
		if resolved > 0 {
			h.ResolvedAt = now.Add(-resolved)
		}
		hist = append(hist, h)
	}
	for i := 0; i < 10; i++ {
		add(fmt.Sprintf("relay%d", i), "H11", model.Warn, 48*time.Hour, 0)
	}
	add("h1", "H1", model.Error, 48*time.Hour, 47*time.Hour)
	add("c3", "C3", model.Info, 24*time.Hour, 0)
	add("ignored", "H11", model.Warn, 72*time.Hour, 24*time.Hour)
	add("snoozed", "H7", model.Warn, 24*time.Hour, 0)
	v := &engine.View{Snapshot: &model.Snapshot{}, Snoozes: map[string]store.Snooze{
		"ignored": {FindingID: "ignored", Kind: store.IgnoreKind},
		"snoozed": {FindingID: "snoozed", Kind: store.SnoozeKind, Until: now.Add(time.Hour)},
	}}
	m := WeeklyMessage(cfg, v, hist, now)
	newS, resS := m.Sections[0], m.Sections[1]
	if newS.Title != "New this week (11)" || len(newS.Lines) != 9 || newS.Lines[8] != "…and 3 more" {
		t.Errorf("new this week: %s %q", newS.Title, newS.Lines)
	}
	if resS.Title != "Resolved this week (1)" || len(resS.Lines) != 1 || !strings.Contains(resS.Lines[0], "h1") {
		t.Errorf("resolved this week: %s %q", resS.Title, resS.Lines)
	}
	for _, l := range append(newS.Lines, resS.Lines...) {
		if strings.Contains(l, "c3") || strings.Contains(l, "ignored") || strings.Contains(l, "snoozed") {
			t.Errorf("listed: %s", l)
		}
	}
}

// The S3 findings of one nested directory on AKKA and SOL share one line,
// and a grouped title names the servers and the checks, which is all a
// phone shows (editor-shared-pc ESP-06, admin-anna AA-29).
func TestUrgentMessageNamesServersAndMergesLines(t *testing.T) {
	cfg := &config.Config{PublicURL: "http://sw.local:8080", Servers: []config.Server{{ID: "akka", Name: "AKKA"}, {ID: "sol", Name: "SOL"}}}
	cfg.ApplyDefaults()
	s3 := func(id, server, name, dir string) *store.Record {
		return &store.Record{Finding: model.Finding{ID: id, Check: "S3", Checks: []string{"S3"}, Severity: model.Error, Server: server, Urgent: true,
			Subject: name + "-LIVE-4FINALS/LIVE-FJORD-4FINALS/" + dir + " on " + name,
			Message: "Project folder " + dir + " is nested inside project folder LIVE-FJORD-4FINALS (" + name + "-LIVE-4FINALS on " + name + ")"}}
	}
	m := UrgentMessage(cfg, []*store.Record{s3("a", "akka", "AKKA", "LIVE-OLDCUT-4FINALS"), s3("b", "sol", "SOL", "LIVE-OLDCUT-4FINALS")})
	if m.Title != "🚨 2 new urgent findings on AKKA, SOL: S3" || !m.Mention {
		t.Errorf("title: %s", m.Title)
	}
	lines := m.Sections[0].Lines
	if len(lines) != 1 || !strings.Contains(lines[0], `\(AKKA-LIVE-4FINALS on AKKA\) _(also on SOL)_`) || !strings.Contains(lines[0], "/problems#f-a)") {
		t.Errorf("merged line: %q", lines)
	}
	// Different directories and other checks keep their own lines.
	h1 := &store.Record{Finding: model.Finding{ID: "h1", Check: "H1", Checks: []string{"H1"}, Severity: model.Error, Server: "sol", Subject: "SOL",
		Message: "Server SOL is unreachable: timeout", Urgent: true}}
	h2 := &store.Record{Finding: model.Finding{ID: "h2", Check: "H2", Checks: []string{"H2"}, Severity: model.Error, Related: []string{"akka", "sol"}, Subject: "AKKA ↔ SOL",
		Message: "Servers AKKA and SOL are not connected", Urgent: true}}
	m = UrgentMessage(cfg, []*store.Record{s3("a", "akka", "AKKA", "LIVE-OLDCUT-4FINALS"), h1, s3("c", "akka", "AKKA", "LIVE-B-4FINALS"), h2})
	if m.Title != "🚨 4 new urgent findings on AKKA, SOL: H1, H2, S3" {
		t.Errorf("title: %s", m.Title)
	}
	if lines := m.Sections[0].Lines; len(lines) != 4 || strings.Contains(strings.Join(lines, "\n"), "also on") {
		t.Errorf("lines: %q", lines)
	}
	// One finding keeps the check title and the subject.
	if m := UrgentMessage(cfg, []*store.Record{h1}); m.Title != "🚨 Server unreachable: SOL" {
		t.Errorf("single title: %s", m.Title)
	}
}

// Two servers that are down are two problems: their H1 lines stay apart.
func TestUrgentMessageKeepsServerFindingsApart(t *testing.T) {
	cfg := &config.Config{Servers: []config.Server{{ID: "akka", Name: "AKKA"}, {ID: "sol", Name: "SOL"}}}
	cfg.ApplyDefaults()
	h1 := func(id, server, name string) *store.Record {
		return &store.Record{Finding: model.Finding{ID: id, Check: "H1", Checks: []string{"H1"}, Severity: model.Error, Server: server, Subject: name,
			Message: "Server " + name + " is unreachable: connection refused", Urgent: true}}
	}
	m := UrgentMessage(cfg, []*store.Record{h1("a", "akka", "AKKA"), h1("b", "sol", "SOL")})
	if lines := m.Sections[0].Lines; len(lines) != 2 || m.Title != "🚨 2 new urgent findings on AKKA, SOL: H1" {
		t.Errorf("%s: %q", m.Title, lines)
	}
}

// The health line names each state for what it is (W-036, W-003, W-005):
// ignored findings are not "snoozed", a changed certificate is not "down",
// a disabled server is listed, the PCs are unknown until a server answered
// a full check, and a PC counts as offline only when every server that has
// it configured is up and loaded.
func TestHealthLineStates(t *testing.T) {
	now := local(2026, 10, 6, 10, 0)
	cfg := &config.Config{Servers: []config.Server{{ID: "akka", Name: "AKKA"}, {ID: "sol", Name: "SOL"}, {ID: "old", Name: "OLD", Disabled: true}}}
	cfg.ApplyDefaults()
	away := now.Add(-10 * 24 * time.Hour)
	akka := &model.Server{ID: "akka", Name: "AKKA", Status: model.StatusUp, Loaded: true, MyID: "AKKA-ID", Devices: map[string]*model.Device{
		"PC1": {ID: "PC1", Name: "pc1", LastSeen: away}, // only on AKKA: offline
		"PC2": {ID: "PC2", Name: "pc2", LastSeen: away}, // also on SOL: unknown
	}}
	sol := &model.Server{ID: "sol", Name: "SOL", Status: model.StatusCertChanged, Loaded: true, MyID: "SOL-ID", Devices: map[string]*model.Device{
		"PC2": {ID: "PC2", Name: "pc2", LastSeen: away},
	}}
	open := func(id string) *store.Record {
		return &store.Record{Finding: model.Finding{ID: id, Check: "H11", Severity: model.Warn}}
	}
	v := &engine.View{Snapshot: &model.Snapshot{Servers: []*model.Server{akka, sol}}, Open: []*store.Record{open("z"), open("i"), open("w")},
		Snoozes: map[string]store.Snooze{
			"z": {FindingID: "z", Kind: store.SnoozeKind, Until: now.Add(time.Hour)},
			"i": {FindingID: "i", Kind: store.IgnoreKind},
		}}
	want := "1 of 3 servers up (SOL certificate changed, OLD disabled) · 2 PCs, 1 offline more than 7 days, 1 unknown · ERROR 0 · WARNING 1 · INFO 0 · 1 snoozed · 1 ignored"
	if got := HealthLine(cfg, v, now); got != want {
		t.Errorf("health line:\n got %s\nwant %s", got, want)
	}
	// Right after a start: nothing answered a full check yet.
	for _, sv := range []*model.Server{akka, sol} {
		sv.Status, sv.Loaded, sv.Devices = model.StatusUnknown, false, nil
	}
	v = &engine.View{Snapshot: &model.Snapshot{Servers: []*model.Server{akka, sol}}}
	want = "0 of 3 servers up (AKKA, SOL not contacted, OLD disabled) · PCs unknown · ERROR 0 · WARNING 0 · INFO 0 · 0 snoozed"
	if got := HealthLine(cfg, v, now); got != want {
		t.Errorf("health line after a start:\n got %s\nwant %s", got, want)
	}
}

// waitFor waits up to 5 seconds for cond (a notice is sent in a goroutine).
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A settings notice gets one log line for each decision (admin-anna AA-36):
// when the channels it went to are still the configured ones and Discord
// refuses it, it is discarded at once, not "retried" and discarded 45 ms
// later. When the webhook was replaced, it is kept for the new one.
// Notices are grey, not in the WARNING colour (W-039).
func TestNoticeLogsOneDecision(t *testing.T) {
	start := local(2026, 10, 6, 10, 0)
	r := newRig(t, start, "AKKA")
	r.n.now = func() time.Time { return r.now }
	logs := &lockedLog{}
	r.n.log = slog.New(slog.NewTextHandler(logs, nil))
	dead := &fakeSender{}
	dead.fail(&httpError{code: 404, text: "Unknown Webhook"})
	r.n.SetSenders(func() []Sender { return []Sender{dead} })
	r.n.NoticeLater()("Server AKKA changed", "The API key is replaced.")
	waitFor(t, func() bool { return strings.Contains(logs.String(), "settings notice") })
	r.at(start.Add(time.Minute))
	r.at(start.Add(5 * time.Minute))
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 || !strings.Contains(logs.String(), "discarded") || pendingNotices(r) != 0 {
		t.Fatalf("refused by the configured webhook: %d warnings, %d pending:\n%s", n, pendingNotices(r), logs.String())
	}
	dead.mu.Lock()
	tried := dead.attempts
	dead.mu.Unlock()
	if tried != 1 {
		t.Fatalf("refused notice tried %d times", tried)
	}

	r = newRig(t, start, "AKKA")
	r.n.now = func() time.Time { return r.now }
	logs = &lockedLog{}
	r.n.log = slog.New(slog.NewTextHandler(logs, nil))
	var mu sync.Mutex
	current := []Sender{dead}
	r.n.SetSenders(func() []Sender {
		mu.Lock()
		defer mu.Unlock()
		return current
	})
	notice := r.n.NoticeLater()
	mu.Lock()
	current = []Sender{r.sender}
	mu.Unlock()
	r.n.ChannelsChanged() // the administrator saved a new webhook
	notice("Discord webhook replaced", "x")
	waitFor(t, func() bool { return pendingNotices(r) == 1 })
	r.at(start.Add(time.Minute))
	msgs := r.sender.take()
	if len(msgs) != 1 || msgs[0].Kind != KindNotice || msgs[0].Color != ColorNotice {
		t.Fatalf("notice for the new webhook: %+v", msgs)
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 || !strings.Contains(logs.String(), "retries") {
		t.Fatalf("replaced webhook: %d warnings:\n%s", n, logs.String())
	}
}
