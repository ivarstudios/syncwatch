package web

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/rules"
	"github.com/ivarstudios/syncwatch/internal/settings"
	"github.com/ivarstudios/syncwatch/internal/stclient"
	"github.com/ivarstudios/syncwatch/internal/store"
)

type checkRow struct {
	config.CheckInfo
	Enabled, Urgent bool
	Debounce        string
	SeverityLabel   string
	Invalid         bool // the debounce of a refused save is not valid
}

type structureDoc struct {
	Structure config.Structure `yaml:"structure"`
	Shares    config.Shares    `yaml:"shares"`
}

func (s *Server) settingsData(info requestInfo, extra map[string]any) map[string]any {
	cfg := s.cfg.Get()
	v := s.eng.View()
	var checks []checkRow
	for _, c := range config.Catalog {
		checks = append(checks, checkRow{CheckInfo: c, Enabled: cfg.CheckEnabled(c.ID), Urgent: cfg.Urgent(c.ID),
			Debounce: config.Duration(cfg.Debounce(c.ID)).String(), SeverityLabel: c.Severity.Label()})
	}
	sdoc, _ := yaml.Marshal(structureDoc{Structure: cfg.Structure, Shares: cfg.Shares})
	log, _ := s.st.Notifications(20)
	_, hasViewer := s.setting(KeyViewerHash)
	data := map[string]any{
		"Title":         "Settings",
		"Cfg":           cfg,
		"Servers":       s.serverSettings(v),
		"Checks":        checks,
		"StructureYAML": string(sdoc),
		"HasWebhook":    s.st.HasSecret(store.SecretDiscordWebhook),
		"HasToken":      s.st.HasSecret(store.SecretAPIToken),
		"HasViewerPW":   hasViewer,
		"Log":           log,
		"Weekdays":      []string{"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday", "off"},
		"Now":           s.fmtTime(s.now()),
		"Rev":           configRev(cfg),
		"Form":          formValues(cfg),
		"Bad":           map[string]bool{},
		"Errors":        map[string]string{},
	}
	for k, val := range extra {
		data[k] = val
	}
	return data
}

// formInputs lists, for each form that refuse shows again, the inputs whose
// typed text it keeps (the rows of the checks table are in checkRow).
var formInputs = map[string][]string{
	"notifications": {"discord_webhook_clear", "discord_role_id", "quiet_hours", "reminder_time", "weekly_day", "weekly_time", "weekly_top", "timezone", "public_url"},
	"checks":        {"failing_files", "stuck_sync", "paused", "pc_offline", "conflict_window", "inactive_project", "old_device", "dead_share"},
	"collector":     {"mode", "event_timeout", "full_check_interval", "poll_interval", "structure_interval", "baseline_interval", "concurrency", "request_timeout", "down_after_failures"},
}

// formValues returns the text that each input of formInputs shows for c.
func formValues(c *config.Config) map[string]string {
	n, th, col := c.Notify, c.Thresholds, c.Collector
	return map[string]string{
		"discord_role_id": n.DiscordRoleID, "quiet_hours": n.QuietHours, "reminder_time": n.ReminderTime, "weekly_day": n.WeeklyDay,
		"weekly_time": n.WeeklyTime, "weekly_top": strconv.Itoa(n.WeeklyTop), "timezone": c.Timezone, "public_url": c.PublicURL,
		"failing_files": th.FailingFiles.String(), "stuck_sync": th.StuckSync.String(), "paused": th.Paused.String(), "pc_offline": th.PCOffline.String(),
		"conflict_window": th.ConflictWindow.String(), "inactive_project": th.InactiveProject.String(), "old_device": th.OldDevice.String(), "dead_share": th.DeadShare.String(),
		"mode": col.Mode, "event_timeout": col.EventTimeout.String(), "full_check_interval": col.FullCheckInterval.String(), "poll_interval": col.PollInterval.String(),
		"structure_interval": col.StructureInterval.String(), "baseline_interval": col.BaselineInterval.String(), "concurrency": strconv.Itoa(col.Concurrency),
		"request_timeout": col.RequestTimeout.String(), "down_after_failures": strconv.Itoa(col.DownAfter),
	}
}

// refuse answers a refused save of the Notifications, Checks or Collector
// form with the settings page itself (HTTP 422) instead of a redirect, so
// the form keeps each value that was typed: a value such as "3 days" cannot
// go through the configuration. The page shows the configuration old with
// its revision (a newer one makes the next save stale), the inputs of
// section show the posted text, msg shows above the Save button of section
// and the inputs in bad are marked.
func (s *Server) refuse(w http.ResponseWriter, r *http.Request, info requestInfo, old *config.Config, section, msg string, bad map[string]bool) {
	d := s.settingsData(info, nil)
	form := d["Form"].(map[string]string)
	for _, name := range formInputs[section] {
		form[name] = r.FormValue(name)
	}
	if section == "checks" {
		rows := d["Checks"].([]checkRow)
		for i := range rows {
			id := rows[i].ID
			rows[i].Enabled = r.FormValue("enabled_"+id) == "on"
			rows[i].Urgent = r.FormValue("urgent_"+id) == "on"
			rows[i].Debounce = r.FormValue("debounce_" + id)
			rows[i].Invalid = bad["debounce_"+id]
		}
	}
	d["Rev"], d["Bad"], d["Errors"] = configRev(old), bad, map[string]string{section: msg}
	s.render(w, r, http.StatusUnprocessableEntity, "settings", info, d)
}

