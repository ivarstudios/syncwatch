package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

const ivarYAML = `
servers:
  - name: AKKA
    url: https://192.168.1.10:8384
    api_key: secret-akka
  - name: SOL
    url: https://192.168.2.10:8384
notifications:
  discord_webhook: https://discord.com/api/webhooks/1/abc
  discord_role_id: "123456789012345678"
structure:
  project_pattern: '^(ME-)?LIVE-[A-Za-z0-9]'
  share_type_from_path: '-(?P<me>ME-)?LIVE-(?P<type>[1-5][A-Z]+)$'
  type_tokens: [1SOURCE, 2PROJECTFILES, 3INTERMEDIATE, 4FINALS, 5COLLECT]
  placement:
    - when_name_matches: '^ME-LIVE-'
      share_must_have: { me: true }
    - when_name_matches: '^LIVE-'
      share_must_have: { me: false }
  ignore_dirs: ['@Recycle', '.streams']
  nested_scan_depth: 3
shares:
  add: { SIRIUS: [/share/CACHEDEV2_DATA/SIRIUS-LIVE-5COLLECT] }
thresholds:
  pc_offline: 10d
checks:
  H7: { urgent: true, debounce: 1h }
`

func TestParseAndExport(t *testing.T) {
	c, err := ParseYAML([]byte(ivarYAML))
	if err != nil {
		t.Fatal(err)
	}
	if c.Servers[0].ID != "akka" || c.Servers[0].APIKey != "secret-akka" {
		t.Fatalf("server: %+v", c.Servers[0])
	}
	if c.Thresholds.PCOffline.D() != 10*24*time.Hour {
		t.Fatalf("pc_offline %v", c.Thresholds.PCOffline)
	}
	if c.Thresholds.StuckSync.D() != 24*time.Hour {
		t.Fatal("default stuck_sync")
	}
	if !c.Urgent("H7") || c.Debounce("H7") != time.Hour || !c.Urgent("H1") || c.Urgent("S4") {
		t.Fatal("check overrides")
	}
	if c.Structure.Placement[0].ShareMustHave["me"] != "true" {
		t.Fatalf("placement bool: %v", c.Structure.Placement[0].ShareMustHave)
	}
	add, _ := c.SharesFor(c.Servers[0])
	if len(add) != 0 {
		t.Fatal("akka has no adds")
	}
	out, err := c.ExportYAML()
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, secret := range []string{"secret-akka", "discord.com", "api_key", "discord_webhook"} {
		if strings.Contains(s, secret) {
			t.Fatalf("export contains %q:\n%s", secret, s)
		}
	}
	// The export must re-import cleanly.
	c2, err := ParseYAML(out)
	if err != nil {
		t.Fatalf("re-import: %v\n%s", err, s)
	}
	if c2.Structure.ProjectPattern != c.Structure.ProjectPattern || c2.Thresholds.PCOffline != c.Thresholds.PCOffline {
		t.Fatal("round trip lost data")
	}
}

func TestValidateErrors(t *testing.T) {
	_, err := ParseYAML([]byte("servers: [{name: a, url: ftp://x}]\nstructure: {project_pattern: '('}\n"))
	if err == nil || !strings.Contains(err.Error(), "url must be") || !strings.Contains(err.Error(), "project_pattern") {
		t.Fatalf("expected errors, got %v", err)
	}
	if _, err := ParseYAML([]byte("bogus: 1\n")); err == nil {
		t.Fatal("unknown key accepted")
	}
	if _, err := ParseYAML([]byte("")); err != nil {
		t.Fatalf("empty file: %v", err)
	}
}

func TestDuration(t *testing.T) {
	cases := map[string]time.Duration{"90d": 90 * 24 * time.Hour, "1w2d": 9 * 24 * time.Hour, "36h": 36 * time.Hour, "1h30m": 90 * time.Minute, "0": 0}
	for in, want := range cases {
		got, err := ParseDuration(in)
		if err != nil || got.D() != want {
			t.Errorf("%s: got %v %v", in, got, err)
		}
	}
	if Duration(48*time.Hour).String() != "2d" || Duration(90*time.Minute).String() != "90m" {
		t.Fatal("format")
	}
	if _, err := ParseDuration("abc"); err == nil {
		t.Fatal("bad duration accepted")
	}
}

