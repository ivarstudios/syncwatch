package web

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/store"
)

// postPage sends a form and returns the answer with its body.
func (e *env) postPage(path string, form url.Values) (*http.Response, string) {
	e.t.Helper()
	resp, err := e.http.PostForm(e.srv.URL+path, form)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// section returns the part of a settings page from the section with the id
// to the next section.
func section(t *testing.T, page, id string) string {
	t.Helper()
	i := strings.Index(page, `<section class="card" id="`+id+`">`)
	if i < 0 {
		t.Fatalf("no section %s", id)
	}
	if j := strings.Index(page[i+1:], "<section "); j >= 0 {
		return page[i : i+1+j]
	}
	return page[i:]
}

// W-016: a refused save of the Checks form showed the stored values again,
// with the error at the top of the page. Now the page comes back with what
// was typed, the error above the Save button and the bad field marked, and
// its revision still accepts the corrected save.
func TestRefusedChecksSaveKeepsTypedValues(t *testing.T) {
	e := adminEnv(t, twoServers)
	_, page := e.get("/settings")
	csrf, rev := csrfRe.FindStringSubmatch(page)[1], revRe.FindStringSubmatch(page)[1]
	before := e.cfg.Get()
	f := checksForm(before, csrf)
	f.Set("rev", rev)
	f.Set("failing_files", "30m")
	f.Set("stuck_sync", "12h")
	f.Set("pc_offline", "5s") // a duration, but below the range of the threshold
	f.Set("debounce_H2", "20m")
	f.Del("urgent_H1")
	resp, body := e.postPage("/settings/checks", f)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("refused save: %d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	if msg, _ := flash(resp); msg != "" {
		t.Fatalf("flash cookie %q: the error belongs in the Checks section", msg)
	}
	checks := section(t, body, "checks")
	for _, want := range []string{`name="failing_files" value="30m"`, `name="stuck_sync" value="12h"`, `name="pc_offline" value="5s" aria-invalid="true"`,
		`name="debounce_H2" value="20m"`, `id="checks-error"`, "H7 PC offline for more than", "between 10 seconds and 3650 days"} {
		if !strings.Contains(checks, want) {
			t.Errorf("Checks section lacks %s", want)
		}
	}
	if strings.Contains(checks, `name="urgent_H1" aria-label="H1 urgent" checked`) {
		t.Error("the cleared Urgent checkbox of H1 is selected again")
	}
	if strings.Index(checks, `id="checks-error"`) > strings.Index(checks, "Save checks") {
		t.Error("the error is not above the Save button")
	}
	if strings.Contains(section(t, body, "collector"), "aria-invalid") {
		t.Error("an input of another section is marked")
	}
	if got := e.cfg.Get(); configRev(got) != configRev(before) {
		t.Fatalf("the refused save changed the configuration: %+v", got.Thresholds)
	}
	for _, m := range revRe.FindAllStringSubmatch(body, -1) {
		if m[1] != rev {
			t.Fatalf("the page has the revision %s, want %s", m[1], rev)
		}
	}
	// The corrected form goes through with the revision of the answer.
	f.Set("pc_offline", "1h")
	if msg, isErr := flash(e.post("/settings/checks", f)); isErr {
		t.Fatalf("corrected save: %s", msg)
	}
	th := e.cfg.Get().Thresholds
	if th.FailingFiles.String() != "30m" || th.StuckSync.String() != "12h" || th.PCOffline.String() != "1h" || e.cfg.Get().Urgent("H1") {
		t.Fatalf("corrected save stored %+v", th)
	}
}

// A value that is not a duration ("3 days") cannot go through the
// configuration: the form shows the typed text, and a bad debounce marks
// the row of its check.
func TestRefusedChecksSaveKeepsTextThatIsNotADuration(t *testing.T) {
	e := adminEnv(t, twoServers)
	csrf := e.csrf("/settings")
	f := checksForm(e.cfg.Get(), csrf)
	f.Set("pc_offline", "3 days")
	f.Set("debounce_H1", "soon")
	resp, body := e.postPage("/settings/checks", f)
	checks := section(t, body, "checks")
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(checks, `name="pc_offline" value="3 days" aria-invalid="true"`) ||
		!strings.Contains(checks, `<tr class="c-err"><td><strong>H1</strong>`) || !strings.Contains(checks, `name="debounce_H1" value="soon" size="5" aria-label="H1 debounce" aria-invalid="true"`) {
		t.Fatalf("%d: %s", resp.StatusCode, checks)
	}
	msg := checks[strings.Index(checks, `id="checks-error"`):]
	if !strings.Contains(msg, "H1 debounce: the value &#34;soon&#34; is not a duration.") || !strings.Contains(msg, "H7 PC offline for more than: the value must be a duration of more than 0.") {
		t.Fatalf("message: %s", msg)
	}
}

// The Collector and Notifications forms keep what was typed in the same way.
func TestRefusedCollectorAndNotificationSavesKeepTypedValues(t *testing.T) {
	e := adminEnv(t, twoServers)
	csrf := e.csrf("/settings")
	resp, body := e.postPage("/settings/collector", url.Values{"csrf": {csrf}, "mode": {"polled"}, "event_timeout": {"4s"}, "full_check_interval": {"2h"},
		"poll_interval": {"10m"}, "structure_interval": {"30m"}, "baseline_interval": {"3d"}, "request_timeout": {"20s"}, "concurrency": {"4"}, "down_after_failures": {"3"}})
	col := section(t, body, "collector")
	for _, want := range []string{`<option value="polled" selected>`, `name="event_timeout" value="4s" aria-invalid="true"`, `name="full_check_interval" value="2h"`,
		`name="baseline_interval" value="3d"`, `name="concurrency" min="1"`, `value="4"`, `id="collector-error"`, "Event long-poll timeout"} {
		if !strings.Contains(col, want) {
			t.Errorf("Collector section lacks %s", want)
		}
	}
	if resp.StatusCode != http.StatusUnprocessableEntity || e.cfg.Get().Collector.Mode != "events" {
		t.Fatalf("refused collector save: %d, mode %s", resp.StatusCode, e.cfg.Get().Collector.Mode)
	}

	f := notifyForm(e.cfg.Get(), csrf)
	f.Set("quiet_hours", "25:00-07:00")
	f.Set("weekly_top", "7")
	f.Set("weekly_day", "friday")
	f.Set("discord_webhook", "https://discord.com/api/webhooks/2/typed-secret")
	resp, body = e.postPage("/settings/notifications", f)
	n := section(t, body, "notifications")
	for _, want := range []string{`name="quiet_hours" value="25:00-07:00" aria-invalid="true"`, `name="weekly_top" min="1"`, `value="7"`, `<option selected>friday</option>`,
		`id="notifications-error"`, "Quiet hours", "Type the Discord webhook URL again."} {
		if !strings.Contains(n, want) {
			t.Errorf("Notifications section lacks %s", want)
		}
	}
	if resp.StatusCode != http.StatusUnprocessableEntity || strings.Contains(body, "typed-secret") {
		t.Fatalf("refused notification save: %d (the page must not show the typed webhook URL)", resp.StatusCode)
	}
	if v, _ := e.st.Secret(store.SecretDiscordWebhook); v != "https://discord.com/api/webhooks/1/secret-hook" {
		t.Fatalf("the refused save stored the webhook: %q", v)
	}
}

// W-017: the Checks table explains Urgent and Debounce, and each Collector
// field that changes the detection time or the server load has a hint.
func TestSettingsHelpTexts(t *testing.T) {
	e := adminEnv(t, twoServers)
	_, page := e.get("/settings")
	checks, col := section(t, page, "checks"), section(t, page, "collector")
	for _, want := range []string{"<dt>Urgent</dt><dd>The check sends an urgent message for each new finding. The message mentions the role.",
		"<dt>Debounce</dt><dd>The time that a finding must stay open before syncwatch sends the urgent message."} {
		if !strings.Contains(checks, want) {
			t.Errorf("Checks section lacks %q", want)
		}
	}
	const interval = "(A shorter interval finds changes sooner and adds server load.)"
	for label, hint := range map[string]string{
		"Event long-poll timeout":           "(Keep it below the idle timeout of the VPN.)",
		"Full check every":                  interval,
		"Poll interval (polled mode)":       interval,
		"Structure scan every":              interval,
		"Baseline scan every":               interval,
		"Parallel requests for each server": "(A higher number makes scans faster and adds server load.)",
		"Request timeout":                   "(syncwatch stops a request to the server after this time.)",
		"Mark unreachable after failures":   "(A higher number gives fewer false H1 findings and a later H1.)",
	} {
		if !strings.Contains(col, ">"+label+` <span class="muted">`+hint+"</span> <input") {
			t.Errorf("%s: no hint %q", label, hint)
		}
	}
}

// W-043: the forms used the old caps (weekly_top 25, down_after_failures
// 10) and the Notifications form dropped a larger weekly_top silently. The
// forms now use the ranges of the validation, and a value out of range gets
// its message.
func TestFormRangesFollowValidation(t *testing.T) {
	e := adminEnv(t, twoServers)
	csrf := e.csrf("/settings")
	_, page := e.get("/settings")
	if !strings.Contains(page, `name="weekly_top" min="1" max="50"`) || !strings.Contains(page, `name="down_after_failures" min="1" max="20"`) {
		t.Fatal("the forms do not use the ranges of the validation")
	}
	f := notifyForm(e.cfg.Get(), csrf)
	f.Set("weekly_top", "40")
	if msg, isErr := flash(e.post("/settings/notifications", f)); isErr || e.cfg.Get().Notify.WeeklyTop != 40 {
		t.Fatalf("weekly_top 40: %q, stored %d", msg, e.cfg.Get().Notify.WeeklyTop)
	}
	f.Set("weekly_top", "51")
	resp, body := e.postPage("/settings/notifications", f)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Findings listed in the weekly report (notifications.weekly_top): the value must be between 1 and 50.") ||
		e.cfg.Get().Notify.WeeklyTop != 40 {
		t.Fatalf("weekly_top 51: %d, stored %d", resp.StatusCode, e.cfg.Get().Notify.WeeklyTop)
	}
	col := url.Values{"csrf": {csrf}, "mode": {"events"}, "event_timeout": {"50s"}, "full_check_interval": {"4h"}, "poll_interval": {"15m"},
		"structure_interval": {"1h"}, "baseline_interval": {"7d"}, "request_timeout": {"30s"}, "concurrency": {"3"}, "down_after_failures": {"15"}}
	if msg, isErr := flash(e.post("/settings/collector", col)); isErr || e.cfg.Get().Collector.DownAfter != 15 {
		t.Fatalf("down_after_failures 15: %q, stored %d", msg, e.cfg.Get().Collector.DownAfter)
	}
	col.Set("down_after_failures", "21")
	resp, body = e.postPage("/settings/collector", col)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Mark unreachable after failures (collector.down_after_failures): the value must be between 1 and 20.") ||
		!strings.Contains(body, `name="down_after_failures" min="1" max="20" value="21" aria-invalid="true"`) {
		t.Fatalf("down_after_failures 21: %d", resp.StatusCode)
	}
}

// W-032: Replace and Revoke acted at once. Each now asks first, in its own
// form, and the API text names both endpoints and what the answer contains.
// The replace notice says that the old API token stops working.
func TestAPITokenFormsAskFirst(t *testing.T) {
	e := adminEnv(t, twoServers)
	_, page := e.get("/settings")
	csrf := csrfRe.FindStringSubmatch(page)[1]
	api := section(t, page, "api")
	const replace = `data-confirm="Replace the API token? Home Assistant and scripts with the current API token lose access immediately."`
	const revoke = `data-confirm="Revoke the API token? Home Assistant and scripts with the current API token lose access immediately."`
	for _, want := range []string{replace, revoke, "<code>GET /api/v1/status</code> or <code>GET /api/v1/ha</code>",
		"The answer contains device IDs, server URLs and folder paths. It does not contain API keys, passwords or the webhook URL."} {
		if !strings.Contains(api, want) {
			t.Errorf("API section lacks %s", want)
		}
	}
	// Each confirmation is on the form of its own button.
	if a, b, c, d := strings.Index(api, replace), strings.Index(api, `value="generate"`), strings.Index(api, revoke), strings.Index(api, `value="revoke"`); !(a < b && b < c && c < d) {
		t.Errorf("forms and buttons are not paired: %d %d %d %d", a, b, c, d)
	}
	resp, body := e.postPage("/settings/api-token", url.Values{"csrf": {csrf}, "op": {"generate"}})
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "for <code>GET /api/v1/status</code> and <code>GET /api/v1/ha</code>.") {
		t.Fatalf("new token panel: %d", resp.StatusCode)
	}
	if n := e.noticesText(); !strings.Contains(n, "API token replaced: The API token gives read access to /api/v1/status and /api/v1/ha. syncwatch refuses the old API token now.") {
		t.Fatalf("notice: %q", n)
	}
	// Without a token, Generate does not ask.
	e.post("/settings/api-token", url.Values{"csrf": {csrf}, "op": {"revoke"}})
	if _, page = e.get("/settings"); strings.Contains(section(t, page, "api"), "data-confirm") || !strings.Contains(page, "Generate API token") {
		t.Fatal("Generate API token asks for a confirmation")
	}
}

