package web

import (
	"html"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
)

// notices returns the notices sent so far.
func (e *env) noticeList() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.notices...)
}

// checksForm fills the checks form with the current configuration.
func checksForm(cfg *config.Config, csrf string) url.Values {
	form := url.Values{"csrf": {csrf}}
	for _, c := range config.Catalog {
		if cfg.CheckEnabled(c.ID) {
			form.Set("enabled_"+c.ID, "on")
		}
		if cfg.Urgent(c.ID) {
			form.Set("urgent_"+c.ID, "on")
		}
		form.Set("debounce_"+c.ID, config.Duration(cfg.Debounce(c.ID)).String())
	}
	th := cfg.Thresholds
	for k, v := range map[string]config.Duration{"failing_files": th.FailingFiles, "stuck_sync": th.StuckSync, "paused": th.Paused, "pc_offline": th.PCOffline,
		"conflict_window": th.ConflictWindow, "inactive_project": th.InactiveProject, "old_device": th.OldDevice, "dead_share": th.DeadShare} {
		form.Set(k, v.String())
	}
	return form
}

// notifyForm fills the notifications form with the current configuration.
func notifyForm(cfg *config.Config, csrf string) url.Values {
	n := cfg.Notify
	return url.Values{"csrf": {csrf}, "discord_role_id": {n.DiscordRoleID}, "quiet_hours": {n.QuietHours}, "reminder_time": {n.ReminderTime},
		"weekly_day": {n.WeeklyDay}, "weekly_time": {n.WeeklyTime}, "weekly_top": {"10"}, "timezone": {cfg.Timezone}, "public_url": {cfg.PublicURL}}
}

