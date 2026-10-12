package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
)

// Sender delivers a message to one channel.
type Sender interface {
	Name() string
	Send(ctx context.Context, m Message) error
}

// Discord limits.
const (
	maxContent     = 2000
	maxDescription = 4096
	maxEmbedTotal  = 6000
	maxTitle       = 256
)

// Channel names in the notification log.
const (
	channelDiscord = "discord"         // the webhook of all notifications
	channelNotices = "discord-notices" // the webhook only for settings notices
)

// Discord posts to a Discord webhook.
type Discord struct {
	URL    string
	RoleID string
	HTTP   *http.Client
	// Channel is the name of the channel in the notification log: "discord"
	// when empty, "discord-notices" for the webhook of settings notices.
	Channel string
}

// Name implements Sender.
func (d *Discord) Name() string {
	if d.Channel != "" {
		return d.Channel
	}
	return channelDiscord
}

type discordEmbed struct {
	Title       string        `json:"title,omitempty"`
	URL         string        `json:"url,omitempty"`
	Description string        `json:"description,omitempty"`
	Color       int           `json:"color"`
	Footer      *discordText  `json:"footer,omitempty"`
	Timestamp   string        `json:"timestamp,omitempty"`
	Fields      []interface{} `json:"fields,omitempty"`
}

type discordText struct {
	Text string `json:"text"`
}

// DiscordPayload is the webhook body.
type DiscordPayload struct {
	Content         string          `json:"content,omitempty"`
	Username        string          `json:"username,omitempty"`
	Embeds          []discordEmbed  `json:"embeds,omitempty"`
	AllowedMentions allowedMentions `json:"allowed_mentions"`
}

type allowedMentions struct {
	Parse []string `json:"parse"`
	Roles []string `json:"roles,omitempty"`
}

// Payload renders a message for Discord within its size limits.
func (d *Discord) Payload(m Message) DiscordPayload {
	p := DiscordPayload{Username: "syncwatch", AllowedMentions: allowedMentions{Parse: []string{}}}
	// Titles carry subjects (folder labels, device names) and no formatting
	// of their own, so they are escaped as a whole, in the embed title too.
	title := escapeMD(m.Title)
	content := "**" + title + "**"
	if m.Mention && d.RoleID != "" {
		content = "<@&" + d.RoleID + "> " + content
		p.AllowedMentions.Roles = []string{d.RoleID}
	}
	p.Content = clip(content, maxContent)

	var desc strings.Builder
	budget := maxDescription - 40
	if s := m.Summary; s != "" {
		if m.Kind == KindNotice {
			// The lines of a settings notice have no formatting of their
			// own, but names and labels from Syncthing: each line is
			// escaped. The line breaks are syncwatch's (see OneLine).
			s = escapeLines(s)
		}
		desc.WriteString(clipBytes(s, budget))
	}
	for _, s := range m.Sections {
		if len(s.Lines) == 0 {
			continue
		}
		head := "\n\n**" + s.Title + "**"
		if desc.Len() == 0 {
			head = "**" + s.Title + "**"
		}
		if desc.Len()+len(head) > budget {
			break
		}
		desc.WriteString(head)
		for i, l := range s.Lines {
			line := "\n" + l
			if desc.Len()+len(line) > budget {
				fmt.Fprintf(&desc, "\n"+moreLine, hiddenLines(s.Lines[i:]))
				break
			}
			desc.WriteString(line)
		}
	}
	e := discordEmbed{
		Title:       clip(title, maxTitle),
		URL:         m.URL,
		Description: desc.String(),
		Color:       m.Color,
		Footer:      &discordText{Text: "syncwatch"},
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	}
	// Keep the whole message under Discord's total embed size.
	if total := len(e.Title) + len(e.Description) + len(e.Footer.Text); total > maxEmbedTotal {
		e.Description = clipBytes(e.Description, maxEmbedTotal-len(e.Title)-len(e.Footer.Text)-10)
	}
	p.Embeds = []discordEmbed{e}
	return p
}

// hiddenLines counts the entries behind lines that didn't fit. A trailing
// "…and N more" from limitLines stands for N of them, not for one.
func hiddenLines(rest []string) int {
	n := len(rest)
	var k int
	if _, err := fmt.Sscanf(rest[len(rest)-1], moreLine, &k); err == nil {
		n += k - 1
	}
	return n
}

// usable checks the stored URL again at send time: a webhook saved while
// SYNCWATCH_ALLOW_ANY_WEBHOOK was set mustn't keep getting messages after
// the variable is gone. The error leaves out the URL, which has the token.
func (d *Discord) usable() error {
	if config.AllowAnyWebhook {
		return nil
	}
	if u, err := url.Parse(d.URL); err != nil || u.Scheme != "https" || !config.DiscordHost(d.URL) {
		return unusableError("Discord: the stored webhook is not a Discord webhook URL (https://discord.com/api/webhooks/…). Save the webhook URL again.")
	}
	return nil
}

