package web

import (
	"crypto/subtle"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/settings"
	"github.com/ivarstudios/syncwatch/internal/stclient"
	"github.com/ivarstudios/syncwatch/internal/store"
)

type checkRow struct {
	config.CheckInfo
	Enabled, Urgent bool
	Debounce        string
	SeverityLabel   string
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
			Debounce: config.Duration(cfg.Debounce(c.ID)).String(), SeverityLabel: c.Severity.String()})
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
	}
	for k, val := range extra {
		data[k] = val
	}
	return data
}

func (s *Server) setting(key string) (string, bool) {
	v, err := s.st.Setting(key)
	return v, err == nil && v != ""
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request, info requestInfo) {
	s.render(w, r, http.StatusOK, "settings", info, s.settingsData(info, nil))
}

// save applies a configuration change and redirects back to a settings section.
func (s *Server) save(w http.ResponseWriter, r *http.Request, c *config.Config, section, okMsg string) {
	if err := s.hooks.SaveConfig(c); err != nil {
		s.redirect(w, r, "/settings#"+section, "Not saved: "+err.Error(), true)
		return
	}
	s.redirect(w, r, "/settings#"+section, okMsg, false)
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
		s.limiter.success(ip)
		return true, false
	}
	s.limiter.fail(ip, s.now())
	return false, false
}

func (s *Server) handleServerSave(w http.ResponseWriter, r *http.Request, info requestInfo) {
	c := s.cfg.Get().Clone()
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
			s.redirect(w, r, "/settings#servers", "Unknown server.", true)
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
		s.redirect(w, r, "/settings#servers", "Not saved: "+err.Error(), true)
		return
	}
	moved := orig != "" && !config.SameEndpoint(old.URL, sv.URL)
	resetPin := r.FormValue("reset_pin") == "on"
	switch {
	case orig == "" && key == "":
		s.redirect(w, r, "/settings#servers", "A new server needs its Syncthing API key.", true)
		return
	case key == "" && moved:
		// The stored key is only ever sent to the URL it was entered for.
		s.redirect(w, r, "/settings#servers", "Enter the API key for "+sv.URL+": syncwatch only sends an API key to the URL it was entered for.", true)
		return
	case resetPin && key == "" && !s.confirmed(r, sv.ID):
		s.redirect(w, r, "/settings#servers", "To forget the pinned certificate, enter the admin password or this server's API key.", true)
		return
	}
	if key != "" {
		// A key for a new URL also forgets the old URL's pinned certificate.
		if err := settings.SetServerKey(s.st, sv.ID, key, sv.URL); err != nil {
			s.redirect(w, r, "/settings#servers", "Saving the API key failed: "+err.Error(), true)
			return
		}
	}
	if err := s.hooks.SaveConfig(c); err != nil {
		s.redirect(w, r, "/settings#servers", "Not saved: "+err.Error(), true)
		return
	}
	if resetPin && !moved && s.hooks.ResetPin != nil {
		if err := s.hooks.ResetPin(sv.ID); err != nil {
			s.redirect(w, r, "/settings#servers", "Forgetting the pinned certificate failed: "+err.Error(), true)
			return
		}
	}
	switch {
	case orig == "":
		s.announce(r, "Server "+name+" added", "API URL "+sv.URL+".")
	case moved:
		s.announce(r, "Server "+name+": API URL changed", "From "+old.URL+" to "+sv.URL+", with a new API key.")
	case key != "":
		s.announce(r, "Server "+name+": API key replaced", "API URL "+sv.URL+".")
	}
	if resetPin && !moved {
		s.announce(r, "Server "+name+": pinned certificate forgotten", "The next certificate "+sv.URL+" presents is trusted on first use.")
	}
	s.redirect(w, r, "/settings#servers", "Server "+name+" saved.", false)
}

func (s *Server) handleServerDelete(w http.ResponseWriter, r *http.Request, info requestInfo) {
	id := r.PathValue("id")
	c := s.cfg.Get().Clone()
	var keep []config.Server
	name := id
	for _, x := range c.Servers {
		if x.ID != id {
			keep = append(keep, x)
		} else {
			name = x.Name
		}
	}
	c.Servers = keep
	if err := s.hooks.SaveConfig(c); err != nil {
		s.redirect(w, r, "/settings#servers", "Not removed: "+err.Error(), true)
		return
	}
	_ = s.st.DeleteSecret(store.ServerAPIKeySecret(id))
	_ = s.st.SetPin(id, "")
	s.announce(r, "Server "+name+" removed", "Its API key and pinned certificate were deleted.")
	s.redirect(w, r, "/settings#servers", "Server removed.", false)
}

