package config

import (
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
  discord_role_id: "123456"
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