// keyRe finds the YAML keys that config.FieldLabel puts after the labels in
// a validation message: "H7 PC offline for more than (thresholds.pc_offline)".
var keyRe = regexp.MustCompile(`\(([a-z_]+(?:\.[A-Za-z0-9_]+)*)\)`)

// badInputs returns the form inputs that a validation error names.
func badInputs(err error) map[string]bool {
	bad := map[string]bool{}
	for _, m := range keyRe.FindAllStringSubmatch(err.Error(), -1) {
		k := m[1]
		if id, ok := strings.CutPrefix(k, "checks."); ok {
			bad["debounce_"+strings.TrimSuffix(id, ".debounce")] = true
			continue
		}
		bad[k[strings.LastIndex(k, ".")+1:]] = true
	}
	return bad
}

func (s *Server) setting(key string) (string, bool) {
	v, err := s.st.Setting(key)
	return v, err == nil && v != ""
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request, info requestInfo) {
	s.render(w, r, http.StatusOK, "settings", info, s.settingsData(info, nil))
}

// configRev identifies the configuration a settings page was rendered from.
func configRev(c *config.Config) string {
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:6])
}

// stale refuses a form from a page that was loaded before the configuration
// last changed, so one admin's save doesn't silently undo another's. Forms
// without a revision (scripts) are taken as they are.
func (s *Server) stale(w http.ResponseWriter, r *http.Request, old *config.Config, section string) bool {
	if rev := r.FormValue("rev"); rev == "" || rev == configRev(old) {
		return false
	}
	s.redirect(w, r, "/settings#"+section, "Not saved. The settings changed after this page was loaded. Reload the page and make the change again.", true)
	return true
}

// save applies a configuration change, announces what changed and redirects
// back to a settings section. When the change is refused, it writes no
// answer and returns the error.
func (s *Server) save(w http.ResponseWriter, r *http.Request, old, c *config.Config, section, title, okMsg string) error {
	if err := s.hooks.SaveConfig(c); err != nil {
		return err
	}
	if ch := configChanges(old, c); len(ch) > 0 {
		s.announce(r, title, strings.Join(ch, "\n"))
	}
	s.redirect(w, r, "/settings#"+section, okMsg, false)
	return nil
}

// announce logs a security-relevant settings change and sends it as a notice.
func (s *Server) announce(r *http.Request, title, summary string) {
	s.announcer(r)(title, summary)
}

// announcer captures the notification channels configured now and returns a
// function that announces a change to them. A change that replaces or
// removes a channel is then still heard by the channel it affects.
func (s *Server) announcer(r *http.Request) func(title, summary string) {
	send := s.hooks.Notice
	if s.hooks.NoticeLater != nil {
		send = s.hooks.NoticeLater()
	}
	ip := clientIP(r)
	return func(title, summary string) {
		s.log.Warn("settings changed", "change", title, "detail", summary, "ip", ip)
		if send != nil {
			send(title, summary+" (from "+ip+")")
		}
	}
}

// confirmed checks the "confirm" field of a form against the admin password
// or the server's stored API key. Accepting a new certificate or forgetting a
// pin can send the stored key to a different machine, so an admin session
// alone isn't enough. Failures count towards the login rate limit.
func (s *Server) confirmed(r *http.Request, serverID string) bool {
	v := r.FormValue("confirm")
	if v == "" {
		return false
	}
	ok, _ := s.limitedCheck(r, func() bool {
		if h, _ := s.st.Setting(KeyAdminHash); h != "" && CheckPassword(v, h) {
			return true
		}
		key, _, err := s.st.ServerKey(serverID)
		return err == nil && key != "" && subtle.ConstantTimeCompare([]byte(v), []byte(key)) == 1
	})
	return ok
}

// limitedCheck runs a password check under the login rate limit, so an
// admin session alone can't be used to guess the admin password. It reports
// whether the check passed, and limited when the client address is over the
// limit (the check isn't run then).
func (s *Server) limitedCheck(r *http.Request, check func() bool) (ok, limited bool) {
	ip := clientIP(r)
	allowed, delay := s.limiter.check(ip, s.now())
	if !allowed {
		s.log.Warn("password check rate limit reached", "ip", ip, "path", r.URL.Path)
		return false, true
	}
	wait(r, delay)
	if check() {
		s.limiter.success(ip, RoleAdmin)
		return true, false
	}
	s.limiter.fail(ip, s.now(), RoleAdmin)
	return false, false
}

