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
	"time"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/notify"
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
	for i := range log {
		log[i].Channel = channelLabel(log[i].Channel)
		// A title has folder labels and device names from Syncthing (text-5).
		log[i].Summary, log[i].Error = showControls(log[i].Summary), showControls(log[i].Error)
	}
	_, hasViewer := s.setting(KeyViewerHash)
	var kept int
	if s.hooks.Delivery != nil {
		kept = s.hooks.Delivery().NoticesKept
	}
	form := formValues(cfg)
	data := map[string]any{
		"Title":              "Settings",
		"Cfg":                cfg,
		"Servers":            s.serverSettings(v),
		"Sites":              siteNames(cfg),
		"Devices":            deviceRows(v.Snapshot, cfg),
		"RelayLost":          cfg.Thresholds.RelayLost.Words(),
		"Checks":             checks,
		"StructureYAML":      string(sdoc),
		"HasWebhook":         s.st.HasSecret(store.SecretDiscordWebhook),
		"WebhookLost":        s.undecryptable(store.SecretDiscordWebhook),
		"HasNoticesWebhook":  s.st.HasSecret(store.SecretNoticesWebhook),
		"NoticesWebhookLost": s.undecryptable(store.SecretNoticesWebhook),
		"NoticesKept":        kept,
		"API":                s.apiSettings(),
		"HasViewerPW":        hasViewer,
		"Log":                log,
		"Weekdays":           []string{"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday", "off"},
		"RepeatOptions":      repeatOptions(form["repeat_urgent"]),
		"Now":                s.fmtTime(s.now()),
		"Rev":                configRev(cfg),
		"Form":               form,
		"Bad":                map[string]bool{},
		"Errors":             map[string]string{},
	}
	for k, val := range extra {
		data[k] = val
	}
	return data
}

// channelLabel names a channel of the notification log ("discord",
// "discord-notices") as the Settings page shows it.
func channelLabel(name string) string {
	switch name {
	case "discord":
		return "Discord"
	case "discord-notices":
		return "Discord (settings notices)"
	}
	return name
}

// siteNames returns the site names in use (the site of syncwatch and of each
// server), in the order of the configuration, each one time. The site inputs
// offer them, thus the same site keeps the same spelling.
func siteNames(c *config.Config) []string {
	var out []string
	add := func(site string) {
		if site != "" && !slices.Contains(out, site) {
			out = append(out, site)
		}
	}
	add(c.Site)
	for _, sv := range c.Servers {
		add(sv.Site)
	}
	return out
}

// deviceRow is one PC of the Devices card.
type deviceRow struct {
	ID, Short, Name string
	State           string // connected, offline, or unknown for a configured device that no server reports
	Relay           bool   // devices[].relay
}

// deviceRows lists the PCs that the servers report (not the servers) and
// then the configured devices that no server reports.
func deviceRows(snap *model.Snapshot, c *config.Config) []deviceRow {
	var out []deviceRow
	seen := map[string]bool{}
	if snap != nil {
		for _, d := range snap.Devices() {
			if d.IsServer {
				continue
			}
			seen[d.ID] = true
			state := "offline"
			if d.Connected {
				state = "connected"
			}
			out = append(out, deviceRow{ID: d.ID, Short: model.ShortID(d.ID), Name: showControls(d.Name), State: state, Relay: c.RelayExpected(d.ID)})
		}
	}
	for _, d := range c.Devices {
		if seen[d.ID] {
			continue
		}
		seen[d.ID] = true
		out = append(out, deviceRow{ID: d.ID, Short: model.ShortID(d.ID), Name: model.ShortID(d.ID), State: "unknown", Relay: d.Relay})
	}
	return out
}

// repeatOption is one option of the select "Repeat urgent messages".
type repeatOption struct {
	Value, Label string
	Selected     bool
}

// repeatOptions returns the options of the select repeat_urgent with the
// value v selected. A value that is not one of the options (from a YAML
// file) is an extra option, thus a save keeps it.
func repeatOptions(v string) []repeatOption {
	out := []repeatOption{{"0", "off", false}, {"1h", "every hour", false}, {"2h", "every 2 hours", false}, {"4h", "every 4 hours", false}}
	found := false
	for i := range out {
		if out[i].Value == v {
			out[i].Selected, found = true, true
		}
	}
	if !found && v != "" {
		label := v
		if d, err := config.ParseDuration(v); err == nil && d > 0 {
			label = "every " + d.Words()
		}
		out = append(out, repeatOption{v, label, true})
	}
	return out
}

// formInputs lists, for each form that refuse shows again, the inputs whose
// typed text it keeps (the rows of the checks table are in checkRow).
var formInputs = map[string][]string{
	"notifications": {"discord_webhook_clear", "discord_notices_webhook_clear", "site", "discord_role_id", "repeat_urgent", "quiet_hours", "reminder_time", "weekly_day", "weekly_time", "weekly_top", "timezone", "public_url"},
	"checks":        {"failing_files", "stuck_sync", "paused", "pc_offline", "conflict_window", "relay_lost", "inactive_project", "old_device", "dead_share"},
	"collector":     {"mode", "event_timeout", "full_check_interval", "poll_interval", "structure_interval", "baseline_interval", "concurrency", "request_timeout", "down_after_failures"},
}

