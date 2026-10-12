package notify

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/engine"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/store"
)

// allowAnyWebhook lets a test post to an httptest server, like a deployment
// with SYNCWATCH_ALLOW_ANY_WEBHOOK set.
func allowAnyWebhook(t *testing.T) {
	t.Helper()
	config.AllowAnyWebhook = true
	t.Cleanup(func() { config.AllowAnyWebhook = false })
}

// The token must not show in transport errors, whatever spelling the
// webhook was saved in: net/http prints the parsed URL, not the stored one.
func TestRedactsWebhookTokenInAnySpelling(t *testing.T) {
	tr := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, errors.New("simulated dial failure")
	}}
	for _, u := range []string{
		"https://discord.com/api/webhooks/123/SECRETTOKEN",
		"HTTPS://discord.com/api/webhooks/123/SECRETTOKEN",
		"https://discord.com/api/webhooks/123/SECRETTOKEN\u200b",
		"https://discord.com/api/webhooks/123/SECRET TOKEN",
		"https://discord.com/api/webhooks/123/SECRETTOKEN?wait=true",
		"https://discord.com/api/v10/webhooks/123/SECRETTOKEN/",
	} {
		d := &Discord{URL: u, HTTP: &http.Client{Transport: tr, Timeout: 5 * time.Second}}
		err := d.Send(context.Background(), Message{Title: "t"})
		if err == nil || !strings.Contains(err.Error(), "simulated dial failure") {
			t.Fatalf("%q: unexpected error %v", u, err)
		}
		if s := err.Error(); strings.Contains(s, "SECRET") || strings.Contains(s, "123") {
			t.Errorf("%q: token in error text: %s", u, s)
		}
	}
	// Error text that spells the path differently is redacted as well.
	stored := "https://discord.com/api/webhooks/123/SECRETTOKEN"
	for _, s := range []string{
		`Post "https://discord.com/api/webhooks/123/SECRETTOKEN%E2%80%8B": x`,
		`Post "https://discordapp.com/api/v10/webhooks/999/OTHERTOKEN/": x`,
		`proxy says /api/webhooks/123/SECRETTOKEN?wait=true is gone`,
	} {
		if got := redactURL(s, stored); strings.Contains(got, "TOKEN") || !strings.Contains(got, "<webhook>") {
			t.Errorf("redactURL(%q) = %q", s, got)
		}
	}
}

// Whatever goes in, the payload stays within Discord's limits; otherwise a
// message is rejected for good and, if it is a notice, blocks the rest.
func TestPayloadStaysWithinDiscordLimits(t *testing.T) {
	d := &Discord{RoleID: "42"}
	var lines []string
	for i := 0; i < 30; i++ {
		lines = append(lines, strings.Repeat("x", 300))
	}
	for name, m := range map[string]Message{
		"long summary":      {Title: "t", Summary: strings.Repeat("a", 5000)},
		"huge summary":      {Title: "t", Summary: strings.Repeat("a", 7000), Sections: []Section{{Title: "New", Lines: lines}}},
		"multibyte summary": {Title: strings.Repeat("é", 300), Summary: strings.Repeat("é", 3000)},
		"long title":        {Title: strings.Repeat("t", 1000), Mention: true, Sections: []Section{{Title: "New", Lines: lines}}},
		"one huge line":     {Title: "t", Sections: []Section{{Title: "New", Lines: []string{strings.Repeat("x", 5000)}}}},
		"role as summary":   {Title: "t", Summary: "Urgent messages now mention role " + strings.Repeat("9", 5000)},
	} {
		p := d.Payload(m)
		e := p.Embeds[0]
		if len(e.Description) > maxDescription {
			t.Errorf("%s: description is %d bytes", name, len(e.Description))
		}
		if len([]rune(e.Title)) > maxTitle {
			t.Errorf("%s: title is %d runes", name, len([]rune(e.Title)))
		}
		if total := len(e.Title) + len(e.Description) + len(e.Footer.Text); total > maxEmbedTotal {
			t.Errorf("%s: embed is %d bytes in total", name, total)
		}
		if len([]rune(p.Content)) > maxContent {
			t.Errorf("%s: content is %d runes", name, len([]rune(p.Content)))
		}
		if m.Summary != "" && !strings.HasPrefix(e.Description, string([]rune(m.Summary)[:100])) {
			t.Errorf("%s: summary lost: %.60s", name, e.Description)
		}
	}
}

