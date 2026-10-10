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
