// Package notify sends urgent pings, resolved messages, daily reminders and
// the weekly report to Discord (native webhook with embeds).
package notify

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

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
	ColorNotice   = 0x80868B // grey: a settings notice is not a finding
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

// clipBytes is clip for a byte budget. Discord counts characters, so a
// byte count stays on the safe side.
func clipBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for i := n - len("…"); i > 0; i-- {
		if utf8.RuneStart(s[i]) {
			return s[:i] + "…"
		}
	}
	return ""
}

// escapeMD stops Discord from interpreting markdown in folder and device
// names, paths and other text syncwatch doesn't control (an unknown device
// picks its own name): no formatting, masked links or headings, no mentions
// or bare links, and no line breaks, list markers or bidi controls that
// would change the text around the name.
var mdEscaper = strings.NewReplacer(`\`, `\\`, "*", `\*`, "_", `\_`, "~", `\~`, "`", "\\`", "|", `\|`, ">", `\>`,
	"<", `\<`, "[", `\[`, "]", `\]`, "(", `\(`, ")", `\)`, "#", `\#`, "@", "@"+zwsp)

var (
	zwsp     = string(rune(0x200b)) // zero-width space
	bareLink = regexp.MustCompile(`[A-Za-z][A-Za-z0-9+.-]*://`)
	listItem = regexp.MustCompile(`^(\d+)\.(\s)`)
)

func escapeMD(s string) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == 0x2028 || r == 0x2029 || unicode.IsControl(r):
			return ' ' // line breaks and other controls
		case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			return -1 // bidi overrides, embeddings and isolates
		}
		return r
	}, strings.ReplaceAll(s, "\r\n", "\n"))
	s = mdEscaper.Replace(s)
	// A zero-width space after the scheme's colon stops the autolink.
	s = bareLink.ReplaceAllStringFunc(s, func(u string) string { return strings.TrimSuffix(u, "//") + zwsp + "//" })
	if strings.HasPrefix(s, "- ") || strings.HasPrefix(s, "+ ") {
		s = `\` + s
	}
	return listItem.ReplaceAllString(s, `$1\.$2`)
}

func findingLine(r *store.Record, url string) string {
	checks := strings.Join(r.Checks, "+")
	line := fmt.Sprintf("%s **%s** %s", Icon(r.Severity), checks, escapeMD(clip(r.Message, 280)))
	if r.Stale {
		line += " _(server unreachable, last known state)_"
	}
	return line + detailsLink(url, r.ID)
}

// detailsLink links a finding on the dashboard's Findings page.
func detailsLink(url, id string) string {
	if url == "" {
		return ""
	}
	return fmt.Sprintf(" — [details](%s/problems#f-%s)", strings.TrimRight(url, "/"), id)
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

// moreLine is the line that stands for lines left out.
const moreLine = "…and %d more"

// limitLines keeps at most n lines and adds "…and N more".
func limitLines(lines []string, n int) []string {
	if len(lines) <= n {
		return lines
	}
	out := append([]string(nil), lines[:n]...)
	return append(out, fmt.Sprintf(moreLine, len(lines)-n))
}
