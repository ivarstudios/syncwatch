package config

import (
	"encoding/json"
	"slices"
	"testing"
	"time"
)

// The settings notice of syncwatch import-config names the sections of the
// Settings page that the file changed. A round trip through the store (JSON)
// or an export and import (YAML) changes nothing, and secrets never count.
func TestChangedSections(t *testing.T) {
	old, err := ParseYAML([]byte(ivarYAML))
	if err != nil {
		t.Fatal(err)
	}
	if got := ChangedSections(old, old.Clone()); len(got) != 0 {
		t.Fatalf("a copy changed %v", got)
	}
	b, _ := json.Marshal(old)
	var stored Config
	if err := json.Unmarshal(b, &stored); err != nil {
		t.Fatal(err)
	}
	stored.ApplyDefaults()
	if got := ChangedSections(old, &stored); len(got) != 0 {
		t.Fatalf("a JSON round trip changed %v", got)
	}
	y, err := old.ExportYAML()
	if err != nil {
		t.Fatal(err)
	}
	again, err := ParseYAML(y)
	if err != nil {
		t.Fatal(err)
	}
	if got := ChangedSections(old, again); len(got) != 0 {
		t.Fatalf("an export and import changed %v", got)
	}
	// An empty list and no list are the same setting.
	empty := old.Clone()
	empty.Structure.TypeTokens, empty.Structure.Placement = nil, nil
	none := empty.Clone()
	none.Structure.TypeTokens, none.Structure.Placement = []string{}, []PlacementRule{}
	if got := ChangedSections(empty, none); len(got) != 0 {
		t.Fatalf("nil and empty lists differ: %v", got)
	}

	secrets := old.Clone()
	secrets.Servers[0].APIKey = "other-key"
	secrets.Notify.DiscordWebhook = "https://discord.com/api/webhooks/2/other"
	secrets.API.Token = "swt_other"
	if got := ChangedSections(old, secrets); len(got) != 0 {
		t.Fatalf("secrets counted as settings: %v", got)
	}

	off := false
	c := old.Clone()
	c.Servers[1].Disabled = true
	c.Timezone = "Europe/Stockholm"
	c.Checks["H7"] = Check{Enabled: &off}
	c.Structure.IgnoreDirs = append(c.Structure.IgnoreDirs, "tmp")
	c.Collector.EventTimeout = Duration(40 * time.Second)
	c.Access.Viewer = ViewerOpen
	want := []string{"Servers", "Notifications", "Checks", "Structure rules and shares", "Collector", "Access"}
	if got := ChangedSections(old, c); !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	c = old.Clone()
	c.Thresholds.PCOffline = Duration(3 * 24 * time.Hour)
	c.Shares.Add["AKKA"] = []string{"/share/extra"}
	if got := ChangedSections(old, c); !slices.Equal(got, []string{"Checks", "Structure rules and shares"}) {
		t.Fatalf("thresholds and shares: %v", got)
	}
}
