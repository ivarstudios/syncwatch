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
	"strconv"
	"strings"
	"time"
	"unicode"

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
	QuietHours     string `yaml:"quiet_hours" json:"quiet_hours"`     // "22:00-07:00" or "off"; "" = default
	ReminderTime   string `yaml:"reminder_time" json:"reminder_time"` // "08:00" or "off"; "" = default
	WeeklyDay      string `yaml:"weekly_day" json:"weekly_day"`       // monday … sunday or "off"; "" = default
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
	// "off" is accepted in any case; the notifier compares the lower-case form.
	for _, f := range []*string{&n.QuietHours, &n.ReminderTime, &n.WeeklyDay} {
		if strings.EqualFold(strings.TrimSpace(*f), "off") {
			*f = "off"
		}
	}
	if n.DiscordWebhook != "" {
		if u, err := CanonicalDiscordWebhook(n.DiscordWebhook); err == nil {
			n.DiscordWebhook = u
		}
	}
	// Explicit ids are reserved first, so a generated id never collides with one.
	taken := map[string]bool{}
	for _, s := range c.Servers {
		if s.ID != "" {
			taken[strings.ToLower(s.ID)] = true
		}
	}
	for i := range c.Servers {
		if c.Servers[i].ID == "" {
			c.Servers[i].ID = uniqueSlug(c.Servers[i].Name, taken)
			taken[c.Servers[i].ID] = true
		}
		if c.Servers[i].Name == "" {
			c.Servers[i].Name = c.Servers[i].ID
		}
	}
}

// setDur fills a zero duration with its default. Negative values are left
// for Validate to reject.
func setDur(d *Duration, def time.Duration) {
	if *d == 0 {
		*d = Duration(def)
	}
}

// maxIDLen is the longest server id.
const maxIDLen = 40

var (
	slugRe     = regexp.MustCompile(`[^a-z0-9]+`)
	serverIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
)

// Slug turns a name into a lowercase ID of at most maxIDLen characters.
func Slug(name string) string {
	s := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(s) > maxIDLen {
		s = strings.TrimRight(s[:maxIDLen], "-")
	}
	if s == "" {
		s = "server"
	}
	return s
}

// uniqueSlug returns Slug(name), or "<slug>-2", "<slug>-3", … when that id is taken.
func uniqueSlug(name string, taken map[string]bool) string {
	base := Slug(name)
	id := base
	for n := 2; taken[id]; n++ {
		suffix := "-" + strconv.Itoa(n)
		id = strings.TrimRight(base[:min(len(base), maxIDLen-len(suffix))], "-") + suffix
	}
	return id
}