// unescaped reports whether tok occurs in s without a backslash before it.
func unescaped(s, tok string) bool {
	for off := 0; ; {
		i := strings.Index(s[off:], tok)
		if i < 0 {
			return false
		}
		if off+i == 0 || s[off+i-1] != '\\' {
			return true
		}
		off += i + 1
	}
}

// Names from Syncthing (device names, folder labels, paths, pending device
// names chosen by strangers) must come through as plain text everywhere:
// content, embed title, summary and lines.
func TestHostileNamesAreNeutralised(t *testing.T) {
	hostile := []string{
		`<script>alert(1)</script><a href="https://evil.invalid">x</a>`,
		"**bold** _it_ ~~s~~ `code` ||spoiler|| > quote # heading [x](https://evil.invalid) ![i](https://evil.invalid/i.png)",
		"@everyone @here <@&123> <@123> <#123> <t:0:R>",
		"see https://evil.invalid/login and http://10.0.0.1:8384/ or steam://run/1",
		"zero\u200bwidth\u200c\u200d\ufeff bidi \u202eevil\u202c \u2066x\u2069 \u200e\u200f",
		strings.Repeat("Å", 500),
		"emoji 🚨✅🔥 👨\u200d👩\u200d👧 🇸🇪",
		"line1\nline2\r\n- list\n1. item\n# heading\n> quote\x00\x07\x1b[31mred\u2028sep",
	}
	cfg := &config.Config{PublicURL: "http://sw.local:8080"}
	cfg.ApplyDefaults()
	now := time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)
	d := &Discord{}
	own := regexp.MustCompile(`\[details\]\(http://sw\.local:8080/problems#f-[0-9a-f]+\)|\[Open the dashboard\]\(http://sw\.local:8080\)`)
	bidi := "\u202a\u202b\u202c\u202d\u202e\u2066\u2067\u2068\u2069"

	for i, name := range hostile {
		id := fmt.Sprintf("%016x", i)
		rec := &store.Record{Finding: model.Finding{ID: id, Check: "H9", Checks: []string{"H9"}, Severity: model.Warn, Subject: name + " on AKKA",
			Message: "Unknown device " + name + " wants to connect to AKKA", Urgent: true}, OpenedAt: now.Add(-time.Hour), PingedAt: now.Add(-time.Hour)}
		res := *rec
		res.ResolvedAt = now
		h := store.HistoryEntry{FindingID: id, Check: "H9", Severity: model.Warn, Message: rec.Message, Urgent: true, OpenedAt: now.Add(-time.Hour), ResolvedAt: now}
		v := &engine.View{Open: []*store.Record{rec}, Snoozes: map[string]store.Snooze{},
			Snapshot: &model.Snapshot{Servers: []*model.Server{{ID: "akka", Name: name, Status: model.StatusDown}}}}
		msgs := map[string]Message{
			"urgent":    UrgentMessage(cfg, []*store.Record{rec}),
			"resolved":  ResolvedMessage(cfg, []*store.Record{&res}, nil),
			"reminder":  ReminderMessage(cfg, []*store.Record{rec}, now),
			"overnight": OvernightMessage(cfg, []*store.Record{rec}, []*store.Record{&res}, []store.HistoryEntry{h}, []*store.Record{rec}, nil),
			"weekly":    WeeklyMessage(cfg, v, []store.HistoryEntry{h}, now),
			"missed":    MissedMessage(cfg, nil, []store.HistoryEntry{h, h}, now.Add(-time.Hour), now),
			"missed1":   MissedMessage(cfg, nil, []store.HistoryEntry{h}, now.Add(-time.Hour), now),
			// Settings notices with a name, as the dashboard words them: the
			// name on one line (OneLine) in syncwatch's own lines (import-1,
			// text-1, notify-1).
			"devices notice": {Kind: KindNotice, Title: noticeIcon + "Devices changed",
				Summary: "The device " + OneLine(name) + " (ANNAPCX) always connects through a relay. H11 is off for it.\nChanged from 127.0.0.1."},
			"hidden notice": {Kind: KindNotice, Title: noticeIcon + "Urgent finding ignored",
				Summary: OneLine("H9 "+rec.Subject+": "+rec.Message) + "\n" + OneLine("The finding is ignored. Reason: "+name) + "\nChanged from 127.0.0.1."},
		}
		for kind, m := range msgs {
			p := d.Payload(m)
			e := p.Embeds[0]
			if len([]rune(p.Content)) > maxContent || len([]rune(e.Title)) > maxTitle || len(e.Description) > maxDescription {
				t.Errorf("name %d %s: over Discord's limits", i, kind)
			}
			for where, s := range map[string]string{"content": p.Content, "title": e.Title, "description": e.Description} {
				if where != "description" && strings.Contains(s, "\n") {
					t.Errorf("name %d %s %s: line break: %q", i, kind, where, s)
				}
				s = own.ReplaceAllString(s, "")
				for _, bad := range []string{"<@", "<#", "<t:", "<script", "@everyone", "@here", "](", "://", "\n- ", "\n1. ", "\n# ", "\n> ", "line1\nline2", "**bold**", "_it_", "~~s~~", "`code`", "||spoiler||", "\x1b", "\x00", "\x07"} {
					if unescaped(s, bad) {
						t.Errorf("name %d %s %s: keeps %q: %s", i, kind, where, bad, s)
					}
				}
				if strings.ContainsAny(s, bidi) {
					t.Errorf("name %d %s %s: keeps bidi controls: %q", i, kind, where, s)
				}
			}
			// What is harmless stays: emoji sequences and syncwatch's own markdown.
			if i == 6 && !strings.Contains(e.Description, "👨\u200d👩\u200d👧 🇸🇪") {
				t.Errorf("%s: emoji damaged: %s", kind, e.Description)
			}
			if m.Kind == KindNotice {
				// The lines of syncwatch stay, the name adds none.
				if lines := strings.Split(e.Description, "\n"); len(lines) != strings.Count(m.Summary, "\n")+1 || lines[len(lines)-1] != "Changed from 127.0.0.1." {
					t.Errorf("name %d %s: lines %q", i, kind, lines)
				}
				continue
			}
			if !strings.Contains(e.Description, "**H9**") {
				t.Errorf("name %d %s: bold check ID lost: %s", i, kind, e.Description)
			}
			if kind == "urgent" && !strings.Contains(e.Description, "[details](http://sw.local:8080/problems#f-"+id+")") {
				t.Errorf("name %d: details link lost: %s", i, e.Description)
			}
			if kind == "weekly" && !strings.Contains(e.Description, "[Open the dashboard](http://sw.local:8080)") {
				t.Errorf("name %d: dashboard link lost: %s", i, e.Description)
			}
		}
	}
	// The exact spelling: escaped markdown, a zero-width space after "@" and
	// after a URL scheme's colon, newlines as spaces.
	got := escapeMD("**b** @everyone https://evil.invalid/x a\nb\r\nc")
	if want := `\*\*b\*\* @` + "\u200b" + `everyone https:` + "\u200b" + `//evil.invalid/x a b c`; got != want {
		t.Errorf("escapeMD = %q, want %q", got, want)
	}
	for _, s := range []string{"- item", "1. item", "+ item"} {
		if e := escapeMD(s); !strings.HasPrefix(e, `\`) && !strings.Contains(e, `\.`) {
			t.Errorf("list marker kept: %q", e)
		}
	}
}

// A webhook saved while SYNCWATCH_ALLOW_ANY_WEBHOOK was set must stop
// receiving messages once the variable is gone.
func TestStoredWebhookIsCheckedAtSendTime(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	d := &Discord{URL: srv.URL + "/api/webhooks/1/x"}
	err := d.Send(context.Background(), Message{Title: "t"})
	if err == nil || hits.Load() != 0 {
		t.Fatalf("sent to a non-Discord host without the override: err=%v hits=%d", err, hits.Load())
	}
	if s := err.Error(); !strings.Contains(s, "Discord") || strings.Contains(s, srv.URL) || strings.Contains(s, "/1/x") {
		t.Fatalf("error: %s", s)
	}
	allowAnyWebhook(t)
	if err := d.Send(context.Background(), Message{Title: "t"}); err != nil || hits.Load() != 1 {
		t.Fatalf("with the override: err=%v hits=%d", err, hits.Load())
	}
}

// A redirecting webhook would hand the message, token and all, to wherever
// it points. Redirects aren't followed.
func TestSendDoesNotFollowRedirects(t *testing.T) {
	allowAnyWebhook(t)
	var followed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			followed.Store(true)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Redirect(w, r, "/elsewhere", http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	for _, d := range []*Discord{{URL: srv.URL + "/api/webhooks/1/x"}, {URL: srv.URL + "/api/webhooks/1/x", HTTP: &http.Client{}}} {
		err := d.Send(context.Background(), Message{Title: "t"})
		if err == nil || followed.Load() || !strings.Contains(err.Error(), "307") {
			t.Fatalf("redirect: err=%v followed=%v", err, followed.Load())
		}
	}
}

// Discord explains a 400 only in the errors object.
func TestDiscordErrorIncludesFieldErrors(t *testing.T) {
	body := `{"message": "Invalid Form Body", "code": 50035, "errors": {"embeds": {"0": {"description": {"_errors": [{"code": "BASE_TYPE_MAX_LENGTH", "message": "Must be 4096 or fewer in length."}]}}}}}`
	got := discordError([]byte(body))
	if !strings.HasPrefix(got, "Invalid Form Body") || !strings.Contains(got, "Must be 4096 or fewer in length.") {
		t.Errorf("400 reply: %q", got)
	}
	long := `{"message": "Invalid Form Body", "code": 50035, "errors": {"a": "` + strings.Repeat("x", 500) + `"}}`
	if got := discordError([]byte(long)); len([]rune(got)) > len("Invalid Form Body")+1+200 {
		t.Errorf("errors object not clipped: %d chars", len([]rune(got)))
	}
	if got := discordError([]byte(`{"message": "Unknown Webhook", "code": 10015}`)); got != "Unknown Webhook" {
		t.Errorf("reply without errors: %q", got)
	}
}

// "…and N more" counts the findings that aren't shown, also when the lines
// already end in limitLines' own "…and N more".
func TestTruncationCountsHiddenLines(t *testing.T) {
	var lines []string
	for i := 0; i < 30; i++ {
		lines = append(lines, fmt.Sprintf("%02d ", i)+strings.Repeat("x", 297))
	}
	more := func(desc string) int {
		var n int
		if _, err := fmt.Sscanf(desc[strings.LastIndex(desc, "…and"):], "…and %d more", &n); err != nil {
			t.Fatalf("no marker: %s", desc)
		}
		return n
	}
	desc := (&Discord{}).Payload(Message{Title: "t", Sections: []Section{{Title: "New", Lines: limitLines(lines, 25)}}}).Embeds[0].Description
	shown := strings.Count(desc, "\n") - 1 // lines plus one marker
	if n := more(desc); shown+n != 30 || strings.Count(desc, "…and") != 1 {
		t.Errorf("%d shown + %d more != 30: %s", shown, n, desc[len(desc)-40:])
	}
	// Nothing cut by the payload: limitLines' own count stands.
	short := limitLines(lines[:26], 25)
	for i := range short[:25] {
		short[i] = "y"
	}
	desc = (&Discord{}).Payload(Message{Title: "t", Sections: []Section{{Title: "New", Lines: short}}}).Embeds[0].Description
	if n := more(desc); n != 1 {
		t.Errorf("untruncated section: …and %d more", n)
	}
}
