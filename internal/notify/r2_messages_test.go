package notify

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/engine"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/store"
)

// twoNAS is a configuration with AKKA and SOL in Stockholm time.
func twoNAS() *config.Config {
	cfg := &config.Config{Timezone: "Europe/Stockholm", PublicURL: "http://sw.local:8080",
		Servers: []config.Server{{ID: "akka", Name: "AKKA"}, {ID: "sol", Name: "SOL"}}}
	cfg.ApplyDefaults()
	return cfg
}

// A resolved message says what is fixed in the resolved form of the finding,
// and how long the fault lasted from its start to the resolution, with the
// clock times. A finding of an older version without the resolved form says
// "Resolved:" and its message. A finding that resolved while syncwatch did
// not run gets that time instead of an end (W-038, admin-anna AA-10, AA-25).
func TestResolvedMessageSaysWhatIsFixed(t *testing.T) {
	cfg := twoNAS()
	h1 := func() *store.Record {
		return &store.Record{Finding: model.Finding{ID: "h1", Check: "H1", Checks: []string{"H1"}, Severity: model.Error, Server: "sol", Subject: "SOL",
			Message: "Server SOL is unreachable: no route to host", ResolvedText: "Server SOL can be reached again.", Urgent: true},
			Since: local(2026, 10, 6, 21, 10), OpenedAt: local(2026, 10, 6, 21, 11), PingedAt: local(2026, 10, 6, 21, 16), ResolvedAt: local(2026, 10, 6, 21, 17)}
	}
	line := func(rs []*store.Record, gaps []store.Gap) string {
		t.Helper()
		m := ResolvedMessage(cfg, rs, gaps)
		if len(m.Sections) != 1 || len(m.Sections[0].Lines) != 1 {
			t.Fatalf("resolved message: %+v", m)
		}
		return m.Sections[0].Lines[0]
	}
	r := h1()
	if got, want := line([]*store.Record{r}, nil), "✅ **H1** Server SOL can be reached again (the fault lasted 7 minutes, 21:10–21:17)"; got != want {
		t.Errorf("resolved line:\n got %s\nwant %s", got, want)
	}
	if m := ResolvedMessage(cfg, []*store.Record{r}, nil); m.Title != "✅ Resolved: Server unreachable: SOL" {
		t.Errorf("title: %s", m.Title)
	}
	r.ResolvedText = ""
	if got, want := line([]*store.Record{r}, nil), `✅ **H1** Resolved: Server SOL is unreachable: no route to host (the fault lasted 7 minutes, 21:10–21:17)`; got != want {
		t.Errorf("record without the resolved form:\n got %s\nwant %s", got, want)
	}
	// Since after the opening (it is not known better): from the opening.
	r = h1()
	r.Since = local(2026, 10, 6, 21, 12)
	if got := line([]*store.Record{r}, nil); !strings.Contains(got, "(the fault lasted 6 minutes, 21:11–21:17)") {
		t.Errorf("from the opening: %s", got)
	}
	// syncwatch did not run from 21:04 to 21:24; the finding resolved at
	// the first evaluation after the start.
	r = h1()
	r.Since, r.OpenedAt, r.PingedAt, r.ResolvedAt = local(2026, 10, 6, 20, 50), local(2026, 10, 6, 20, 51), local(2026, 10, 6, 20, 56), local(2026, 10, 6, 21, 25)
	gap := store.Gap{From: local(2026, 10, 6, 21, 4), To: local(2026, 10, 6, 21, 24)}
	want := "✅ **H1** Server SOL can be reached again (the fault started at 20:50 and was resolved while syncwatch did not run, 21:04–21:24)"
	if got := line([]*store.Record{r}, []store.Gap{gap}); got != want {
		t.Errorf("resolved in a gap:\n got %s\nwant %s", got, want)
	}
	// A gap that ended more than 5 minutes before the resolution, or that
	// began before the finding opened, does not change the line.
	for _, g := range []store.Gap{
		{From: local(2026, 10, 6, 21, 4), To: local(2026, 10, 6, 21, 19)},
		{From: local(2026, 10, 6, 20, 40), To: local(2026, 10, 6, 21, 24)},
	} {
		if got := line([]*store.Record{r}, []store.Gap{g}); !strings.Contains(got, "(the fault lasted 35 minutes, 20:50–21:25)") {
			t.Errorf("gap %s–%s: %s", g.From.Format("15:04"), g.To.Format("15:04"), got)
		}
	}
	// The overnight summary uses the same form; a fault that began on the
	// day before has the date at its start.
	r = h1()
	r.Since, r.OpenedAt = local(2026, 10, 5, 23, 50), local(2026, 10, 5, 23, 51)
	m := OvernightMessage(cfg, nil, []*store.Record{r}, nil, nil, nil)
	if got := m.Sections[1].Lines; len(got) != 1 || !strings.HasSuffix(got[0], "(the fault lasted 21 hours, 2026-10-05 23:50–21:17)") {
		t.Errorf("overnight resolved line: %q", got)
	}
}