func (s *Server) handleAcceptCert(w http.ResponseWriter, r *http.Request, info requestInfo) {
	id := r.PathValue("id")
	if !s.confirmed(r, id) {
		s.redirect(w, r, "/settings#servers", "To accept the new certificate, enter the admin password or this server's API key.", true)
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
	s.announce(r, "Server "+name+": new certificate accepted", "Pinned SHA-256 "+stclient.ShortFP(fp)+".")
	s.redirect(w, r, "/settings#servers", "New certificate accepted. Reconnecting…", false)
}

func (s *Server) handleNotifySave(w http.ResponseWriter, r *http.Request, info requestInfo) {
	old := s.cfg.Get()
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
	if top, err := strconv.Atoi(r.FormValue("weekly_top")); err == nil && top > 0 && top <= 25 {
		n.WeeklyTop = top
	}
	c.Timezone = strings.TrimSpace(r.FormValue("timezone"))
	c.PublicURL = strings.TrimRight(strings.TrimSpace(r.FormValue("public_url")), "/")
	if err := c.Validate(); err != nil {
		s.redirect(w, r, "/settings#notifications", "Not saved: "+err.Error(), true)
		return
	}
	clearHook, hook := r.FormValue("discord_webhook_clear") == "on", strings.TrimSpace(r.FormValue("discord_webhook"))
	if !clearHook && hook != "" {
		if err := config.CheckDiscordWebhook(hook); err != nil {
			s.redirect(w, r, "/settings#notifications", "Not saved: "+err.Error()+".", true)
			return
		}
	}

	// Changes that can keep problems from being announced are announced
	// to the channels configured before them.
	announce := s.announcer(r)
	changes := roleChange(old, c)
	channels := false
	hadHook := s.st.HasSecret(store.SecretDiscordWebhook)
	switch {
	case clearHook && hadHook:
		_ = s.st.DeleteSecret(store.SecretDiscordWebhook)
		changes = append(changes, "Discord webhook removed: notifications no longer go to Discord.")
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
		s.redirect(w, r, "/settings#notifications", "Not saved: "+err.Error(), true)
		return
	}
	if len(changes) > 0 {
		announce("Notification settings changed", strings.Join(changes, "\n"))
	}
	s.redirect(w, r, "/settings#notifications", "Notification settings saved.", false)
}

func replacedOrAdded(had bool, what string) string {
	if had {
		return what + " replaced."
	}
	return what + " added."
}

// roleChange describes a change to the role urgent messages mention.
func roleChange(old, c *config.Config) []string {
	was, now := old.Notify.DiscordRoleID, c.Notify.DiscordRoleID
	switch {
	case was == now:
		return nil
	case now == "":
		return []string{"Urgent messages no longer mention a role (was " + was + ")."}
	case was == "":
		return []string{"Urgent messages now mention role " + now + "."}
	}
	return []string{"Urgent messages now mention role " + now + " instead of " + was + "."}
}

// checkChanges describes check changes that can keep problems from being
// announced: a check switched off, its urgent pings switched off, or a
// longer wait before an urgent ping.
func checkChanges(old, c *config.Config) []string {
	var out []string
	for _, ci := range config.Catalog {
		name := ci.ID + " (" + ci.Title + ")"
		switch {
		case old.CheckEnabled(ci.ID) && !c.CheckEnabled(ci.ID):
			out = append(out, name+" switched off.")
		case old.Urgent(ci.ID) && !c.Urgent(ci.ID):
			out = append(out, name+" no longer sends urgent pings.")
		case c.Urgent(ci.ID) && c.Debounce(ci.ID) > old.Debounce(ci.ID):
			out = append(out, fmt.Sprintf("%s waits %s instead of %s before an urgent ping.", name,
				config.Duration(c.Debounce(ci.ID)), config.Duration(old.Debounce(ci.ID))))
		}
	}
	return out
}

// channelsChanged tells the notifier to retry held-back messages at once.
func (s *Server) channelsChanged() {
	if s.hooks.ChannelsChanged != nil {
		s.hooks.ChannelsChanged()
	}
}

func (s *Server) handleNotifyTest(w http.ResponseWriter, r *http.Request, info requestInfo) {
	if err := s.hooks.SendTest(r.Context()); err != nil {
		s.redirect(w, r, "/settings#notifications", "Test failed: "+err.Error(), true)
		return
	}
	s.redirect(w, r, "/settings#notifications", "Test message sent.", false)
}

func (s *Server) handleChecksSave(w http.ResponseWriter, r *http.Request, info requestInfo) {
	old := s.cfg.Get()
	c := old.Clone()
	c.Checks = map[string]config.Check{}
	var errs []string
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
				errs = append(errs, ci.ID+": "+err.Error())
			} else if dv.D() != ci.Debounce {
				ch.Debounce = &dv
			}
		}
		if ch.Enabled != nil || ch.Urgent != nil || ch.Debounce != nil {
			c.Checks[ci.ID] = ch
		}
	}
	th := &c.Thresholds
	for name, dst := range map[string]*config.Duration{
		"failing_files": &th.FailingFiles, "stuck_sync": &th.StuckSync, "paused": &th.Paused, "pc_offline": &th.PCOffline, "conflict_window": &th.ConflictWindow,
		"inactive_project": &th.InactiveProject, "old_device": &th.OldDevice, "dead_share": &th.DeadShare,
	} {
		d, err := config.ParseDuration(r.FormValue(name))
		if err != nil || d <= 0 {
			errs = append(errs, name+": invalid duration")
			continue
		}
		*dst = d
	}
	if len(errs) > 0 {
		s.redirect(w, r, "/settings#checks", "Not saved: "+strings.Join(errs, "; "), true)
		return
	}
	if err := s.hooks.SaveConfig(c); err != nil {
		s.redirect(w, r, "/settings#checks", "Not saved: "+err.Error(), true)
		return
	}
	if ch := checkChanges(old, c); len(ch) > 0 {
		s.announce(r, "Checks changed", strings.Join(ch, "\n"))
	}
	s.redirect(w, r, "/settings#checks", "Checks saved.", false)
}

