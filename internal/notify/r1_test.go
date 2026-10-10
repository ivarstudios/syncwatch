package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

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