func (s *Server) handleServerSave(w http.ResponseWriter, r *http.Request, info requestInfo) {
	cur := s.cfg.Get()
	if s.stale(w, r, cur, "servers") {
		return
	}
	c := cur.Clone()
	orig := r.FormValue("orig_id")
	name := strings.TrimSpace(r.FormValue("name"))
	key := strings.TrimSpace(r.FormValue("api_key"))
	sv := config.Server{
		Name:      name,
		URL:       strings.TrimRight(strings.TrimSpace(r.FormValue("url")), "/"),
		GUIURL:    strings.TrimRight(strings.TrimSpace(r.FormValue("gui_url")), "/"),
		Disabled:  r.FormValue("disabled") == "on",
		AllowHTTP: r.FormValue("allow_http") == "on",
	}
	if name == "" {
		s.redirect(w, r, "/settings#servers", "A server needs a name.", true)
		return
	}
	var old config.Server
	idx := -1
	if orig != "" {
		for i, x := range c.Servers {
			if x.ID == orig {
				idx = i
			}
		}
		if idx < 0 {
			s.redirect(w, r, "/settings#servers", "The server does not exist. Reload the page.", true)
			return
		}
		old = c.Servers[idx]
		sv.ID = orig
		c.Servers[idx] = sv
	} else {
		base := config.Slug(name)
		id := base
		for n := 2; ; n++ {
			if _, ok := c.ServerByKey(id); !ok {
				break
			}
			id = fmt.Sprintf("%s-%d", base, n)
		}
		sv.ID = id
		c.Servers = append(c.Servers, sv)
	}
	if err := c.Validate(); err != nil {
		s.redirect(w, r, "/settings#servers", "Not saved. "+sentence(err.Error()), true)
		return
	}
	moved := orig != "" && !config.SameEndpoint(old.URL, sv.URL)
	resetPin := r.FormValue("reset_pin") == "on"
	switch {
	case orig == "" && key == "":
		s.redirect(w, r, "/settings#servers", "A new server needs its API key. Type the API key before you save.", true)
		return
	case key == "" && moved:
		// The stored key is only ever sent to the URL it was entered for.
		s.redirect(w, r, "/settings#servers", "The API URL changed. Type the API key again for "+sv.URL+". "+keyOnlyToURL, true)
		return
	case resetPin && key == "" && !s.confirmed(r, sv.ID):
		s.redirect(w, r, "/settings#servers", "To remove the pinned certificate, type the administrator password or this server's API key.", true)
		return
	}
	if key != "" {
		// A key for a new URL also forgets the old URL's pinned certificate.
		if err := settings.SetServerKey(s.st, sv.ID, key, sv.URL); err != nil {
			s.redirect(w, r, "/settings#servers", "syncwatch cannot save the API key: "+err.Error(), true)
			return
		}
	}
	if err := s.hooks.SaveConfig(c); err != nil {
		s.redirect(w, r, "/settings#servers", "Not saved. "+sentence(err.Error()), true)
		return
	}
	if resetPin && !moved && s.hooks.ResetPin != nil {
		if err := s.hooks.ResetPin(sv.ID); err != nil {
			s.redirect(w, r, "/settings#servers", "syncwatch cannot remove the pinned certificate: "+err.Error(), true)
			return
		}
	}
	if orig == "" {
		s.announce(r, "Server "+name+" added", "The API URL is "+sv.URL+".")
	} else {
		// Disabling a server or changing its name or URL changes what is
		// checked and what the messages say, so every change is listed.
		changes := configChanges(cur, c)
		if key != "" {
			changes = append(changes, "The API key is replaced.")
		}
		if resetPin && !moved {
			changes = append(changes, "The pinned certificate is removed. syncwatch accepts and pins the next certificate from "+sv.URL+" (trust on first use).")
		}
		if len(changes) > 0 {
			s.announce(r, "Server "+name+" changed", strings.Join(changes, "\n"))
		}
	}
	s.redirect(w, r, "/settings#servers", "Server "+name+" is saved.", false)
}

func (s *Server) handleServerDelete(w http.ResponseWriter, r *http.Request, info requestInfo) {
	id := r.PathValue("id")
	old := s.cfg.Get()
	if s.stale(w, r, old, "servers") {
		return
	}
	c := old.Clone()
	var keep []config.Server
	name := ""
	for _, x := range c.Servers {
		if x.ID != id {
			keep = append(keep, x)
		} else {
			name = x.Name
		}
	}
	if name == "" {
		s.redirect(w, r, "/settings#servers", "The server does not exist. Reload the page.", true)
		return
	}
	c.Servers = keep
	if err := s.hooks.SaveConfig(c); err != nil {
		s.redirect(w, r, "/settings#servers", "The server is not removed: "+err.Error(), true)
		return
	}
	_ = s.st.DeleteSecret(store.ServerAPIKeySecret(id))
	_ = s.st.SetPin(id, "")
	s.announce(r, "Server "+name+" removed", "syncwatch deleted its API key and pinned certificate.")
	s.redirect(w, r, "/settings#servers", "The server is removed.", false)
}

func (s *Server) handleAcceptCert(w http.ResponseWriter, r *http.Request, info requestInfo) {
	id := r.PathValue("id")
	if !s.confirmed(r, id) {
		s.redirect(w, r, "/settings#servers", "To accept the new certificate, type the administrator password or this server's API key.", true)
		return
	}
	fp := r.FormValue("fingerprint")
	if err := s.hooks.AcceptCert(id, fp); err != nil {
		s.redirect(w, r, "/settings#servers", err.Error(), true)
		return
	}
	name := id
	if sv, ok := s.cfg.Get().ServerByKey(id); ok {
		name = sv.Name
	}
	s.announce(r, "Server "+name+": new certificate accepted", "The pinned SHA-256 fingerprint is "+stclient.ShortFP(fp)+".")
	s.redirect(w, r, "/settings#servers", "The new certificate is accepted. syncwatch reconnects to the server.", false)
}