// Large d/w values used to overflow into a negative duration that passed
// validation; a check debounce of 1000000w then pinged at once and the stored
// configuration could not be loaded again.
func TestDurationOverflowAndLimit(t *testing.T) {
	for _, s := range []string{"1000000w", "999999999d", "106752d", "2562047h2562047h", "9223372036854775808ns", "3651d", "522w", "87601h", "3650d1s", "-5m", "1e3h"} {
		d, err := ParseDuration(s)
		if err == nil || d != 0 {
			t.Errorf("%s: got %v (%d ns), err %v; want an error", s, d, int64(d), err)
		}
	}
	for s, want := range map[string]time.Duration{"3650d": MaxDuration, "521w": 521 * 7 * 24 * time.Hour, "87600h": MaxDuration, "1.5d": 36 * time.Hour, "3649d23h": MaxDuration - time.Hour} {
		if d, err := ParseDuration(s); err != nil || d.D() != want {
			t.Errorf("%s: got %v %v, want %v", s, d, err, want)
		}
	}
	if Duration(MaxDuration).String() != "3650d" {
		t.Fatalf("format of the maximum: %s", Duration(MaxDuration))
	}
	c, err := ParseYAML([]byte("servers: [{name: a, url: 'https://10.0.0.1:8384'}]\nchecks: {H1: {debounce: 1000000w}}\n"))
	if err == nil {
		t.Fatalf("overflowed debounce accepted: H1 debounce %v", c.Debounce("H1"))
	}
}

// Validate must reject a negative duration wherever one can get in: through a
// JSON number of nanoseconds (the stored form), a check debounce, or a value
// set in code. ApplyDefaults only replaces zero, so negatives reach Validate.
func TestNegativeDurationsRejected(t *testing.T) {
	for _, js := range []string{
		`{"collector":{"request_timeout":-1}}`,
		`{"thresholds":{"paused":-3600000000000}}`,
		`{"checks":{"H1":{"debounce":-1}}}`,
		`{"access":{"session_ttl":-1}}`,
	} {
		var c Config
		if err := json.Unmarshal([]byte(js), &c); err != nil {
			t.Fatal(err)
		}
		c.ApplyDefaults()
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "must be between") {
			t.Errorf("%s: negative duration accepted: %v", js, err)
		}
	}
	c := Default()
	c.Thresholds.PCOffline = Duration(-time.Hour)
	if err := c.Validate(); err == nil {
		t.Fatal("negative threshold accepted")
	}
}