// Validate checks the configuration for errors.
func (c *Config) Validate() error {
	var errs []error
	add := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	// Lookups by id or name ignore case, so ids and names must be unique
	// ignoring case, and a name must not be another server's id.
	seen := map[string]int{}
	for i, s := range c.Servers {
		if s.ID == "" {
			errs = append(errs, errors.New("Servers: a server has no name or ID. Give each server a name."))
			continue
		}
		if !serverIDRe.MatchString(s.ID) {
			errs = append(errs, fmt.Errorf("Servers: the server ID %q is not valid. Use 1 to 40 characters from a-z, 0-9 and the hyphen, and start with a letter or a digit.", s.ID))
		}
		for _, key := range []string{s.ID, s.Name} {
			k := strings.ToLower(key)
			if key == "" {
				continue
			}
			if j, ok := seen[k]; ok && j != i {
				errs = append(errs, fmt.Errorf("Server %s: the ID or name %q is already in use by the server %s. syncwatch compares IDs and names without case.", s.ID, key, c.Servers[j].ID))
			}
			seen[k] = i
		}
		// The URL isn't a secret: it's shown, exported and sent in notices.
		u, err := url.Parse(s.URL)
		switch {
		case err != nil || (u.Scheme != "https" && u.Scheme != "http") || !hostOK(u):
			errs = append(errs, fmt.Errorf("Server %s: the API URL (url) must be https://address:port or http://address:port.", s.ID))
		case u.User != nil:
			errs = append(errs, fmt.Errorf("Server %s: the API URL (url) must not contain a user name or a password. syncwatch uses the API key only.", s.ID))
		case strings.ContainsAny(s.URL, "?#"):
			errs = append(errs, fmt.Errorf("Server %s: the API URL (url) must not contain a question mark (?) or a number sign (#).", s.ID))
		case u.Scheme == "http" && !s.AllowHTTP:
			errs = append(errs, fmt.Errorf("Server %s: a plain http:// API URL sends the API key unencrypted. Use https://, or permit the plain URL with the checkbox (allow_http).", s.ID))
		}
		if s.GUIURL != "" && !webURL(s.GUIURL) {
			errs = append(errs, fmt.Errorf("Server %s: the GUI URL (gui_url) must be an https:// or http:// URL without a user name or a password.", s.ID))
		}
	}
	if c.Collector.Mode != ModeEvents && c.Collector.Mode != ModePolled {
		errs = append(errs, fmt.Errorf("Collector mode (collector.mode): the value must be %q or %q.", ModeEvents, ModePolled))
	}
	col := &c.Collector
	add(durRange("collector.event_timeout", col.EventTimeout, 5*time.Second, 10*time.Minute))
	add(durRange("collector.full_check_interval", col.FullCheckInterval, time.Minute, 7*24*time.Hour))
	add(durRange("collector.poll_interval", col.PollInterval, time.Minute, 24*time.Hour))
	add(durRange("collector.structure_interval", col.StructureInterval, time.Minute, 7*24*time.Hour))
	add(durRange("collector.baseline_interval", col.BaselineInterval, time.Hour, 90*24*time.Hour))
	add(durRange("collector.request_timeout", col.RequestTimeout, time.Second, 10*time.Minute))
	add(intRange("collector.concurrency", col.Concurrency, 1, 16))
	add(intRange("collector.down_after_failures", col.DownAfter, 1, 20))
	for id, ch := range c.Checks {
		if _, ok := CatalogByID[id]; !ok {
			errs = append(errs, fmt.Errorf("Checks: %q is not a check ID. Use an ID from the Checks table.", id))
		}
		if ch.Debounce != nil {
			add(durRange("checks."+id+".debounce", *ch.Debounce, 0, 30*24*time.Hour))
		}
	}
	th := &c.Thresholds
	for _, t := range []struct {
		name string
		d    Duration
	}{
		{"failing_files", th.FailingFiles}, {"stuck_sync", th.StuckSync}, {"paused", th.Paused}, {"pc_offline", th.PCOffline},
		{"conflict_window", th.ConflictWindow}, {"inactive_project", th.InactiveProject}, {"old_device", th.OldDevice}, {"dead_share", th.DeadShare},
	} {
		add(durRange("thresholds."+t.name, t.d, minThreshold, MaxDuration))
	}
	add(intRange("structure.nested_scan_depth", c.Structure.NestedScanDepth, 1, 10))
	add(intRange("notifications.weekly_top", c.Notify.WeeklyTop, 1, 50))
	add(durRange("access.session_ttl", c.Access.SessionTTL, time.Hour, 365*24*time.Hour))
	if _, err := c.Structure.Compile(); err != nil {
		errs = append(errs, err)
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		errs = append(errs, fmt.Errorf("Time zone (timezone): the value is not a time zone name (%v). Use a name such as Europe/Stockholm.", err))
	}
	if c.Notify.QuietHours != "" && c.Notify.QuietHours != "off" {
		if _, _, err := ParseRange(c.Notify.QuietHours); err != nil {
			errs = append(errs, fmt.Errorf("Quiet hours (notifications.quiet_hours): %v", err))
		}
	}
	if c.Notify.ReminderTime != "" && c.Notify.ReminderTime != "off" {
		t, err := ParseClock(c.Notify.ReminderTime)
		if err != nil {
			errs = append(errs, fmt.Errorf("Daily reminder (notifications.reminder_time): %v", err))
		} else if qs, qe, qerr := ParseRange(c.Notify.QuietHours); qerr == nil && qs != qe {
			// The reminder goes out at the first tick after its time that
			// isn't quiet, up to 12 hours later: a time inside quiet hours
			// only works when the window ends later the same day.
			quiet := (qs < qe && t >= qs && t < qe) || (qs > qe && (t >= qs || t < qe))
			if quiet && (t >= qe || qe-t > 12*60) {
				errs = append(errs, fmt.Errorf("Daily reminder (notifications.reminder_time): the time %s is inside the quiet hours %s, thus syncwatch can never send the reminder. Use a time outside the quiet hours.", c.Notify.ReminderTime, c.Notify.QuietHours))
			}
		}
	}
	if c.Notify.WeeklyDay != "" && c.Notify.WeeklyDay != "off" {
		if _, err := ParseWeekday(c.Notify.WeeklyDay); err != nil {
			errs = append(errs, fmt.Errorf("Weekly report day (notifications.weekly_day): %v", err))
		}
		if _, err := ParseClock(c.Notify.WeeklyTime); err != nil {
			errs = append(errs, fmt.Errorf("Weekly report time (notifications.weekly_time): %v", err))
		}
	}
	if c.Notify.DiscordWebhook != "" {
		if err := CheckDiscordWebhook(c.Notify.DiscordWebhook); err != nil {
			errs = append(errs, err)
		}
	}
	if c.Notify.DiscordRoleID != "" && !roleIDRe.MatchString(c.Notify.DiscordRoleID) {
		errs = append(errs, errors.New("Role ID (notifications.discord_role_id): the value must be the ID of the Discord role (17 to 20 digits)."))
	}
	switch c.Access.Mode {
	case AccessBuiltin, AccessProxy:
	default:
		errs = append(errs, fmt.Errorf("Login (access.mode): the value must be %q or %q.", AccessBuiltin, AccessProxy))
	}
	switch c.Access.Viewer {
	case ViewerNone, ViewerPassword, ViewerOpen:
	default:
		errs = append(errs, fmt.Errorf("Viewer access (access.viewer): the value must be none, password or open."))
	}
	if c.PublicURL != "" && !webURL(c.PublicURL) {
		errs = append(errs, errors.New("Dashboard URL (public_url): the value must be an https:// or http:// URL without a user name or a password."))
	}
	return errors.Join(errs...)
}

