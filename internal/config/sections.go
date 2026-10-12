package config

import (
	"bytes"

	"gopkg.in/yaml.v3"
)

// ChangedSections names the sections of the Settings page in which c differs
// from old, in the order of the page, for a notice about a change that came
// from outside the dashboard (syncwatch import-config). Secrets are not part
// of the configuration and never count. The values are compared as YAML, so
// an empty list and a missing list are the same.
func ChangedSections(old, c *Config) []string {
	parts := func(c *Config) []any {
		r := c.Redacted()
		return []any{
			r.Servers,
			r.Devices,
			[]any{r.Notify, r.Timezone, r.PublicURL, r.Site},
			[]any{r.Checks, r.Thresholds},
			[]any{r.Structure, r.Shares},
			r.Collector,
			r.Access,
		}
	}
	labels := []string{"Servers", "Devices", "Notifications", "Checks", "Structure rules and shares", "Collector", "Access"}
	was, now := parts(old), parts(c)
	var out []string
	for i, label := range labels {
		a, errA := yaml.Marshal(was[i])
		b, errB := yaml.Marshal(now[i])
		if errA != nil || errB != nil || !bytes.Equal(a, b) {
			out = append(out, label)
		}
	}
	return out
}
