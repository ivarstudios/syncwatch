package notify

import (
	"context"
	"encoding/json"
	"fmt"
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

// noticeRows returns the lines of the notification log about settings
// notices, oldest first.
func noticeRows(t *testing.T, st *store.Store) []store.NotificationEntry {
	t.Helper()
	log, err := st.Notifications(1000)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.NotificationEntry
	for i := len(log) - 1; i >= 0; i-- {
		if log[i].Kind == KindNotice {
			out = append(out, log[i])
		}
	}
	return out
}

// storedNotices returns the kept notices in the settings.
func storedNotices(t *testing.T, st *store.Store) []pendingNotice {
	t.Helper()
	var kept []pendingNotice
	if _, err := st.JSONSetting(keyKept, &kept); err != nil {
		t.Fatal(err)
	}
	return kept
}

// A security notice that Discord refuses (a deleted webhook: HTTP 404) is
// kept until a channel accepts it: through 2 days and a restart. It has one
// line in the notification log for the failure, not one for each retry, and
// one when it is sent (W-051, ops-chaos OC-03). A notice that is not about
// security keeps the rule of r1: dropped when the webhook refuses it.
func TestSecurityNoticeKeptUntilAChannelAcceptsIt(t *testing.T) {
	start := local(2026, 10, 6, 10, 0)
	r := newRig(t, start, "AKKA")
	r.n.now = func() time.Time { return r.now }
	logs := &lockedLog{}
	r.n.log = slog.New(slog.NewTextHandler(logs, nil))
	r.sender.fail(&httpError{code: 404, text: "Unknown Webhook"})
	r.n.Notice(Notice{Title: "Server AKKA changed", Summary: "The API key is replaced.", Security: true})
	r.n.Notice(Notice{Title: "Checks changed", Summary: "H7 is off."})
	waitFor(t, func() bool { return pendingNotices(r) == 1 && strings.Count(logs.String(), "settings notice") == 2 })
	if !strings.Contains(logs.String(), `msg="settings notice kept until a channel accepts it" title="Server AKKA changed"`) ||
		!strings.Contains(logs.String(), `msg="settings notice discarded, Discord refused it" title="Checks changed"`) {
		t.Fatalf("log:\n%s", logs.String())
	}
	if d := r.n.Delivery(); d.NoticesKept != 1 {
		t.Fatalf("NoticesKept: %+v", d)
	}
	for at := start; at.Before(start.Add(48 * time.Hour)); at = at.Add(10 * time.Minute) {
		r.at(at)
	}
	if pendingNotices(r) != 1 {
		t.Fatal("the security notice is not kept for 2 days")
	}
	if kept := storedNotices(t, r.st); len(kept) != 1 || !kept[0].Security || kept[0].Title != "🔧 Server AKKA changed" || !kept[0].GiveUp.IsZero() {
		t.Fatalf("stored: %+v", kept)
	}
	rows := noticeRows(t, r.st)
	if len(rows) != 2 || rows[0].OK || rows[1].OK {
		t.Fatalf("notification log: %+v", rows)
	}
	for _, row := range rows {
		switch row.Summary {
		case "🔧 Server AKKA changed":
			if row.Error != "kept until a channel accepts it: Discord: HTTP 404: Unknown Webhook" {
				t.Errorf("kept line: %+v", row)
			}
		case "🔧 Checks changed":
			if row.Error != "discarded: Discord: HTTP 404: Unknown Webhook" {
				t.Errorf("discarded line: %+v", row)
			}
		default:
			t.Errorf("line: %+v", row)
		}
	}

	// A restart: the new notifier reads the kept notice.
	r.restart()
	r.n.now = func() time.Time { return r.now }
	if d := r.n.Delivery(); d.NoticesKept != 1 {
		t.Fatalf("NoticesKept after a restart: %+v", d)
	}
	r.at(start.Add(48 * time.Hour))
	if pendingNotices(r) != 1 {
		t.Fatal("the security notice is lost at a restart")
	}
	r.sender.fail(nil) // a new webhook, or Discord works again
	r.at(start.Add(49 * time.Hour))
	msgs := r.sender.take()
	if len(msgs) != 1 || msgs[0].Kind != KindNotice || msgs[0].Title != "🔧 Server AKKA changed" || msgs[0].Color != ColorNotice ||
		!strings.Contains(msgs[0].Summary, "The API key is replaced.\nChanged at 2026-10-06 10:00. syncwatch kept this settings notice because the first send failed.") {
		t.Fatalf("kept notice: %+v", msgs)
	}
	if d := r.n.Delivery(); d.NoticesKept != 0 || pendingNotices(r) != 0 || len(storedNotices(t, r.st)) != 0 {
		t.Fatalf("still kept after the send: %+v", d)
	}
	if rows := noticeRows(t, r.st); len(rows) != 3 || !rows[2].OK || rows[2].Summary != "🔧 Server AKKA changed" {
		t.Fatalf("notification log after the send: %+v", rows)
	}
}

// Discord refuses a message that is not valid (HTTP 400) on every webhook:
// then also a security notice is dropped, with one warning.
func TestInvalidSecurityNoticeIsDropped(t *testing.T) {
	start := local(2026, 10, 6, 10, 0)
	r := newRig(t, start, "AKKA")
	r.n.now = func() time.Time { return r.now }
	logs := &lockedLog{}
	r.n.log = slog.New(slog.NewTextHandler(logs, nil))
	r.sender.fail(&httpError{code: 400, text: "Invalid Form Body"})
	r.n.Notice(Notice{Title: "Access settings changed", Summary: "x", Security: true})
	waitFor(t, func() bool { return strings.Contains(logs.String(), "settings notice") })
	r.at(start.Add(time.Minute))
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 || !strings.Contains(logs.String(), "discarded") || pendingNotices(r) != 0 {
		t.Fatalf("%d warnings, %d kept:\n%s", n, pendingNotices(r), logs.String())
	}
	if rows := noticeRows(t, r.st); len(rows) != 1 || rows[0].Error != "discarded: Discord: HTTP 400: Invalid Form Body" {
		t.Fatalf("notification log: %+v", rows)
	}
}

// With a webhook for settings notices, a notice goes only there: grey,
// without the role mention, logged on the channel discord-notices. The
// test notice goes there too; without that webhook it is an error.
func TestNoticesWebhookGetsTheNotices(t *testing.T) {
	allowAnyWebhook(t)
	var mu sync.Mutex
	posts := map[string][]DiscordPayload{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		var p DiscordPayload
		if err := json.Unmarshal(b, &p); err != nil {
			t.Error(err)
		}
		mu.Lock()
		posts[req.URL.Path] = append(posts[req.URL.Path], p)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	got := func(path string) []DiscordPayload {
		mu.Lock()
		defer mu.Unlock()
		return append([]DiscordPayload(nil), posts[path]...)
	}
	r := newRig(t, local(2026, 10, 6, 10, 0), "AKKA")
	r.n.now = func() time.Time { return r.now }
	r.n.senders = r.n.defaultSenders // the stored webhooks
	setNotify(t, r, func(c *config.Config) { c.Notify.DiscordRoleID = "424242424242424242" })
	if err := r.n.SendNoticesTest(context.Background()); err == nil || err.Error() != "No Discord webhook for settings notices is configured." {
		t.Fatalf("test notice without the webhook: %v", err)
	}
	if err := r.st.SetSecret(store.SecretDiscordWebhook, srv.URL+"/api/webhooks/1/alerts"); err != nil {
		t.Fatal(err)
	}
	if err := r.st.SetSecret(store.SecretNoticesWebhook, srv.URL+"/api/webhooks/2/notices"); err != nil {
		t.Fatal(err)
	}
	r.n.Notice(Notice{Title: "Server AKKA changed", Summary: "The API key is replaced.", Security: true})
	waitFor(t, func() bool { return len(got("/api/webhooks/2/notices")) == 1 })
	p := got("/api/webhooks/2/notices")[0]
	if len(p.Embeds) != 1 || p.Embeds[0].Color != ColorNotice || strings.Contains(p.Content, "<@") || len(p.AllowedMentions.Roles) != 0 ||
		p.Embeds[0].Title != "🔧 Server AKKA changed" {
		t.Fatalf("notice payload: %+v", p)
	}
	if n := len(got("/api/webhooks/1/alerts")); n != 0 {
		t.Fatalf("the webhook of the findings got %d notices", n)
	}
	if err := r.n.SendNoticesTest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ps := got("/api/webhooks/2/notices"); len(ps) != 2 || ps[1].Embeds[0].Title != "syncwatch test settings notice" || strings.Contains(ps[1].Content, "<@") {
		t.Fatalf("test notice: %+v", ps)
	}
	log, _ := r.st.Notifications(10)
	if len(log) != 2 || log[0].Channel != "discord-notices" || log[0].Kind != KindTest || log[1].Channel != "discord-notices" || log[1].Kind != KindNotice {
		t.Fatalf("notification log: %+v", log)
	}
	// The messages about findings still go to the webhook of the findings,
	// with the mention.
	if err := r.n.SendTest(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ps := got("/api/webhooks/1/alerts"); len(ps) != 1 || !strings.HasPrefix(ps[0].Content, "<@&424242424242424242>") {
		t.Fatalf("test message: %+v", ps)
	}
}

// The webhook for settings notices fails: its failures have their own
// back-off and do not show in Delivery (that is the channel of the
// findings), and they do not hold up the urgent messages. A kept security
// notice also goes to the channel of the findings when that webhook fails
// for more than 1 hour; a notice that is not about security does not.
func TestNoticesWebhookFallsBackAfterAnHour(t *testing.T) {
	start := local(2026, 10, 6, 10, 0)
	r := newRig(t, start, "AKKA", "SOL")
	r.n.now = func() time.Time { return r.now }
	own := &fakeSender{name: "discord-notices"}
	own.fail(&httpError{code: 404, text: "Unknown Webhook"})
	r.n.SetNoticeSenders(func() []Sender { return []Sender{own} })
	r.n.Notice(Notice{Title: "Server SOL: new certificate accepted", Summary: "x", Security: true})
	waitFor(t, func() bool { return pendingNotices(r) == 1 })
	r.n.Notice(Notice{Title: "Checks changed", Summary: "y"})
	waitFor(t, func() bool { return own.tries() == 2 })
	if pendingNotices(r) != 1 {
		t.Fatal("the notice that is not about security is kept after a 404")
	}
	r.setDown("SOL", true)
	r.at(start)
	r.at(start.Add(5 * time.Minute).Add(holdUrgent))
	if msgs := r.sender.take(); len(msgs) != 1 || msgs[0].Kind != KindUrgent {
		t.Fatalf("the urgent message: %+v", msgs)
	}
	for m := 6; m <= 60; m++ {
		r.at(start.Add(time.Duration(m) * time.Minute))
		if d := r.n.Delivery(); d.Failing || d.NoticesKept != 1 {
			t.Fatalf("%d min: %+v", m, d)
		}
	}
	if msgs := r.sender.take(); len(msgs) != 0 {
		t.Fatalf("the channel of the findings got %+v within the first hour", msgs[0].Title)
	}
	tries := own.tries()
	if tries > 2+6 { // 1, 2, 4, 8, 16 … minutes, not each tick
		t.Fatalf("%d tries in 1 hour", tries)
	}
	r.at(start.Add(2 * time.Hour))
	msgs := r.sender.take()
	if len(msgs) != 1 || msgs[0].Kind != KindNotice || msgs[0].Title != "🔧 Server SOL: new certificate accepted" {
		t.Fatalf("after 1 hour: %+v", msgs)
	}
	if d := r.n.Delivery(); d.NoticesKept != 0 || d.Failing {
		t.Fatalf("after the fallback: %+v", d)
	}
	// A new security notice while the webhook still fails goes to the
	// channel of the findings at once; another notice does not.
	r.now = start.Add(2*time.Hour + time.Minute)
	r.n.Notice(Notice{Title: "Access settings changed", Summary: "z", Security: true})
	r.n.Notice(Notice{Title: "Structure rules changed", Summary: "w"})
	waitFor(t, func() bool { return r.sender.sent() == 1 && own.tries() >= tries+2 })
	time.Sleep(50 * time.Millisecond)
	if msgs := r.sender.take(); len(msgs) != 1 || msgs[0].Title != "🔧 Access settings changed" || pendingNotices(r) != 0 {
		t.Fatalf("new notices: %+v, %d kept", msgs, pendingNotices(r))
	}
}

// At most 50 notices wait. The oldest one that is not about security goes
// first, then the oldest security notice.
func TestKeptNoticesLimit(t *testing.T) {
	r := newRig(t, local(2026, 10, 6, 10, 0), "AKKA")
	at := local(2026, 10, 6, 10, 0)
	r.n.queueNotice(pendingNotice{Key: "s0", Title: "🔧 Access settings changed", Security: true, At: at})
	r.n.queueNotice(pendingNotice{Key: "other", Title: "🔧 Checks changed", At: at, GiveUp: at.Add(noticeRetryFor)})
	for i := 1; i < maxNotices; i++ {
		r.n.queueNotice(pendingNotice{Key: fmt.Sprint("s", i), Title: "🔧 Access settings changed", Security: true, At: at})
	}
	kept := storedNotices(t, r.st)
	if len(kept) != maxNotices || kept[0].Key != "s0" || kept[1].Key != "s1" {
		t.Fatalf("%d kept, first %s, %s", len(kept), kept[0].Key, kept[1].Key)
	}
	r.n.queueNotice(pendingNotice{Key: "last", Title: "🔧 Access settings changed", Security: true, At: at})
	if kept := storedNotices(t, r.st); len(kept) != maxNotices || kept[0].Key != "s1" || kept[len(kept)-1].Key != "last" {
		t.Fatalf("%d kept, first %s", len(kept), kept[0].Key)
	}
}

// The notification log has a line for a settings notice also when no
// webhook is configured.
func TestNoticeWithoutWebhookIsLogged(t *testing.T) {
	r := newRig(t, local(2026, 10, 6, 10, 0), "AKKA")
	r.n.SetSenders(func() []Sender { return nil })
	r.n.Notice(Notice{Title: "Access settings changed", Summary: "x", Security: true})
	waitFor(t, func() bool { return len(noticeRows(t, r.st)) == 1 })
	if rows := noticeRows(t, r.st); rows[0].Error != "No Discord webhook is configured." || rows[0].Summary != "🔧 Access settings changed" || pendingNotices(r) != 0 {
		t.Fatalf("notification log: %+v", rows)
	}
}

// tries is the number of sends to f.
func (f *fakeSender) tries() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

// sent is the number of messages that f has and nobody took.
func (f *fakeSender) sent() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.msgs)
}

// A webhook for settings notices that cannot be decrypted (the secret key
// changed) fails like a dead webhook: the test notice says why, a security
// notice is kept, and the log has the error once, not at each tick.
func TestUndecryptableNoticesWebhook(t *testing.T) {
	dir := t.TempDir()
	keyA, keyB := make([]byte, 32), make([]byte, 32)
	keyB[0] = 1
	st, err := store.Open(filepath.Join(dir, "db.sqlite"), keyA)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSecret(store.SecretNoticesWebhook, "https://discord.com/api/webhooks/2/x"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	st2, err := store.Open(filepath.Join(dir, "db.sqlite"), keyB)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	h, _ := config.NewHolder(config.Default())
	logs := &lockedLog{}
	eng, err := engine.New(st2, model.NewHub(nil), h, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	n := New(eng, st2, h, slog.New(slog.NewTextHandler(logs, nil)))
	alert := &fakeSender{}
	n.SetSenders(func() []Sender { return []Sender{alert} })
	const want = "syncwatch cannot decrypt the Discord webhook for settings notices: check the secret key"
	if err := n.SendNoticesTest(context.Background()); err == nil || err.Error() != want {
		t.Fatalf("SendNoticesTest: %v", err)
	}
	n.Notice(Notice{Title: "Access settings changed", Summary: "x", Security: true})
	waitFor(t, func() bool { return n.Delivery().NoticesKept == 1 })
	start := time.Now()
	for i := range 5 {
		n.Tick(context.Background(), start.Add(time.Duration(i)*10*time.Minute))
	}
	if c := strings.Count(logs.String(), "cannot read the Discord webhook for settings notices"); c != 1 {
		t.Fatalf("%d error lines:\n%s", c, logs.String())
	}
	if alert.sent() != 0 || n.Delivery().Failing {
		t.Fatalf("the channel of the findings: %d messages, %+v", alert.sent(), n.Delivery())
	}
}

// A notice that is not about security keeps the rule of r1 for a webhook
// that was replaced after the change: Discord's refusal (also HTTP 400) of
// the old webhook keeps it for the new webhook, which gets one try.
func TestOtherNoticeKeepsTheRuleOfR1(t *testing.T) {
	r := newRig(t, local(2026, 10, 6, 10, 0), "AKKA")
	r.n.now = func() time.Time { return r.now }
	old := &fakeSender{}
	old.fail(&httpError{code: 400, text: "Invalid Form Body"})
	r.n.SetSenders(func() []Sender { return []Sender{old} })
	notice := r.n.NoticeLater()
	r.n.SetSenders(func() []Sender { return []Sender{r.sender} })
	r.n.ChannelsChanged() // a new webhook
	notice(Notice{Title: "Checks changed", Summary: "x"})
	waitFor(t, func() bool { return pendingNotices(r) == 1 })
	if kept := storedNotices(t, r.st); len(kept) != 1 || kept[0].Security || kept[0].GiveUp.IsZero() {
		t.Fatalf("kept: %+v", kept)
	}
	if rows := noticeRows(t, r.st); len(rows) != 1 || rows[0].Error != "kept for 1 day: Discord: HTTP 400: Invalid Form Body" {
		t.Fatalf("notification log: %+v", rows)
	}
	r.at(local(2026, 10, 6, 10, 1))
	if m := r.sender.take(); len(m) != 1 || m[0].Kind != KindNotice {
		t.Fatalf("on the new webhook: %+v", m)
	}
}