// h5 is the H5 finding of the PC PC-1 for LIVE-FJORD-4FINALS, seen from a
// server.
func h5(id, server, name, pct string) *store.Record {
	return &store.Record{Finding: model.Finding{ID: id, Check: "H5", Checks: []string{"H5"}, Severity: model.Warn, Server: server, Urgent: true,
		Subject:      "LIVE-FJORD-4FINALS on PC-1",
		Message:      fmt.Sprintf("Device PC-1 is stuck at %s%% of folder LIVE-FJORD-4FINALS for more than 24 hours (18 remaining items, seen from %s)", pct, name),
		ResolvedText: fmt.Sprintf("Device PC-1 has all files of folder LIVE-FJORD-4FINALS (seen from %s).", name)},
		OpenedAt: local(2026, 10, 6, 8, 0), PingedAt: local(2026, 10, 6, 8, 5), ResolvedAt: local(2026, 10, 6, 11, 0)}
}

// The views of one PC from AKKA and SOL are one line in every message, also
// with different percentages: "… _(also seen from SOL)_". The findings stay
// separate (dashboard, API). Two servers that are down stay 2 lines, and a
// section title counts the lines (W-046b, editor-shared-pc ESP-13).
func TestViewsOfOnePCShareOneLine(t *testing.T) {
	cfg := twoNAS()
	now := local(2026, 10, 6, 12, 0)
	a, b := h5("a", "akka", "AKKA", "99.9"), h5("b", "sol", "SOL", "97.5")
	both := []*store.Record{a, b}
	one := func(what string, lines []string, want string) {
		t.Helper()
		if len(lines) != 1 || !strings.Contains(lines[0], want) {
			t.Errorf("%s: %q", what, lines)
		}
	}
	urgent := UrgentMessage(cfg, both)
	one("urgent", urgent.Sections[0].Lines, "(18 remaining items, seen from AKKA\\) _(also seen from SOL)_ — [details](http://sw.local:8080/problems#f-a)")
	if urgent.Title != "🚨 2 new urgent findings on AKKA, SOL: H5" {
		t.Errorf("urgent title: %s", urgent.Title)
	}
	one("repeat", RepeatMessage(cfg, both, now).Sections[0].Lines, "seen from AKKA\\) _(also seen from SOL)_ (open 4 hours) — [details]")
	one("reminder", ReminderMessage(cfg, both, now).Sections[0].Lines, "_(also seen from SOL)_ (open 4 hours)")
	one("resolved", ResolvedMessage(cfg, both, nil).Sections[0].Lines, `\(seen from AKKA\) _(also seen from SOL)_ (the fault lasted 3 hours, 08:00–11:00)`)
	night := OvernightMessage(cfg, both, both, nil, nil, nil)
	if night.Sections[0].Title != "New and still open (1)" || night.Sections[1].Title != "Resolved during the night (1)" {
		t.Errorf("overnight sections: %+v", night.Sections)
	}
	hist := func(r *store.Record) store.HistoryEntry {
		return store.HistoryEntry{FindingID: r.ID, Check: r.Check, Severity: r.Severity, Server: r.Server, Subject: r.Subject, Message: r.Message,
			Urgent: true, OpenedAt: r.OpenedAt, ResolvedAt: r.ResolvedAt}
	}
	v := &engine.View{Snapshot: &model.Snapshot{}, Open: both, Snoozes: map[string]store.Snooze{}}
	weekly := WeeklyMessage(cfg, v, []store.HistoryEntry{hist(a), hist(b)}, now)
	if s := weekly.Sections; s[0].Title != "New this week (1)" || s[1].Title != "Resolved this week (1)" || len(s[2].Lines) != 1 {
		t.Errorf("weekly sections: %+v", s)
	}
	one("weekly new", weekly.Sections[0].Lines, "seen from AKKA\\) _(also seen from SOL)_")
	one("catch-up", MissedMessage(cfg, nil, []store.HistoryEntry{hist(a), hist(b)}, now.Add(-time.Hour), now).Sections[0].Lines, "_(also seen from SOL)_ (08:00–11:00)")

	// Two servers that are down are 2 faults: 2 lines everywhere.
	h1 := func(id, server, name string) *store.Record {
		return &store.Record{Finding: model.Finding{ID: id, Check: "H1", Checks: []string{"H1"}, Severity: model.Error, Server: server, Subject: name,
			Message: "Server " + name + " is unreachable: connection refused", Urgent: true}, OpenedAt: now.Add(-time.Hour), ResolvedAt: now}
	}
	down := []*store.Record{h1("x", "akka", "AKKA"), h1("y", "sol", "SOL")}
	weekly = WeeklyMessage(cfg, &engine.View{Snapshot: &model.Snapshot{}, Snoozes: map[string]store.Snooze{}}, []store.HistoryEntry{hist(down[0]), hist(down[1])}, now)
	if weekly.Sections[0].Title != "New this week (2)" || len(ResolvedMessage(cfg, down, nil).Sections[0].Lines) != 2 || len(ReminderMessage(cfg, down, now).Sections[0].Lines) != 2 {
		t.Errorf("2 servers down: %+v", weekly.Sections)
	}
	// 2 occurrences of one finding on one server stay 2 lines.
	again := hist(a)
	again.OpenedAt = now.Add(-2 * time.Hour)
	if lines := historyLines(cfg, []store.HistoryEntry{hist(a), again}, false); len(lines) != 2 {
		t.Errorf("2 occurrences: %q", lines)
	}
	// A finding of no server (H7, X1) has its own line.
	h7 := func(id string) *store.Record {
		return &store.Record{Finding: model.Finding{ID: id, Check: "H7", Checks: []string{"H7"}, Severity: model.Warn, Related: []string{"akka", "sol"},
			Subject: "PC-1", Message: "PC PC-1 is offline for 8 days"}}
	}
	if lines := recordLines(cfg, []*store.Record{h7("p"), h7("q")}, now, 0); len(lines) != 2 {
		t.Errorf("findings of no server: %q", lines)
	}
}

