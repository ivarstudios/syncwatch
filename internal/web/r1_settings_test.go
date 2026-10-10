package web

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

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