func (s *Server) handleNotifySave(w http.ResponseWriter, r *http.Request, info requestInfo) {
	old := s.cfg.Get()
	if s.stale(w, r, old, "notifications") {
		return
	}
	c := old.Clone()
	n := &c.Notify
	n.DiscordRoleID = strings.TrimSpace(strings.TrimPrefix(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(r.FormValue("discord_role_id")), "<@&"), ">"), "@"))
	n.QuietHours = strings.TrimSpace(r.FormValue("quiet_hours"))
	if n.QuietHours == "" {
		n.QuietHours = "off"
	}
	n.ReminderTime = strings.TrimSpace(r.FormValue("reminder_time"))
	if n.ReminderTime == "" {
		n.ReminderTime = "off"
	}
	n.WeeklyDay = r.FormValue("weekly_day")
	n.WeeklyTime = strings.TrimSpace(r.FormValue("weekly_time"))
	if top, err := strconv.Atoi(r.FormValue("weekly_top")); err == nil {
		n.WeeklyTop = top // out of range: Validate names the field and the range
	}
	c.Timezone = strings.TrimSpace(r.FormValue("timezone"))
	c.PublicURL = strings.TrimRight(strings.TrimSpace(r.FormValue("public_url")), "/")
	clearHook, hook := r.FormValue("discord_webhook_clear") == "on", strings.TrimSpace(r.FormValue("discord_webhook"))
	typedHook := hook != ""
	refused := func(err error) {
		msg := "Not saved. " + sentence(err.Error())
		if typedHook {
			msg += "\nType the Discord webhook URL again." // the page does not show it again
		}
		s.refuse(w, r, info, old, "notifications", msg, badInputs(err))
	}
	if err := c.Validate(); err != nil {
		refused(err)
		return
	}
	if !clearHook && hook != "" {
		var err error
		if hook, err = config.CanonicalDiscordWebhook(hook); err != nil {
			refused(err)
			return
		}
	}

	// Changes that can keep problems from being announced are announced
	// to the channels configured before them.
	announce := s.announcer(r)
	changes := configChanges(old, c)
	channels := false
	hadHook := s.st.HasSecret(store.SecretDiscordWebhook)
	switch {
	case clearHook && hadHook:
		_ = s.st.DeleteSecret(store.SecretDiscordWebhook)
		changes = append(changes, "The Discord webhook is removed. syncwatch does not send notifications to Discord now.")
		channels = true
	case !clearHook && hook != "":
		_ = s.st.SetSecret(store.SecretDiscordWebhook, hook)
		changes = append(changes, replacedOrAdded(hadHook, "Discord webhook"))
		channels = true
	}
	if channels {
		s.channelsChanged()
	}
	if err := s.hooks.SaveConfig(c); err != nil {
		s.redirect(w, r, "/settings#notifications", "Not saved. "+sentence(err.Error()), true)
		return
	}
	if len(changes) > 0 {
		announce("Notification settings changed", strings.Join(changes, "\n"))
	}
	s.redirect(w, r, "/settings#notifications", "The notification settings are saved.", false)
}

// replacedOrAdded words a stored secret's change as a notice line.
func replacedOrAdded(had bool, what string) string {
	if had {
		return "The " + what + " is replaced."
	}
	return "The " + what + " is added."
}

// keyOnlyToURL is the sentence that explains why a changed API URL needs the
// API key again; the Servers section and the import flash use the same one.
const keyOnlyToURL = "syncwatch sends an API key only to the URL that you typed it for."

// sentence makes a machine text (a parser or store error) readable after a
// flash prefix: a capital first letter and a final period on every line.
func sentence(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		r, n := utf8.DecodeRuneInString(l)
		l = string(unicode.ToUpper(r)) + l[n:]
		if !strings.HasSuffix(l, ".") && !strings.HasSuffix(l, "!") && !strings.HasSuffix(l, "?") {
			l += "."
		}
		lines[i] = l
	}
	return strings.Join(lines, "\n")
}

// maxChangeLines keeps a notice well inside Discord's description limit.
const maxChangeLines = 40

