package notify

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/rules"
	"github.com/ivarstudios/syncwatch/internal/store"
)

// The lines of the messages. One rule merges lines in every list (urgent,
// repeat, reminder, resolved, overnight, catch-up and weekly): the same
// finding on more than one server is one line, "… _(also on SOL)_". The
// findings stay separate on the dashboard and in the API. Two findings (or
// occurrences in the history) share a line when they have the same checks,
// severity and stale flag, are on different servers, and
//   - have the same subject, which does not name their server: the views of
//     one device from each server (H5 and H6 of a PC). Their messages can
//     differ (the percentage, "seen from AKKA"); the line says "also seen
//     from SOL".
//   - or have a subject and a message that are the same apart from the name
//     of their own server: an S3 for a directory that both servers sync.
// A finding about a server itself (H1: the subject is the server name), and
// a finding of no server or of several (H2, X1), has its own line. Syncthing
// does not make device names unique: findings about a device (its device ID
// in the details) share a line only when it is the same device. An
// occurrence in the history has no device ID.

// mergeItem is what the merge rule compares of a finding or an occurrence.
type mergeItem struct {
	checks, server, subject, message string
	device                           string // the device ID of the details, if any
	severity                         model.Severity
	stale                            bool
}

func recordItems(rs []*store.Record) []mergeItem {
	out := make([]mergeItem, len(rs))
	for i, r := range rs {
		out[i] = mergeItem{checks: strings.Join(r.Checks, "+"), server: r.Server, subject: r.Subject, message: r.Message, severity: r.Severity, stale: r.Stale}
		for _, d := range r.Details {
			if d.Key == "Device ID" || d.Key == "Offered by" {
				out[i].device = d.Value
			}
		}
	}
	return out
}

func historyItems(hs []store.HistoryEntry) []mergeItem {
	out := make([]mergeItem, len(hs))
	for i, h := range hs {
		out[i] = mergeItem{checks: h.Check, server: h.Server, subject: h.Subject, message: h.Message, severity: h.Severity}
	}
	return out
}

// lineGroup is one line of a list: the item first, in the order of the
// list, and the names of the other servers that have the same finding.
type lineGroup struct {
	first int
	also  []string
	seen  bool // the views of a device: "also seen from"
}

// suffix names the other servers of a line.
func (g lineGroup) suffix() string {
	switch {
	case len(g.also) == 0:
		return ""
	case g.seen:
		return " _(also seen from " + escapeMD(andList(g.also)) + ")_"
	}
	return " _(also on " + escapeMD(andList(g.also)) + ")_"
}

// mergeLines applies the merge rule to a list. A line never names the same
// server twice.
func mergeLines(cfg *config.Config, items []mergeItem) []lineGroup {
	var out []lineGroup
	var on []map[string]bool
	byKey := map[string]int{}
	for i, it := range items {
		k, seen := mergeKey(cfg, it)
		if j, ok := byKey[k]; k != "" && ok && !on[j][it.server] {
			out[j].also = append(out[j].also, serverName(cfg, it.server))
			on[j][it.server] = true
			continue
		}
		if _, ok := byKey[k]; k != "" && !ok {
			byKey[k] = len(out)
		}
		out = append(out, lineGroup{first: i, seen: seen})
		on = append(on, map[string]bool{it.server: true})
	}
	return out
}

// mergeKey is what an item has in common with the same finding on another
// server, "" when it always has its own line. seen: the subject does not
// name the server.
func mergeKey(cfg *config.Config, it mergeItem) (key string, seen bool) {
	if it.server == "" {
		return "", false
	}
	name := regexp.MustCompile(`\b` + regexp.QuoteMeta(serverName(cfg, it.server)) + `\b`)
	subject := name.ReplaceAllString(it.subject, "\x01")
	if strings.TrimSpace(strings.ReplaceAll(subject, "\x01", "")) == "" {
		return "", false // about the server itself
	}
	head := fmt.Sprint(it.checks, "\x00", int(it.severity), "\x00", it.stale, "\x00", it.device, "\x00")
	if subject == it.subject {
		return head + "same\x00" + it.subject, true
	}
	return head + "named\x00" + subject + "\x00" + name.ReplaceAllString(it.message, "\x01"), false
}

// Options of recordLines.
const (
	withOpen   = 1 << iota // "(open 2 hours)": the time from the start of the fault
	withAdvice             // the first advice row of the finding as a second line
)

// recordLines writes a line for each finding (merged by mergeLines):
// "🔴 **H1** Server SOL is unreachable: timeout (open 2 hours) — [details](…)",
// and with withAdvice "↳ Possible causes: …" under it.
func recordLines(cfg *config.Config, rs []*store.Record, now time.Time, opts int) []string {
	url := dashboard(cfg)
	var out []string
	for _, g := range mergeLines(cfg, recordItems(rs)) {
		r := rs[g.first]
		s := findingText(r) + g.suffix()
		if opts&withOpen != 0 {
			s += " (open " + rules.Ago(now.Sub(started(r))) + ")"
		}
		s += detailsLink(url, r.ID)
		if opts&withAdvice != 0 {
			s += adviceLine(r)
		}
		out = append(out, s)
	}
	return out
}

