// Package notify sends urgent pings, resolved messages, daily reminders and
// the weekly report to Discord (native webhook with embeds).
package notify

import (
	"fmt"
	"strings"

	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/store"
)

// Message kinds.
const (
	KindUrgent    = "urgent"
	KindResolved  = "resolved"
	KindReminder  = "reminder"
	KindOvernight = "overnight"
	KindWeekly    = "weekly"
	KindTest      = "test"
	KindNotice    = "notice" // a security-relevant settings change
)

// Embed colours.
const (
	ColorError    = 0xD93025
	ColorWarn     = 0xF29900
	ColorInfo     = 0x1A73E8
	ColorResolved = 0x188038
)

// Message is one notification, rendered per channel.
type Message struct {
	Kind     string
	Mention  bool
	Title    string
	Summary  string
	Color    int
	Sections []Section
	URL      string // dashboard link
	// Key identifies a scheduled message across retries (its text can
	// change, e.g. "open 3 hours"), so a channel that already got it isn't
	// sent it again. Empty for one-off messages.
	Key string
}

// Section is a titled list of lines.
type Section struct {
	Title string
	Lines []string
}

// Icon returns a severity marker that doesn't rely on colour alone.
func Icon(s model.Severity) string {
	switch s {
	case model.Error:
		return "🔴"
	case model.Warn:
		return "🟠"
	default:
		return "🔵"
	}
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// escapeMD stops Discord from interpreting markdown in folder and device
// names, paths and other text syncwatch doesn't control (an unknown device
// picks its own name): no formatting, masked links or headings.
var mdEscaper = strings.NewReplacer(`\`, `\\`, "*", `\*`, "_", `\_`, "~", `\~`, "`", "\\`", "|", `\|`, ">", `\>`,
	"<", `\<`, "[", `\[`, "]", `\]`, "(", `\(`, ")", `\)`, "#", `\#`, "@", "@​")

func escapeMD(s string) string { return mdEscaper.Replace(s) }

func findingLine(r *store.Record, url string) string {
	checks := strings.Join(r.Checks, "+")
	line := fmt.Sprintf("%s **%s** %s", Icon(r.Severity), checks, escapeMD(clip(r.Message, 280)))
	if r.Stale {
		line += " _(server unreachable, last known state)_"
	}
	if url != "" {
		line += fmt.Sprintf(" — [details](%s/problems#f-%s)", strings.TrimRight(url, "/"), r.ID)
	}
	return line
}

func resolvedLine(r *store.Record) string {
	return fmt.Sprintf("✅ **%s** %s", strings.Join(r.Checks, "+"), escapeMD(clip(r.Message, 280)))
}

func historyLine(h store.HistoryEntry, resolved bool) string {
	icon := Icon(h.Severity)
	if resolved {
		icon = "✅"
	}
	return fmt.Sprintf("%s **%s** %s", icon, h.Check, escapeMD(clip(h.Message, 200)))
}

func colorFor(rs []*store.Record) int {
	c := ColorInfo
	for _, r := range rs {
		switch r.Severity {
		case model.Error:
			return ColorError
		case model.Warn:
			c = ColorWarn
		}
	}
	return c
}

// limitLines keeps at most n lines and adds "…and N more".
func limitLines(lines []string, n int) []string {
	if len(lines) <= n {
		return lines
	}
	out := append([]string(nil), lines[:n]...)
	return append(out, fmt.Sprintf("…and %d more", len(lines)-n))
}
