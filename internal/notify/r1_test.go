package notify

import (
	"context"
	"encoding/json"
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