// Send implements Sender. It retries once on rate limiting.
func (d *Discord) Send(ctx context.Context, m Message) error {
	if err := d.usable(); err != nil {
		return err
	}
	body, err := json.Marshal(d.Payload(m))
	if err != nil {
		return err
	}
	hc := &http.Client{Timeout: 20 * time.Second}
	if d.HTTP != nil {
		c := *d.HTTP
		hc = &c
	}
	// A redirect would hand the message, token and all, to wherever the
	// webhook points. It is reported instead.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL, bytes.NewReader(body))
		if err != nil {
			return unusableError("Discord: the webhook URL is not valid")
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			return fmt.Errorf("Discord: %v", redactURL(err.Error(), d.URL))
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		if resp.StatusCode >= 300 && resp.StatusCode < 400 {
			return &httpError{code: resp.StatusCode, text: "the webhook answers with a redirect, which syncwatch does not follow"}
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			wait := 2 * time.Second
			var rl struct {
				RetryAfter float64 `json:"retry_after"`
			}
			if json.Unmarshal(respBody, &rl) == nil && rl.RetryAfter > 0 {
				wait = time.Duration(rl.RetryAfter * float64(time.Second))
			} else if s := resp.Header.Get("Retry-After"); s != "" {
				if n, err := strconv.ParseFloat(s, 64); err == nil {
					wait = time.Duration(n * float64(time.Second))
				}
			}
			if wait > 30*time.Second {
				wait = 30 * time.Second
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		if !config.DiscordHost(d.URL) {
			// A test setup's fake Discord, or whatever answers there: don't
			// relay its replies to the settings page.
			return &httpError{code: resp.StatusCode}
		}
		return &httpError{code: resp.StatusCode, text: discordError(respBody)}
	}
	return fmt.Errorf("Discord: too many requests (HTTP 429) after 3 tries")
}

// httpError is a reply other than 2xx, with Discord's explanation when the
// host is Discord's.
type httpError struct {
	code int
	text string
}

func (e *httpError) Error() string {
	if e.text == "" {
		return fmt.Sprintf("Discord: HTTP %d", e.code)
	}
	return fmt.Sprintf("Discord: HTTP %d: %s", e.code, e.text)
}

// rejected says whether a reply won't change on a retry: a 4xx other than
// 429 blames the message or the webhook itself.
func rejected(err error) bool {
	var e *httpError
	return errors.As(err, &e) && e.code >= 400 && e.code < 500 && e.code != http.StatusTooManyRequests
}

// invalidMessage says whether Discord refuses the message itself (HTTP 400):
// no channel accepts it. 401, 403 and 404 blame the webhook.
func invalidMessage(err error) bool {
	var e *httpError
	return errors.As(err, &e) && e.code == http.StatusBadRequest
}

// unusableError is a webhook that syncwatch cannot use until an
// administrator saves it again: a retry cannot help.
type unusableError string

func (e unusableError) Error() string { return string(e) }

// transient says whether a failure can end without a change of the
// webhook: Discord or the network is down (HTTP 5xx or 429, a timeout, no
// connection). A refused, redirected or unusable webhook cannot.
func transient(err error) bool {
	var e *httpError
	if errors.As(err, &e) {
		return e.code >= 500 || e.code == http.StatusTooManyRequests
	}
	var u unusableError
	return !errors.As(err, &u)
}

// discordError is the readable part of a Discord error reply: its message
// ("Unknown Webhook") rather than the JSON around it.
func discordError(body []byte) string {
	var e struct {
		Message string          `json:"message"`
		Errors  json.RawMessage `json:"errors"`
	}
	if json.Unmarshal(body, &e) == nil && e.Message != "" {
		msg := clip(e.Message, 200)
		// A 400 names the field at fault only in here.
		var errs bytes.Buffer
		if len(e.Errors) > 0 && json.Compact(&errs, e.Errors) == nil {
			msg += " " + clip(errs.String(), 200)
		}
		return msg
	}
	return clip(strings.TrimSpace(string(body)), 200)
}

// webhookPath is anything shaped like a webhook path, however an error
// spells it.
var webhookPath = regexp.MustCompile(`/api/(v\d+/)?webhooks/[^/\s"'?#]+/[^/\s"'?#]+`)

// redactURL removes the webhook URL (which contains its token) from error
// text. net/http prints the parsed URL, which can differ from the stored
// string (scheme case, escaped characters), so the id and token are also
// removed on their own, in both spellings, and wherever a webhook path shows.
func redactURL(s, u string) string {
	if u == "" {
		return s
	}
	s = strings.ReplaceAll(s, u, "<webhook>")
	if p, err := url.Parse(u); err == nil {
		s = strings.ReplaceAll(s, p.String(), "<webhook>")
		if segs := strings.Split(strings.Trim(p.Path, "/"), "/"); len(segs) >= 2 {
			id, token := segs[len(segs)-2], segs[len(segs)-1]
			s = strings.ReplaceAll(s, id+"/"+token, "<webhook>")
			s = strings.ReplaceAll(s, url.PathEscape(id)+"/"+url.PathEscape(token), "<webhook>")
		}
	}
	return webhookPath.ReplaceAllString(s, "/api/webhooks/<webhook>")
}