// W-070: Settings → Servers said that only self-signed certificates are
// pinned; CA-signed certificates are pinned too (r0-a).
func TestServersTextPinsEachCertificate(t *testing.T) {
	e := adminEnv(t, twoServers)
	_, page := e.get("/settings")
	servers := section(t, page, "servers")
	if !strings.Contains(servers, "syncwatch pins the certificate of each server on first use, self-signed or CA-signed. A changed certificate opens H3.") ||
		strings.Contains(servers, "accepts a self-signed certificate on first use") {
		t.Fatal("the Servers text does not say that each certificate is pinned")
	}
}

// W-018: with a role, the test message mentions it (notify), so the flash
// tells the administrator what to check when the phone shows nothing.
func TestTestMessageFlashNamesTheRole(t *testing.T) {
	e := adminEnv(t, twoServers)
	csrf := e.csrf("/settings")
	if msg, isErr := flash(e.post("/settings/notifications/test", url.Values{"csrf": {csrf}})); isErr || msg != "syncwatch sent the test message." {
		t.Fatalf("without a role: %q %v", msg, isErr)
	}
	f := notifyForm(e.cfg.Get(), csrf)
	f.Set("discord_role_id", "420000000000000000")
	if msg, isErr := flash(e.post("/settings/notifications", f)); isErr {
		t.Fatal(msg)
	}
	msg, isErr := flash(e.post("/settings/notifications/test", url.Values{"csrf": {csrf}}))
	if isErr || msg != "syncwatch sent the test message with the role mention. If your phone shows no notification, check the role ID. Also make sure that you have the role." {
		t.Fatalf("with a role: %q %v", msg, isErr)
	}
}