// configChanges lists what differs between two configurations, one line per
// changed setting with its old and new value, so a notice shows what a save
// did. Changes that matter for who hears about problems are worded. Secrets
// are kept outside the configuration and never appear.
func configChanges(old, c *config.Config) []string {
	var out []string
	before := map[string]config.Server{}
	for _, sv := range old.Servers {
		before[sv.ID] = sv
	}
	for _, sv := range c.Servers {
		o, ok := before[sv.ID]
		if !ok {
			out = append(out, "The server "+sv.Name+" is added (API URL "+sv.URL+").")
			continue
		}
		delete(before, sv.ID)
		name := "Server " + sv.Name
		switch {
		case !config.SameEndpoint(o.URL, sv.URL):
			out = append(out, "The API URL of the server "+sv.Name+" changed from "+o.URL+" to "+sv.URL+".")
		case o.URL != sv.URL:
			out = append(out, "The API URL of the server "+sv.Name+" is now "+sv.URL+" (before: "+o.URL+").")
		}
		if o.Name != sv.Name {
			out = append(out, "The server "+o.Name+" has the new name "+sv.Name+".")
		}
		if o.Disabled != sv.Disabled {
			if sv.Disabled {
				out = append(out, "The server "+sv.Name+" is disabled. syncwatch does not poll it now.")
			} else {
				out = append(out, "The server "+sv.Name+" is enabled.")
			}
		}
		if o.GUIURL != sv.GUIURL {
			out = append(out, "The GUI URL of the server "+sv.Name+" changed from "+orNone(o.GUIURL)+" to "+orNone(sv.GUIURL)+".")
		}
		if o.AllowHTTP != sv.AllowHTTP {
			out = append(out, fmt.Sprintf("%s: allow_http changed from %v to %v.", name, o.AllowHTTP, sv.AllowHTTP))
		}
	}
	for _, sv := range old.Servers {
		if _, gone := before[sv.ID]; gone {
			out = append(out, "The server "+sv.Name+" is removed.")
		}
	}
	for _, ci := range config.Catalog {
		name := ci.ID + " (" + ci.Title + ")"
		if was, now := old.CheckEnabled(ci.ID), c.CheckEnabled(ci.ID); was != now {
			if now {
				out = append(out, "The check "+name+" is enabled.")
			} else {
				out = append(out, "The check "+name+" is disabled.")
			}
		}
		if was, now := old.Urgent(ci.ID), c.Urgent(ci.ID); was != now {
			if now {
				out = append(out, "The check "+name+" now sends urgent messages.")
			} else {
				out = append(out, "The check "+name+" does not send urgent messages now.")
			}
		}
		if was, now := old.Debounce(ci.ID), c.Debounce(ci.ID); was != now {
			out = append(out, fmt.Sprintf("The check %s waits %s instead of %s before an urgent message.", name, rules.Ago(now), rules.Ago(was)))
		}
	}
	was, now := flatConfig(old), flatConfig(c)
	keys := map[string]bool{}
	for k := range was {
		keys[k] = true
	}
	for k := range now {
		keys[k] = true
	}
	for _, k := range slices.Sorted(maps.Keys(keys)) {
		if strings.HasPrefix(k, "servers") || strings.HasPrefix(k, "checks") {
			continue // described above
		}
		a, b := orNone(was[k]), orNone(now[k])
		// A list of rules has no value of its own once it has entries.
		if a == b || a == "[]" && b == "(none)" || a == "(none)" && b == "[]" {
			continue
		}
		out = append(out, describe(k, a, b))
	}
	if len(out) > maxChangeLines {
		out = append(out[:maxChangeLines], fmt.Sprintf("…and %d more changes.", len(out)-maxChangeLines))
	}
	return out
}

// describe words one changed setting. Settings that decide who hears about
// problems say what the change does.
func describe(key, was, now string) string {
	switch key {
	case "access.mode":
		if now == config.AccessProxy {
			return "Built-in login is off (proxy mode). Each person who can reach syncwatch is an administrator."
		}
		return "Built-in login is on."
	case "access.viewer":
		return "Viewer access without the administrator password changed from " + viewerText(was) + " to " + viewerText(now) + "."
	case "notifications.discord_role_id":
		switch {
		case now == "(none)":
			return "Urgent messages do not mention a role now (before: " + was + ")."
		case was == "(none)":
			return "Urgent messages now mention the role " + now + "."
		}
		return "Urgent messages now mention the role " + now + " (before: " + was + ")."
	}
	return key + " changed from " + was + " to " + now + "."
}

// viewerText is the option text of the Viewer access field for a value.
func viewerText(v string) string {
	switch v {
	case config.ViewerNone:
		return "administrator only"
	case config.ViewerPassword:
		return "with the viewer password"
	case config.ViewerOpen:
		return "open, without login"
	}
	return v
}

// flatConfig renders a configuration as the keys of its YAML form with their
// values: nested keys joined with dots, a list of values as one value, and
// secrets removed.
func flatConfig(c *config.Config) map[string]string {
	out := map[string]string{}
	b, _ := yaml.Marshal(c.Redacted())
	var doc map[string]any
	_ = yaml.Unmarshal(b, &doc)
	flatten("", doc, out)
	return out
}

func flatten(key string, v any, out map[string]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, item := range x {
			if key != "" {
				k = key + "." + k
			}
			flatten(k, item, out)
		}
	case []any:
		var items []string
		for i, item := range x {
			if _, ok := item.(map[string]any); ok {
				flatten(fmt.Sprintf("%s[%d]", key, i), item, out)
				continue
			}
			items = append(items, orNone(fmt.Sprint(item)))
		}
		if len(items) == len(x) {
			out[key] = "[" + strings.Join(items, ", ") + "]"
		}
	case nil:
		out[key] = ""
	default:
		out[key] = fmt.Sprint(v)
	}
}

// orNone shows an empty value as "(none)".
func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// channelsChanged tells the notifier to retry held-back messages at once.
func (s *Server) channelsChanged() {
	if s.hooks.ChannelsChanged != nil {
		s.hooks.ChannelsChanged()
	}
}

func (s *Server) handleNotifyTest(w http.ResponseWriter, r *http.Request, info requestInfo) {
	if err := s.hooks.SendTest(r.Context()); err != nil {
		s.redirect(w, r, "/settings#notifications", "The test message failed. "+sentence(err.Error()), true)
		return
	}
	s.redirect(w, r, "/settings#notifications", "syncwatch sent the test message.", false)
}