func (s *Server) handleStructureSave(w http.ResponseWriter, r *http.Request, info requestInfo) {
	var doc structureDoc
	dec := yaml.NewDecoder(strings.NewReader(r.FormValue("yaml")))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil && err != io.EOF {
		s.redirect(w, r, "/settings#structure", "Not saved: "+err.Error(), true)
		return
	}
	c := s.cfg.Get().Clone()
	c.Structure, c.Shares = doc.Structure, doc.Shares
	if c.Structure.IgnoreDirs == nil {
		c.Structure.IgnoreDirs = []string{}
	}
	s.save(w, r, c, "structure", "Structure rules saved. The next structure scan uses them.")
}

func (s *Server) handleCollectorSave(w http.ResponseWriter, r *http.Request, info requestInfo) {
	c := s.cfg.Get().Clone()
	col := &c.Collector
	col.Mode = r.FormValue("mode")
	var errs []string
	for name, dst := range map[string]*config.Duration{
		"event_timeout": &col.EventTimeout, "full_check_interval": &col.FullCheckInterval, "poll_interval": &col.PollInterval,
		"structure_interval": &col.StructureInterval, "baseline_interval": &col.BaselineInterval, "request_timeout": &col.RequestTimeout,
	} {
		d, err := config.ParseDuration(r.FormValue(name))
		if err != nil || d <= 0 {
			errs = append(errs, name+": invalid duration")
			continue
		}
		*dst = d
	}
	if n, err := strconv.Atoi(r.FormValue("concurrency")); err == nil {
		col.Concurrency = n
	}
	if n, err := strconv.Atoi(r.FormValue("down_after_failures")); err == nil && n > 0 {
		col.DownAfter = n
	}
	if len(errs) > 0 {
		s.redirect(w, r, "/settings#collector", "Not saved: "+strings.Join(errs, "; "), true)
		return
	}
	s.save(w, r, c, "collector", "Collector settings saved; collectors restarted.")
}

func (s *Server) handleAccessSave(w http.ResponseWriter, r *http.Request, info requestInfo) {
	old := s.cfg.Get()
	c := old.Clone()
	mode := r.FormValue("mode")
	if mode == config.AccessProxy && c.Access.Mode != config.AccessProxy && r.FormValue("confirm_proxy") != "on" {
		s.redirect(w, r, "/settings#access", "Tick the confirmation box to switch off built-in login.", true)
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
			s.redirect(w, r, "/settings#access", "Set a viewer password to use password-protected viewing.", true)
			return
		}
	}
	if c.Access.Mode == config.AccessBuiltin && !s.adminConfigured() {
		s.redirect(w, r, "/settings#access", "Set an admin password before switching built-in login on.", true)
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
		s.redirect(w, r, "/settings#access", "Not saved: "+err.Error(), true)
		return
	}
	changes := accessChanges(old, c)
	if newViewerPW {
		changes = append(changes, "New viewer password set; viewers were logged out.")
	}
	for _, ch := range changes {
		s.announce(r, "Access settings changed", ch)
	}
	s.redirect(w, r, "/settings#access", "Access settings saved.", false)
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
	const need = "This change lets more people in: enter the admin password to confirm it."
	pw := r.FormValue("confirm")
	if pw == "" {
		return need
	}
	ok, limited := s.limitedCheck(r, func() bool { return CheckPassword(pw, h) })
	switch {
	case limited:
		return "Too many failed attempts. Try again in a few minutes."
	case !ok:
		return "The admin password is wrong. " + need
	}
	return ""
}