// keepWhenMissing are the inputs of wave 3 (r2). A form that does not send
// one of them (a script written for an older version) keeps the stored
// value, as for an empty webhook input. A browser always sends them.
var keepWhenMissing = map[string]bool{"site": true, "repeat_urgent": true, "relay_lost": true}

// formHas reports whether the form sent the input name, also with an empty
// value.
func formHas(r *http.Request, name string) bool {
	_ = r.ParseForm()
	_, ok := r.Form[name]
	return ok
}

// formValues returns the text that each input of formInputs shows for c.
func formValues(c *config.Config) map[string]string {
	n, th, col := c.Notify, c.Thresholds, c.Collector
	return map[string]string{
		"site": c.Site, "discord_role_id": n.DiscordRoleID, "repeat_urgent": n.RepeatUrgent.String(), "quiet_hours": n.QuietHours, "reminder_time": n.ReminderTime, "weekly_day": n.WeeklyDay,
		"weekly_time": n.WeeklyTime, "weekly_top": strconv.Itoa(n.WeeklyTop), "timezone": c.Timezone, "public_url": c.PublicURL,
		"failing_files": th.FailingFiles.String(), "stuck_sync": th.StuckSync.String(), "paused": th.Paused.String(), "pc_offline": th.PCOffline.String(),
		"conflict_window": th.ConflictWindow.String(), "relay_lost": th.RelayLost.String(), "inactive_project": th.InactiveProject.String(), "old_device": th.OldDevice.String(), "dead_share": th.DeadShare.String(),
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
		if keepWhenMissing[name] && !formHas(r, name) {
			continue
		}
		form[name] = r.FormValue(name)
	}
	d["RepeatOptions"] = repeatOptions(form["repeat_urgent"])
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

// undecryptable reports whether a secret is stored but cannot be decrypted,
// for example after the secret key changed. The page then does not say
// "Set" as if the secret worked.
func (s *Server) undecryptable(name string) bool {
	if !s.st.HasSecret(name) {
		return false
	}
	_, err := s.st.Secret(name)
	return err != nil
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
	s.redirect(w, r, "/settings#"+section, staleText, true)
	return true
}

// staleText is the message for a form from a page that is older than the
// configuration.
const staleText = "Not saved. The settings changed after you opened this page. Reload the page. Then make the change again."

// save applies a configuration change, announces what changed and redirects
// back to a settings section. When the change is refused, it writes no
// answer and returns the error. The sections that save is for (checks,
// structure rules, collector, devices) change no secret, pinned certificate
// or access: their settings notices are not security notices (C5).
func (s *Server) save(w http.ResponseWriter, r *http.Request, old, c *config.Config, section, title, okMsg string) error {
	if err := s.hooks.SaveConfig(c); err != nil {
		return err
	}
	if ch := s.changesOf(old, c); len(ch) > 0 {
		s.announce(r, notify.Notice{Title: title, Summary: strings.Join(ch, "\n")})
	}
	s.redirect(w, r, "/settings#"+section, okMsg, false)
	return nil
}

// changesOf is configChanges with the device names of the newest snapshot.
func (s *Server) changesOf(old, c *config.Config) []string {
	name := model.ShortID
	if v := s.eng.View(); v != nil && v.Snapshot != nil {
		name = v.Snapshot.DeviceName
	}
	return changeLines(old, c, name)
}

// announce logs a security-relevant settings change and sends it as a notice.
func (s *Server) announce(r *http.Request, n notify.Notice) {
	s.announcer(r)(n)
}

// announcer captures the notification channels configured now and returns a
// function that announces a change to them. A change that replaces or
// removes a channel is then still heard by the channel it affects.
func (s *Server) announcer(r *http.Request) func(notify.Notice) {
	send := s.hooks.Notice
	if s.hooks.NoticeLater != nil {
		send = s.hooks.NoticeLater()
	}
	ip := clientIP(r)
	return func(n notify.Notice) {
		s.log.Warn("settings changed", "change", n.Title, "detail", n.Summary, "ip", ip)
		if send != nil {
			// On a line of its own: after the last change line it read as a
			// part of that change.
			n.Summary += "\nChanged from " + ip + "."
			send(n)
		}
	}
}

// confirmed checks the "confirm" field of a form against the admin password
// or the server's stored API key. Accepting a new certificate or forgetting a
// pin can send the stored key to a different machine, so an admin session
// alone isn't enough. Failures count in the rate limit of confirmations and
// are logged as refused (the limit bounds the lines). It reports whether the
// confirmation is correct, or the message to show when the rate limit
// refuses it.
func (s *Server) confirmed(r *http.Request, info requestInfo, serverID, refused string) (ok bool, limited string) {
	v := r.FormValue("confirm")
	if v == "" {
		return false, ""
	}
	ok, limited = s.limitedCheck(r, info, func() bool {
		if h, _ := s.st.Setting(KeyAdminHash); h != "" && CheckPassword(v, h) {
			return true
		}
		key, _, err := s.st.ServerKey(serverID)
		return err == nil && key != "" && subtle.ConstantTimeCompare([]byte(v), []byte(key)) == 1
	})
	if !ok && limited == "" {
		s.log.Warn(refused, "ip", clientIP(r), "server", serverID)
	}
	return ok, limited
}

// limitedCheck runs a password check under the rate limit of confirmations,
// so an admin session alone can't be used to guess the admin password: at
// most 5 failures in 10 minutes for each session (confirmKey). It reports
// whether the check passed, or the message to show when the session is over
// the limit (the check isn't run then).
func (s *Server) limitedCheck(r *http.Request, info requestInfo, check func() bool) (ok bool, limited string) {
	key := confirmKey(r, info)
	now := s.now()
	allowed, delay := s.limiter.check(key, now)
	if !allowed {
		// One line per session (or proxy address) and minute, with the
		// number of refused checks, as for logins (W-025).
		if n := s.logs.count("confirm "+key, now); n > 0 {
			s.log.Warn("password check rate limit reached", "ip", clientIP(r), "path", r.URL.Path, "count", n)
		}
		return false, tooManyTries(s.limiter.retryAfter(key, now))
	}
	wait(r, delay)
	if check() {
		s.limiter.success(key, RoleAdmin)
		return true, ""
	}
	s.limiter.fail(key, s.now(), RoleAdmin)
	return false, ""
}

// confirmKey is the limiter bucket of a password typed to confirm a change:
// the administrator's session, so that failed logins of other people at the
// same address (a VPN, a reverse proxy) cannot block it, or the client
// address when there is no session (proxy mode). Logins count by address.
func confirmKey(r *http.Request, info requestInfo) string {
	if info.Session != nil {
		return "session " + info.Session.TokenHash
	}
	return "confirm " + clientIP(r)
}

// tooManyTries is the message for a confirmation that the rate limit
// refuses, with the wait in whole minutes.
func tooManyTries(d time.Duration) string {
	return "Too many failed tries. Try again in " + plural(max(1, int((d+time.Minute-1)/time.Minute)), "minute", "minutes") + "."
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
		Site:      config.CleanSite(r.FormValue("site")),
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
		if !formHas(r, "site") {
			sv.Site = old.Site // a form without the input (a script) keeps the site
		}
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
	c.SpellSite(sv.Site) // the typed spelling of a site is its spelling everywhere (import-4)
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
	case resetPin && key == "":
		if ok, limited := s.confirmed(r, info, sv.ID, "pinned certificate removal refused"); !ok {
			s.redirect(w, r, "/settings#servers", orText(limited, "To remove the pinned certificate, type the administrator password or this server's API key."), true)
			return
		}
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
		// A new server always comes with its API key.
		summary := "The API URL is " + sv.URL + "."
		if sv.Site != "" {
			summary += "\nThe site of the server is " + sv.Site + "."
		}
		// The spelling of the site can change the site of other servers and
		// of syncwatch (SpellSite).
		for _, l := range s.changesOf(cur, c) {
			if !strings.HasPrefix(l, "The server "+sv.Name+" is added") {
				summary += "\n" + l
			}
		}
		s.announce(r, notify.Notice{Title: "Server " + name + " added", Summary: summary, Security: true})
	} else {
		// Disabling a server or changing its name or URL changes what is
		// checked and what the messages say, so every change is listed.
		changes := s.changesOf(cur, c)
		if key != "" {
			changes = append(changes, "The API key is replaced.")
		}
		if resetPin && !moved {
			changes = append(changes, "The pinned certificate is removed. syncwatch accepts and pins the next certificate from "+sv.URL+" (trust on first use).")
		}
		if len(changes) > 0 {
			// A new API key, a URL that needs the API key again or a removed
			// pinned certificate is a security change. The name, the GUI URL,
			// the site, allow_http and disabled alone are not (C5).
			security := key != "" || moved || resetPin
			s.announce(r, notify.Notice{Title: "Server " + name + " changed", Summary: strings.Join(changes, "\n"), Security: security})
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
	s.announce(r, notify.Notice{Title: "Server " + name + " removed", Summary: "syncwatch deleted its API key and pinned certificate.", Security: true})
	s.redirect(w, r, "/settings#servers", "The server is removed.", false)
}

func (s *Server) handleAcceptCert(w http.ResponseWriter, r *http.Request, info requestInfo) {
	id := r.PathValue("id")
	if ok, limited := s.confirmed(r, info, id, "certificate accept refused"); !ok {
		s.redirect(w, r, "/settings#servers", orText(limited, "To accept the new certificate, type the administrator password or this server's API key."), true)
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
	s.announce(r, notify.Notice{Title: "Server " + name + ": new certificate accepted", Summary: "The pinned SHA-256 fingerprint is " + stclient.ShortFP(fp) + ".", Security: true})
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
	if formHas(r, "site") {
		c.Site = config.CleanSite(r.FormValue("site"))
		c.SpellSite(c.Site) // the typed spelling of a site is its spelling everywhere (import-4)
	}
	clearHook, hook := r.FormValue("discord_webhook_clear") == "on", strings.TrimSpace(r.FormValue("discord_webhook"))
	clearNotices, noticesHook := r.FormValue("discord_notices_webhook_clear") == "on", strings.TrimSpace(r.FormValue("discord_notices_webhook"))
	typedHook, typedNotices := hook != "", noticesHook != ""
	refused := func(err error) {
		msg := "Not saved. " + sentence(err.Error())
		// The page does not show a typed webhook URL again.
		if typedHook {
			msg += "\nType the Discord webhook URL again."
		}
		if typedNotices {
			msg += "\nType the Discord webhook for settings notices again."
		}
		s.refuse(w, r, info, old, "notifications", msg, badInputs(err))
	}
	if formHas(r, "repeat_urgent") {
		d, err := config.ParseDuration(r.FormValue("repeat_urgent"))
		if err != nil {
			refused(fmt.Errorf("%s: %w", config.FieldLabel("notifications.repeat_urgent"), err))
			return
		}
		n.RepeatUrgent = d // out of range: Validate names the field and the range
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
	if !clearNotices && noticesHook != "" {
		var err error
		if noticesHook, err = config.CanonicalNoticesWebhook(noticesHook); err != nil {
			refused(err)
			return
		}
	}

	// Changes that can keep problems from being announced are announced
	// to the channels configured before them.
	announce := s.announcer(r)
	changes := s.changesOf(old, c)
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
	hadNotices := s.st.HasSecret(store.SecretNoticesWebhook)
	switch {
	case clearNotices && hadNotices:
		_ = s.st.DeleteSecret(store.SecretNoticesWebhook)
		changes = append(changes, "The Discord webhook for settings notices is removed.")
		channels = true
	case !clearNotices && noticesHook != "":
		_ = s.st.SetSecret(store.SecretNoticesWebhook, noticesHook)
		changes = append(changes, replacedOrAdded(hadNotices, "Discord webhook for settings notices"))
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
		// A webhook change is a security change; the other notification
		// settings are not (C5).
		announce(notify.Notice{Title: "Notification settings changed", Summary: strings.Join(changes, "\n"), Security: channels})
	}
	s.redirect(w, r, "/settings#notifications", "The notification settings are saved.", false)
}

// orText returns msg, or else when msg is empty.
func orText(msg, other string) string {
	if msg == "" {
		return other
	}
	return msg
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
// are kept outside the configuration and never appear. It names a device by
// its short ID; changesOf names it as the servers do.
func configChanges(old, c *config.Config) []string {
	return changeLines(old, c, model.ShortID)
}

// changeLines is configChanges with deviceName for the names of devices.
func changeLines(old, c *config.Config, deviceName func(id string) string) []string {
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
		if o.Site != sv.Site {
			out = append(out, "The site of the server "+sv.Name+" changed from "+orNone(o.Site)+" to "+orNone(sv.Site)+".")
		}
		switch {
		case o.AllowHTTP == sv.AllowHTTP:
		case !sv.AllowHTTP:
			out = append(out, "The server "+sv.Name+" does not permit a plain http:// URL now.")
		case strings.HasPrefix(sv.URL, "http://"):
			out = append(out, "The server "+sv.Name+" permits a plain http:// URL now. syncwatch sends the API key unencrypted.")
		default:
			out = append(out, "The server "+sv.Name+" permits a plain http:// URL now.")
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
			// RT-1: the debounce of a check that sends no urgent message
			// delays nothing; the line says so.
			if old.Urgent(ci.ID) || c.Urgent(ci.ID) {
				out = append(out, fmt.Sprintf("The check %s waits %s instead of %s before an urgent message.", name, rules.Ago(now), rules.Ago(was)))
			} else {
				out = append(out, fmt.Sprintf("The check %s does not send urgent messages. Its debounce changed from %s to %s.", name, rules.Ago(was), rules.Ago(now)))
			}
		}
	}
	out = append(out, deviceChanges(old, c, deviceName)...)
	was, wasLists := flatConfig(old)
	now, nowLists := flatConfig(c)
	keys := map[string]bool{}
	for k := range was {
		keys[k] = true
	}
	for k := range now {
		keys[k] = true
	}
	for _, k := range slices.Sorted(maps.Keys(keys)) {
		if strings.HasPrefix(k, "servers") || strings.HasPrefix(k, "checks") || strings.HasPrefix(k, "devices") {
			continue // described above
		}
		a, b := orNone(was[k]), orNone(now[k])
		// A list of rules has no value of its own once it has entries.
		if a == b || a == "[]" && b == "(none)" || a == "(none)" && b == "[]" {
			continue
		}
		wl, wok := wasLists[k]
		nl, nok := nowLists[k]
		if line := listChange(k, wl, nl); (wok || nok) && line != "" {
			out = append(out, line)
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
		return "Viewer access: " + viewerText(was) + " → " + viewerText(now)
	case "notifications.repeat_urgent":
		return changeLabel(key) + ": " + repeatWords(was) + " → " + repeatWords(now)
	case "notifications.discord_role_id":
		switch {
		case now == "(none)":
			return "Urgent messages do not mention a role now (before: " + was + ")."
		case was == "(none)":
			return "Urgent messages now mention the role " + now + "."
		}
		return "Urgent messages now mention the role " + now + " (before: " + was + ")."
	}
	return changeLabel(key) + ": " + was + " → " + now
}

// keyLabels names the settings that config.FieldLabel does not label, as
// the Settings page and the validation messages name them.
var keyLabels = map[string]string{
	"collector.mode":              "Collector mode",
	"notifications.quiet_hours":   "Quiet hours",
	"notifications.reminder_time": "Daily reminder",
	"notifications.weekly_day":    "Weekly report day",
	"notifications.weekly_time":   "Weekly report time",
	"timezone":                    "Time zone",
	"public_url":                  "Dashboard URL",
}

// changeLabel names a setting in a notice line: the label of the Settings
// page (config.FieldLabel without the key, or keyLabels). A setting that only
// the YAML has (structure rules, shares, access.session_ttl) is named by its
// part of the Settings page and its key (W-039).
func changeLabel(key string) string {
	if l := config.FieldLabel(key); l != key {
		return strings.TrimSuffix(l, " ("+key+")")
	}
	if l, ok := keyLabels[key]; ok {
		return l
	}
	switch {
	case strings.HasPrefix(key, "structure."):
		return "Structure rules (" + strings.TrimPrefix(key, "structure.") + ")"
	case strings.HasPrefix(key, "shares.add."):
		return "Shares to add for " + strings.TrimPrefix(key, "shares.add.") + " (shares.add)"
	case strings.HasPrefix(key, "shares.exclude."):
		return "Shares to exclude for " + strings.TrimPrefix(key, "shares.exclude.") + " (shares.exclude)"
	case strings.HasPrefix(key, "access."):
		return "Access (" + strings.TrimPrefix(key, "access.") + ")"
	}
	return key
}

// repeatWords is a value of notifications.repeat_urgent in a notice line:
// off, or the interval in words ("2 hours").
func repeatWords(v string) string {
	if v == "(none)" || v == "0" {
		return "off"
	}
	if d, err := config.ParseDuration(v); err == nil {
		return d.Words()
	}
	return v
}

// deviceChanges words the changes of devices[].relay (W-047): syncwatch
// reports no H11 for a device that always connects through a relay.
func deviceChanges(old, c *config.Config, deviceName func(id string) string) []string {
	device := func(id string) string {
		name := notify.OneLine(deviceName(id)) // the PC names itself
		if short := model.ShortID(id); name != short {
			name += " (" + short + ")"
		}
		return "The device " + name
	}
	var out []string
	for _, d := range c.Devices {
		if d.Relay && !old.RelayExpected(d.ID) {
			out = append(out, device(d.ID)+" always connects through a relay. H11 is off for it.")
		}
	}
	for _, d := range old.Devices {
		if d.Relay && !c.RelayExpected(d.ID) {
			out = append(out, device(d.ID)+" does not always connect through a relay now. H11 is on again for it.")
		}
	}
	return out
}

// listChange words a changed list of values (ignore_dirs, type_tokens, the
// shares of a server) as the entries added and removed, not as the two
// complete lists. It returns "" when only the order changed.
func listChange(key string, was, now []string) string {
	var parts []string
	if added := missing(now, was); len(added) > 0 {
		parts = append(parts, "added "+strings.Join(added, ", "))
	}
	if removed := missing(was, now); len(removed) > 0 {
		parts = append(parts, "removed "+strings.Join(removed, ", "))
	}
	if len(parts) == 0 {
		return ""
	}
	return changeLabel(key) + ": " + strings.Join(parts, " · ")
}

// missing returns the entries of a that are not in b. An entry that is in a
// more times than in b is missing that many times.
func missing(a, b []string) []string {
	left := map[string]int{}
	for _, v := range b {
		left[v]++
	}
	var out []string
	for _, v := range a {
		if left[v] > 0 {
			left[v]--
			continue
		}
		out = append(out, v)
	}
	return out
}

// viewerText is the short text of a Viewer access value in a notice line.
func viewerText(v string) string {
	switch v {
	case config.ViewerNone:
		return "administrator only"
	case config.ViewerPassword:
		return "viewer password"
	case config.ViewerOpen:
		return "open (no login)"
	}
	return v
}

// flatConfig renders a configuration as the keys of its YAML form with their
// values: nested keys joined with dots, a list of values as one value, and
// secrets removed. lists has the entries of each list of values.
func flatConfig(c *config.Config) (out map[string]string, lists map[string][]string) {
	out, lists = map[string]string{}, map[string][]string{}
	b, _ := yaml.Marshal(c.Redacted())
	var doc map[string]any
	_ = yaml.Unmarshal(b, &doc)
	flatten("", doc, out, lists)
	return out, lists
}

func flatten(key string, v any, out map[string]string, lists map[string][]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, item := range x {
			if key != "" {
				k = key + "." + k
			}
			flatten(k, item, out, lists)
		}
	case []any:
		var items []string
		for i, item := range x {
			if _, ok := item.(map[string]any); ok {
				flatten(fmt.Sprintf("%s[%d]", key, i), item, out, lists)
				continue
			}
			items = append(items, orNone(fmt.Sprint(item)))
		}
		if len(items) == len(x) {
			out[key] = "[" + strings.Join(items, ", ") + "]"
			lists[key] = items
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
	msg := "syncwatch sent the test message."
	if s.cfg.Get().Notify.DiscordRoleID != "" {
		// The test message mentions the role, as an urgent message does.
		msg = "syncwatch sent the test message with the role mention. If your phone shows no notification, check the role ID. Also make sure that you have the role."
	}
	s.redirect(w, r, "/settings#notifications", msg, false)
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
		"relay_lost": &th.RelayLost, "inactive_project": &th.InactiveProject, "old_device": &th.OldDevice, "dead_share": &th.DeadShare,
	}
	for _, name := range formInputs["checks"] { // in the order of the form
		if keepWhenMissing[name] && !formHas(r, name) {
			continue
		}
		d, msg := formDuration(r, name)
		if msg != "" {
			errs = append(errs, msg)
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

// formDuration reads the duration input name of the Checks or the Collector
// form. It returns the message for a value that is not a duration (RT-2: the
// message of config.ParseDuration names the format) or that is not more than
// 0, with the label of the input.
func formDuration(r *http.Request, name string) (config.Duration, string) {
	d, err := config.ParseDuration(r.FormValue(name))
	switch {
	case err != nil:
		return 0, fieldLabel(name) + ": " + err.Error()
	case d <= 0:
		return 0, fieldLabel(name) + ": the value must be more than 0. Use a number and a unit, for example 30s, 15m, 24h, 3d or 2w."
	}
	return d, ""
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
		d, msg := formDuration(r, name)
		if msg != "" {
			errs = append(errs, msg)
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
	// A new viewer password lets someone else in, too. It needs the
	// administrator password also when it has no effect now: the check
	// below must not tell a session without the administrator password
	// whether a guess is the administrator password.
	newViewerPW := viewerPW != "" && c.Access.Mode == config.AccessBuiltin && c.Access.Viewer == config.ViewerPassword
	if weakensAccess(old, c) || viewerPW != "" {
		if msg := s.confirmAdmin(r, info, "access change refused"); msg != "" {
			s.redirect(w, r, "/settings#access", msg, true)
			return
		}
	}
	// The login checks the administrator password first: with the same
	// password, each viewer is an administrator (sessions-1). After the
	// confirmation, "confirm" is the administrator password.
	if viewerPW != "" && s.adminConfigured() && viewerPW == r.FormValue("confirm") {
		s.redirect(w, r, "/settings#access", "The viewer password must be different from the administrator password.", true)
		return
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
	changes := s.changesOf(old, c)
	if newViewerPW {
		changes = append(changes, "The new viewer password is set. syncwatch logged out all viewers.")
	}
	if len(changes) > 0 {
		s.announce(r, notify.Notice{Title: "Access settings changed", Summary: strings.Join(changes, "\n"), Security: true})
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
// It returns "" when confirmed, otherwise the message to show; a password
// that is not correct is logged as refused. With no admin password set
// (proxy mode) there's nothing to confirm with.
func (s *Server) confirmAdmin(r *http.Request, info requestInfo, refused string) string {
	return s.confirmAdminFor(r, info, refused, "This change permits access to more people. Type the administrator password to accept the change.")
}

// confirmAdminFor is confirmAdmin for a change that need describes: the
// sentences that say why the change needs the administrator password.
func (s *Server) confirmAdminFor(r *http.Request, info requestInfo, refused, need string) string {
	h, _ := s.setting(KeyAdminHash)
	if h == "" {
		return ""
	}
	pw := r.FormValue("confirm")
	if pw == "" {
		return need
	}
	ok, limited := s.limitedCheck(r, info, func() bool { return CheckPassword(pw, h) })
	switch {
	case limited != "":
		return limited
	case !ok:
		s.log.Warn(refused, "ip", clientIP(r))
		return "The administrator password is not correct. " + need
	}
	return ""
}

func (s *Server) handlePasswordSave(w http.ResponseWriter, r *http.Request, info requestInfo) {
	if h, _ := s.setting(KeyAdminHash); h != "" {
		ok, limited := s.limitedCheck(r, info, func() bool { return CheckPassword(r.FormValue("current"), h) })
		switch {
		case limited != "":
			s.redirect(w, r, "/settings#access", limited, true)
			return
		case !ok:
			s.log.Warn("password change refused", "ip", clientIP(r))
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
	if s.sameAsViewer(pw) {
		s.redirect(w, r, "/settings#access", errSameAsViewer, true)
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
	s.announce(r, notify.Notice{Title: "Administrator password changed", Summary: "syncwatch logged out the other administrator sessions.", Security: true})
	s.redirect(w, r, "/settings#access", "The administrator password is changed. syncwatch logged out the other administrator sessions.", false)
}

// errSameAsViewer refuses an administrator password that is the viewer
// password.
const errSameAsViewer = "The administrator password must be different from the viewer password."

// sameAsViewer reports whether pw is the viewer password. The login checks
// the administrator password first: an administrator password that is also
// the viewer password makes each login with the viewer password an
// administrator session (sessions-1).
func (s *Server) sameAsViewer(pw string) bool {
	h, ok := s.setting(KeyViewerHash)
	return ok && CheckPassword(pw, h)
}

// handleDevicesSave saves the devices that always connect through a relay
// (W-047): devices[] is the list of the selected device IDs, each with relay
// true. A device that is not selected leaves the list.
func (s *Server) handleDevicesSave(w http.ResponseWriter, r *http.Request, info requestInfo) {
	old := s.cfg.Get()
	if s.stale(w, r, old, "devices") {
		return
	}
	c := old.Clone()
	c.Devices = nil
	_ = r.ParseForm()
	for _, id := range r.Form["relay"] {
		id = strings.ToUpper(strings.TrimSpace(id))
		if id != "" && !c.RelayExpected(id) {
			c.Devices = append(c.Devices, config.Device{ID: id, Relay: true})
		}
	}
	if err := c.Validate(); err != nil {
		s.redirect(w, r, "/settings#devices", "Not saved. "+sentence(err.Error()), true)
		return
	}
	if err := s.save(w, r, old, c, "devices", "Devices changed", "The devices are saved."); err != nil {
		s.redirect(w, r, "/settings#devices", "Not saved. "+sentence(err.Error()), true)
	}
}

// handleNoticesTest sends a test notice to the webhook for settings notices
// (W-051).
func (s *Server) handleNoticesTest(w http.ResponseWriter, r *http.Request, info requestInfo) {
	if s.hooks.SendNoticesTest == nil {
		s.redirect(w, r, "/settings#notifications", "The test settings notice failed. syncwatch cannot send settings notices.", true)
		return
	}
	if err := s.hooks.SendNoticesTest(r.Context()); err != nil {
		s.redirect(w, r, "/settings#notifications", "The test settings notice failed. "+sentence(err.Error()), true)
		return
	}
	s.redirect(w, r, "/settings#notifications", "syncwatch sent the test settings notice.", false)
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

// handleImport imports a YAML file in 2 steps (W-055). Step 1 (no import_id)
// reads and checks the file, keeps it as a pending import and shows the
// preview in the card "Export and import". It changes nothing. Step 2 (the
// form of the preview, with import_id) applies the file of the same session.
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request, info requestInfo) {
	if id := r.FormValue("import_id"); id != "" {
		s.confirmImport(w, r, info, id)
		return
	}
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
	old := s.cfg.Get()
	if s.stale(w, r, old, "import") {
		return
	}
	c, err := config.ParseYAML(data)
	if err != nil {
		s.redirect(w, r, "/settings#import", "Import failed. "+sentence(err.Error()), true)
		return
	}
	if s.locksEveryoneOut(w, r, c) {
		return
	}
	p := s.buildImportPreview(data, old, c)
	p.ID = s.pendingImports().add(data, configRev(old), confirmKey(r, info), s.now())
	s.render(w, r, http.StatusOK, "settings", info, s.settingsData(info, map[string]any{"ImportPreview": p}))
}

// locksEveryoneOut refuses a file that sets built-in login to on while no
// administrator password is set. Everyone would get a setup page whose
// token was never logged (the process started in proxy mode).
func (s *Server) locksEveryoneOut(w http.ResponseWriter, r *http.Request, c *config.Config) bool {
	if c.Access.Mode != config.AccessBuiltin || s.adminConfigured() {
		return false
	}
	s.redirect(w, r, "/settings#import", "Not imported. The file sets built-in login to on, but no administrator password is set. Set an administrator password in Settings → Access first.", true)
	return true
}

// confirmImport is step 2 of an import: it applies the pending import id of
// this session. An import that removes servers needs the checkbox
// delete_servers and the administrator password; an import that gives access
// to more people needs the administrator password. A refused confirmation
// shows the preview again with the reason, and the pending import stays.
func (s *Server) confirmImport(w http.ResponseWriter, r *http.Request, info requestInfo, id string) {
	imports := s.pendingImports()
	p := imports.claim(id, confirmKey(r, info), s.now())
	if p == nil {
		s.redirect(w, r, "/settings#import", "The preview of the import expired. Select the file again.", true)
		return
	}
	old := s.cfg.Get()
	if s.stale(w, r, old, "import") {
		imports.remove(id)
		return
	}
	if p.rev != configRev(old) {
		// The configuration changed after the preview (a form without rev).
		imports.remove(id)
		s.redirect(w, r, "/settings#import", staleText, true)
		return
	}
	c, err := config.ParseYAML(p.data)
	if err != nil {
		imports.remove(id)
		s.redirect(w, r, "/settings#import", "Import failed. "+sentence(err.Error()), true)
		return
	}
	if s.locksEveryoneOut(w, r, c) {
		imports.remove(id)
		return
	}
	refuse := func(msg string) {
		imports.release(id) // the pending import stays for a corrected confirmation
		pv := s.buildImportPreview(p.data, old, c)
		pv.ID, pv.Error = id, showControls(msg) // msg can name servers
		s.render(w, r, http.StatusUnprocessableEntity, "settings", info, s.settingsData(info, map[string]any{"ImportPreview": pv}))
	}
	removed := removedServers(old, c)
	if len(removed) > 0 && r.FormValue("delete_servers") != "on" {
		what := "the server " + removed[0]
		if len(removed) > 1 {
			what = "the servers " + andList(removed)
		}
		refuse("Not imported. Select the checkbox to remove " + what + ".")
		return
	}
	weakens := weakensAccess(old, c)
	switch {
	case len(removed) > 0:
		// In proxy mode without an administrator password, the checkbox
		// alone confirms.
		need := "The import removes servers and deletes their API keys. Type the administrator password to accept the import."
		if weakens {
			need = "The import removes servers and permits access to more people. Type the administrator password to accept the import."
		}
		if msg := s.confirmAdminFor(r, info, "import refused", need); msg != "" {
			refuse("Not imported. " + msg)
			return
		}
	case weakens:
		if msg := s.confirmAdmin(r, info, "import refused"); msg != "" {
			refuse("Not imported. The file sets login to off or gives viewer access to more people. " + msg)
			return
		}
	}
	imports.remove(id)
	s.applyImport(w, r, old, c)
}

// legacyTokenName names the API token that an imported api.token sets: the
// API token "API token" (contract with fixer 5).
const legacyTokenName = `API token "` + store.LegacyTokenName + `"`

// hasLegacyToken reports whether the API token "API token" exists: the API
// token whose value an imported api.token replaces (settings.ImportSecrets
// uses the same lookup). A secret api.token of an older version that the
// store did not move (it cannot be decrypted) does not count: the import
// adds the API token "API token" next to it.
func (s *Server) hasLegacyToken() bool {
	t, err := s.st.APITokenByName(store.LegacyTokenName)
	return err == nil && t != nil
}

// legacyTokenScope returns the scope that an imported api.token gets: the
// scope of the API token "API token", or full when the import adds it.
func (s *Server) legacyTokenScope() string {
	if t, err := s.st.APITokenByName(store.LegacyTokenName); err == nil && t != nil {
		return t.Scope
	}
	return store.ScopeFull
}

// applyImport stores the secrets of the file c, saves c as the configuration
// and deletes the API keys and pinned certificates of the removed servers.
func (s *Server) applyImport(w http.ResponseWriter, r *http.Request, old, c *config.Config) {
	// Announced to the channels configured before the file replaces any.
	announce := s.announcer(r)
	changes := s.changesOf(old, c)
	for _, sv := range c.Servers {
		if sv.APIKey != "" {
			changes = append(changes, replacedOrAdded(s.st.HasSecret(store.ServerAPIKeySecret(sv.ID)), "API key of the server "+sv.Name))
		}
	}
	if c.Notify.DiscordWebhook != "" {
		changes = append(changes, replacedOrAdded(s.st.HasSecret(store.SecretDiscordWebhook), "Discord webhook"))
	}
	if c.Notify.DiscordNoticesWebhook != "" {
		changes = append(changes, replacedOrAdded(s.st.HasSecret(store.SecretNoticesWebhook), "Discord webhook for settings notices"))
	}
	if c.API.Token != "" {
		// "The API token "API token" is replaced." or "… is added.", with
		// what it can read.
		changes = append(changes, replacedOrAdded(s.hasLegacyToken(), legacyTokenName)+" "+scopeSentence(s.legacyTokenScope()))
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
		announce(notify.Notice{Title: "Configuration imported", Summary: strings.Join(changes, "\n"), Security: true})
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

// errTooManyLogins is the one text for the rate limit of logins.
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
	"relay_lost":          "H12 no relay connection for more than",
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
