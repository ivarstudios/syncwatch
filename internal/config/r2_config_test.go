package config

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
)

// deviceID makes the n-th valid device ID: 8 groups of 7 characters.
func deviceID(n int) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
	group := make([]byte, 7)
	for i := range group {
		group[6-i] = alphabet[n%32]
		n /= 32
	}
	return strings.Repeat(string(group)+"-", 7) + string(group)
}

const r2Servers = "servers: [{name: AKKA, url: 'https://10.0.0.1:8384'}, {name: SOL, url: 'https://10.0.0.2:8384'}]\n"

// The keys of wave 3 go through an import and an export of the YAML file and
// through the stored JSON form. repeat_urgent 0 (off), an empty site and an
// empty device list are not written. The webhook for settings notices is a
// secret: the export never has it.
func TestR2KeysRoundTrip(t *testing.T) {
	dev := deviceID(1)
	in := `site: '  IVAR Office '
servers:
  - {name: AKKA, url: 'https://10.0.0.1:8384', site: IVAR Office}
  - {name: SOL, url: 'https://10.0.0.2:8384', site: ' Branch office'}
devices:
  - {id: ' ` + strings.ToLower(dev) + ` ', relay: true}
  - {id: ` + deviceID(2) + `}
thresholds: {relay_lost: 45m}
notifications:
  repeat_urgent: 2h
  discord_notices_webhook: ' https://DISCORD.com/api/webhooks/9/notices/ '
`
	c, err := ParseYAML([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if c.Site != "IVAR Office" || c.Servers[0].Site != "IVAR Office" || c.Servers[1].Site != "Branch office" {
		t.Fatalf("sites: %q %q %q", c.Site, c.Servers[0].Site, c.Servers[1].Site)
	}
	if want := []Device{{ID: dev, Relay: true}, {ID: deviceID(2)}}; !slices.Equal(c.Devices, want) {
		t.Fatalf("devices %+v, want %+v", c.Devices, want)
	}
	if c.Thresholds.RelayLost.D() != 45*time.Minute || c.Notify.RepeatUrgent.D() != 2*time.Hour {
		t.Fatalf("relay_lost %v, repeat_urgent %v", c.Thresholds.RelayLost, c.Notify.RepeatUrgent)
	}
	if c.Notify.DiscordNoticesWebhook != "https://discord.com/api/webhooks/9/notices" {
		t.Fatalf("notices webhook %q", c.Notify.DiscordNoticesWebhook)
	}
	out, err := c.ExportYAML()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"site: IVAR Office\n", "    site: Branch office\n", "devices:\n", "relay: true", "relay_lost: 45m", "repeat_urgent: 2h",
		"# syncwatch configuration export. The file does not contain the API keys, the webhook URLs or the API token.\n"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("export lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(string(out), "notices") {
		t.Fatalf("export contains the webhook for settings notices:\n%s", out)
	}
	again, err := ParseYAML(out)
	if err != nil {
		t.Fatalf("re-import: %v\n%s", err, out)
	}
	if again.Site != c.Site || again.Servers[1].Site != c.Servers[1].Site || !slices.Equal(again.Devices, c.Devices) ||
		again.Thresholds.RelayLost != c.Thresholds.RelayLost || again.Notify.RepeatUrgent != c.Notify.RepeatUrgent {
		t.Fatalf("round trip lost data: %+v", again)
	}
	if got := ChangedSections(c, again); len(got) != 0 {
		t.Fatalf("an export and import changed %v", got)
	}

	// The stored form (JSON) has the keys too, but never the webhook.
	b, _ := json.Marshal(c)
	if strings.Contains(string(b), "notices") {
		t.Fatalf("the stored form contains the webhook for settings notices: %s", b)
	}
	var stored Config
	if err := json.Unmarshal(b, &stored); err != nil {
		t.Fatal(err)
	}
	stored.ApplyDefaults()
	if stored.Site != c.Site || !slices.Equal(stored.Devices, c.Devices) || stored.Notify.RepeatUrgent != c.Notify.RepeatUrgent || stored.Thresholds.RelayLost != c.Thresholds.RelayLost {
		t.Fatalf("JSON round trip: %+v", stored)
	}

	// Defaults: relay_lost 30 minutes, repeat_urgent off; the empty values
	// are not written.
	d, err := ParseYAML([]byte(r2Servers))
	if err != nil {
		t.Fatal(err)
	}
	if d.Thresholds.RelayLost.D() != 30*time.Minute || d.Notify.RepeatUrgent != 0 || d.Site != "" || d.Devices != nil {
		t.Fatalf("defaults: relay_lost %v, repeat_urgent %v, site %q, devices %v", d.Thresholds.RelayLost, d.Notify.RepeatUrgent, d.Site, d.Devices)
	}
	out, _ = d.ExportYAML()
	for _, absent := range []string{"repeat_urgent", "site", "devices", "notices"} {
		if strings.Contains(string(out), absent) {
			t.Errorf("export of the defaults contains %q:\n%s", absent, out)
		}
	}
	if !strings.Contains(string(out), "relay_lost: 30m") {
		t.Errorf("export lacks the default relay_lost:\n%s", out)
	}
	if b, _ := json.Marshal(d); strings.Contains(string(b), "repeat_urgent") || strings.Contains(string(b), `"site"`) || strings.Contains(string(b), "devices") {
		t.Errorf("the stored form of the defaults: %s", b)
	}
}

// Each rule of the new keys refuses with a message that names the label and
// the key, and accepts the values at the limits.
func TestR2Validation(t *testing.T) {
	long := strings.Repeat("å", MaxSiteLen+1)
	var many strings.Builder
	many.WriteString("devices:\n")
	for i := 0; i <= MaxDevices; i++ {
		fmt.Fprintf(&many, "  - {id: %s}\n", deviceID(i))
	}
	bad := map[string]string{
		"site: '" + long + "'":                                               "Site where syncwatch runs (site): the value must have at most 40 characters and no control characters.",
		"site: \"IVAR\\aOffice\"":                                            "Site where syncwatch runs (site): the value must have at most 40 characters and no control characters.",
		"site: \"IVAR\\u200bOffice\"":                                        "Site where syncwatch runs (site): the value must have at most 40 characters and no control characters.",
		"devices: [{id: ABC}]":                                               `Devices (devices): "ABC" is not a Syncthing device ID. Use the complete device ID, 8 groups of 7 characters.`,
		"devices: [{id: " + deviceID(1) + "-ABCDEFG}]":                       "is not a Syncthing device ID",
		"devices: [{id: " + strings.Replace(deviceID(1), "A", "1", 1) + "}]": "is not a Syncthing device ID",
		"devices: [{id: " + deviceID(3) + "}, {id: '" + strings.ToLower(deviceID(3)) + "', relay: true}]": "Devices (devices): the device ID " + deviceID(3) + " is in the list 2 times.",
		strings.TrimSuffix(many.String(), "\n"):                                                           "Devices (devices): the list has 501 entries. The maximum is 500.",
		"thresholds: {relay_lost: 9s}":                                                                    "H12 no relay connection for more than (thresholds.relay_lost): the value must be between 10 seconds and 3650 days.",
		"notifications: {repeat_urgent: 29m}":                                                             "Repeat urgent messages every (notifications.repeat_urgent): the value must be between 30 minutes and 1 day.",
		"notifications: {repeat_urgent: 25h}":                                                             "Repeat urgent messages every (notifications.repeat_urgent): the value must be between 30 minutes and 1 day.",
		"notifications: {discord_notices_webhook: 'https://evil.example/api/webhooks/1/x'}":               "Discord webhook for settings notices (notifications.discord_notices_webhook): the value must be a webhook URL from Discord (https://discord.com/api/webhooks/…).",
		"notifications: {discord_notices_webhook: 'http://discord.com/api/webhooks/1/x'}":                 "Discord webhook for settings notices (notifications.discord_notices_webhook)",
	}
	for in, want := range bad {
		_, err := ParseYAML([]byte(r2Servers + in + "\n"))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%.80s: got %v, want %q", in, err, want)
		}
	}
	_, err := ParseYAML([]byte("servers: [{name: SOL, url: 'https://10.0.0.2:8384', site: '" + long + "'}]\n"))
	if want := "Server sol: the site (servers[].site) must have at most 40 characters and no control characters."; err == nil || err.Error() != want {
		t.Errorf("server site: got %v, want %q", err, want)
	}

	var ok strings.Builder
	ok.WriteString("devices:\n")
	for i := 0; i < MaxDevices; i++ {
		fmt.Fprintf(&ok, "  - {id: %s, relay: %v}\n", deviceID(i), i%2 == 0)
	}
	for _, in := range []string{
		"site: '" + strings.Repeat("ö", MaxSiteLen) + "'\nservers: [{name: AKKA, url: 'https://10.0.0.1:8384', site: '" + strings.Repeat("x", MaxSiteLen) + "'}]",
		"site: ''\nservers: [{name: AKKA, url: 'https://10.0.0.1:8384', site: ''}]",
		r2Servers + ok.String() + "thresholds: {relay_lost: 10s}",
		r2Servers + "thresholds: {relay_lost: 3650d}\nnotifications: {repeat_urgent: 0}",
		r2Servers + "notifications: {repeat_urgent: 30m}",
		r2Servers + "notifications: {repeat_urgent: 24h}",
		r2Servers + "notifications: {repeat_urgent: 1d, discord_notices_webhook: 'https://discord.com/api/webhooks/1/abc'}",
		r2Servers + "devices: []",
	} {
		if _, err := ParseYAML([]byte(in + "\n")); err != nil {
			t.Errorf("%.80s: %v", in, err)
		}
	}
	// A negative value can only come through the stored JSON form.
	var c Config
	if err := json.Unmarshal([]byte(`{"notifications":{"repeat_urgent":-1}}`), &c); err != nil {
		t.Fatal(err)
	}
	c.ApplyDefaults()
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "notifications.repeat_urgent") {
		t.Fatalf("negative repeat_urgent: %v", err)
	}
	// Each webhook error names its own setting.
	if _, err := CanonicalNoticesWebhook("https://discord.com/api/users/@me"); err == nil || !strings.HasPrefix(err.Error(), "Discord webhook for settings notices (notifications.discord_notices_webhook): ") {
		t.Fatalf("notices webhook error: %v", err)
	}
	if _, err := CanonicalDiscordWebhook("https://discord.com/api/users/@me"); err == nil || !strings.HasPrefix(err.Error(), "Discord webhook URL (notifications.discord_webhook): ") {
		t.Fatalf("webhook error: %v", err)
	}
}