func TestConfigChanges(t *testing.T) {
	base := "servers: [{name: AKKA, url: 'https://a:1'}, {name: SOL, url: 'https://b:1'}]\n"
	parse := func(y string) *config.Config {
		c, err := config.ParseYAML([]byte(y))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	for _, tc := range []struct {
		name, old, new string
		want           []string
	}{
		{"nothing", base, base, nil},
		{"server fields", base, "servers: [{name: Akka, id: akka, url: 'https://a:1', disabled: true, gui_url: 'http://a:2', allow_http: true}, {name: SOL, url: 'https://b:1'}]\n",
			[]string{"The server AKKA has the new name Akka.", "The server Akka is disabled.", "GUI URL of the server Akka changed from (none) to http://a:2.", "allow_http changed from false to true."}},
		{"server moved, added, removed", base, "servers: [{name: AKKA, url: 'https://a:2'}, {name: NEW, url: 'https://c:1'}]\n",
			[]string{"The API URL of the server AKKA changed from https://a:1 to https://a:2.", "The server NEW is added (API URL https://c:1).", "The server SOL is removed."}},
		{"checks both ways", base + "checks: {H1: {enabled: false}, S1: {debounce: 30d}}\n", base + "checks: {H4: {urgent: false}, S1: {urgent: true}}\n",
			[]string{"The check H1 (Server unreachable) is enabled.", "The check H4 (Folder sync stopped) does not send urgent messages now.", "The check S1 (Syncthing folder inside another) waits 5 minutes instead of 30 days before an urgent message."}},
		{"thresholds and collector", base, base + "thresholds: {pc_offline: 2d}\ncollector: {mode: polled, down_after_failures: 5}\n",
			[]string{"collector.down_after_failures changed from 2 to 5.", "collector.mode changed from events to polled.", "thresholds.pc_offline changed from 7d to 2d."}},
		{"notifications", base + "notifications: {discord_role_id: '420000000000000000'}\n", base + "notifications: {quiet_hours: 'off', weekly_top: 5}\ntimezone: Europe/Stockholm\npublic_url: 'http://dash.invalid'\n",
			[]string{"Urgent messages do not mention a role now (before: 420000000000000000).", "notifications.quiet_hours changed from 22:00-07:00 to off.", "notifications.weekly_top changed from 10 to 5.", "public_url changed from (none) to http://dash.invalid.", "timezone changed from Local to Europe/Stockholm."}},
		// share_type_from_path names the group that share_must_have refers to
		// (required since r0-g's placement validation).
		{"structure and shares", base, base + "structure: {project_pattern: '^LIVE-', ignore_dirs: ['@*'], share_type_from_path: '/(?P<me>me)?/', placement: [{when_name_matches: '^LIVE-', share_must_have: {me: 'false'}}]}\nshares: {add: {AKKA: [/x/LIVE-1]}}\n",
			[]string{"shares.add.AKKA changed from (none) to [/x/LIVE-1].", "structure.ignore_dirs changed from [.*, @*, #recycle, #snapshot, $RECYCLE.BIN, System Volume Information, lost+found] to [@*].",
				"structure.placement[0].share_must_have.me changed from (none) to false.", "structure.placement[0].when_name_matches changed from (none) to ^LIVE-.", "structure.project_pattern changed from (none) to ^LIVE-.", "structure.share_type_from_path changed from (none) to /(?P<me>me)?/."}},
		{"access", base, base + "access: {mode: proxy, viewer: open, session_ttl: 1d}\n",
			[]string{"Built-in login is off (proxy mode). Each person who can reach syncwatch is an administrator.", "access.session_ttl changed from 30d to 1d.", "Viewer access without the administrator password changed from administrator only to open, without login."}},
	} {
		got := configChanges(parse(tc.old), parse(tc.new))
		if len(got) != len(tc.want) {
			t.Errorf("%s: got %d lines %q, want %d", tc.name, len(got), got, len(tc.want))
			continue
		}
		for i := range got {
			if !strings.Contains(got[i], tc.want[i]) {
				t.Errorf("%s: line %d is %q, want %q", tc.name, i, got[i], tc.want[i])
			}
		}
	}
	// A secret in the file never appears.
	got := configChanges(parse(base), parse(base+"notifications: {discord_webhook: 'https://discord.com/api/webhooks/1/sekrit'}\napi: {token: swt_sekrit}\n"))
	if s := strings.Join(got, "\n"); strings.Contains(s, "sekrit") {
		t.Fatalf("secret in the changes: %q", s)
	}
}

// Every save that changes something sends one notice listing the changes
// with their old and new values; a save that changes nothing sends none.
func TestEverySettingsChangeIsAnnounced(t *testing.T) {
	e := adminEnv(t, twoServers)
	csrf := e.csrf("/settings")
	seen := len(e.noticeList())
	// expect checks that exactly one notice was sent since the last call and
	// that it mentions every want.
	expect := func(step string, want ...string) {
		t.Helper()
		all := e.noticeList()
		if len(all) != seen+1 {
			t.Fatalf("%s: %d notices, want 1: %q", step, len(all)-seen, all[seen:])
		}
		for _, w := range want {
			if !strings.Contains(all[seen], w) {
				t.Errorf("%s: notice %q lacks %q", step, all[seen], w)
			}
		}
		seen = len(all)
	}
	none := func(step string) {
		t.Helper()
		if all := e.noticeList(); len(all) != seen {
			t.Fatalf("%s: unexpected notices %q", step, all[seen:])
		}
	}
	post := func(path string, form url.Values) {
		t.Helper()
		form.Set("csrf", csrf)
		if msg, isErr := flash(e.post(path, form)); isErr {
			t.Fatalf("POST %s: %s", path, msg)
		}
	}

	post("/settings/servers", url.Values{"orig_id": {"akka"}, "name": {"AKKA"}, "url": {"https://a:1"}, "disabled": {"on"}})
	expect("disable", "Server AKKA changed", "The server AKKA is disabled.")
	post("/settings/servers", url.Values{"orig_id": {"akka"}, "name": {"Akka"}, "url": {"https://a:1"}, "gui_url": {"http://a.invalid:8384"}, "allow_http": {"on"}})
	expect("rename", "has the new name Akka", "GUI URL of the server Akka changed from (none) to http://a.invalid:8384.", "allow_http changed from false to true.")
	post("/settings/servers", url.Values{"orig_id": {"akka"}, "name": {"Akka"}, "url": {"https://a:1"}, "gui_url": {"http://a.invalid:8384"}, "allow_http": {"on"}})
	none("same server again")
	post("/settings/servers", url.Values{"orig_id": {"akka"}, "name": {"Akka"}, "url": {"https://a:1"}, "gui_url": {"http://a.invalid:8384"}, "allow_http": {"on"}, "api_key": {"k2"}})
	expect("key", "The API key is replaced.")

	cfg := e.cfg.Get()
	post("/settings/notifications", notifyForm(cfg, csrf))
	none("notifications unchanged")
	f := notifyForm(cfg, csrf)
	f.Set("quiet_hours", "")
	f.Set("reminder_time", "off")
	f.Set("weekly_day", "off")
	f.Set("weekly_top", "5")
	f.Set("timezone", "Europe/Stockholm")
	f.Set("public_url", "http://dash.invalid/")
	post("/settings/notifications", f)
	expect("notifications", "Notification settings changed", "notifications.quiet_hours changed from 22:00-07:00 to off.", "notifications.reminder_time changed from 08:00 to off.",
		"notifications.weekly_day changed from monday to off.", "notifications.weekly_top changed from 10 to 5.", "timezone changed from Local to Europe/Stockholm.", "public_url changed from (none) to http://dash.invalid.")

	cfg = e.cfg.Get()
	post("/settings/checks", checksForm(cfg, csrf))
	none("checks unchanged")
	f = checksForm(cfg, csrf)
	f.Set("pc_offline", "2d")
	f.Set("debounce_H1", "1m")
	f.Del("enabled_S7")
	post("/settings/checks", f)
	expect("checks", "Checks changed", "thresholds.pc_offline changed from 7d to 2d.", "The check H1 (Server unreachable) waits 60 seconds instead of 5 minutes", "S7 (")
	f.Set("enabled_S7", "on")
	post("/settings/checks", f)
	expect("check on again", "S7 (", "is enabled.")

	post("/settings/structure", url.Values{"yaml": {"structure: {project_pattern: '^LIVE-', ignore_dirs: ['@*']}"}})
	expect("structure", "Structure rules changed", "structure.project_pattern changed from (none) to ^LIVE-.", "structure.ignore_dirs changed from [.*, @*, #recycle, #snapshot, $RECYCLE.BIN, System Volume Information, lost+found] to [@*].")
	_, body := e.get("/settings")
	i := strings.Index(body, `name="yaml"`)
	j := strings.Index(body[i:], "</textarea>")
	post("/settings/structure", url.Values{"yaml": {html.UnescapeString(body[i+strings.Index(body[i:], ">")+1 : i+j])}})
	none("structure form round trip")

	post("/settings/collector", url.Values{"mode": {"polled"}, "event_timeout": {"50s"}, "full_check_interval": {"4h"}, "poll_interval": {"15m"},
		"structure_interval": {"1h"}, "baseline_interval": {"7d"}, "request_timeout": {"30s"}, "concurrency": {"3"}, "down_after_failures": {"5"}})
	expect("collector", "Collector settings changed", "collector.mode changed from events to polled.", "collector.down_after_failures changed from 2 to 5.")

	post("/settings/access", url.Values{"mode": {"builtin"}, "viewer": {"password"}, "viewer_password": {"viewer password 1"}, "confirm": {"correct horse battery"}})
	expect("access", "Access settings changed", "Viewer access without the administrator password changed from administrator only to with the viewer password.", "The new viewer password is set.")

	post("/settings/password", url.Values{"current": {"correct horse battery"}, "password": {"another long password"}, "password2": {"another long password"}})
	expect("password", "Administrator password changed")

	e.post("/settings/api-token", url.Values{"csrf": {csrf}, "op": {"generate"}})
	expect("token", "API token replaced")
	post("/settings/api-token", url.Values{"op": {"revoke"}})
	expect("revoke", "API token revoked")

	post("/settings/import", url.Values{"yaml": {"servers: [{name: Akka, id: akka, url: 'https://a:1', api_key: k3}, {name: SOL, url: 'https://b:1'}]\nthresholds: {paused: 1d}\napi: {token: swt_imported_token}\n"}})
	expect("import", "Configuration imported", "thresholds.paused changed from 3d to 1d.", "The API token is added.", "The API key of the server Akka is replaced.")
	if n := e.noticeList()[seen-1]; strings.Contains(n, "k3") || strings.Contains(n, "swt_imported") {
		t.Fatalf("import notice shows a secret: %q", n)
	}

	// Removing a server that doesn't exist changes nothing and says so.
	if msg, isErr := flash(e.post("/settings/servers/no-such-server/delete", url.Values{"csrf": {csrf}})); !isErr || msg != "The server does not exist. Reload the page." {
		t.Fatalf("delete unknown: %q %v", msg, isErr)
	}
	none("delete unknown")
}

// Snoozing or ignoring an urgent problem is announced, since nobody will be
// pinged about it; other problems are hidden quietly.
func TestHidingUrgentProblemsIsAnnounced(t *testing.T) {
	e := adminEnv(t, twoServers)
	csrf := e.csrf("/problems")
	var urgent, quiet string
	for _, r := range e.eng.View().Open {
		if r.Urgent && urgent == "" {
			urgent = r.ID
		}
		if !r.Urgent && quiet == "" {
			quiet = r.ID
		}
	}
	if urgent == "" || quiet == "" {
		t.Skipf("need an urgent and a non-urgent finding: %q %q", urgent, quiet)
	}
	e.post("/problems/"+quiet+"/ignore", url.Values{"csrf": {csrf}, "reason": {"known"}})
	if n := e.noticesText(); n != "" {
		t.Fatalf("non-urgent ignore announced: %q", n)
	}
	e.post("/problems/"+urgent+"/snooze", url.Values{"csrf": {csrf}, "for": {"1w"}})
	if n := e.noticesText(); !strings.Contains(n, "Urgent finding snoozed") || !strings.Contains(n, "H1") || !strings.Contains(n, "snoozed until") {
		t.Fatalf("urgent snooze: %q", n)
	}
	e.post("/problems/"+urgent+"/ignore", url.Values{"csrf": {csrf}, "reason": {"decommissioned"}})
	if n := e.noticesText(); !strings.Contains(n, "Urgent finding ignored") || !strings.Contains(n, "decommissioned") {
		t.Fatalf("urgent ignore: %q", n)
	}
}

// In proxy mode no admin password may exist. A file without an access
// section switches built-in login on, which would lock everyone out (the
// setup token is only logged at a start in built-in mode), so it is refused.
func TestImportCannotLockEveryoneOut(t *testing.T) {
	e := newEnv(t, twoServers+"access: {mode: proxy}\n")
	csrf := e.csrf("/settings")
	imp := func(yaml string) (string, bool) {
		return flash(e.post("/settings/import", url.Values{"csrf": {csrf}, "yaml": {yaml}}))
	}
	file := "servers: [{name: AKKA, url: 'https://a:1'}, {name: SOL, url: 'https://b:1'}]\n"
	if msg, isErr := imp(file); !isErr || !strings.Contains(msg, "administrator password") {
		t.Fatalf("import to built-in login without an admin password: %q %v", msg, isErr)
	}
	if e.cfg.Get().Access.Mode != "proxy" {
		t.Fatal("mode changed")
	}
	if resp, _ := e.get("/"); resp.StatusCode != 200 {
		t.Fatalf("dashboard after the refused import: %d", resp.StatusCode)
	}
	// With an admin password set (the password form works in proxy mode), the same file is fine.
	e.post("/settings/password", url.Values{"csrf": {csrf}, "password": {"correct horse battery"}, "password2": {"correct horse battery"}})
	if msg, isErr := imp(file); isErr {
		t.Fatalf("import with an admin password: %q", msg)
	}
	if e.cfg.Get().Access.Mode != "builtin" {
		t.Fatal("mode not changed")
	}
}

// YAML pasted into the Structure form without ignore_dirs keeps the current
// list; an explicit empty list clears it. The form shows the current list,
// so what it renders can be saved back unchanged.
func TestStructureFormKeepsIgnoreDirs(t *testing.T) {
	e := adminEnv(t, twoServers)
	csrf := e.csrf("/settings")
	save := func(yaml string) {
		t.Helper()
		if msg, isErr := flash(e.post("/settings/structure", url.Values{"csrf": {csrf}, "yaml": {yaml}})); isErr {
			t.Fatalf("structure %q: %s", yaml, msg)
		}
	}
	want := strings.Join(config.DefaultIgnoreDirs, ",")
	save("structure: {project_pattern: '^LIVE-'}")
	if got := strings.Join(e.cfg.Get().Structure.IgnoreDirs, ","); got != want {
		t.Fatalf("ignore_dirs after a save without the key: %q, want %q", got, want)
	}
	save("structure: {project_pattern: '^LIVE-', ignore_dirs: ['@*']}")
	if got := strings.Join(e.cfg.Get().Structure.IgnoreDirs, ","); got != "@*" {
		t.Fatalf("ignore_dirs after an explicit list: %q", got)
	}
	save("structure: {project_pattern: '^LIVE-'}")
	if got := strings.Join(e.cfg.Get().Structure.IgnoreDirs, ","); got != "@*" {
		t.Fatalf("ignore_dirs after another save without the key: %q", got)
	}
	save("structure: {project_pattern: '^LIVE-', ignore_dirs: []}")
	if got := e.cfg.Get().Structure.IgnoreDirs; len(got) != 0 {
		t.Fatalf("ignore_dirs after an explicit empty list: %q", got)
	}
	save("")
	if got := e.cfg.Get().Structure.IgnoreDirs; len(got) != 0 {
		t.Fatalf("ignore_dirs after an empty form: %q", got)
	}
	if _, body := e.get("/settings"); !strings.Contains(body, "ignore_dirs: []") {
		t.Fatal("the form doesn't show the current ignore_dirs")
	}
}

var revRe = regexp.MustCompile(`name="rev" value="([^"]+)"`)

// Two admins (or two tabs) saving different sections must not overwrite
// each other: a form from a page loaded before another save is refused,
// before anything (a secret included) is written. Forms without a revision
// (scripts) are taken as they are.
func TestStaleSettingsPageRefused(t *testing.T) {
	e := adminEnv(t, twoServers)
	_, page := e.get("/settings")
	csrf, rev := csrfRe.FindStringSubmatch(page)[1], revRe.FindStringSubmatch(page)[1]
	if n := len(revRe.FindAllString(page, -1)); n < 9 {
		t.Fatalf("%d forms carry the revision, want the 9 that change the configuration", n)
	}
	collector := func(rev, mode string) (string, bool) {
		return flash(e.post("/settings/collector", url.Values{"csrf": {csrf}, "rev": {rev}, "mode": {mode}, "event_timeout": {"50s"}, "full_check_interval": {"4h"},
			"poll_interval": {"15m"}, "structure_interval": {"1h"}, "baseline_interval": {"7d"}, "request_timeout": {"30s"}, "concurrency": {"3"}, "down_after_failures": {"2"}}))
	}
	// Tab B saves the collector from a fresh page.
	if msg, isErr := collector(rev, "polled"); isErr {
		t.Fatalf("fresh save: %s", msg)
	}
	// Tab A, loaded before that, saves the structure and a server with a new key.
	msg, isErr := flash(e.post("/settings/structure", url.Values{"csrf": {csrf}, "rev": {rev}, "yaml": {"structure: {project_pattern: '^LIVE-'}"}}))
	if !isErr || !strings.Contains(msg, "Reload the page") {
		t.Fatalf("stale structure save: %q %v", msg, isErr)
	}
	if e.cfg.Get().Structure.ProjectPattern != "" || e.cfg.Get().Collector.Mode != "polled" {
		t.Fatalf("stale save applied: %+v", e.cfg.Get().Structure)
	}
	if _, isErr := flash(e.post("/settings/servers", url.Values{"csrf": {csrf}, "rev": {rev}, "orig_id": {"akka"}, "name": {"AKKA"}, "url": {"https://a:1"}, "api_key": {"stale-key"}})); !isErr {
		t.Fatal("stale server save accepted")
	}
	if key, _, _ := e.st.ServerKey("akka"); key != "secret-key-akka" {
		t.Fatalf("stale save wrote the key: %q", key)
	}
	if _, isErr := flash(e.post("/settings/import", url.Values{"csrf": {csrf}, "rev": {rev}, "yaml": {"servers: [{name: AKKA, url: 'https://a:1', api_key: stale-key}]"}})); !isErr {
		t.Fatal("stale import accepted")
	}
	if key, _, _ := e.st.ServerKey("akka"); key != "secret-key-akka" || len(e.cfg.Get().Servers) != 2 {
		t.Fatal("stale import applied")
	}
	// After a reload the save goes through; a form without a revision does too.
	_, page = e.get("/settings")
	if msg, isErr := flash(e.post("/settings/structure", url.Values{"csrf": {csrf}, "rev": {revRe.FindStringSubmatch(page)[1]}, "yaml": {"structure: {project_pattern: '^LIVE-'}"}})); isErr {
		t.Fatalf("save after reload: %s", msg)
	}
	if msg, isErr := collector("", "events"); isErr || e.cfg.Get().Collector.Mode != "events" {
		t.Fatalf("save without a revision: %q %v", msg, isErr)
	}
}

// The notification log shows the date and the time in the configured zone,
// not the time in the process zone.
func TestNotificationLogTimeZone(t *testing.T) {
	e := adminEnv(t, twoServers+"timezone: Pacific/Auckland\n")
	at := time.Date(2026, 10, 5, 23, 30, 0, 0, time.UTC) // 2026-10-06 12:30 in Auckland
	_ = e.st.LogNotification(at, "test", "discord", true, "", "tz probe")
	_, body := e.get("/settings")
	i := strings.Index(body, "tz probe")
	if i < 0 {
		t.Fatal("log row missing")
	}
	if row := body[strings.LastIndex(body[:i], "<tr>"):i]; !strings.Contains(row, "2026-10-06 12:30") {
		t.Fatalf("row shows %q", row)
	}
}
