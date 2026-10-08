package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// Discord posts to a Discord webhook.
type Discord struct {
	URL    string
	RoleID string
	HTTP   *http.Client
}

// Name implements Sender.
func (d *Discord) Name() string { return "discord" }

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
	// of their own. The embed title renders no links, so it stays as is.
	content := "**" + escapeMD(m.Title) + "**"
	if m.Mention && d.RoleID != "" {
		content = "<@&" + d.RoleID + "> " + content
		p.AllowedMentions.Roles = []string{d.RoleID}
	}
	p.Content = clip(content, maxContent)

	var desc strings.Builder
	if m.Summary != "" {
		desc.WriteString(m.Summary)
	}
	budget := maxDescription - 40
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
				fmt.Fprintf(&desc, "\n…and %d more", len(s.Lines)-i)
				break
			}
			desc.WriteString(line)
		}
	}
	e := discordEmbed{
		Title:       clip(m.Title, maxTitle),
		URL:         m.URL,
		Description: desc.String(),
		Color:       m.Color,
		Footer:      &discordText{Text: "syncwatch"},
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	}
	// Keep the whole message under Discord's total embed size.
	if total := len(e.Title) + len(e.Description) + len(e.Footer.Text); total > maxEmbedTotal {
		e.Description = clip(e.Description, maxEmbedTotal-len(e.Title)-len(e.Footer.Text)-10)
	}
	p.Embeds = []discordEmbed{e}
	return p
}

// Send implements Sender. It retries once on rate limiting.
func (d *Discord) Send(ctx context.Context, m Message) error {
	body, err := json.Marshal(d.Payload(m))
	if err != nil {
		return err
	}
	hc := d.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.URL, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("discord: invalid webhook URL")
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := hc.Do(req)
		if err != nil {
			return fmt.Errorf("discord: %v", redactURL(err.Error(), d.URL))
		}
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
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
			return fmt.Errorf("discord: HTTP %d", resp.StatusCode)
		}
		return fmt.Errorf("discord: HTTP %d: %s", resp.StatusCode, discordError(respBody))
	}
	return fmt.Errorf("discord: still rate limited")
}

// discordError is the readable part of a Discord error reply: its message
// ("Unknown Webhook") rather than the JSON around it.
func discordError(body []byte) string {
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) == nil && e.Message != "" {
		return clip(e.Message, 200)
	}
	return clip(strings.TrimSpace(string(body)), 200)
}

// redactURL removes the webhook URL (which contains its token) from error text.
func redactURL(s, u string) string {
	if u == "" {
		return s
	}
	return strings.ReplaceAll(s, u, "<webhook>")
}