func (s *Server) handleChecksSave(w http.ResponseWriter, r *http.Request, info requestInfo) {
	old := s.cfg.Get()
	if s.stale(w, r, old, "checks") {
		return
	}
	c := old.Clone()
	c.Checks = map[string]config.Check{}
	var errs []string
	bad := map[string]bool{}
	for _, ci := range config.Catalog {
		var ch config.Check
		en := r.FormValue("enabled_"+ci.ID) == "on"
		if !en {
			ch.Enabled = &en
		}
		ur := r.FormValue("urgent_"+ci.ID) == "on"
		if ur != ci.Urgent {
			ch.Urgent = &ur
		}
		if d := strings.TrimSpace(r.FormValue("debounce_" + ci.ID)); d != "" {
			dv, err := config.ParseDuration(d)
			if err != nil {
				errs = append(errs, ci.ID+" debounce: "+err.Error())
				bad["debounce_"+ci.ID] = true
			} else if dv.D() != ci.Debounce {
				ch.Debounce = &dv
			}
		}
		if ch.Enabled != nil || ch.Urgent != nil || ch.Debounce != nil {
			c.Checks[ci.ID] = ch
		}
	}
	th := &c.Thresholds
	durations := map[string]*config.Duration{
		"failing_files": &th.FailingFiles, "stuck_sync": &th.StuckSync, "paused": &th.Paused, "pc_offline": &th.PCOffline, "conflict_window": &th.ConflictWindow,
		"inactive_project": &th.InactiveProject, "old_device": &th.OldDevice, "dead_share": &th.DeadShare,
	}
	for _, name := range formInputs["checks"] { // in the order of the form
		d, err := config.ParseDuration(r.FormValue(name))
		if err != nil || d <= 0 {
			errs = append(errs, fieldLabel(name)+": the value must be a duration of more than 0. Use a number and a unit, for example 30s, 15m, 24h, 3d or 2w.")
			bad[name] = true
			continue
		}
		*durations[name] = d
	}
	if len(errs) > 0 {
		s.refuse(w, r, info, old, "checks", "Not saved. "+strings.Join(errs, "\n"), bad)
		return
	}
	if err := s.save(w, r, old, c, "checks", "Checks changed", "The checks are saved."); err != nil {
		s.refuse(w, r, info, old, "checks", "Not saved. "+sentence(err.Error()), badInputs(err))
	}
}

func (s *Server) handleStructureSave(w http.ResponseWriter, r *http.Request, info requestInfo) {
	var doc structureDoc
	dec := yaml.NewDecoder(strings.NewReader(r.FormValue("yaml")))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil && err != io.EOF {
		s.redirect(w, r, "/settings#structure", "Not saved. "+sentence(err.Error()), true)
		return
	}
	old := s.cfg.Get()
	if s.stale(w, r, old, "structure") {
		return
	}
	c := old.Clone()
	c.Structure, c.Shares = doc.Structure, doc.Shares
	if c.Structure.IgnoreDirs == nil {
		// Left out of the YAML: keep the current list rather than start
		// scanning recycle bins and snapshots. An explicit [] clears it.
		c.Structure.IgnoreDirs = old.Structure.IgnoreDirs
	}
	if err := s.save(w, r, old, c, "structure", "Structure rules changed", "The structure rules are saved. The next structure scan uses them."); err != nil {
		s.redirect(w, r, "/settings#structure", "Not saved. "+sentence(err.Error()), true)
	}
}

func (s *Server) handleCollectorSave(w http.ResponseWriter, r *http.Request, info requestInfo) {
	old := s.cfg.Get()
	if s.stale(w, r, old, "collector") {
		return
	}
	c := old.Clone()
	col := &c.Collector
	col.Mode = r.FormValue("mode")
	var errs []string
	bad := map[string]bool{}
	durations := map[string]*config.Duration{
		"event_timeout": &col.EventTimeout, "full_check_interval": &col.FullCheckInterval, "poll_interval": &col.PollInterval,
		"structure_interval": &col.StructureInterval, "baseline_interval": &col.BaselineInterval, "request_timeout": &col.RequestTimeout,
	}
	for _, name := range formInputs["collector"] { // in the order of the form
		dst, ok := durations[name]
		if !ok {
			continue
		}
		d, err := config.ParseDuration(r.FormValue(name))
		if err != nil || d <= 0 {
			errs = append(errs, fieldLabel(name)+": the value must be a duration of more than 0. Use a number and a unit, for example 30s, 15m, 24h, 3d or 2w.")
			bad[name] = true
			continue
		}
		*dst = d
	}
	if n, err := strconv.Atoi(r.FormValue("concurrency")); err == nil {
		col.Concurrency = n
	}
	if n, err := strconv.Atoi(r.FormValue("down_after_failures")); err == nil {
		col.DownAfter = n // out of range: Validate names the field and the range
	}
	if len(errs) > 0 {
		s.refuse(w, r, info, old, "collector", "Not saved. "+strings.Join(errs, "\n"), bad)
		return
	}
	if err := s.save(w, r, old, c, "collector", "Collector settings changed", "The collector settings are saved. syncwatch restarted the collectors."); err != nil {
		s.refuse(w, r, info, old, "collector", "Not saved. "+sentence(err.Error()), badInputs(err))
	}
}