// Urgent, repeat and reminder lines have the first advice row of the finding
// (Possible causes, What to check, What to do) as a second line: people at
// the other site read the cause without the dashboard (W-057c). The other
// messages do not, and the advice is cut to 200 characters.
func TestAdviceRowUnderTheFindingLine(t *testing.T) {
	cfg := twoNAS()
	now := local(2026, 10, 6, 12, 0)
	causes := "The server is off or does not answer, or the network path to it (for example the VPN between the sites) is down."
	r := &store.Record{Finding: model.Finding{ID: "h1", Check: "H1", Checks: []string{"H1"}, Severity: model.Error, Server: "sol", Subject: "SOL", Urgent: true,
		Message: "Server SOL is unreachable: no route to host", Details: []model.Detail{{Key: "URL", Value: "https://sol.invalid:8384"}, {Key: "Possible causes", Value: causes}}},
		OpenedAt: now.Add(-2 * time.Hour), PingedAt: now.Add(-2 * time.Hour), ResolvedAt: now}
	want := "\n↳ Possible causes: The server is off or does not answer, or the network path to it \\(for example the VPN between the sites\\) is down."
	for name, m := range map[string]Message{
		"urgent":   UrgentMessage(cfg, []*store.Record{r}),
		"repeat":   RepeatMessage(cfg, []*store.Record{r}, now),
		"reminder": ReminderMessage(cfg, []*store.Record{r}, now),
	} {
		if lines := m.Sections[0].Lines; len(lines) != 1 || !strings.HasSuffix(lines[0], ")"+want) {
			t.Errorf("%s: %q", name, lines)
		}
	}
	for name, m := range map[string]Message{
		"resolved":  ResolvedMessage(cfg, []*store.Record{r}, nil),
		"overnight": OvernightMessage(cfg, []*store.Record{r}, []*store.Record{r}, nil, []*store.Record{r}, nil),
		"weekly":    WeeklyMessage(cfg, &engine.View{Snapshot: &model.Snapshot{}, Open: []*store.Record{r}, Snoozes: map[string]store.Snooze{}}, nil, now),
	} {
		if strings.Contains(messageText(m), "↳") {
			t.Errorf("%s has the advice: %s", name, messageText(m))
		}
	}
	// The first advice row in the order of the details; an empty one is not
	// advice. Long advice is cut.
	r.Details = []model.Detail{{Key: "What to check", Value: " "}, {Key: "What to do", Value: strings.Repeat("Type the API key again. ", 20)}, {Key: "Possible causes", Value: causes}}
	line := UrgentMessage(cfg, []*store.Record{r}).Sections[0].Lines[0]
	advice := line[strings.Index(line, "\n↳ What to do: ")+len("\n↳ What to do: "):]
	if n := len([]rune(advice)); n != maxAdvice || !strings.HasSuffix(advice, "…") || strings.Contains(line, "Possible causes") {
		t.Errorf("cut advice (%d characters): %q", n, line)
	}
	r.Details = nil
	if line := UrgentMessage(cfg, []*store.Record{r}).Sections[0].Lines[0]; strings.Contains(line, "\n") {
		t.Errorf("a finding without advice: %q", line)
	}
}

