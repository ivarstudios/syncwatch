// Package config defines syncwatch's settings, their defaults, validation and
// YAML import/export. Secrets (API keys, webhook URLs) can be imported from
// YAML but are never exported and are stored separately, encrypted.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the complete monitor configuration.
type Config struct {
	PublicURL  string           `yaml:"public_url" json:"public_url"`
	Timezone   string           `yaml:"timezone" json:"timezone"`
	Servers    []Server         `yaml:"servers" json:"servers"`
	Collector  Collector        `yaml:"collector" json:"collector"`
	Checks     map[string]Check `yaml:"checks,omitempty" json:"checks"`
	Thresholds Thresholds       `yaml:"thresholds" json:"thresholds"`
	Structure  Structure        `yaml:"structure" json:"structure"`
	Shares     Shares           `yaml:"shares" json:"shares"`
	Notify     Notify           `yaml:"notifications" json:"notifications"`
	Access     Access           `yaml:"access" json:"access"`
	API        API              `yaml:"api,omitempty" json:"-"`
}

// Server is one Syncthing instance to monitor.
type Server struct {
	ID       string `yaml:"id" json:"id"`
	Name     string `yaml:"name" json:"name"`
	URL      string `yaml:"url" json:"url"`
	GUIURL   string `yaml:"gui_url,omitempty" json:"gui_url,omitempty"`
	APIKey   string `yaml:"api_key,omitempty" json:"-"` // import only
	Disabled bool   `yaml:"disabled,omitempty" json:"disabled,omitempty"`
	// AllowHTTP permits a plain http:// URL, which sends the API key unencrypted.
	AllowHTTP bool `yaml:"allow_http,omitempty" json:"allow_http,omitempty"`
}

// SameEndpoint reports whether two server URLs address the same scheme, host,
// port and path. An API key is only ever sent to the URL it was entered for.
func SameEndpoint(a, b string) bool {
	na, okA := normalizeEndpoint(a)
	nb, okB := normalizeEndpoint(b)
	return okA && okB && na == nb
}

func normalizeEndpoint(s string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Host == "" {
		return "", false
	}
	scheme := strings.ToLower(u.Scheme)
	port := u.Port()
	if port == "" {
		switch scheme {
		case "https":
			port = "443"
		case "http":
			port = "80"
		}
	}
	host := strings.ToLower(u.Hostname())
	return scheme + "://" + u.User.String() + "@" + host + ":" + port + strings.TrimRight(u.EscapedPath(), "/") + "?" + u.RawQuery, true
}

// LinkURL is where "Open in Syncthing" links point.
func (s Server) LinkURL() string {
	if s.GUIURL != "" {
		return strings.TrimRight(s.GUIURL, "/")
	}
	return strings.TrimRight(s.URL, "/")
}

// Collector settings.
type Collector struct {
	Mode              string   `yaml:"mode" json:"mode"` // events or polled
	EventTimeout      Duration `yaml:"event_timeout" json:"event_timeout"`
	FullCheckInterval Duration `yaml:"full_check_interval" json:"full_check_interval"`
	PollInterval      Duration `yaml:"poll_interval" json:"poll_interval"`
	StructureInterval Duration `yaml:"structure_interval" json:"structure_interval"`
	BaselineInterval  Duration `yaml:"baseline_interval" json:"baseline_interval"`
	Concurrency       int      `yaml:"concurrency" json:"concurrency"`
	RequestTimeout    Duration `yaml:"request_timeout" json:"request_timeout"`
	DownAfter         int      `yaml:"down_after_failures" json:"down_after_failures"`
}