// minThreshold is the shortest threshold. The engine evaluates every 30 s,
// so anything shorter means "at once" anyway.
const minThreshold = 10 * time.Second

var roleIDRe = regexp.MustCompile(`^\d{17,20}$`)

// durRange reports a duration outside [lo, hi] as an error naming the field.
// Zero means "use the default" (ApplyDefaults fills it), so it passes.
func durRange(name string, d Duration, lo, hi time.Duration) error {
	if d != 0 && (d.D() < lo || d.D() > hi) {
		return fmt.Errorf("%s: the value must be between %s and %s.", FieldLabel(name), Duration(lo).Words(), Duration(hi).Words())
	}
	return nil
}

// intRange reports an integer outside [lo, hi] as an error naming the field.
// Zero means "use the default", so it passes.
func intRange(name string, n, lo, hi int) error {
	if n != 0 && (n < lo || n > hi) {
		return fmt.Errorf("%s: the value must be between %d and %d.", FieldLabel(name), lo, hi)
	}
	return nil
}

// hostOK reports whether a parsed URL has a host name without whitespace.
// url.Parse accepts "https://:8384" and non-ASCII spaces in the host.
func hostOK(u *url.URL) bool {
	return u.Hostname() != "" && strings.IndexFunc(u.Host, unicode.IsSpace) < 0
}