// W-039: settings notices showed YAML keys ("thresholds.inactive_project
// changed from 180d to 10m."), both complete lists for one changed entry,
// and the client address glued to the last line. The lines now name the
// labels of the Settings page, a list shows the entries it gained or lost,
// and the address is a line of its own.
func TestNoticeLinesUseLabels(t *testing.T) {
	parse := func(y string) *config.Config {
		t.Helper()
		c, err := config.ParseYAML([]byte(y))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	base := "servers: [{name: AKKA, url: 'https://a:1'}]\n"
	for _, tc := range []struct {
		name, old, new string
		want           []string
	}{
		{"labels and a list", base + "thresholds: {inactive_project: 180d}\nnotifications: {quiet_hours: '22:30-07:00'}\nstructure: {ignore_dirs: ['.*', '#recycle', '@*']}\n",
			base + "thresholds: {inactive_project: 10m}\nnotifications: {quiet_hours: '21:43-21:48'}\nstructure: {ignore_dirs: ['.*', '@*', '@eaDir']}\n",
			[]string{"Quiet hours: 22:30-07:00 → 21:43-21:48", "structure.ignore_dirs: added @eaDir · removed #recycle", "C1 no file change for: 180d → 10m"}},
		{"only the order", base + "structure: {ignore_dirs: [a, b]}\n", base + "structure: {ignore_dirs: [b, a]}\n", []string{"structure.ignore_dirs: [a, b] → [b, a]"}},
		{"an entry two times", base + "structure: {ignore_dirs: [a, b]}\n", base + "structure: {ignore_dirs: [b, a, b]}\n", []string{"structure.ignore_dirs: added b"}},
		{"viewer access", base + "access: {viewer: password}\n", base + "access: {viewer: open}\n", []string{"Viewer access: viewer password → open (no login)"}},
		{"plain http", base, "servers: [{name: AKKA, url: 'http://a:1', allow_http: true}]\n",
			[]string{"The API URL of the server AKKA changed from https://a:1 to http://a:1.", "The server AKKA permits a plain http:// URL now. syncwatch sends the API key unencrypted."}},
	} {
		if got := configChanges(parse(tc.old), parse(tc.new)); strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
	e := adminEnv(t, twoServers)
	csrf := e.csrf("/settings")
	f := checksForm(e.cfg.Get(), csrf)
	f.Set("inactive_project", "10m")
	if msg, isErr := flash(e.post("/settings/checks", f)); isErr {
		t.Fatal(msg)
	}
	if n := e.noticesText(); !strings.HasSuffix(n, "Checks changed: C1 no file change for: 180d → 10m\nChanged from 127.0.0.1.") {
		t.Fatalf("notice: %q", n)
	}
}

// stranger logs in from a browser of its own at the address of the test
// client (another person behind the same VPN or reverse proxy).
func (e *env) stranger(password string) int {
	e.t.Helper()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(e.srv.URL + "/login")
	if err != nil {
		e.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	resp, err = c.PostForm(e.srv.URL+"/login", url.Values{"csrf": {csrfRe.FindStringSubmatch(string(b))[1]}, "password": {password}})
	if err != nil {
		e.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// W-026 (t_limiter_roles.py): an administrator session S, then 5 wrong
// logins from the same address. S then changed access or the password with
// the correct password and got "Too many failed logins", and accept-cert
// asked for the password that S had typed. The confirmations of a session
// now count in a bucket of that session.
func TestFailedLoginsDoNotBlockAdminConfirmations(t *testing.T) {
	e := adminEnv(t, twoServers)
	csrf := e.csrf("/settings")
	for i := 0; i < 5; i++ {
		if code := e.stranger(fmt.Sprintf("typo %d", i)); code != http.StatusUnauthorized {
			t.Fatalf("wrong login %d: %d", i, code)
		}
	}
	// DECISION (W-026): logins keep the count of the address for each role.
	if code := e.stranger("correct horse battery"); code != http.StatusTooManyRequests {
		t.Fatalf("login after 5 failures at the address: %d, want 429", code)
	}
	if msg, isErr := flash(e.post("/settings/servers/akka/accept-cert", url.Values{"csrf": {csrf}, "fingerprint": {"ff"}, "confirm": {"correct horse battery"}})); isErr {
		t.Fatalf("accept-cert with the correct password: %s", msg)
	}
	if msg, isErr := flash(e.post("/settings/servers", url.Values{"csrf": {csrf}, "orig_id": {"akka"}, "name": {"AKKA"}, "url": {"https://a:1"},
		"reset_pin": {"on"}, "confirm": {"secret-key-akka"}})); isErr {
		t.Fatalf("pinned certificate removal with the API key: %s", msg)
	}
	if msg, isErr := flash(e.post("/settings/access", url.Values{"csrf": {csrf}, "mode": {"builtin"}, "viewer": {"open"}, "confirm": {"correct horse battery"}})); isErr {
		t.Fatalf("access change with the correct password: %s", msg)
	}
	if msg, isErr := flash(e.post("/settings/password", url.Values{"csrf": {csrf}, "current": {"correct horse battery"},
		"password": {"a new long password"}, "password2": {"a new long password"}})); isErr {
		t.Fatalf("password change with the correct password: %s", msg)
	}
}

// The session's own wrong confirmations are limited, with a message that
// says so. They block neither another administrator session nor the logins
// at the same address.
func TestAdminConfirmationsAreLimitedBySession(t *testing.T) {
	e := adminEnv(t, twoServers)
	csrf := e.csrf("/settings")
	accept := func(confirm string) (string, bool) {
		return flash(e.post("/settings/servers/akka/accept-cert", url.Values{"csrf": {csrf}, "fingerprint": {"ff"}, "confirm": {confirm}}))
	}
	for i := 0; i < 5; i++ {
		if msg, isErr := accept(fmt.Sprintf("guess %d", i)); !isErr || !strings.Contains(msg, "type the administrator password") {
			t.Fatalf("guess %d: %q %v", i, msg, isErr)
		}
	}
	const limited = "Too many failed tries. Try again in 10 minutes."
	if msg, _ := accept("correct horse battery"); msg != limited {
		t.Fatalf("accept-cert after 5 wrong confirmations: %q", msg)
	}
	if msg, _ := flash(e.post("/settings/access", url.Values{"csrf": {csrf}, "mode": {"builtin"}, "viewer": {"open"}, "confirm": {"correct horse battery"}})); msg != limited {
		t.Fatalf("access change: %q", msg)
	}
	if msg, _ := flash(e.post("/settings/password", url.Values{"csrf": {csrf}, "current": {"correct horse battery"}, "password": {"a new long password"}, "password2": {"a new long password"}})); msg != limited {
		t.Fatalf("password change: %q", msg)
	}
	e.mu.Lock()
	accepted := len(e.accepted)
	e.mu.Unlock()
	if accepted != 0 || e.cfg.Get().Access.Viewer != "none" {
		t.Fatal("a change went through while the session was over the limit")
	}
	// A login at the same address and another administrator session work.
	if code := e.stranger("correct horse battery"); code != http.StatusSeeOther {
		t.Fatalf("login at the same address: %d", code)
	}
	jar, _ := cookiejar.New(nil)
	c2 := &http.Client{Jar: jar, CheckRedirect: e.http.CheckRedirect}
	post2 := func(path string, form url.Values) *http.Response {
		t.Helper()
		resp, err := c2.PostForm(e.srv.URL+path, form)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	resp, err := c2.Get(e.srv.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	csrf2 := csrfRe.FindStringSubmatch(string(b))[1]
	if r := post2("/login", url.Values{"csrf": {csrf2}, "password": {"correct horse battery"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("second session login: %d", r.StatusCode)
	}
	if msg, isErr := flash(post2("/settings/servers/akka/accept-cert", url.Values{"csrf": {csrf2}, "fingerprint": {"ff"}, "confirm": {"correct horse battery"}})); isErr {
		t.Fatalf("accept-cert from another session: %s", msg)
	}
}

// The limiter tells how long a key stays refused, and the message rounds
// it up to whole minutes.
func TestLimiterRetryAfter(t *testing.T) {
	l := newLimiter()
	now := time.Now()
	if d := l.retryAfter("k", now); d != 0 {
		t.Fatalf("unknown key: %v", d)
	}
	for i := 0; i < l.maxIP; i++ {
		l.check("k", now.Add(time.Duration(i)*time.Minute))
		l.fail("k", now.Add(time.Duration(i)*time.Minute), RoleAdmin)
	}
	// The oldest failure (at 0) expires at 10 minutes.
	if d := l.retryAfter("k", now.Add(5*time.Minute)); d != 5*time.Minute {
		t.Fatalf("retry after %v, want 5m", d)
	}
	if ok, _ := l.check("k", now.Add(10*time.Minute+time.Second)); !ok {
		t.Fatal("still refused after the oldest failure expired")
	}
	for d, want := range map[time.Duration]string{10 * time.Minute: "10 minutes", 61 * time.Second: "2 minutes", time.Second: "1 minute", 0: "1 minute"} {
		if got := tooManyTries(d); got != "Too many failed tries. Try again in "+want+"." {
			t.Errorf("%v: %q", d, got)
		}
	}
}

// W-030: after the secret key changed, Settings showed the webhook and the
// API token as "Set" although syncwatch could not decrypt them.
func TestUndecryptableSecretsAreNotShownAsSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "syncwatch.db")
	keyA, keyB := make([]byte, 32), make([]byte, 32)
	keyB[0] = 1
	st, err := store.Open(path, keyA)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SetSecret(store.SecretDiscordWebhook, "https://discord.com/api/webhooks/1/x")
	_ = st.SetSecret(store.SecretAPIToken, "swt_x")
	st.Close()
	st, err = store.Open(path, keyB) // another secret key
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := newEnv(t, twoServers)
	e.web.st, e.st = st, st
	h, _ := HashPassword("correct horse battery")
	_ = st.SetSetting(KeyAdminHash, h)
	if r := e.post("/login", url.Values{"csrf": {e.csrf("/login")}, "password": {"correct horse battery"}}); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("login: %d", r.StatusCode)
	}
	_, page := e.get("/settings")
	if n := section(t, page, "notifications"); !strings.Contains(n, "(Set, but syncwatch cannot decrypt it: the secret key changed. Type it again or restore the old secret key.)") || strings.Contains(n, "(Set. Keep") {
		t.Errorf("webhook: %s", n[:min(len(n), 600)])
	}
	if a := section(t, page, "api"); !strings.Contains(a, "API token: <span class=\"c-err\">set, but syncwatch cannot decrypt it: the secret key changed. Replace the API token or restore the old secret key</span>.") {
		t.Errorf("API token: %s", a)
	}
	// Secrets that syncwatch can decrypt are "Set" as before.
	_, page = adminEnv(t, twoServers).get("/settings")
	if strings.Contains(page, "cannot decrypt") || !strings.Contains(page, "(Set. Keep the field empty to keep the current value.)") || !strings.Contains(page, "API token: <strong>set</strong>.") {
		t.Error("a readable secret is not shown as set")
	}
}

// lockedBuffer collects log lines that the server goroutines write.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// W-031: a refused confirmation (accept certificate with a wrong password
// or API key) left no log line. Each refused confirmation now logs a WARN
// line with the client address (and the server); an empty field is no try.
func TestRefusedConfirmationsAreLogged(t *testing.T) {
	e := adminEnv(t, twoServers)
	logs := &lockedBuffer{}
	e.web.log = slog.New(slog.NewTextHandler(logs, nil))
	csrf := e.csrf("/settings")
	e.post("/settings/servers/akka/accept-cert", url.Values{"csrf": {csrf}, "fingerprint": {"ff"}, "confirm": {"wrong password"}})
	e.post("/settings/servers/akka/accept-cert", url.Values{"csrf": {csrf}, "fingerprint": {"ff"}, "confirm": {""}})
	e.post("/settings/servers", url.Values{"csrf": {csrf}, "orig_id": {"akka"}, "name": {"AKKA"}, "url": {"https://a:1"}, "reset_pin": {"on"}, "confirm": {"wrong key"}})
	e.post("/settings/access", url.Values{"csrf": {csrf}, "mode": {"builtin"}, "viewer": {"open"}, "confirm": {"wrong password"}})
	e.post("/settings/import", url.Values{"csrf": {csrf}, "confirm": {"wrong password"}, "yaml": {"servers: [{name: AKKA, url: 'https://a:1'}]\naccess: {mode: proxy}\n"}})
	e.post("/settings/password", url.Values{"csrf": {csrf}, "current": {"wrong password"}, "password": {"a new long password"}, "password2": {"a new long password"}})
	got := logs.String()
	for _, want := range []string{`level=WARN msg="certificate accept refused" ip=127.0.0.1 server=akka`, `level=WARN msg="pinned certificate removal refused" ip=127.0.0.1 server=akka`,
		`level=WARN msg="access change refused" ip=127.0.0.1`, `level=WARN msg="import refused" ip=127.0.0.1`, `level=WARN msg="password change refused" ip=127.0.0.1`} {
		if strings.Count(got, want) != 1 {
			t.Errorf("log has %d lines %s:\n%s", strings.Count(got, want), want, got)
		}
	}
	if strings.Contains(got, "wrong") {
		t.Fatal("the log shows a typed password")
	}
}