// With an advice row under every line, a full urgent message still fits in
// Discord's limits, and "…and N more" counts the findings left out.
func TestAdviceRowsStayWithinDiscordLimits(t *testing.T) {
	cfg := twoNAS()
	var rs []*store.Record
	for i := range 30 {
		rs = append(rs, &store.Record{Finding: model.Finding{ID: fmt.Sprintf("%016x", i), Check: "H4", Checks: []string{"H4"}, Severity: model.Error,
			Server: "sol", Subject: fmt.Sprintf("F%d on SOL", i), Message: fmt.Sprintf("F%d ", i) + strings.Repeat("é", 300), Urgent: true,
			Details: []model.Detail{{Key: "What to check", Value: strings.Repeat("ü ", 300)}}}})
	}
	for _, m := range []Message{UrgentMessage(cfg, rs), RepeatMessage(cfg, rs, local(2026, 10, 6, 12, 0)), ReminderMessage(cfg, rs, local(2026, 10, 6, 12, 0))} {
		p := (&Discord{RoleID: "42"}).Payload(m)
		e := p.Embeds[0]
		if len(e.Description) > maxDescription || len(e.Title)+len(e.Description)+len(e.Footer.Text) > maxEmbedTotal || len([]rune(p.Content)) > maxContent {
			t.Fatalf("%s: over the limits: %d bytes", m.Kind, len(e.Description))
		}
		var shown, more int
		for _, l := range strings.Split(e.Description, "\n") {
			if strings.HasPrefix(l, "🔴 **H4**") {
				shown++
			}
			if strings.HasPrefix(l, "…and ") {
				fmt.Sscanf(l, "…and %d more", &more)
			}
		}
		if shown == 0 || shown+more != 30 {
			t.Errorf("%s: %d shown, %d more", m.Kind, shown, more)
		}
	}
}

// W-038 (live re-test of wave 3, P1): a finding that an evaluation after
// the end of a time when syncwatch did not run saw open did not resolve in
// that time. Its resolved line gives the duration of the fault.
func TestResolvedLineOfAFaultSeenAfterTheGap(t *testing.T) {
	cfg := twoNAS()
	gap := store.Gap{From: local(2026, 10, 11, 23, 38), To: local(2026, 10, 11, 23, 41)}
	r := &store.Record{Finding: model.Finding{ID: "h4", Check: "H4", Checks: []string{"H4"}, Severity: model.Error, Server: "akka", Urgent: true,
		Subject: "LIVE-AURORA-1SOURCE on AKKA", Message: "Folder LIVE-AURORA-1SOURCE on AKKA cannot sync: folder error: folder marker missing",
		ResolvedText: "Folder LIVE-AURORA-1SOURCE on AKKA can sync again."},
		Since: local(2026, 10, 11, 23, 23), OpenedAt: local(2026, 10, 11, 23, 23), PingedAt: local(2026, 10, 11, 23, 24),
		LastSeen: gap.To.Add(68 * time.Second), ResolvedAt: gap.To.Add(73 * time.Second)}
	line := func() string {
		t.Helper()
		m := ResolvedMessage(cfg, []*store.Record{r}, []store.Gap{gap})
		if len(m.Sections) != 1 || len(m.Sections[0].Lines) != 1 {
			t.Fatalf("resolved message: %+v", m)
		}
		return m.Sections[0].Lines[0]
	}
	want := "✅ **H4** Folder LIVE-AURORA-1SOURCE on AKKA can sync again (the fault lasted 19 minutes, 23:23–23:42)"
	if got := line(); got != want {
		t.Errorf("seen open after the start:\n got %s\nwant %s", got, want)
	}
	// The last evaluation that saw it open was before the stop: it resolved
	// while syncwatch did not run.
	r.LastSeen = gap.From.Add(-20 * time.Second)
	want = "✅ **H4** Folder LIVE-AURORA-1SOURCE on AKKA can sync again (the fault started at 23:23 and was resolved while syncwatch did not run, 23:38–23:41)"
	if got := line(); got != want {
		t.Errorf("not seen open after the start:\n got %s\nwant %s", got, want)
	}
}