func (s *Server) handleAccessSave(w http.ResponseWriter, r *http.Request, info requestInfo) {
	old := s.cfg.Get()
	if s.stale(w, r, old, "access") {
		return
	}
	c := old.Clone()
	mode := r.FormValue("mode")
	if mode == config.AccessProxy && c.Access.Mode != config.AccessProxy && r.FormValue("confirm_proxy") != "on" {
		s.redirect(w, r, "/settings#access", "Select the checkbox to set built-in login to off.", true)
		return
	}
	c.Access.Mode = mode
	c.Access.Viewer = r.FormValue("viewer")
	viewerPW := r.FormValue("viewer_password")
	if viewerPW != "" {
		if err := checkNewPassword(viewerPW); err != nil {
			s.redirect(w, r, "/settings#access", err.Error(), true)
			return
		}
	}
	if c.Access.Viewer == config.ViewerPassword && viewerPW == "" {
		if _, ok := s.setting(KeyViewerHash); !ok {
			s.redirect(w, r, "/settings#access", "Type a viewer password first. The option 'with the viewer password' needs one.", true)
			return
		}
	}
	if c.Access.Mode == config.AccessBuiltin && !s.adminConfigured() {
		s.redirect(w, r, "/settings#access", "Set an administrator password first. Then set built-in login to on.", true)
		return
	}
	// A new viewer password lets someone else in, too.
	newViewerPW := viewerPW != "" && c.Access.Mode == config.AccessBuiltin && c.Access.Viewer == config.ViewerPassword
	if weakensAccess(old, c) || newViewerPW {
		if msg := s.confirmAdmin(r); msg != "" {
			s.redirect(w, r, "/settings#access", msg, true)
			return
		}
	}
	if viewerPW != "" {
		h, err := HashPassword(viewerPW)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		_ = s.st.SetSetting(KeyViewerHash, h)
		_ = s.st.DeleteSessions(RoleViewer)
	}
	if err := s.hooks.SaveConfig(c); err != nil {
		s.redirect(w, r, "/settings#access", "Not saved. "+sentence(err.Error()), true)
		return
	}
	changes := configChanges(old, c)
	if newViewerPW {
		changes = append(changes, "The new viewer password is set. syncwatch logged out all viewers.")
	}
	if len(changes) > 0 {
		s.announce(r, "Access settings changed", strings.Join(changes, "\n"))
	}
	s.redirect(w, r, "/settings#access", "The access settings are saved.", false)
}

var viewerRank = map[string]int{config.ViewerNone: 0, config.ViewerPassword: 1, config.ViewerOpen: 2}

// weakensAccess reports whether c lets more people in than old: built-in
// login switched off, or viewing opened up (none → password → open).
func weakensAccess(old, c *config.Config) bool {
	switch {
	case old.Access.Mode == config.AccessProxy:
		return false // everyone who could reach it was admin already
	case c.Access.Mode == config.AccessProxy:
		return true
	}
	return viewerRank[c.Access.Viewer] > viewerRank[old.Access.Viewer]
}

// confirmAdmin checks the admin password in the "confirm" field, which a
// change that weakens access needs: an admin session alone isn't enough.
// It returns "" when confirmed, otherwise the message to show. With no admin
// password set (proxy mode) there's nothing to confirm with.
func (s *Server) confirmAdmin(r *http.Request) string {
	h, _ := s.setting(KeyAdminHash)
	if h == "" {
		return ""
	}
	const need = "This change permits access to more people. Type the administrator password to accept the change."
	pw := r.FormValue("confirm")
	if pw == "" {
		return need
	}
	ok, limited := s.limitedCheck(r, func() bool { return CheckPassword(pw, h) })
	switch {
	case limited:
		return errTooManyLogins
	case !ok:
		return "The administrator password is not correct. " + need
	}
	return ""
}

func (s *Server) handlePasswordSave(w http.ResponseWriter, r *http.Request, info requestInfo) {
	if h, _ := s.setting(KeyAdminHash); h != "" {
		ok, limited := s.limitedCheck(r, func() bool { return CheckPassword(r.FormValue("current"), h) })
		switch {
		case limited:
			s.redirect(w, r, "/settings#access", errTooManyLogins, true)
			return
		case !ok:
			s.redirect(w, r, "/settings#access", "The current password is not correct.", true)
			return
		}
	}
	pw := r.FormValue("password")
	if err := checkNewPassword(pw); err != nil {
		s.redirect(w, r, "/settings#access", err.Error(), true)
		return
	}
	if pw != r.FormValue("password2") {
		s.redirect(w, r, "/settings#access", "The new passwords are not the same.", true)
		return
	}
	nh, err := HashPassword(pw)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	_ = s.st.SetSetting(KeyAdminHash, nh)
	_ = s.st.DeleteSessions(RoleAdmin)
	if s.cfg.Get().Access.Mode == config.AccessBuiltin {
		_ = s.startSession(w, r, RoleAdmin)
	}
	s.announce(r, "Administrator password changed", "syncwatch logged out the other administrator sessions.")
	s.redirect(w, r, "/settings#access", "The administrator password is changed. syncwatch logged out the other administrator sessions.", false)
}