// Every bound names the field and the allowed range. Zero means the default.
func TestValidateBounds(t *testing.T) {
	const srv = "servers: [{name: a, url: 'https://10.0.0.1:8384'}]\n"
	bad := map[string]string{
		"collector: {request_timeout: 500ms}":   "collector.request_timeout must be between 1s and 10m",
		"collector: {request_timeout: 11m}":     "collector.request_timeout must be between 1s and 10m",
		"collector: {down_after_failures: 21}":  "collector.down_after_failures must be between 1 and 20",
		"collector: {concurrency: 17}":          "collector.concurrency must be between 1 and 16",
		"collector: {event_timeout: 4s}":        "collector.event_timeout must be between 5s and 10m",
		"collector: {event_timeout: 11m}":       "collector.event_timeout must be between 5s and 10m",
		"collector: {full_check_interval: 59s}": "collector.full_check_interval must be between 1m and 7d",
		"collector: {full_check_interval: 8d}":  "collector.full_check_interval must be between 1m and 7d",
		"collector: {poll_interval: 30s}":       "collector.poll_interval must be between 1m and 1d",
		"collector: {poll_interval: 25h}":       "collector.poll_interval must be between 1m and 1d",
		"collector: {structure_interval: 10s}":  "collector.structure_interval must be between 1m and 7d",
		"collector: {structure_interval: 2w}":   "collector.structure_interval must be between 1m and 7d",
		"collector: {baseline_interval: 59m}":   "collector.baseline_interval must be between 1h and 90d",
		"collector: {baseline_interval: 91d}":   "collector.baseline_interval must be between 1h and 90d",
		"structure: {nested_scan_depth: 11}":    "structure.nested_scan_depth must be between 1 and 10",
		"notifications: {weekly_top: 51}":       "notifications.weekly_top must be between 1 and 50",
		"access: {session_ttl: 59m}":            "access.session_ttl must be between 1h and 365d",
		"access: {session_ttl: 366d}":           "access.session_ttl must be between 1h and 365d",
		"checks: {H1: {debounce: 31d}}":         "checks.H1.debounce must be between 0 and 30d",
		"thresholds: {paused: 9s}":              "thresholds.paused must be between 10s and 3650d",
		"thresholds: {failing_files: 5s}":       "thresholds.failing_files must be between 10s and 3650d",
		"thresholds: {stuck_sync: 1ms}":         "thresholds.stuck_sync must be between 10s and 3650d",
		"thresholds: {pc_offline: 1s}":          "thresholds.pc_offline must be between 10s and 3650d",
		"thresholds: {conflict_window: 1s}":     "thresholds.conflict_window must be between 10s and 3650d",
		"thresholds: {inactive_project: 1s}":    "thresholds.inactive_project must be between 10s and 3650d",
		"thresholds: {old_device: 1s}":          "thresholds.old_device must be between 10s and 3650d",
		"thresholds: {dead_share: 1s}":          "thresholds.dead_share must be between 10s and 3650d",
	}
	for in, want := range bad {
		_, err := ParseYAML([]byte(srv + in + "\n"))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", in, err, want)
		}
	}
	ok := []string{
		"collector: {request_timeout: 1s, down_after_failures: 20, concurrency: 1, event_timeout: 5s, full_check_interval: 1m, poll_interval: 24h, structure_interval: 7d, baseline_interval: 1h}",
		"collector: {request_timeout: 10m, down_after_failures: 1, concurrency: 16, full_check_interval: 7d, poll_interval: 1m, baseline_interval: 90d}",
		"structure: {nested_scan_depth: 10}\nnotifications: {weekly_top: 50}\naccess: {session_ttl: 365d}",
		"checks: {H1: {debounce: 0}, H2: {debounce: 30d}}",
		"thresholds: {paused: 10s, pc_offline: 3650d, failing_files: 0}",
	}
	for _, in := range ok {
		if _, err := ParseYAML([]byte(srv + in + "\n")); err != nil {
			t.Errorf("%s: %v", in, err)
		}
	}
	c, err := ParseYAML([]byte(srv + "thresholds: {failing_files: 0}\ncollector: {concurrency: 0}\n"))
	if err != nil || c.Thresholds.FailingFiles.D() != time.Hour || c.Collector.Concurrency != 3 {
		t.Fatalf("zero should mean the default: %v %v", c, err)
	}
}

// Clone used to be a YAML round trip with errors ignored: one unreadable
// value silently reset every later section. Now it is a field-by-field deep
// copy that shares nothing with the original.
func TestCloneIsDeepAndComplete(t *testing.T) {
	c, err := ParseYAML([]byte(ivarYAML + "access: {viewer: open, mode: proxy}\n"))
	if err != nil {
		t.Fatal(err)
	}
	c.API.Token = "swt_x"
	c.Notify.QuietHours, c.Notify.WeeklyTop = "off", 7
	c.Structure.TypeTokens = nil // nil stays nil, so an export doesn't change
	cl := c.Clone()
	if !reflect.DeepEqual(c, cl) {
		t.Fatalf("clone differs:\n%+v\n%+v", c, cl)
	}
	if cl.Servers[0].APIKey != "secret-akka" || cl.API.Token != "swt_x" {
		t.Fatal("secrets lost")
	}
	// Mutate every reference in the clone; the original must not change.
	cl.Servers[0].Name = "changed"
	*cl.Checks["H7"].Debounce = Duration(time.Minute)
	*cl.Checks["H7"].Urgent = false
	cl.Checks["H1"] = Check{}
	cl.Structure.IgnoreDirs[0] = "changed"
	cl.Structure.Placement[0].ShareMustHave["me"] = "changed"
	cl.Structure.Placement[0].WhenNameMatches = "changed"
	cl.Shares.Add["SIRIUS"][0] = "changed"
	cl.Shares.Add["new"] = []string{"x"}
	if c.Servers[0].Name != "AKKA" || c.Debounce("H7") != time.Hour || !c.Urgent("H7") || len(c.Checks) != 1 ||
		c.Structure.IgnoreDirs[0] != "@Recycle" || c.Structure.Placement[0].ShareMustHave["me"] != "true" ||
		c.Structure.Placement[0].WhenNameMatches != "^ME-LIVE-" || c.Shares.Add["SIRIUS"][0] == "changed" || len(c.Shares.Add) != 1 {
		t.Fatalf("clone shares memory with the original: %+v", c)
	}
	// A clone of a config with a bad value keeps every section instead of
	// resetting the ones after it.
	bad := Default()
	bad.Checks["H1"] = Check{Debounce: new(Duration)}
	*bad.Checks["H1"].Debounce = Duration(-1)
	bad.Access.Viewer = ViewerOpen
	bad.Thresholds.PCOffline = Duration(10 * 24 * time.Hour)
	bad.Structure.ProjectPattern = "^LIVE-"
	bc := bad.Clone()
	if bc.Access.Viewer != ViewerOpen || bc.Thresholds.PCOffline != bad.Thresholds.PCOffline || bc.Structure.ProjectPattern != "^LIVE-" || bc.Debounce("H1") != -1 {
		t.Fatalf("clone reset sections: %+v", bc)
	}
}