// resolvedLines writes a line in the resolved form for each finding (merged
// by mergeLines): "✅ **H1** Server SOL can be reached again (the fault
// lasted 7 minutes, 21:10–21:17)".
func resolvedLines(cfg *config.Config, rs []*store.Record, gaps []store.Gap) []string {
	var out []string
	for _, g := range mergeLines(cfg, recordItems(rs)) {
		r := rs[g.first]
		out = append(out, fmt.Sprintf("✅ **%s** %s%s (%s)", strings.Join(r.Checks, "+"), escapeMD(clip(resolvedText(r), 280)), g.suffix(), lasted(cfg, r, gaps)))
	}
	return out
}

// historyLines writes a line for each occurrence (merged by mergeLines).
func historyLines(cfg *config.Config, hs []store.HistoryEntry, resolved bool) []string {
	var out []string
	for _, g := range mergeLines(cfg, historyItems(hs)) {
		out = append(out, historyLine(hs[g.first], resolved)+g.suffix())
	}
	return out
}

// blipLines writes a line for each occurrence that opened and resolved
// without a message, with its times (merged by mergeLines).
func blipLines(cfg *config.Config, hs []store.HistoryEntry) []string {
	loc := cfg.Location()
	var out []string
	for _, g := range mergeLines(cfg, historyItems(hs)) {
		h := hs[g.first]
		from, to := zoned(h.OpenedAt.In(loc).Format("15:04"), h.OpenedAt, h.ResolvedAt.In(loc).Format("15:04"), h.ResolvedAt, loc)
		out = append(out, historyLine(h, true)+g.suffix()+" ("+from+"–"+to+")")
	}
	return out
}

// started is when the fault of a finding began: its Since when the source
// data knows it and it is not after the opening, else the opening. The
// dashboard counts "open for" from the same time.
func started(r *store.Record) time.Time {
	if !r.Since.IsZero() && !r.Since.After(r.OpenedAt) {
		return r.Since
	}
	return r.OpenedAt
}

// resolvedText is the resolved form of a finding without its final period,
// or for a finding of an older version, which has none, "Resolved: " and
// its message.
func resolvedText(r *store.Record) string {
	if t := strings.TrimSpace(r.ResolvedText); t != "" {
		return strings.TrimSuffix(t, ".")
	}
	return "Resolved: " + r.Message
}

// resolveAfterGap: a finding that resolves less than this after the end of
// a time when syncwatch did not run (that began after it opened) resolved
// in that time, as far as syncwatch knows, when no evaluation after that
// time saw it open.
const resolveAfterGap = 5 * time.Minute

// lasted is the duration of a resolved fault from its start to the
// resolution, with the clock times: "the fault lasted 7 minutes,
// 21:10–21:17". When the fault ended while syncwatch did not run, the end
// is not known: "the fault started at 21:00 and was resolved while
// syncwatch did not run, 21:04–21:24". A fault that syncwatch saw open
// after its start did not end in that time: its LastSeen is after
// the end of the gap.
func lasted(cfg *config.Config, r *store.Record, gaps []store.Gap) string {
	loc := cfg.Location()
	from := started(r)
	for _, g := range gaps {
		if g.From.After(r.OpenedAt) && !g.To.After(r.ResolvedAt) && r.ResolvedAt.Sub(g.To) < resolveAfterGap && !r.LastSeen.After(g.To) {
			return "the fault started at " + clockOn(from, g.From, loc) + " and was resolved while syncwatch did not run, " + span(g.From, g.To, loc)
		}
	}
	return "the fault lasted " + rules.Ago(r.ResolvedAt.Sub(from)) + ", " + span(from, r.ResolvedAt, loc)
}

// span writes a time range in loc: "21:10–21:17", or with the date of the
// start when it is on another day: "2026-10-06 21:10–08:17", and with the
// zones in the night of a clock change: "02:15 CEST–02:15 CET".
func span(from, to time.Time, loc *time.Location) string {
	a, b := zoned(clockOn(from, to, loc), from, to.In(loc).Format("15:04"), to, loc)
	return a + "–" + b
}

// adviceKeys are the detail keys of advice (the rules put advice only
// there).
var adviceKeys = map[string]bool{"Possible causes": true, "What to check": true, "What to do": true}

// maxAdvice is the number of characters of advice in a line.
const maxAdvice = 200

// adviceLine is the first advice row of a finding, as a second line of its
// line: "\n↳ Possible causes: …"; "" when it has none. People who cannot
// open the dashboard (a VPN that is down) read the advice in Discord.
func adviceLine(r *store.Record) string {
	for _, d := range r.Details {
		if v := strings.TrimSpace(d.Value); adviceKeys[d.Key] && v != "" {
			return "\n↳ " + d.Key + ": " + escapeMD(clip(v, maxAdvice))
		}
	}
	return ""
}