// accessChanges describes changes to who can reach the dashboard.
func accessChanges(old, c *config.Config) []string {
	var out []string
	if old.Access.Mode != c.Access.Mode {
		if c.Access.Mode == config.AccessProxy {
			out = append(out, "Built-in login switched off (proxy mode): everyone who can reach syncwatch is admin.")
		} else {
			out = append(out, "Built-in login switched on.")
		}
	}
	if old.Access.Viewer != c.Access.Viewer {
		out = append(out, "Viewing without the admin password: "+old.Access.Viewer+" → "+c.Access.Viewer+".")
	}
	return out
}

// serverChanges describes servers added, removed or moved to another URL.
func serverChanges(old, c *config.Config) []string {
	var out []string
	before := map[string]config.Server{}
	for _, s := range old.Servers {
		before[s.ID] = s
	}
	for _, s := range c.Servers {
		o, ok := before[s.ID]
		switch {
		case !ok:
			out = append(out, "Server "+s.Name+" added ("+s.URL+").")
		case !config.SameEndpoint(o.URL, s.URL):
			out = append(out, "Server "+s.Name+": API URL changed from "+o.URL+" to "+s.URL+".")
		}
		delete(before, s.ID)
	}
	for _, s := range old.Servers {
		if _, gone := before[s.ID]; gone {
			out = append(out, "Server "+s.Name+" removed.")
		}
	}
	return out
}

func (s *Server) handlePasswordSave(w http.ResponseWriter, r *http.Request, info requestInfo) {
	if h, _ := s.setting(KeyAdminHash); h != "" {
		ok, limited := s.limitedCheck(r, func() bool { return CheckPassword(r.FormValue("current"), h) })
		switch {
		case limited:
			s.redirect(w, r, "/settings#access", "Too many failed attempts. Try again in a few minutes.", true)
			return
		case !ok:
			s.redirect(w, r, "/settings#access", "The current password is wrong.", true)
			return
		}
	}
	pw := r.FormValue("password")
	if err := checkNewPassword(pw); err != nil {
		s.redirect(w, r, "/settings#access", err.Error(), true)
		return
	}
	if pw != r.FormValue("password2") {
		s.redirect(w, r, "/settings#access", "The new passwords don't match.", true)
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
	s.redirect(w, r, "/settings#access", "Admin password changed; other admin sessions were logged out.", false)
}

func (s *Server) handleAPIToken(w http.ResponseWriter, r *http.Request, info requestInfo) {
	if r.FormValue("op") == "revoke" {
		_ = s.st.DeleteSecret(store.SecretAPIToken)
		s.redirect(w, r, "/settings#api", "API token revoked.", false)
		return
	}
	tok := "swt_" + randomToken(24)
	if err := s.st.SetSecret(store.SecretAPIToken, tok); err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
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
		s.redirect(w, r, "/settings#import", "Choose a YAML file or paste one.", true)
		return
	}
	c, err := config.ParseYAML(data)
	if err != nil {
		s.redirect(w, r, "/settings#import", "Import failed: "+err.Error(), true)
		return
	}
	old := s.cfg.Get()
	// Checked before anything from the file is stored.
	if weakensAccess(old, c) {
		if msg := s.confirmAdmin(r); msg != "" {
			s.redirect(w, r, "/settings#import", "Not imported: the file switches login off or opens viewing. "+msg, true)
			return
		}
	}
	// Announced to the channels configured before the file replaces any.
	announce := s.announcer(r)
	changes := append(serverChanges(old, c), accessChanges(old, c)...)
	changes = append(changes, checkChanges(old, c)...)
	changes = append(changes, roleChange(old, c)...)
	if c.Notify.DiscordWebhook != "" {
		changes = append(changes, replacedOrAdded(s.st.HasSecret(store.SecretDiscordWebhook), "Discord webhook"))
	}
	n, err := settings.ImportSecrets(s.st, c)
	if err != nil {
		s.redirect(w, r, "/settings#import", "Import failed: "+err.Error(), true)
		return
	}
	if err := s.hooks.SaveConfig(c); err != nil {
		s.redirect(w, r, "/settings#import", "Not saved: "+err.Error(), true)
		return
	}
	if err := settings.DropRemovedServers(s.st, old, c); err != nil {
		s.log.Error("deleting secrets of removed servers", "err", err)
	}
	if n > 0 {
		s.channelsChanged() // the file may have brought a new webhook
	}
	if len(changes) > 0 {
		announce("Configuration imported", strings.Join(changes, "\n"))
	}
	msg := "Configuration imported."
	if n > 0 {
		msg += fmt.Sprintf(" %d secret(s) from the file were stored encrypted.", n)
	}
	if need := settings.NeedKey(s.st, c); len(need) > 0 {
		msg += " Enter the API key again for " + strings.Join(need, ", ") + ": an API key is only sent to the URL it was entered for."
	}
	s.redirect(w, r, "/settings#import", msg, false)
}