func TestParseClockStrict(t *testing.T) {
	ok := map[string]int{"08:00": 480, "8:05": 485, "23:59": 23*60 + 59, "0:00": 0, "00:00": 0, " 12:30 ": 750, "19:00": 19 * 60}
	for in, want := range ok {
		if got, err := ParseClock(in); err != nil || got != want {
			t.Errorf("%q: got %d %v, want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"08:00junk", "8:5", "+8:00", "-8:00", "24:00", "8:60", "8", "8:00pm", "", "08:00:00", "8.00", "08-00", "a8:00", "008:00"} {
		if got, err := ParseClock(in); err == nil {
			t.Errorf("%q accepted as %d", in, got)
		}
	}
	if s, e, err := ParseRange("22:00-07:00"); err != nil || s != 22*60 || e != 7*60 {
		t.Fatalf("range: %d %d %v", s, e, err)
	}
	for _, in := range []string{"22:00-22:00", "22:00", "22:00-07:00-1", "22:00-7", "22:00-07:00x", "-"} {
		if _, _, err := ParseRange(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
	const srv = "servers: [{name: a, url: 'https://10.0.0.1:8384'}]\n"
	for _, in := range []string{
		"notifications: {quiet_hours: '22:00-22:00'}",
		"notifications: {quiet_hours: '22:00-07:00junk'}",
		"notifications: {reminder_time: '8:5'}",
		"notifications: {reminder_time: '+8:00'}",
		"notifications: {weekly_time: '08:00pm'}",
	} {
		if _, err := ParseYAML([]byte(srv + in + "\n")); err == nil {
			t.Errorf("%s accepted", in)
		}
	}
	// "off" in any case, and "" means the default (not off).
	c, err := ParseYAML([]byte(srv + "notifications: {quiet_hours: OFF, reminder_time: Off, weekly_day: ' oFF '}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Notify.QuietHours != "off" || c.Notify.ReminderTime != "off" || c.Notify.WeeklyDay != "off" {
		t.Fatalf("off not canonical: %+v", c.Notify)
	}
	c, err = ParseYAML([]byte(srv + "notifications: {quiet_hours: '', reminder_time: '', weekly_day: ''}\n"))
	if err != nil || c.Notify.QuietHours != "22:00-07:00" || c.Notify.ReminderTime != "08:00" || c.Notify.WeeklyDay != "monday" {
		t.Fatalf("empty should mean the default: %+v %v", c.Notify, err)
	}
}

// Ids are looked up ignoring case and used in URLs, JSON keys and secret
// names, so they have a fixed charset and are unique ignoring case; names
// too, since lookups also match names.
func TestServerIDs(t *testing.T) {
	const u = "url: 'https://10.0.0.1:8384'"
	bad := map[string]string{
		"servers: [{id: AKKA, name: a, " + u + "}]":                                 "server id \"AKKA\" must be 1-40",
		"servers: [{id: 'a,b', name: a, " + u + "}]":                                "server id \"a,b\" must be",
		"servers: [{id: '<b>x</b>', name: a, " + u + "}]":                           "must be 1-40 characters",
		"servers: [{id: '-a', name: a, " + u + "}]":                                 "must be 1-40 characters",
		"servers: [{id: 'a b', name: a, " + u + "}]":                                "must be 1-40 characters",
		"servers: [{id: " + strings.Repeat("a", 41) + ", name: a, " + u + "}]":      "must be 1-40 characters",
		"servers: [{id: akka, name: A, " + u + "}, {id: AKKA, name: B, " + u + "}]": "already used by server akka",
		"servers: [{id: akka, name: A, " + u + "}, {id: akka, name: B, " + u + "}]": "already used by server akka",
		"servers: [{name: Akka, " + u + "}, {name: AKKA, " + u + "}]":               "name \"AKKA\" is already used by server akka",
		"servers: [{name: A, id: nas, " + u + "}, {name: nas, id: b, " + u + "}]":   "server b: id or name \"nas\" is already used by server nas",
		"servers: [{name: nas, id: a, " + u + "}, {name: B, id: NAS, " + u + "}]":   "already used by server a",
	}
	for in, want := range bad {
		_, err := ParseYAML([]byte(in + "\n"))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", in, err, want)
		}
	}
	c, err := ParseYAML([]byte("servers: [{id: " + strings.Repeat("a", 40) + ", name: a, " + u + "}, {id: nas-1, name: 'NAS 1', " + u + "}, {id: '9', name: nine, " + u + "}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if sv, ok := c.ServerByKey("nas 1"); !ok || sv.ID != "nas-1" {
		t.Fatalf("lookup by name: %+v %v", sv, ok)
	}
	// Generated ids: a slug that is empty or taken gets -2, -3, …; long names are cut to 40.
	c, err = ParseYAML([]byte("servers: [{name: ÅÄÖ, " + u + "}, {name: ÖÄÅ, " + u + "}, {name: 'NAS 1', " + u + "}, {id: nas-1, name: Other, " + u + "}, {name: '" + strings.Repeat("x", 39) + "yz', " + u + "}, {name: '" + strings.Repeat("x", 39) + "yzz', " + u + "}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range c.Servers {
		ids = append(ids, s.ID)
	}
	want := []string{"server", "server-2", "nas-1-2", "nas-1", strings.Repeat("x", 39) + "y", strings.Repeat("x", 38) + "-2"}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("ids %q, want %q", ids, want)
	}
	if Slug("--Hello, World!--") != "hello-world" || Slug("") != "server" || len(Slug(strings.Repeat("ab-", 30))) > 40 {
		t.Fatalf("slug: %q %q %q", Slug("--Hello, World!--"), Slug(""), Slug(strings.Repeat("ab-", 30)))
	}
}

// An omitted ignore_dirs means the defaults; an explicit empty list means
// none. Both through YAML and through the stored JSON form.
func TestIgnoreDirsNilVersusEmpty(t *testing.T) {
	const srv = "servers: [{name: a, url: 'https://10.0.0.1:8384'}]\n"
	c, err := ParseYAML([]byte(srv + "structure: {project_pattern: '^LIVE-'}\n"))
	if err != nil || !reflect.DeepEqual(c.Structure.IgnoreDirs, DefaultIgnoreDirs) {
		t.Fatalf("omitted: %q %v", c.Structure.IgnoreDirs, err)
	}
	e, err := ParseYAML([]byte(srv + "structure: {ignore_dirs: []}\n"))
	if err != nil || e.Structure.IgnoreDirs == nil || len(e.Structure.IgnoreDirs) != 0 {
		t.Fatalf("explicit empty: %#v %v", e.Structure.IgnoreDirs, err)
	}
	for _, in := range []*Config{c, e} {
		b, _ := json.Marshal(in)
		var back Config
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		back.ApplyDefaults()
		if !reflect.DeepEqual(back.Structure.IgnoreDirs, in.Structure.IgnoreDirs) || !reflect.DeepEqual(in.Clone().Structure.IgnoreDirs, in.Structure.IgnoreDirs) {
			t.Fatalf("round trip changed ignore_dirs: %q -> %q, clone %q", in.Structure.IgnoreDirs, back.Structure.IgnoreDirs, in.Clone().Structure.IgnoreDirs)
		}
	}
	var nilCfg Config
	nilCfg.ApplyDefaults()
	if !reflect.DeepEqual(nilCfg.Structure.IgnoreDirs, DefaultIgnoreDirs) {
		t.Fatal("nil should get the defaults")
	}
}

// A placement attribute that is not a named group of share_type_from_path
// can never be satisfied, so every matching project would fail placement.
func TestPlacementAttributesMatchGroups(t *testing.T) {
	const srv = "servers: [{name: a, url: 'https://10.0.0.1:8384'}]\n"
	for in, want := range map[string]string{
		"structure: {share_type_from_path: '-(?P<me>ME-)?LIVE-(?P<type>[1-5][A-Z]+)$', placement: [{when_name_matches: '^ME-', share_must_have: {ME: true}}]}": "share_must_have attribute \"ME\" is not a named group of share_type_from_path (have: me, type)",
		"structure: {placement: [{when_name_matches: '^ME-', share_must_have: {me: true}}]}":                                                                   "attribute \"me\" is not a named group",
		"structure: {share_type_from_path: '-LIVE-[1-5][A-Z]+$', placement: [{when_name_matches: '^ME-', share_must_have: {me: true}}]}":                       "(have: none)",
	} {
		_, err := ParseYAML([]byte(srv + in + "\n"))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", in, err, want)
		}
	}
	c, err := ParseYAML([]byte(srv + "structure: {case_insensitive: true, share_type_from_path: '-LIVE-(?P<type>[1-5][A-Z]+)$', placement: [{when_name_matches: '^LIVE-', share_must_have: {type: 1SOURCE}}]}\n"))
	if err != nil {
		t.Fatal(err)
	}
	cs, _ := c.Structure.Compile()
	if attrs, ok := cs.ShareAttrs("sol-live-1source"); !ok || attrs["type"] != "1source" {
		t.Fatalf("case_insensitive not applied to share_type_from_path: %v %v", attrs, ok)
	}
	if !cs.Placement[0].When.MatchString("live-x") {
		t.Fatal("case_insensitive not applied to when_name_matches")
	}
}

func TestDiscordRoleID(t *testing.T) {
	const srv = "servers: [{name: a, url: 'https://10.0.0.1:8384'}]\n"
	for _, id := range []string{"123456", "42", "1234567890123456", "123456789012345678901", "12345678901234567a", "<@&123456789012345678>"} {
		if _, err := ParseYAML([]byte(srv + "notifications: {discord_role_id: '" + id + "'}\n")); err == nil || !strings.Contains(err.Error(), "17-20 digits") {
			t.Errorf("%q: %v", id, err)
		}
	}
	for _, id := range []string{"12345678901234567", "777000111000000000", "12345678901234567890", ""} {
		if _, err := ParseYAML([]byte(srv + "notifications: {discord_role_id: '" + id + "'}\n")); err != nil {
			t.Errorf("%q: %v", id, err)
		}
	}
}

// The stored webhook is the canonical form, so an invisible character from a
// paste can't turn into a 404 from Discord later.
func TestDiscordWebhookCanonical(t *testing.T) {
	for in, want := range map[string]string{
		" https://DISCORD.com/api/webhooks/123/abc-DEF_1/ ":                              "https://discord.com/api/webhooks/123/abc-DEF_1",
		"\u200bhttps://discord.com/api/webhooks/123/abc\u200b\ufeff\n":                   "https://discord.com/api/webhooks/123/abc",
		"https://canary.discord.com/api/v10/webhooks/123/abc?thread_id=9#frag":           "https://canary.discord.com/api/v10/webhooks/123/abc?thread_id=9",
		"HTTPS://discordapp.com/api/webhooks/123/abc":                                    "https://discordapp.com/api/webhooks/123/abc",
		"\u00a0https://ptb.discordapp.com/api/webhooks/123/abc\u2060":                    "https://ptb.discordapp.com/api/webhooks/123/abc",
		"https://discord.com/api/webhooks/123/" + strings.Repeat("a", 68) + "?wait=true": "https://discord.com/api/webhooks/123/" + strings.Repeat("a", 68) + "?wait=true",
	} {
		got, err := CanonicalDiscordWebhook(in)
		if err != nil || got != want {
			t.Errorf("%q: got %q %v, want %q", in, got, err, want)
		}
		if err := CheckDiscordWebhook(in); err != nil {
			t.Errorf("%q refused: %v", in, err)
		}
	}
	for _, in := range []string{
		"https://discord.com/api/webhooks/abc/def",
		"https://discord.com/api/webhooks/123/ab\u200bc",
		"https://discord.com/api/webhooks/123/ab c",
		"https://discord.com/api/webhooks/123/a%20b",
		"https://discord\u200b.com/api/webhooks/123/abc",
		"https://discord.com/api/webhooks/123/abc/def",
		"https://discord.com/api/webhooks/123/",
		"https://discord.com/api/webhooks/123",
		"https://discord.com/api/webhooks/123/abc\u200bdef",
	} {
		if _, err := CanonicalDiscordWebhook(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
	// An import stores the canonical form.
	c, err := ParseYAML([]byte("servers: [{name: a, url: 'https://10.0.0.1:8384'}]\nnotifications: {discord_webhook: ' https://DISCORD.com/api/webhooks/1/abc/ '}\n"))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if c.Notify.DiscordWebhook != "https://discord.com/api/webhooks/1/abc" {
		t.Fatalf("import stored %q", c.Notify.DiscordWebhook)
	}
	AllowAnyWebhook = true
	defer func() { AllowAnyWebhook = false }()
	if got, err := CanonicalDiscordWebhook(" http://discord.e2e.invalid:8080/api/webhooks/1/x\u200b"); err != nil || got != "http://discord.e2e.invalid:8080/api/webhooks/1/x" {
		t.Errorf("override: %q %v", got, err)
	}
}

// url.Parse accepts an empty host name with a port and non-ASCII whitespace
// in the host; both are rejected.
func TestServerURLHost(t *testing.T) {
	for _, bad := range []string{
		"servers: [{name: a, url: 'https://:8384'}]",
		"servers: [{name: a, url: 'https://\u00a0nas:8384'}]",
		"servers: [{name: a, url: 'https://nas\u00a0:8384'}]",
		"servers: [{name: a, url: 'https://'}]",
		"servers: [{name: a, url: 'https://10.0.0.1:8384', gui_url: 'https://:8384'}]",
		"public_url: 'https://:8080'",
		"public_url: 'https://\u00a0sw.lan'",
	} {
		if _, err := ParseYAML([]byte(bad + "\n")); err == nil {
			t.Errorf("accepted: %q", bad)
		}
	}
	for _, ok := range []string{
		"servers: [{name: a, url: 'https://[::1]:8384'}]",
		"servers: [{name: a, url: 'https://nas.lan'}]",
		"servers: [{name: a, url: 'https://10.0.0.1:8384/st'}]",
	} {
		if _, err := ParseYAML([]byte(ok + "\n")); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
}

func TestStructureHelpers(t *testing.T) {
	c, _ := ParseYAML([]byte(ivarYAML))
	cs, err := c.Structure.Compile()
	if err != nil {
		t.Fatal(err)
	}
	attrs, ok := cs.ShareAttrs("SOL-ME-LIVE-2PROJECTFILES")
	if !ok || attrs["me"] != "ME-" || attrs["type"] != "2PROJECTFILES" {
		t.Fatalf("attrs %v %v", attrs, ok)
	}
	if _, ok := cs.ShareAttrs("Containers"); ok {
		t.Fatal("non-share matched")
	}
	if got := cs.NameTokens("LIVE-KYRGYZ-2projectfiles_BLENDER-1SOURCE"); len(got) != 2 || got[0] != "2PROJECTFILES" {
		t.Fatalf("tokens %v", got)
	}
	if !cs.Ignored("@Recycle") || cs.Ignored("LIVE-X") {
		t.Fatal("ignore")
	}
	if !AttrSatisfies("ME-", "true") || AttrSatisfies("", "true") || !AttrSatisfies("", "false") {
		t.Fatal("attr")
	}
}

func TestSameEndpoint(t *testing.T) {
	same := [][2]string{
		{"https://192.168.1.10:8384", "https://192.168.1.10:8384/"},
		{"https://AKKA.lan:8384", "https://akka.lan:8384"},
		{"HTTPS://nas", "https://nas:443"},
		{"https://nas/st/", "https://nas:443/st"},
	}
	for _, p := range same {
		if !SameEndpoint(p[0], p[1]) {
			t.Errorf("%s and %s should be the same endpoint", p[0], p[1])
		}
	}
	different := [][2]string{
		{"https://192.168.1.10:8384", "http://192.168.1.10:8384"},
		{"https://192.168.1.10:8384", "https://192.168.1.11:8384"},
		{"https://nas:8384", "https://nas:8385"},
		{"https://nas/a", "https://nas/b"},
		{"https://nas", "https://user@nas"},
		{"", "https://nas"},
		{"", ""},
		{"not a url", "not a url"},
	}
	for _, p := range different {
		if SameEndpoint(p[0], p[1]) {
			t.Errorf("%q and %q must not be the same endpoint", p[0], p[1])
		}
	}
}

// Server URLs aren't secret (shown, exported, sent in notices), so they
// must not carry credentials.
func TestURLsWithoutCredentials(t *testing.T) {
	for _, bad := range []string{
		"servers: [{name: a, url: 'https://admin:secret@10.0.0.1:8384'}]",
		"servers: [{name: a, url: 'https://admin@10.0.0.1:8384'}]",
		"servers: [{name: a, url: 'https://10.0.0.1:8384/?apikey=x'}]",
		"servers: [{name: a, url: 'https://10.0.0.1:8384#x'}]",
		"servers: [{name: a, url: 'https://10.0.0.1:8384', gui_url: 'javascript://x/%0aalert(1)'}]",
		"servers: [{name: a, url: 'https://10.0.0.1:8384', gui_url: 'https://u:p@nas.lan:8384'}]",
		"public_url: 'ftp://sw.lan'",
		"public_url: 'https://u:p@sw.lan'",
	} {
		if _, err := ParseYAML([]byte(bad + "\n")); err == nil {
			t.Errorf("accepted: %s", bad)
		}
	}
	if _, err := ParseYAML([]byte("servers: [{name: a, url: 'https://10.0.0.1:8384/st', gui_url: 'https://nas.lan:8384/#settings'}]\npublic_url: 'https://sw.lan:8080'\n")); err != nil {
		t.Fatal(err)
	}
}

// The webhook only goes to Discord unless a test setup lifts that.
func TestDiscordWebhookHosts(t *testing.T) {
	ok := []string{
		"https://discord.com/api/webhooks/123/abc-DEF_1",
		"https://discordapp.com/api/webhooks/123/abc",
		"https://canary.discord.com/api/v10/webhooks/123/abc?thread_id=9",
		"https://DISCORD.com/api/webhooks/123/abc/",
	}
	bad := []string{
		"http://discord.com/api/webhooks/123/abc",
		"https://discord.com:8443/api/webhooks/123/abc",
		"https://discord.com.evil.example/api/webhooks/123/abc",
		"https://evil.example/api/webhooks/123/abc",
		"https://10.0.0.5/api/webhooks/123/abc",
		"https://discord.com/api/users/@me",
		"https://u:p@discord.com/api/webhooks/123/abc",
	}
	for _, u := range ok {
		if err := CheckDiscordWebhook(u); err != nil {
			t.Errorf("%s refused: %v", u, err)
		}
	}
	for _, u := range bad {
		if CheckDiscordWebhook(u) == nil {
			t.Errorf("%s accepted", u)
		}
	}
	if _, err := ParseYAML([]byte("notifications: {discord_webhook: 'http://10.0.0.5:8080/hook'}\n")); err == nil {
		t.Error("import accepted a non-Discord webhook")
	}
	AllowAnyWebhook = true
	defer func() { AllowAnyWebhook = false }()
	if err := CheckDiscordWebhook("http://discord.e2e.invalid:8080/api/webhooks/1/x"); err != nil {
		t.Errorf("override: %v", err)
	}
	if CheckDiscordWebhook("ftp://x/y") == nil {
		t.Error("override accepted a non-http URL")
	}
}

func TestPlainHTTPNeedsOptIn(t *testing.T) {
	if _, err := ParseYAML([]byte("servers: [{name: a, url: 'http://10.0.0.1:8384'}]\n")); err == nil || !strings.Contains(err.Error(), "unencrypted") {
		t.Fatalf("http:// accepted without allow_http: %v", err)
	}
	c, err := ParseYAML([]byte("servers: [{name: a, url: 'http://10.0.0.1:8384', allow_http: true}]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Clone().Servers[0].AllowHTTP {
		t.Fatal("allow_http lost in a clone")
	}
}