// Check overrides the defaults of one check.
type Check struct {
	Enabled  *bool     `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Urgent   *bool     `yaml:"urgent,omitempty" json:"urgent,omitempty"`
	Debounce *Duration `yaml:"debounce,omitempty" json:"debounce,omitempty"`
}

// Thresholds for time-based checks.
type Thresholds struct {
	FailingFiles    Duration `yaml:"failing_files" json:"failing_files"`       // H4 when failing files are its only reason
	StuckSync       Duration `yaml:"stuck_sync" json:"stuck_sync"`             // H5
	Paused          Duration `yaml:"paused" json:"paused"`                     // H6
	PCOffline       Duration `yaml:"pc_offline" json:"pc_offline"`             // H7
	ConflictWindow  Duration `yaml:"conflict_window" json:"conflict_window"`   // H10
	InactiveProject Duration `yaml:"inactive_project" json:"inactive_project"` // C1
	OldDevice       Duration `yaml:"old_device" json:"old_device"`             // C2
	DeadShare       Duration `yaml:"dead_share" json:"dead_share"`             // C3
}

// Structure holds the naming and placement rules.
type Structure struct {
	ProjectPattern    string          `yaml:"project_pattern" json:"project_pattern"`
	CaseInsensitive   bool            `yaml:"case_insensitive,omitempty" json:"case_insensitive,omitempty"`
	ShareTypeFromPath string          `yaml:"share_type_from_path" json:"share_type_from_path"`
	TypeTokens        []string        `yaml:"type_tokens" json:"type_tokens"`
	Placement         []PlacementRule `yaml:"placement" json:"placement"`
	IgnoreDirs        []string        `yaml:"ignore_dirs" json:"ignore_dirs"`
	NestedScanDepth   int             `yaml:"nested_scan_depth" json:"nested_scan_depth"`
}

// PlacementRule requires shares to have attributes when a name matches.
type PlacementRule struct {
	WhenNameMatches string            `yaml:"when_name_matches" json:"when_name_matches"`
	ShareMustHave   map[string]string `yaml:"share_must_have" json:"share_must_have"`
}

// Shares adds or excludes share directories per server (keyed by server ID or name).
type Shares struct {
	Add     map[string][]string `yaml:"add" json:"add"`
	Exclude map[string][]string `yaml:"exclude" json:"exclude"`
}

// Notify settings.
type Notify struct {
	DiscordWebhook string `yaml:"discord_webhook,omitempty" json:"-"` // import only
	DiscordRoleID  string `yaml:"discord_role_id" json:"discord_role_id"`
	QuietHours     string `yaml:"quiet_hours" json:"quiet_hours"`     // "22:00-07:00", "" = off
	ReminderTime   string `yaml:"reminder_time" json:"reminder_time"` // "08:00", "" = off
	WeeklyDay      string `yaml:"weekly_day" json:"weekly_day"`       // monday … sunday, "" = off
	WeeklyTime     string `yaml:"weekly_time" json:"weekly_time"`     // "08:00"
	WeeklyTop      int    `yaml:"weekly_top" json:"weekly_top"`
}

// Access settings.
type Access struct {
	Mode       string   `yaml:"mode" json:"mode"`     // builtin or proxy
	Viewer     string   `yaml:"viewer" json:"viewer"` // none, password or open
	SessionTTL Duration `yaml:"session_ttl" json:"session_ttl"`
}

// API settings. The token is import-only.
type API struct {
	Token string `yaml:"token,omitempty" json:"-"`
}

// Access modes.
const (
	AccessBuiltin = "builtin"
	AccessProxy   = "proxy"

	ViewerNone     = "none"
	ViewerPassword = "password"
	ViewerOpen     = "open"

	ModeEvents = "events"
	ModePolled = "polled"
)

// Default returns a configuration with every default filled in and no servers.
func Default() *Config {
	c := &Config{}
	c.ApplyDefaults()
	return c
}

// DefaultIgnoreDirs are skipped by the structure scan unless configured otherwise.
var DefaultIgnoreDirs = []string{".*", "@*", "#recycle", "#snapshot", "$RECYCLE.BIN", "System Volume Information", "lost+found"}

// ApplyDefaults fills zero values with defaults.
func (c *Config) ApplyDefaults() {
	if c.Timezone == "" {
		c.Timezone = "Local"
	}
	col := &c.Collector
	if col.Mode == "" {
		col.Mode = ModeEvents
	}
	setDur(&col.EventTimeout, 50*time.Second)
	setDur(&col.FullCheckInterval, 4*time.Hour)
	setDur(&col.PollInterval, 15*time.Minute)
	setDur(&col.StructureInterval, time.Hour)
	setDur(&col.BaselineInterval, 7*24*time.Hour)
	setDur(&col.RequestTimeout, 30*time.Second)
	if col.Concurrency <= 0 {
		col.Concurrency = 3
	}
	if col.DownAfter <= 0 {
		col.DownAfter = 2
	}
	t := &c.Thresholds
	setDur(&t.FailingFiles, time.Hour)
	setDur(&t.StuckSync, 24*time.Hour)
	setDur(&t.Paused, 3*24*time.Hour)
	setDur(&t.PCOffline, 7*24*time.Hour)
	setDur(&t.ConflictWindow, 7*24*time.Hour)
	setDur(&t.InactiveProject, 180*24*time.Hour)
	setDur(&t.OldDevice, 90*24*time.Hour)
	setDur(&t.DeadShare, 30*24*time.Hour)
	if c.Structure.IgnoreDirs == nil {
		c.Structure.IgnoreDirs = append([]string(nil), DefaultIgnoreDirs...)
	}
	if c.Structure.NestedScanDepth <= 0 {
		c.Structure.NestedScanDepth = 3
	}
	if c.Checks == nil {
		c.Checks = map[string]Check{}
	}
	n := &c.Notify
	if n.QuietHours == "" {
		n.QuietHours = "22:00-07:00"
	}
	if n.ReminderTime == "" {
		n.ReminderTime = "08:00"
	}
	if n.WeeklyDay == "" {
		n.WeeklyDay = "monday"
	}
	if n.WeeklyTime == "" {
		n.WeeklyTime = "08:00"
	}
	if n.WeeklyTop <= 0 {
		n.WeeklyTop = 10
	}
	if c.Access.Mode == "" {
		c.Access.Mode = AccessBuiltin
	}
	if c.Access.Viewer == "" {
		c.Access.Viewer = ViewerNone
	}
	setDur(&c.Access.SessionTTL, 30*24*time.Hour)
	if c.Shares.Add == nil {
		c.Shares.Add = map[string][]string{}
	}
	if c.Shares.Exclude == nil {
		c.Shares.Exclude = map[string][]string{}
	}
	for i := range c.Servers {
		if c.Servers[i].ID == "" {
			c.Servers[i].ID = Slug(c.Servers[i].Name)
		}
		if c.Servers[i].Name == "" {
			c.Servers[i].Name = c.Servers[i].ID
		}
	}
}

func setDur(d *Duration, def time.Duration) {
	if *d <= 0 {
		*d = Duration(def)
	}
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// Slug turns a name into a lowercase ID.
func Slug(name string) string {
	s := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if s == "" {
		s = "server"
	}
	return s
}

// Validate checks the configuration for errors.
func (c *Config) Validate() error {
	var errs []error
	ids := map[string]bool{}
	for _, s := range c.Servers {
		if s.ID == "" {
			errs = append(errs, errors.New("a server has no id or name"))
			continue
		}
		if ids[s.ID] {
			errs = append(errs, fmt.Errorf("duplicate server id %q", s.ID))
		}
		ids[s.ID] = true
		// The URL isn't a secret: it's shown, exported and sent in notices.
		u, err := url.Parse(s.URL)
		switch {
		case err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "":
			errs = append(errs, fmt.Errorf("server %s: url must be http(s)://host:port", s.ID))
		case u.User != nil:
			errs = append(errs, fmt.Errorf("server %s: url must not contain a user name or password; syncwatch authenticates with the API key only", s.ID))
		case strings.ContainsAny(s.URL, "?#"):
			errs = append(errs, fmt.Errorf("server %s: url must not contain a query (?) or fragment (#)", s.ID))
		case u.Scheme == "http" && !s.AllowHTTP:
			errs = append(errs, fmt.Errorf("server %s: an http:// URL sends the API key unencrypted; use https:// or set allow_http", s.ID))
		}
		if s.GUIURL != "" && !webURL(s.GUIURL) {
			errs = append(errs, fmt.Errorf("server %s: gui_url must be an http(s) URL without a user name or password", s.ID))
		}
	}
	if c.Collector.Mode != ModeEvents && c.Collector.Mode != ModePolled {
		errs = append(errs, fmt.Errorf("collector.mode must be %q or %q", ModeEvents, ModePolled))
	}
	if c.Collector.EventTimeout.D() < 5*time.Second || c.Collector.EventTimeout.D() > 10*time.Minute {
		errs = append(errs, errors.New("collector.event_timeout must be between 5s and 10m"))
	}
	if c.Collector.PollInterval.D() < time.Minute {
		errs = append(errs, errors.New("collector.poll_interval must be at least 1m"))
	}
	if c.Collector.FullCheckInterval.D() < 5*time.Minute {
		errs = append(errs, errors.New("collector.full_check_interval must be at least 5m"))
	}
	if c.Collector.StructureInterval.D() < time.Minute {
		errs = append(errs, errors.New("collector.structure_interval must be at least 1m"))
	}
	if c.Collector.Concurrency > 16 {
		errs = append(errs, errors.New("collector.concurrency must be at most 16"))
	}
	for id := range c.Checks {
		if _, ok := CatalogByID[id]; !ok {
			errs = append(errs, fmt.Errorf("checks: unknown check %q", id))
		}
	}
	if _, err := c.Structure.Compile(); err != nil {
		errs = append(errs, err)
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		errs = append(errs, fmt.Errorf("timezone: %v", err))
	}
	if c.Notify.QuietHours != "" && c.Notify.QuietHours != "off" {
		if _, _, err := ParseRange(c.Notify.QuietHours); err != nil {
			errs = append(errs, fmt.Errorf("notifications.quiet_hours: %v", err))
		}
	}
	if c.Notify.ReminderTime != "" && c.Notify.ReminderTime != "off" {
		if _, err := ParseClock(c.Notify.ReminderTime); err != nil {
			errs = append(errs, fmt.Errorf("notifications.reminder_time: %v", err))
		}
	}
	if c.Notify.WeeklyDay != "" && c.Notify.WeeklyDay != "off" {
		if _, err := ParseWeekday(c.Notify.WeeklyDay); err != nil {
			errs = append(errs, fmt.Errorf("notifications.weekly_day: %v", err))
		}
		if _, err := ParseClock(c.Notify.WeeklyTime); err != nil {
			errs = append(errs, fmt.Errorf("notifications.weekly_time: %v", err))
		}
	}
	if c.Notify.DiscordWebhook != "" {
		if err := CheckDiscordWebhook(c.Notify.DiscordWebhook); err != nil {
			errs = append(errs, fmt.Errorf("notifications.discord_webhook: %v", err))
		}
	}
	if c.Notify.DiscordRoleID != "" && !regexp.MustCompile(`^\d+$`).MatchString(c.Notify.DiscordRoleID) {
		errs = append(errs, errors.New("notifications.discord_role_id must be the numeric role ID"))
	}
	switch c.Access.Mode {
	case AccessBuiltin, AccessProxy:
	default:
		errs = append(errs, fmt.Errorf("access.mode must be %q or %q", AccessBuiltin, AccessProxy))
	}
	switch c.Access.Viewer {
	case ViewerNone, ViewerPassword, ViewerOpen:
	default:
		errs = append(errs, fmt.Errorf("access.viewer must be none, password or open"))
	}
	if c.PublicURL != "" && !webURL(c.PublicURL) {
		errs = append(errs, errors.New("public_url must be an http(s) URL without a user name or password"))
	}
	return errors.Join(errs...)
}

// webURL reports whether s is an absolute http(s) URL without credentials.
func webURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil
}

// AllowAnyWebhook lifts the restriction of the Discord webhook to Discord's
// own hosts, for test setups with a fake Discord. It is set from
// SYNCWATCH_ALLOW_ANY_WEBHOOK.
var AllowAnyWebhook bool

var (
	discordHosts = map[string]bool{"discord.com": true, "discordapp.com": true, "ptb.discord.com": true,
		"canary.discord.com": true, "ptb.discordapp.com": true, "canary.discordapp.com": true}
	webhookPath = regexp.MustCompile(`^/api/(v\d+/)?webhooks/[^/]+/[^/]+/?$`)
)

// CheckDiscordWebhook validates a Discord webhook URL. Only Discord's own
// hosts are accepted, so a webhook can't be pointed at an internal address.
func CheckDiscordWebhook(raw string) error {
	u, err := url.Parse(raw)
	if AllowAnyWebhook {
		if err != nil || !webURL(raw) {
			return errors.New("the Discord webhook must be an http(s) URL")
		}
		return nil
	}
	if err != nil || u.Scheme != "https" || u.User != nil || !DiscordHost(raw) || !webhookPath.MatchString(u.Path) {
		return errors.New("the Discord webhook must be a Discord webhook URL, https://discord.com/api/webhooks/…")
	}
	return nil
}

// DiscordHost reports whether a URL points at one of Discord's own hosts.
func DiscordHost(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Port() == "" && discordHosts[strings.ToLower(u.Hostname())]
}

// Location returns the configured time zone.
func (c *Config) Location() *time.Location {
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return time.Local
	}
	return loc
}

// ServerByKey finds a server by ID or by name (case-insensitive).
func (c *Config) ServerByKey(key string) (Server, bool) {
	for _, s := range c.Servers {
		if s.ID == key || strings.EqualFold(s.Name, key) || strings.EqualFold(s.ID, key) {
			return s, true
		}
	}
	return Server{}, false
}

// SharesFor returns the manual add and exclude lists for a server.
func (c *Config) SharesFor(s Server) (add, exclude []string) {
	for k, v := range c.Shares.Add {
		if k == s.ID || strings.EqualFold(k, s.Name) || strings.EqualFold(k, s.ID) {
			add = append(add, v...)
		}
	}
	for k, v := range c.Shares.Exclude {
		if k == s.ID || strings.EqualFold(k, s.Name) || strings.EqualFold(k, s.ID) {
			exclude = append(exclude, v...)
		}
	}
	return add, exclude
}

// CheckEnabled reports whether a check runs.
func (c *Config) CheckEnabled(id string) bool {
	if ch, ok := c.Checks[id]; ok && ch.Enabled != nil {
		return *ch.Enabled
	}
	return true
}

// Urgent reports whether a check sends urgent pings.
func (c *Config) Urgent(id string) bool {
	if ch, ok := c.Checks[id]; ok && ch.Urgent != nil {
		return *ch.Urgent
	}
	return CatalogByID[id].Urgent
}

// Debounce is how long an urgent finding must stay open before it is sent.
func (c *Config) Debounce(id string) time.Duration {
	if ch, ok := c.Checks[id]; ok && ch.Debounce != nil {
		return ch.Debounce.D()
	}
	return CatalogByID[id].Debounce
}

// Clone returns a deep copy (including secrets).
func (c *Config) Clone() *Config {
	b, _ := yaml.Marshal(c)
	var out Config
	_ = yaml.Unmarshal(b, &out)
	// yaml round-trip keeps import-only secrets because they are yaml-visible.
	out.ApplyDefaults()
	return &out
}

// Redacted returns a copy with every secret removed.
func (c *Config) Redacted() *Config {
	out := c.Clone()
	for i := range out.Servers {
		out.Servers[i].APIKey = ""
	}
	out.Notify.DiscordWebhook = ""
	out.API.Token = ""
	return out
}

// ParseYAML reads a configuration file. Unknown keys are errors.
func ParseYAML(b []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		if errors.Is(err, io.EOF) {
			c = Config{}
		} else {
			return nil, err
		}
	}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// ExportYAML writes the configuration without secrets.
func (c *Config) ExportYAML() ([]byte, error) {
	r := c.Redacted()
	var buf bytes.Buffer
	buf.WriteString("# syncwatch configuration export. Secrets (API keys, webhook URLs, tokens) are not included.\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(r); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ParseClock parses "HH:MM" into minutes after midnight.
func ParseClock(s string) (int, error) {
	var h, m int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d:%d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("invalid time %q, want HH:MM", s)
	}
	return h*60 + m, nil
}

// ParseRange parses "HH:MM-HH:MM".
func ParseRange(s string) (start, end int, err error) {
	parts := strings.Split(s, "-")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("invalid range %q, want HH:MM-HH:MM", s)
	}
	if start, err = ParseClock(parts[0]); err != nil {
		return
	}
	end, err = ParseClock(parts[1])
	return
}

// ParseWeekday parses an English weekday name.
func ParseWeekday(s string) (time.Weekday, error) {
	for d := time.Sunday; d <= time.Saturday; d++ {
		if strings.EqualFold(d.String(), s) || strings.EqualFold(d.String()[:3], s) {
			return d, nil
		}
	}
	return 0, fmt.Errorf("invalid weekday %q", s)
}

// SortedCheckIDs returns the catalog order of check IDs.
func SortedCheckIDs() []string {
	ids := make([]string, len(Catalog))
	for i, c := range Catalog {
		ids[i] = c.ID
	}
	return ids
}