// webURL reports whether s is an absolute http(s) URL without credentials.
func webURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && hostOK(u) && u.User == nil
}

// AllowAnyWebhook lifts the restriction of the Discord webhook to Discord's
// own hosts, for test setups with a fake Discord. It is set from
// SYNCWATCH_ALLOW_ANY_WEBHOOK.
var AllowAnyWebhook bool

var (
	discordHosts = map[string]bool{"discord.com": true, "discordapp.com": true, "ptb.discord.com": true,
		"canary.discord.com": true, "ptb.discordapp.com": true, "canary.discordapp.com": true}
	webhookPath = regexp.MustCompile(`^/api/(v\d+/)?webhooks/\d+/[A-Za-z0-9_-]+/?$`)
)

// CheckDiscordWebhook validates a Discord webhook URL. Only Discord's own
// hosts are accepted, so a webhook can't be pointed at an internal address.
func CheckDiscordWebhook(raw string) error {
	_, err := CanonicalDiscordWebhook(raw)
	return err
}

// CanonicalDiscordWebhook validates a Discord webhook URL and returns the
// form to store: trimmed of whitespace and invisible format characters that
// come with a paste, lower-case host, no trailing slash and no fragment.
func CanonicalDiscordWebhook(raw string) (string, error) {
	raw = strings.TrimFunc(raw, func(r rune) bool { return unicode.IsSpace(r) || unicode.Is(unicode.Cf, r) })
	u, err := url.Parse(raw)
	if AllowAnyWebhook {
		if err != nil || !webURL(raw) {
			return "", errors.New("Discord webhook URL (notifications.discord_webhook): the value must be an https:// or http:// URL.")
		}
		return raw, nil
	}
	if err != nil || u.Scheme != "https" || u.User != nil || !DiscordHost(raw) || !webhookPath.MatchString(u.Path) {
		return "", errors.New("Discord webhook URL (notifications.discord_webhook): the value must be a webhook URL from Discord (https://discord.com/api/webhooks/…).")
	}
	u.Host = strings.ToLower(u.Host)
	u.Path, u.RawPath = strings.TrimSuffix(u.Path, "/"), ""
	u.Fragment, u.RawFragment = "", ""
	return u.String(), nil
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

// Clone returns a deep copy (including secrets). It copies field by field
// rather than through YAML, so it can't fail or drop sections.
func (c *Config) Clone() *Config {
	out := *c
	out.Servers = append([]Server(nil), c.Servers...)
	if c.Checks != nil {
		out.Checks = make(map[string]Check, len(c.Checks))
		for id, ch := range c.Checks {
			if ch.Enabled != nil {
				v := *ch.Enabled
				ch.Enabled = &v
			}
			if ch.Urgent != nil {
				v := *ch.Urgent
				ch.Urgent = &v
			}
			if ch.Debounce != nil {
				v := *ch.Debounce
				ch.Debounce = &v
			}
			out.Checks[id] = ch
		}
	}
	out.Structure.TypeTokens = cloneStrings(c.Structure.TypeTokens)
	out.Structure.IgnoreDirs = cloneStrings(c.Structure.IgnoreDirs)
	if c.Structure.Placement != nil {
		out.Structure.Placement = make([]PlacementRule, len(c.Structure.Placement))
		for i, p := range c.Structure.Placement {
			if p.ShareMustHave != nil {
				m := make(map[string]string, len(p.ShareMustHave))
				for k, v := range p.ShareMustHave {
					m[k] = v
				}
				p.ShareMustHave = m
			}
			out.Structure.Placement[i] = p
		}
	}
	out.Shares.Add = cloneLists(c.Shares.Add)
	out.Shares.Exclude = cloneLists(c.Shares.Exclude)
	out.ApplyDefaults()
	return &out
}

func cloneStrings(s []string) []string {
	if s == nil {
		return nil
	}
	return append([]string{}, s...)
}

func cloneLists(m map[string][]string) map[string][]string {
	if m == nil {
		return nil
	}
	out := make(map[string][]string, len(m))
	for k, v := range m {
		out[k] = cloneStrings(v)
	}
	return out
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
	buf.WriteString("# syncwatch configuration export. The file does not contain the API keys, the webhook URL or the API token.\n")
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(r); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

var clockRe = regexp.MustCompile(`^([01]?[0-9]|2[0-3]):([0-5][0-9])$`)

// ParseClock parses "HH:MM" (24-hour clock, leading zero optional) into
// minutes after midnight.
func ParseClock(s string) (int, error) {
	m := clockRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("the value %q is not a time. Use the form HH:MM, for example 07:30.", s)
	}
	h, _ := strconv.Atoi(m[1])
	min, _ := strconv.Atoi(m[2])
	return h*60 + min, nil
}

// ParseRange parses "HH:MM-HH:MM". The ends must differ: a range with equal
// ends has no meaning (use "off" to disable).
func ParseRange(s string) (start, end int, err error) {
	parts := strings.Split(s, "-")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("the value %q is not a time range. Use the form HH:MM-HH:MM, for example 22:00-07:00.", s)
	}
	if start, err = ParseClock(parts[0]); err != nil {
		return
	}
	if end, err = ParseClock(parts[1]); err != nil {
		return
	}
	if start == end {
		return 0, 0, fmt.Errorf("the value %q is not a time range: the start and the end are the same. Use off to set the quiet hours to off.", s)
	}
	return
}