func TestValidDeviceID(t *testing.T) {
	for _, id := range []string{deviceID(0), deviceID(12345), "MFZWI3D-BONSGYC-YLTMRWG-C43ENR5-QXGZDMM-FZWI3DP-BONSGYY-LTMRWAD"} {
		if !ValidDeviceID(id) {
			t.Errorf("%s refused", id)
		}
	}
	for _, id := range []string{"", "ABC", strings.ToLower(deviceID(1)), deviceID(1) + "-", " " + deviceID(1), strings.ReplaceAll(deviceID(1), "-", ""),
		strings.Replace(deviceID(1), "A", "8", 1), strings.Replace(deviceID(1), "-", "_", 1), model.ShortID(deviceID(1))} {
		if ValidDeviceID(id) {
			t.Errorf("%q accepted", id)
		}
	}
}

// Clone copies the device list, Redacted removes the webhook for settings
// notices, and the lookups find the values of a server and a device.
func TestR2CloneRedactedAndLookups(t *testing.T) {
	c, err := ParseYAML([]byte("servers: [{name: AKKA, url: 'https://10.0.0.1:8384', site: IVAR Office}, {name: SOL, url: 'https://10.0.0.2:8384'}]\n" +
		"devices: [{id: " + deviceID(1) + ", relay: true}, {id: " + deviceID(2) + "}]\n" +
		"notifications: {discord_notices_webhook: 'https://discord.com/api/webhooks/9/notices'}\n"))
	if err != nil {
		t.Fatal(err)
	}
	cl := c.Clone()
	if !slices.Equal(cl.Devices, c.Devices) || cl.Notify.DiscordNoticesWebhook != c.Notify.DiscordNoticesWebhook {
		t.Fatalf("clone: %+v", cl)
	}
	cl.Devices[0].Relay = false
	cl.Devices = append(cl.Devices, Device{ID: deviceID(3)})
	if !c.Devices[0].Relay || len(c.Devices) != 2 {
		t.Fatal("the clone shares the device list with the original")
	}
	if r := c.Redacted(); r.Notify.DiscordNoticesWebhook != "" || c.Notify.DiscordNoticesWebhook == "" {
		t.Fatalf("redacted: %q", r.Notify.DiscordNoticesWebhook)
	}
	if !c.RelayExpected(deviceID(1)) || !c.RelayExpected(strings.ToLower(deviceID(1))) || c.RelayExpected(deviceID(2)) || c.RelayExpected(deviceID(3)) {
		t.Fatal("RelayExpected")
	}
	if c.SiteOf("akka") != "IVAR Office" || c.SiteOf("sol") != "" || c.SiteOf("nope") != "" {
		t.Fatalf("SiteOf: %q %q %q", c.SiteOf("akka"), c.SiteOf("sol"), c.SiteOf("nope"))
	}

	// The settings notice of the command line names the sections: the
	// devices have a section of their own, the site of syncwatch is part of
	// the Notifications section, and a server's site is part of Servers.
	d := c.Clone()
	d.Devices = nil
	if got := ChangedSections(c, d); !slices.Equal(got, []string{"Devices"}) {
		t.Fatalf("devices: %v", got)
	}
	d = c.Clone()
	d.Site = "Branch office"
	d.Notify.RepeatUrgent = Duration(time.Hour)
	if got := ChangedSections(c, d); !slices.Equal(got, []string{"Notifications"}) {
		t.Fatalf("site: %v", got)
	}
	d = c.Clone()
	d.Servers[1].Site = "Branch office"
	d.Thresholds.RelayLost = Duration(time.Hour)
	if got := ChangedSections(c, d); !slices.Equal(got, []string{"Servers", "Checks"}) {
		t.Fatalf("server site and relay_lost: %v", got)
	}
	d = c.Clone()
	d.Notify.DiscordNoticesWebhook = "https://discord.com/api/webhooks/9/other"
	if got := ChangedSections(c, d); len(got) != 0 {
		t.Fatalf("a secret counted as a setting: %v", got)
	}
}

// H12 and H13 come after H11 in the catalog: the Checks table, the filters
// and the messages use this order.
func TestR2CatalogOrder(t *testing.T) {
	ids := SortedCheckIDs()
	i := slices.Index(ids, "H11")
	if i < 0 || i+3 >= len(ids) || !slices.Equal(ids[i:i+4], []string{"H11", "H12", "H13", "X1"}) {
		t.Fatalf("catalog order %v", ids)
	}
	h12, h13 := CatalogByID["H12"], CatalogByID["H13"]
	if h12 != (CheckInfo{"H12", "Relay connection lost", model.CatHealth, model.Warn, false, 5 * time.Minute}) {
		t.Errorf("H12: %+v", h12)
	}
	if h13 != (CheckInfo{"H13", "Certificate expires soon", model.CatHealth, model.Info, false, 5 * time.Minute}) {
		t.Errorf("H13: %+v", h13)
	}
	if FieldLabel("thresholds.relay_lost") != "H12 no relay connection for more than (thresholds.relay_lost)" ||
		FieldLabel("notifications.repeat_urgent") != "Repeat urgent messages every (notifications.repeat_urgent)" {
		t.Error("field labels")
	}
}