func (s *Server) handleAPIToken(w http.ResponseWriter, r *http.Request, info requestInfo) {
	had := s.st.HasSecret(store.SecretAPIToken)
	if r.FormValue("op") == "revoke" {
		_ = s.st.DeleteSecret(store.SecretAPIToken)
		if had {
			s.announce(r, "API token revoked", "syncwatch refuses requests with the old API token.")
		}
		s.redirect(w, r, "/settings#api", "The API token is revoked.", false)
		return
	}
	tok := "swt_" + randomToken(24)
	if err := s.st.SetSecret(store.SecretAPIToken, tok); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	title, summary := "API token generated", "The API token gives read access to /api/v1/status and /api/v1/ha."
	if had {
		title, summary = "API token replaced", summary+" syncwatch refuses the old API token now."
	}
	s.announce(r, title, summary)
	s.render(w, r, http.StatusOK, "settings", info, s.settingsData(info, map[string]any{"NewToken": tok}))
}

func (s *Server) handleExport(w http.ResponseWriter, r *http.Request, info requestInfo) {
	b, err := s.cfg.Get().ExportYAML()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="syncwatch-%s.yaml"`, s.now().In(s.cfg.Get().Location()).Format("2006-01-02")))
	_, _ = w.Write(b)
}

func (s *Server) handleImport(w http.ResponseWriter, r *http.Request, info requestInfo) {
	var data []byte
	if f, _, err := r.FormFile("file"); err == nil {
		defer f.Close()
		data, _ = io.ReadAll(io.LimitReader(f, 1<<20))
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		data = []byte(r.FormValue("yaml"))
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		s.redirect(w, r, "/settings#import", "Select a YAML file or paste the YAML.", true)
		return
	}
	if s.stale(w, r, s.cfg.Get(), "import") {
		return
	}
	c, err := config.ParseYAML(data)
	if err != nil {
		s.redirect(w, r, "/settings#import", "Import failed. "+sentence(err.Error()), true)
		return
	}
	old := s.cfg.Get()
	// Checked before anything from the file is stored. Built-in login with
	// no admin password would send everyone to a setup page whose token was
	// never logged (the process started in proxy mode).
	if c.Access.Mode == config.AccessBuiltin && !s.adminConfigured() {
		s.redirect(w, r, "/settings#import", "Not imported. The file sets built-in login to on, but no administrator password is set. Set an administrator password in Settings → Access first.", true)
		return
	}
	if weakensAccess(old, c) {
		if msg := s.confirmAdmin(r); msg != "" {
			s.redirect(w, r, "/settings#import", "Not imported. The file sets login to off or gives viewer access to more people. "+msg, true)
			return
		}
	}
	// Announced to the channels configured before the file replaces any.
	announce := s.announcer(r)
	changes := configChanges(old, c)
	for _, sv := range c.Servers {
		if sv.APIKey != "" {
			changes = append(changes, replacedOrAdded(s.st.HasSecret(store.ServerAPIKeySecret(sv.ID)), "API key of the server "+sv.Name))
		}
	}
	if c.Notify.DiscordWebhook != "" {
		changes = append(changes, replacedOrAdded(s.st.HasSecret(store.SecretDiscordWebhook), "Discord webhook"))
	}
	if c.API.Token != "" {
		changes = append(changes, replacedOrAdded(s.st.HasSecret(store.SecretAPIToken), "API token"))
	}
	n, err := settings.ImportSecrets(s.st, c)
	if err != nil {
		s.redirect(w, r, "/settings#import", "Import failed. "+sentence(err.Error()), true)
		return
	}
	if err := s.hooks.SaveConfig(c); err != nil {
		s.redirect(w, r, "/settings#import", "Not saved. "+sentence(err.Error()), true)
		return
	}
	if err := settings.DropRemovedServers(s.st, old, c); err != nil {
		s.log.Error("cannot delete the secrets of removed servers", "err", err)
	}
	if n > 0 {
		s.channelsChanged() // the file may have brought a new webhook
	}
	if len(changes) > 0 {
		announce("Configuration imported", strings.Join(changes, "\n"))
	}
	msg := "The configuration is imported."
	if n > 0 {
		msg += " syncwatch encrypted and stored " + plural(n, "secret", "secrets") + " from the file."
	}
	if need := settings.NeedKey(s.st, c); len(need) > 0 {
		msg += " Type the API key again for " + strings.Join(need, ", ") + ". " + keyOnlyToURL
	}
	s.redirect(w, r, "/settings#import", msg, false)
}

// errTooManyLogins is the one text for every password rate limit.
const errTooManyLogins = "Too many failed logins. Try again in 10 minutes."

// fieldLabels maps the duration fields of the Checks and Collector forms to
// the labels of settings.html, so a validation message names the field as
// the page shows it.
var fieldLabels = map[string]string{
	"failing_files":       "H4 with only failed items after",
	"stuck_sync":          "H5 stuck sync after",
	"paused":              "H6 paused for more than",
	"pc_offline":          "H7 PC offline for more than",
	"conflict_window":     "H10 conflicts in the last",
	"inactive_project":    "C1 no file change for",
	"old_device":          "C2 device not seen for",
	"dead_share":          "C3 folder not used by device after",
	"event_timeout":       "Event long-poll timeout",
	"full_check_interval": "Full check every",
	"poll_interval":       "Poll interval (polled mode)",
	"structure_interval":  "Structure scan every",
	"baseline_interval":   "Baseline scan every",
	"request_timeout":     "Request timeout",
}

func fieldLabel(name string) string {
	if l, ok := fieldLabels[name]; ok {
		return l
	}
	return name
}