// ParseWeekday parses an English weekday name.
func ParseWeekday(s string) (time.Weekday, error) {
	for d := time.Sunday; d <= time.Saturday; d++ {
		if strings.EqualFold(d.String(), s) || strings.EqualFold(d.String()[:3], s) {
			return d, nil
		}
	}
	return 0, fmt.Errorf("the value %q is not a weekday. Use a weekday name such as monday, or off.", s)
}

// SortedCheckIDs returns the catalog order of check IDs.
func SortedCheckIDs() []string {
	ids := make([]string, len(Catalog))
	for i, c := range Catalog {
		ids[i] = c.ID
	}
	return ids
}

// fieldLabels maps YAML keys to the labels of the Settings page, so a
// validation message names a field as the dashboard shows it. Keys without
// a label (YAML-only settings) are named by the key.
var fieldLabels = map[string]string{
	"collector.event_timeout":       "Event long-poll timeout",
	"collector.full_check_interval": "Full check every",
	"collector.poll_interval":       "Poll interval (polled mode)",
	"collector.structure_interval":  "Structure scan every",
	"collector.baseline_interval":   "Baseline scan every",
	"collector.request_timeout":     "Request timeout",
	"collector.concurrency":         "Parallel requests for each server",
	"collector.down_after_failures": "Mark unreachable after failures",
	"notifications.weekly_top":      "Findings listed in the weekly report",
	"thresholds.failing_files":      "H4 with only failed items after",
	"thresholds.stuck_sync":         "H5 stuck sync after",
	"thresholds.paused":             "H6 paused for more than",
	"thresholds.pc_offline":         "H7 PC offline for more than",
	"thresholds.conflict_window":    "H10 conflicts in the last",
	"thresholds.inactive_project":   "C1 no file change for",
	"thresholds.old_device":         "C2 device not seen for",
	"thresholds.dead_share":         "C3 folder not used by device after",
}

// FieldLabel returns "Label (key)" for a YAML key that has a label on the
// Settings page, "ID debounce (key)" for a check debounce, else the key.
func FieldLabel(key string) string {
	if l, ok := fieldLabels[key]; ok {
		return l + " (" + key + ")"
	}
	if strings.HasPrefix(key, "checks.") && strings.HasSuffix(key, ".debounce") {
		return strings.TrimSuffix(strings.TrimPrefix(key, "checks."), ".debounce") + " debounce (" + key + ")"
	}
	return key
}
