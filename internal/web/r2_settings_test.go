package web

import (
	"context"
	"errors"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/notify"
	"github.com/ivarstudios/syncwatch/internal/store"
)

// The helpers of the import in 2 steps (W-055).

var (
	impIDRe    = regexp.MustCompile(`name="import_id" value="([^"]+)"`)
	impErrorRe = regexp.MustCompile(`id="import-error"[^>]*>✖ ([^<]*)</p>`)
)

// impPreview sends step 1 of an import with the pasted YAML. It returns the
// answer, the page and the ID of the pending import ("" when the answer has
// no preview).
func impPreview(e *env, csrf, yaml string) (*http.Response, string, string) {
	e.t.Helper()
	resp, body := e.postPage("/settings/import", url.Values{"csrf": {csrf}, "yaml": {yaml}})
	id := ""
	if m := impIDRe.FindStringSubmatch(body); m != nil {
		id = m[1]
	}
	return resp, body, id
}

// impConfirm sends step 2 of an import: the form of the preview with the
// fields of form (delete_servers, confirm).
func impConfirm(e *env, csrf, id string, form url.Values) (*http.Response, string) {
	e.t.Helper()
	f := url.Values{"csrf": {csrf}, "import_id": {id}}
	for k, v := range form {
		f[k] = v
	}
	return e.postPage("/settings/import", f)
}

// impError returns the message of a refused step 2 on the preview page.
func impError(body string) string {
	if m := impErrorRe.FindStringSubmatch(body); m != nil {
		return html.UnescapeString(m[1])
	}
	return ""
}

// impFile sends both steps of an import. It returns the message of the
// answer and whether it is an error: the flash of a redirect, or the error
// of the preview page when step 2 is refused.
func impFile(e *env, csrf, yaml string, form url.Values) (string, bool) {
	e.t.Helper()
	resp, _, id := impPreview(e, csrf, yaml)
	if id == "" {
		return flash(resp)
	}
	resp, body := impConfirm(e, csrf, id, form)
	if resp.StatusCode == http.StatusUnprocessableEntity {
		return impError(body), true
	}
	return flash(resp)
}

// lastNotice returns the newest settings notice and checks that it is the
// notice number n (1 = the first).
func lastNotice(t *testing.T, e *env, n int) notify.Notice {
	t.Helper()
	all := e.noticeValues()
	if len(all) != n {
		t.Fatalf("%d notices, want %d: %q", len(all), n, e.noticeList())
	}
	return all[n-1]
}

const impBase = `servers: [{name: AKKA, url: "https://a:1", api_key: secret-key-akka}, {name: SOL, url: "https://b:1"}, {name: SIRIUS, url: "https://c:1", api_key: secret-key-sirius}]
notifications: {discord_webhook: "https://discord.com/api/webhooks/1/secret-hook"}
api: {token: swt_test_token}
access: {viewer: password}
thresholds: {paused: 1d}
`

// W-055: step 1 of an import only shows a preview: what the file changes,
// the sections that go back to their default values, the servers that it
// removes with their API keys, the servers that need the API key again and
// the kinds of secrets in the file. It changes nothing, and the page has no
// secret from the file. Step 2 applies the file.
func TestImportPreviewChangesNothing(t *testing.T) {
	e := adminEnv(t, impBase)
	_ = e.st.SetPin("akka", "pinned-akka")
	_ = e.st.SetPin("sirius", "pinned-sirius")
	csrf := e.csrf("/settings")
	before := e.cfg.Get()
	file := "servers: [{name: SOL, url: 'https://b:1', api_key: sol-file-key-secret}, {name: SIRIUS, url: 'https://sirius.invalid:8384'}]\n" +
		"notifications: {discord_webhook: 'https://discord.com/api/webhooks/9/file-hook-secret', discord_notices_webhook: 'https://discord.com/api/webhooks/8/file-notices-secret'}\n" +
		"api: {token: swt_file_token_secret}\n"
	resp, body, id := impPreview(e, csrf, file)
	if resp.StatusCode != http.StatusOK || id == "" {
		t.Fatalf("preview: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	card := html.UnescapeString(section(t, body, "import"))
	for _, want := range []string{
		"Preview of the import",
		"Sections that the file changes:</strong> Servers, Checks, Access.", // the webhooks are secrets, not settings
		"Access: not in the file. Viewer access changes to administrator only.",
		"Thresholds of the checks: not in the file. syncwatch uses the default values.",
		"syncwatch removes the server AKKA and deletes its API key and pinned certificate.",
		"The API URL of the server SIRIUS changes. Type the API key again for SIRIUS.",
		`Secrets in the file:</strong> the API key of the server SOL, the Discord webhook, the Discord webhook for settings notices, the API token "API token".`,
		"H6 paused for more than: 1d → 3d", // the lines of the settings notice
		`<input type="checkbox" name="delete_servers"> Remove the server AKKA (syncwatch deletes its API key)</label>`,
		`<input type="password" name="confirm"`,
		`<a href="/settings#import">Cancel</a>`,
	} {
		if !strings.Contains(card, want) {
			t.Errorf("preview lacks %q", want)
		}
	}
	for _, unchanged := range []string{"Time zone", "Collector", "Dashboard URL", "Devices: not in the file"} {
		if strings.Contains(card, unchanged+": not in the file") {
			t.Errorf("preview names %q, which keeps its value", unchanged)
		}
	}
	if strings.Contains(card, "The file gives access to more people") {
		t.Error("the file gives access to fewer people")
	}
	for _, secret := range []string{"sol-file-key-secret", "file-hook-secret", "file-notices-secret", "swt_file_token_secret", "secret-key-akka", "secret-hook", "swt_test_token"} {
		if strings.Contains(body, secret) {
			t.Errorf("the page has the secret %q", secret)
		}
	}
	// Step 1 changed nothing.
	if configRev(e.cfg.Get()) != configRev(before) {
		t.Fatal("the preview changed the configuration")
	}
	if v, _ := e.st.Secret(store.SecretDiscordWebhook); v != "https://discord.com/api/webhooks/1/secret-hook" || e.st.HasSecret(store.SecretNoticesWebhook) {
		t.Fatalf("the preview changed a webhook: %q", v)
	}
	if !e.hasAPIToken("swt_test_token") || e.hasAPIToken("swt_file_token_secret") {
		t.Fatal("the preview changed the API token")
	}
	if key, _, _ := e.st.ServerKey("akka"); key != "secret-key-akka" || e.st.HasSecret(store.ServerAPIKeySecret("sol")) {
		t.Fatalf("the preview changed an API key: %q", key)
	}
	if pin, _ := e.st.Pin("akka"); pin != "pinned-akka" {
		t.Fatal("the preview removed a pinned certificate")
	}
	if n := len(e.noticeList()); n != 0 {
		t.Fatalf("the preview sent %d notices", n)
	}

	// Step 2 with the checkbox and the password applies the file.
	resp, _ = impConfirm(e, csrf, id, url.Values{"delete_servers": {"on"}, "confirm": {"correct horse battery"}})
	if msg, isErr := flash(resp); isErr || !strings.HasPrefix(msg, "The configuration is imported. syncwatch encrypted and stored 4 secrets from the file.") {
		t.Fatalf("step 2: %q %v", msg, isErr)
	}
	c := e.cfg.Get()
	if len(c.Servers) != 2 || c.Access.Viewer != "none" || c.Thresholds.Paused.String() != "3d" {
		t.Fatalf("imported: %+v %+v", c.Servers, c.Access)
	}
	if e.st.HasSecret(store.ServerAPIKeySecret("akka")) {
		t.Error("the API key of the removed server AKKA is kept")
	}
	if pin, _ := e.st.Pin("akka"); pin != "" {
		t.Error("the pinned certificate of the removed server AKKA is kept")
	}
	if key, _, _ := e.st.ServerKey("sol"); key != "sol-file-key-secret" {
		t.Errorf("API key of SOL: %q", key)
	}
	if v, _ := e.st.Secret(store.SecretNoticesWebhook); v != "https://discord.com/api/webhooks/8/file-notices-secret" {
		t.Errorf("webhook for settings notices: %q", v)
	}
	nt := lastNotice(t, e, 1)
	if nt.Title != "Configuration imported" || !nt.Security {
		t.Fatalf("notice %+v", nt)
	}
	for _, want := range []string{"The server AKKA is removed.", "The API URL of the server SIRIUS changed from https://c:1 to https://sirius.invalid:8384.",
		"The API key of the server SOL is added.", "The Discord webhook is replaced.", "The Discord webhook for settings notices is added.", `The API token "API token" is replaced.`} {
		if !strings.Contains(nt.Summary, want) {
			t.Errorf("notice lacks %q: %q", want, nt.Summary)
		}
	}
	// The pending import is gone.
	if msg, isErr := flash(e.post("/settings/import", url.Values{"csrf": {csrf}, "import_id": {id}})); !isErr || msg != "The preview of the import expired. Select the file again." {
		t.Fatalf("second step 2: %q %v", msg, isErr)
	}
}

// W-055 (verification observation 3): an import of servers: [] with a wrong
// password removed both servers and their API keys. Now the removal needs
// the checkbox and the administrator password.
func TestImportThatRemovesServersNeedsCheckboxAndPassword(t *testing.T) {
	e := adminEnv(t, twoServers)
	logs := &lockedBuffer{}
	e.web.log = slog.New(slog.NewTextHandler(logs, nil))
	_ = e.st.SetPin("akka", "pinned-akka")
	_ = e.st.SetServerKey("sol", "secret-key-sol", "https://b:1")
	csrf := e.csrf("/settings")
	before := configRev(e.cfg.Get())
	_, body, id := impPreview(e, csrf, "servers: []\n")
	card := section(t, body, "import")
	for _, want := range []string{"syncwatch removes the server AKKA and deletes its API key and pinned certificate.", "syncwatch removes the server SOL and deletes its API key and pinned certificate.",
		"Remove the servers AKKA and SOL (syncwatch deletes their API keys)", `name="confirm"`} {
		if !strings.Contains(card, want) {
			t.Errorf("preview lacks %q", want)
		}
	}
	unchanged := func(step string) {
		t.Helper()
		if configRev(e.cfg.Get()) != before || !e.st.HasSecret(store.ServerAPIKeySecret("akka")) || !e.st.HasSecret(store.ServerAPIKeySecret("sol")) {
			t.Fatalf("%s: the refused import changed the configuration or deleted an API key", step)
		}
		if pin, _ := e.st.Pin("akka"); pin != "pinned-akka" {
			t.Fatalf("%s: the refused import removed the pinned certificate", step)
		}
		if n := len(e.noticeList()); n != 0 {
			t.Fatalf("%s: %d notices", step, n)
		}
	}
	for _, tc := range []struct {
		name string
		form url.Values
		want string
	}{
		{"no checkbox", url.Values{"confirm": {"correct horse battery"}}, "Not imported. Select the checkbox to remove the servers AKKA and SOL."},
		{"no password", url.Values{"delete_servers": {"on"}}, "Not imported. The import removes servers and deletes their API keys. Type the administrator password to accept the import."},
		{"wrong password", url.Values{"delete_servers": {"on"}, "confirm": {"wrong password"}},
			"Not imported. The administrator password is not correct. The import removes servers and deletes their API keys. Type the administrator password to accept the import."},
	} {
		resp, body := impConfirm(e, csrf, id, tc.form)
		if got := impError(body); resp.StatusCode != http.StatusUnprocessableEntity || got != tc.want {
			t.Fatalf("%s: %d %q", tc.name, resp.StatusCode, got)
		}
		// The refused step shows the preview again with the same pending import.
		if m := impIDRe.FindStringSubmatch(body); m == nil || m[1] != id {
			t.Fatalf("%s: the page has no preview of the pending import", tc.name)
		}
		unchanged(tc.name)
	}
	if got := strings.Count(logs.String(), `level=WARN msg="import refused" ip=127.0.0.1`); got != 1 {
		t.Fatalf("%d WARN lines for the wrong password:\n%s", got, logs.String())
	}
	resp, _ := impConfirm(e, csrf, id, url.Values{"delete_servers": {"on"}, "confirm": {"correct horse battery"}})
	if msg, isErr := flash(resp); isErr || msg != "The configuration is imported." {
		t.Fatalf("checkbox and password: %q %v", msg, isErr)
	}
	if len(e.cfg.Get().Servers) != 0 || e.st.HasSecret(store.ServerAPIKeySecret("akka")) || e.st.HasSecret(store.ServerAPIKeySecret("sol")) {
		t.Fatal("the servers or their API keys are kept")
	}
	if pin, _ := e.st.Pin("akka"); pin != "" {
		t.Fatal("the pinned certificate of AKKA is kept")
	}
	if nt := lastNotice(t, e, 1); !nt.Security || !strings.Contains(nt.Summary, "The server AKKA is removed.\nThe server SOL is removed.") {
		t.Fatalf("notice %+v", nt)
	}
}

// In proxy mode without an administrator password, the checkbox alone
// confirms the removal of servers: there is no password to type.
func TestImportInProxyModeNeedsOnlyTheCheckbox(t *testing.T) {
	e := newEnv(t, twoServers+"access: {mode: proxy}\n")
	csrf := e.csrf("/settings")
	file := "servers: [{name: AKKA, url: 'https://a:1'}]\naccess: {mode: proxy}\n"
	_, body, id := impPreview(e, csrf, file)
	if card := section(t, body, "import"); !strings.Contains(card, "Remove the server SOL (syncwatch deletes its API key)") || strings.Contains(card, `name="confirm"`) {
		t.Fatalf("preview: %s", card)
	}
	if resp, body := impConfirm(e, csrf, id, nil); resp.StatusCode != http.StatusUnprocessableEntity || impError(body) != "Not imported. Select the checkbox to remove the server SOL." {
		t.Fatalf("without the checkbox: %d %q", resp.StatusCode, impError(body))
	}
	if msg, isErr := flash(e.post("/settings/import", url.Values{"csrf": {csrf}, "import_id": {id}, "delete_servers": {"on"}})); isErr || len(e.cfg.Get().Servers) != 1 {
		t.Fatalf("with the checkbox: %q %v", msg, isErr)
	}
}

// The pending import belongs to the session that sent the file, expires
// after 15 minutes, and at most 20 wait (the oldest goes first).
func TestImportPreviewIsBoundToSessionAndTime(t *testing.T) {
	e := adminEnv(t, twoServers)
	csrf := e.csrf("/settings")
	file := "servers: [{name: AKKA, url: 'https://a:1'}, {name: SOL, url: 'https://b:1'}]\nthresholds: {paused: 1d}\n"
	_, _, id := impPreview(e, csrf, file)
	const expired = "The preview of the import expired. Select the file again."

	// Another administrator session cannot confirm it.
	jar, _ := cookiejar.New(nil)
	c2 := &http.Client{Jar: jar, CheckRedirect: e.http.CheckRedirect}
	resp, err := c2.Get(e.srv.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	csrf2 := csrfRe.FindStringSubmatch(string(b))[1]
	if resp, err = c2.PostForm(e.srv.URL+"/login", url.Values{"csrf": {csrf2}, "password": {"correct horse battery"}}); err != nil || resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("second session: %v", err)
	}
	resp.Body.Close()
	if resp, err = c2.PostForm(e.srv.URL+"/settings/import", url.Values{"csrf": {csrf2}, "import_id": {id}}); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if msg, isErr := flash(resp); !isErr || msg != expired || e.cfg.Get().Thresholds.Paused.String() != "3d" {
		t.Fatalf("another session: %q %v", msg, isErr)
	}

	// After 15 minutes, the preview expired.
	e.web.now = func() time.Time { return time.Now().Add(15*time.Minute + time.Second) }
	msg, isErr := flash(e.post("/settings/import", url.Values{"csrf": {csrf}, "import_id": {id}}))
	e.web.now = time.Now
	if !isErr || msg != expired || e.cfg.Get().Thresholds.Paused.String() != "3d" {
		t.Fatalf("after 15 minutes: %q %v", msg, isErr)
	}

	// At most 20 pending imports: the 21st preview drops the oldest.
	var ids []string
	for range maxPendingImports + 1 {
		_, _, id := impPreview(e, csrf, file)
		ids = append(ids, id)
	}
	if msg, isErr := flash(e.post("/settings/import", url.Values{"csrf": {csrf}, "import_id": {ids[0]}})); !isErr || msg != expired {
		t.Fatalf("the oldest of 21: %q %v", msg, isErr)
	}
	if msg, isErr := flash(e.post("/settings/import", url.Values{"csrf": {csrf}, "import_id": {ids[1]}})); isErr || e.cfg.Get().Thresholds.Paused.String() != "1d" {
		t.Fatalf("the second of 21: %q %v", msg, isErr)
	}
	// The configuration changed: a preview of the configuration before is stale.
	if msg, isErr := flash(e.post("/settings/import", url.Values{"csrf": {csrf}, "import_id": {ids[2]}})); !isErr || !strings.Contains(msg, "Reload the page") {
		t.Fatalf("a preview of an older configuration: %q %v", msg, isErr)
	}
}

// The step 1 form has no data-confirm any more (the preview replaces it) and
// no password input; the card names the notices webhook as a secret.
func TestImportCardTexts(t *testing.T) {
	e := adminEnv(t, twoServers)
	_, page := e.get("/settings")
	card := section(t, page, "import")
	if strings.Contains(card, "data-confirm") || strings.Contains(card, `name="confirm"`) || !strings.Contains(card, "<code>discord_notices_webhook</code>") ||
		!strings.Contains(card, "First, syncwatch shows a preview of the changes.") || !strings.Contains(card, "Show the preview</button>") {
		t.Fatalf("card: %s", card)
	}
}

// An imported api.token replaces the value of the API token "API token" or
// adds that API token (contract with fixer 5). The line names the API token.
func TestImportAPITokenLine(t *testing.T) {
	for _, tc := range []struct {
		name, env string
		row       string // the name of an API token row before the import
		want      string
	}{
		{"old single API token", twoServers, "", `The API token "API token" is replaced.`},
		{"no API token", `servers: [{name: AKKA, url: "https://a:1"}]`, "", `The API token "API token" is added.`},
		{"row API token", `servers: [{name: AKKA, url: "https://a:1"}]`, "API token", `The API token "API token" is replaced.`},
		{"other row", `servers: [{name: AKKA, url: "https://a:1"}]`, "Home Assistant", `The API token "API token" is added.`},
	} {
		e := adminEnv(t, tc.env)
		if tc.row != "" {
			if _, err := e.st.AddAPIToken(tc.row, store.ScopeFull, "swt_row_"+strings.ReplaceAll(tc.row, " ", "_"), time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		csrf := e.csrf("/settings")
		servers := "servers: [{name: AKKA, url: 'https://a:1'}]\n"
		if strings.Contains(tc.env, "SOL") {
			servers = "servers: [{name: AKKA, url: 'https://a:1'}, {name: SOL, url: 'https://b:1'}]\n"
		}
		if msg, isErr := impFile(e, csrf, servers+"api: {token: swt_imported}\n", nil); isErr {
			t.Fatalf("%s: %s", tc.name, msg)
		}
		if n := e.noticesText(); !strings.Contains(n, tc.want) || strings.Contains(n, "swt_imported") {
			t.Errorf("%s: %q", tc.name, n)
		}
	}
}

// W-051: the webhook for settings notices has its own input, a checkbox to
// remove it and a button for a test notice. Its changes are security
// notices.
func TestNoticesWebhookForm(t *testing.T) {
	e := adminEnv(t, twoServers)
	csrf := e.csrf("/settings")
	notifications := func() string {
		t.Helper()
		_, page := e.get("/settings")
		return section(t, page, "notifications")
	}
	n := notifications()
	if !strings.Contains(n, `Discord webhook for settings notices (optional) <span class="muted">(not set)</span>`) ||
		!strings.Contains(n, "Settings notices go to this webhook. Without it, they go to the webhook above.") ||
		strings.Contains(n, "discord_notices_webhook_clear") || strings.Contains(n, "Send test settings notice") {
		t.Fatalf("not set: %s", n)
	}
	count := 0
	save := func(f url.Values) (string, bool) {
		t.Helper()
		f.Set("csrf", csrf)
		return flash(e.post("/settings/notifications", f))
	}

	f := notifyForm(e.cfg.Get(), csrf)
	f.Set("discord_notices_webhook", " https://discord.com/api/webhooks/7/notices-hook/ ")
	if msg, isErr := save(f); isErr {
		t.Fatal(msg)
	}
	if v, _ := e.st.Secret(store.SecretNoticesWebhook); v != "https://discord.com/api/webhooks/7/notices-hook" {
		t.Fatalf("stored %q", v)
	}
	count++
	if nt := lastNotice(t, e, count); !nt.Security || !strings.HasPrefix(nt.Summary, "The Discord webhook for settings notices is added.\n") {
		t.Fatalf("notice %+v", nt)
	}
	n = notifications()
	if !strings.Contains(n, `Discord webhook for settings notices (optional) <span class="muted">(Set. Keep the field empty to keep the current value.)</span>`) ||
		!strings.Contains(n, `name="discord_notices_webhook_clear"`) || !strings.Contains(n, "Remove the webhook for settings notices") ||
		!strings.Contains(n, `action="/settings/notifications/test-notices"`) || strings.Contains(n, "notices-hook") {
		t.Fatalf("set: %s", n)
	}

	f = notifyForm(e.cfg.Get(), csrf)
	f.Set("discord_notices_webhook", "https://discord.com/api/webhooks/7/notices-hook-2")
	if msg, isErr := save(f); isErr {
		t.Fatal(msg)
	}
	count++
	if nt := lastNotice(t, e, count); !nt.Security || !strings.Contains(nt.Summary, "The Discord webhook for settings notices is replaced.") {
		t.Fatalf("notice %+v", nt)
	}

	// A refused save asks to type it again and stores nothing.
	f = notifyForm(e.cfg.Get(), csrf)
	f.Set("discord_notices_webhook", "http://10.0.0.5:8080/api/webhooks/1/typed-notices")
	resp, body := e.postPage("/settings/notifications", f)
	n = section(t, body, "notifications")
	if resp.StatusCode != http.StatusUnprocessableEntity || strings.Contains(body, "typed-notices") ||
		!strings.Contains(n, "Discord webhook for settings notices (notifications.discord_notices_webhook): the value must be a webhook URL from Discord") ||
		!strings.Contains(n, "Type the Discord webhook for settings notices again.") || !strings.Contains(n, `name="discord_notices_webhook" autocomplete="off" placeholder="https://discord.com/api/webhooks/…" aria-invalid="true"`) {
		t.Fatalf("refused: %d %s", resp.StatusCode, n)
	}
	f.Set("discord_notices_webhook", "https://discord.com/api/webhooks/7/typed-notices")
	f.Set("quiet_hours", "25:00-07:00")
	if resp, body := e.postPage("/settings/notifications", f); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Type the Discord webhook for settings notices again.") {
		t.Fatalf("refused for another field: %d", resp.StatusCode)
	}
	if v, _ := e.st.Secret(store.SecretNoticesWebhook); v != "https://discord.com/api/webhooks/7/notices-hook-2" {
		t.Fatalf("a refused save stored %q", v)
	}

	// The test button calls the hook and shows its result.
	calls := 0
	e.web.hooks.SendNoticesTest = func(context.Context) error { calls++; return nil }
	if msg, isErr := flash(e.post("/settings/notifications/test-notices", url.Values{"csrf": {csrf}})); isErr || msg != "syncwatch sent the test settings notice." || calls != 1 {
		t.Fatalf("test notice: %q %v %d", msg, isErr, calls)
	}
	e.web.hooks.SendNoticesTest = func(context.Context) error { return errors.New("discord: HTTP 404 unknown webhook") }
	if msg, isErr := flash(e.post("/settings/notifications/test-notices", url.Values{"csrf": {csrf}})); !isErr || msg != "The test settings notice failed. Discord: HTTP 404 unknown webhook." {
		t.Fatalf("failed test notice: %q %v", msg, isErr)
	}

	f = notifyForm(e.cfg.Get(), csrf)
	f.Set("discord_notices_webhook_clear", "on")
	if msg, isErr := save(f); isErr {
		t.Fatal(msg)
	}
	count++
	if nt := lastNotice(t, e, count); !nt.Security || !strings.HasPrefix(nt.Summary, "The Discord webhook for settings notices is removed.\n") || e.st.HasSecret(store.SecretNoticesWebhook) {
		t.Fatalf("notice %+v", nt)
	}
	// Other notification settings are not security notices.
	f = notifyForm(e.cfg.Get(), csrf)
	f.Set("discord_role_id", "420000000000000000")
	if msg, isErr := save(f); isErr {
		t.Fatal(msg)
	}
	count++
	if nt := lastNotice(t, e, count); nt.Security {
		t.Fatalf("role ID notice %+v", nt)
	}
}

// W-030 for the webhook for settings notices: after the secret key changed,
// the form does not say "Set" for a webhook that syncwatch cannot decrypt.
func TestUndecryptableNoticesWebhookIsNotShownAsSet(t *testing.T) {
	path := filepath.Join(t.TempDir(), "syncwatch.db")
	keyA, keyB := make([]byte, 32), make([]byte, 32)
	keyB[0] = 1
	st, err := store.Open(path, keyA)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SetSecret(store.SecretNoticesWebhook, "https://discord.com/api/webhooks/7/x")
	st.Close()
	if st, err = store.Open(path, keyB); err != nil { // another secret key
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
	if n := section(t, page, "notifications"); !strings.Contains(n, `Discord webhook for settings notices (optional) <span class="c-err">(Set, but syncwatch cannot decrypt it: the secret key changed. Type it again or restore the old secret key.)</span>`) {
		t.Fatalf("notices webhook: %s", n)
	}
}

// W-051: the Settings page says how many settings notices wait for a
// channel, and the notification log names the channel of the settings
// notices.
func TestKeptNoticesAndTheirChannel(t *testing.T) {
	e := adminEnv(t, twoServers)
	_, page := e.get("/settings")
	if strings.Contains(page, "wait for a channel") || strings.Contains(page, "waits for a channel") {
		t.Fatal("a line without kept notices")
	}
	e.setDelivery(notify.Delivery{NoticesKept: 2})
	if _, page = e.get("/settings"); !strings.Contains(section(t, page, "notifications"), "2 settings notices wait for a channel that accepts them.") {
		t.Fatal("no line for 2 kept notices")
	}
	e.setDelivery(notify.Delivery{NoticesKept: 1})
	if _, page = e.get("/settings"); !strings.Contains(section(t, page, "notifications"), "1 settings notice waits for a channel that accepts it.") {
		t.Fatal("no line for 1 kept notice")
	}
	_ = e.st.LogNotification(time.Now(), "notice", "discord-notices", true, "", "probe notice")
	_ = e.st.LogNotification(time.Now(), "urgent", "discord", true, "", "probe urgent")
	_, page = e.get("/settings")
	if !strings.Contains(page, "<td>Discord (settings notices)</td>") || !strings.Contains(page, "<td>Discord</td>") {
		t.Fatal("the log does not name the channels")
	}
}

// C5: every settings notice of the Settings page has the Security flag of
// its change.
func TestSecurityFlagOfEachNotice(t *testing.T) {
	e := adminEnv(t, twoServers)
	csrf := e.csrf("/settings")
	n := 0
	expect := func(step string, security bool, f func()) {
		t.Helper()
		f()
		n++
		if nt := lastNotice(t, e, n); nt.Security != security {
			t.Errorf("%s: Security %v, want %v (%q)", step, nt.Security, security, nt.Title)
		}
	}
	post := func(path string, form url.Values) {
		t.Helper()
		form.Set("csrf", csrf)
		if msg, isErr := flash(e.post(path, form)); isErr {
			t.Fatalf("POST %s: %s", path, msg)
		}
	}
	server := func(extra url.Values) url.Values {
		f := url.Values{"orig_id": {"akka"}, "name": {"AKKA"}, "url": {"https://a:1"}}
		for k, v := range extra {
			f[k] = v
		}
		return f
	}
	expect("server added", true, func() {
		post("/settings/servers", url.Values{"name": {"SIRIUS"}, "url": {"https://c:1"}, "api_key": {"k-sirius"}})
	})
	expect("name", false, func() { post("/settings/servers", server(url.Values{"name": {"Akka"}})) })
	expect("GUI URL", false, func() {
		post("/settings/servers", server(url.Values{"name": {"Akka"}, "gui_url": {"https://a.invalid"}}))
	})
	expect("allow_http", false, func() {
		post("/settings/servers", server(url.Values{"name": {"Akka"}, "gui_url": {"https://a.invalid"}, "allow_http": {"on"}}))
	})
	expect("disabled and site", false, func() {
		post("/settings/servers", server(url.Values{"name": {"Akka"}, "gui_url": {"https://a.invalid"}, "allow_http": {"on"}, "disabled": {"on"}, "site": {"IVAR Office"}}))
	})
	base := url.Values{"name": {"Akka"}, "gui_url": {"https://a.invalid"}, "allow_http": {"on"}, "disabled": {"on"}, "site": {"IVAR Office"}}
	expect("API key", true, func() { post("/settings/servers", server(merge(base, url.Values{"api_key": {"k2"}}))) })
	expect("API URL with the API key", true, func() {
		post("/settings/servers", server(merge(base, url.Values{"url": {"https://a2:1"}, "api_key": {"k3"}})))
	})
	_ = e.st.SetPin("akka", "pin")
	expect("pinned certificate removed", true, func() {
		post("/settings/servers", server(merge(base, url.Values{"url": {"https://a2:1"}, "reset_pin": {"on"}, "confirm": {"correct horse battery"}})))
	})
	expect("certificate accepted", true, func() {
		post("/settings/servers/akka/accept-cert", url.Values{"fingerprint": {"ff"}, "confirm": {"correct horse battery"}})
	})
	expect("server removed", true, func() { post("/settings/servers/sirius/delete", url.Values{}) })
	f := notifyForm(e.cfg.Get(), csrf)
	f.Set("quiet_hours", "off")
	expect("notification settings", false, func() { post("/settings/notifications", f) })
	f.Set("discord_webhook", "https://discord.com/api/webhooks/2/new")
	expect("webhook", true, func() { post("/settings/notifications", f) })
	f.Del("discord_webhook")
	f.Set("discord_notices_webhook", "https://discord.com/api/webhooks/3/notices")
	expect("notices webhook", true, func() { post("/settings/notifications", f) })
	cf := checksForm(e.cfg.Get(), csrf)
	cf.Set("pc_offline", "2d")
	expect("checks", false, func() { post("/settings/checks", cf) })
	expect("structure", false, func() { post("/settings/structure", url.Values{"yaml": {"structure: {project_pattern: '^LIVE-'}"}}) })
	expect("collector", false, func() {
		post("/settings/collector", url.Values{"mode": {"polled"}, "event_timeout": {"50s"}, "full_check_interval": {"4h"}, "poll_interval": {"15m"},
			"structure_interval": {"1h"}, "baseline_interval": {"7d"}, "request_timeout": {"30s"}, "concurrency": {"3"}, "down_after_failures": {"2"}})
	})
	expect("devices", false, func() { post("/settings/devices", url.Values{"relay": {relayPC}}) })
	expect("access", true, func() {
		post("/settings/access", url.Values{"mode": {"builtin"}, "viewer": {"open"}, "confirm": {"correct horse battery"}})
	})
	expect("access tighter", true, func() { post("/settings/access", url.Values{"mode": {"builtin"}, "viewer": {"none"}}) })
	expect("administrator password", true, func() {
		post("/settings/password", url.Values{"current": {"correct horse battery"}, "password": {"another long password"}, "password2": {"another long password"}})
	})
	expect("import", true, func() {
		if msg, isErr := impFile(e, csrf, "servers: [{name: Akka, id: akka, url: 'https://a2:1'}, {name: SOL, url: 'https://b:1'}]\n", nil); isErr {
			t.Fatal(msg)
		}
	})
}

func merge(a, b url.Values) url.Values {
	out := url.Values{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// W-050: the select "Repeat urgent messages" (off, 1, 2 or 4 hours) keeps a
// value from a YAML file as an extra option.
func TestRepeatUrgentForm(t *testing.T) {
	e := adminEnv(t, twoServers)
	csrf := e.csrf("/settings")
	notifications := func() string {
		t.Helper()
		_, page := e.get("/settings")
		return section(t, page, "notifications")
	}
	if n := notifications(); !strings.Contains(n, `<select name="repeat_urgent"><option value="0" selected>off</option><option value="1h">every hour</option><option value="2h">every 2 hours</option><option value="4h">every 4 hours</option></select>`) ||
		!strings.Contains(n, "syncwatch sends the urgent message again while an ERROR finding of a server stays open.") {
		t.Fatalf("form: %s", n)
	}
	f := notifyForm(e.cfg.Get(), csrf)
	f.Set("repeat_urgent", "2h")
	if msg, isErr := flash(e.post("/settings/notifications", f)); isErr || e.cfg.Get().Notify.RepeatUrgent.D() != 2*time.Hour {
		t.Fatalf("2h: %q %v", msg, isErr)
	}
	if nt := lastNotice(t, e, 1); nt.Security || !strings.HasPrefix(nt.Summary, "Repeat urgent messages every: off → 2 hours\n") {
		t.Fatalf("notice %+v", nt)
	}
	if n := notifications(); !strings.Contains(n, `<option value="2h" selected>every 2 hours</option>`) {
		t.Fatal("2h is not selected")
	}
	// A refused save keeps the selected value.
	f.Set("repeat_urgent", "4h")
	f.Set("quiet_hours", "25:00-07:00")
	resp, body := e.postPage("/settings/notifications", f)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(section(t, body, "notifications"), `<option value="4h" selected>every 4 hours</option>`) ||
		e.cfg.Get().Notify.RepeatUrgent.D() != 2*time.Hour {
		t.Fatalf("refused save: %d", resp.StatusCode)
	}
	// A value out of range from a script: refused with the label and kept.
	f.Set("quiet_hours", "22:00-07:00")
	f.Set("repeat_urgent", "10m")
	resp, body = e.postPage("/settings/notifications", f)
	if n := section(t, body, "notifications"); resp.StatusCode != http.StatusUnprocessableEntity ||
		!strings.Contains(n, "Repeat urgent messages every (notifications.repeat_urgent): the value must be between 30 minutes and 1 day.") ||
		!strings.Contains(n, `<option value="10m" selected>every 10 minutes</option>`) || !strings.Contains(n, `<select name="repeat_urgent" aria-invalid="true"`) {
		t.Fatalf("10m: %d %s", resp.StatusCode, n)
	}
	f.Set("repeat_urgent", "0")
	if msg, isErr := flash(e.post("/settings/notifications", f)); isErr || e.cfg.Get().Notify.RepeatUrgent != 0 {
		t.Fatalf("off: %q %v", msg, isErr)
	}
	if nt := lastNotice(t, e, 2); !strings.HasPrefix(nt.Summary, "Repeat urgent messages every: 2 hours → off\n") {
		t.Fatalf("notice %+v", nt)
	}

	// A value from a YAML file is an extra option, and a save keeps it. A
	// form without the input (a script for an older version) keeps it too.
	e = adminEnv(t, strings.Replace(twoServers, "notifications: {", "notifications: {repeat_urgent: 3h, ", 1)+"\n")
	csrf = e.csrf("/settings")
	if n := notifications(); !strings.Contains(n, `<option value="4h">every 4 hours</option><option value="3h" selected>every 3 hours</option>`) {
		t.Fatalf("3h: %s", n)
	}
	f = notifyForm(e.cfg.Get(), csrf)
	f.Set("quiet_hours", "off")
	if msg, isErr := flash(e.post("/settings/notifications", f)); isErr || e.cfg.Get().Notify.RepeatUrgent.D() != 3*time.Hour {
		t.Fatalf("a form without repeat_urgent: %q %v %v", msg, isErr, e.cfg.Get().Notify.RepeatUrgent)
	}
	f.Set("repeat_urgent", "3h")
	f.Set("quiet_hours", "22:00-07:00")
	if msg, isErr := flash(e.post("/settings/notifications", f)); isErr || e.cfg.Get().Notify.RepeatUrgent.D() != 3*time.Hour {
		t.Fatalf("3h again: %q %v", msg, isErr)
	}
	if strings.Contains(e.noticesText(), "Repeat urgent") {
		t.Fatalf("notice for an unchanged value: %q", e.noticesText())
	}
}

// W-057: the site of each server (Servers form, with the names in use) and
// the site of syncwatch (Notifications form).
func TestSiteForms(t *testing.T) {
	e := adminEnv(t, twoServers)
	csrf := e.csrf("/settings")
	server := func(f url.Values) (string, bool) {
		t.Helper()
		f.Set("csrf", csrf)
		return flash(e.post("/settings/servers", f))
	}
	if msg, isErr := server(url.Values{"orig_id": {"sol"}, "name": {"SOL"}, "url": {"https://b:1"}, "site": {"  Branch office "}}); isErr || e.cfg.Get().SiteOf("sol") != "Branch office" {
		t.Fatalf("site of SOL: %q %v", msg, isErr)
	}
	if nt := lastNotice(t, e, 1); nt.Security || !strings.HasPrefix(nt.Summary, "The site of the server SOL changed from (none) to Branch office.\n") {
		t.Fatalf("notice %+v", nt)
	}
	if msg, isErr := server(url.Values{"name": {"SIRIUS"}, "url": {"https://c:1"}, "api_key": {"k"}, "site": {"IVAR Office"}}); isErr || e.cfg.Get().SiteOf("sirius") != "IVAR Office" {
		t.Fatalf("new server with a site: %q %v", msg, isErr)
	}
	if nt := lastNotice(t, e, 2); !nt.Security || nt.Summary != "The API URL is https://c:1.\nThe site of the server is IVAR Office.\nChanged from 127.0.0.1." {
		t.Fatalf("notice %+v", nt)
	}
	_, page := e.get("/settings")
	servers := section(t, page, "servers")
	if !strings.Contains(servers, `<datalist id="site-names"><option value="Branch office"><option value="IVAR Office"></datalist>`) ||
		!strings.Contains(servers, `<label>Site (optional) <input name="site" value="Branch office" list="site-names" placeholder="IVAR Office"`) ||
		!strings.Contains(servers, `<label>Site (optional) <input name="site" list="site-names" placeholder="IVAR Office"`) {
		t.Fatalf("Servers: %s", servers)
	}
	// 41 characters: refused with the label, nothing changes.
	if msg, isErr := server(url.Values{"orig_id": {"sol"}, "name": {"SOL"}, "url": {"https://b:1"}, "site": {strings.Repeat("x", config.MaxSiteLen+1)}}); !isErr ||
		msg != "Not saved. Server sol: the site (servers[].site) must have at most 40 characters and no control characters." || e.cfg.Get().SiteOf("sol") != "Branch office" {
		t.Fatalf("41 characters: %q %v", msg, isErr)
	}
	// An empty input removes the site.
	if msg, isErr := server(url.Values{"orig_id": {"sol"}, "name": {"SOL"}, "url": {"https://b:1"}, "site": {""}}); isErr || e.cfg.Get().SiteOf("sol") != "" {
		t.Fatalf("no site: %q %v", msg, isErr)
	}
	if nt := lastNotice(t, e, 3); !strings.HasPrefix(nt.Summary, "The site of the server SOL changed from Branch office to (none).\n") {
		t.Fatalf("notice %+v", nt)
	}

	// The site of syncwatch.
	n := section(t, page, "notifications")
	if !strings.Contains(n, `Site where syncwatch runs (optional) <input name="site" value="" list="site-names" placeholder="IVAR Office"`) ||
		!strings.Contains(n, "syncwatch names the sites in messages, for example: unreachable from IVAR Office.") {
		t.Fatalf("Notifications: %s", n)
	}
	f := notifyForm(e.cfg.Get(), csrf)
	f.Set("site", "IVAR Office")
	if msg, isErr := flash(e.post("/settings/notifications", f)); isErr || e.cfg.Get().Site != "IVAR Office" {
		t.Fatalf("site: %q %v", msg, isErr)
	}
	if nt := lastNotice(t, e, 4); nt.Security || !strings.HasPrefix(nt.Summary, "Site where syncwatch runs: (none) → IVAR Office\n") {
		t.Fatalf("notice %+v", nt)
	}
	long := strings.Repeat("ö", config.MaxSiteLen+1)
	f.Set("site", long)
	resp, body := e.postPage("/settings/notifications", f)
	if n := section(t, body, "notifications"); resp.StatusCode != http.StatusUnprocessableEntity ||
		!strings.Contains(n, "Site where syncwatch runs (site): the value must have at most 40 characters and no control characters.") ||
		!strings.Contains(n, `<input name="site" value="`+long+`" list="site-names" placeholder="IVAR Office" autocomplete="off" aria-invalid="true"`) || e.cfg.Get().Site != "IVAR Office" {
		t.Fatalf("41 characters: %d %s", resp.StatusCode, n)
	}
	// A form without the input (a script) keeps the site.
	f.Del("site")
	f.Set("quiet_hours", "off")
	if msg, isErr := flash(e.post("/settings/notifications", f)); isErr || e.cfg.Get().Site != "IVAR Office" {
		t.Fatalf("a form without the site: %q %v", msg, isErr)
	}
}

// Device IDs of the test PCs (complete Syncthing device IDs).
const (
	relayPC = "LAPTOPD-AAAAAAA-BBBBBBB-CCCCCCC-DDDDDDD-EEEEEEE-FFFFFFF-GGGGGGG"
	lanPC   = "ANNAPCX-AAAAAAA-BBBBBBB-CCCCCCC-DDDDDDD-EEEEEEE-FFFFFFF-GGGGGGG"
	gonePC  = "GONEPCX-AAAAAAA-BBBBBBB-CCCCCCC-DDDDDDD-EEEEEEE-FFFFFFF-GGGGGGG"
)

// devicesEnv is an administrator env with 2 PCs: LAPTOP-DANI connected
// through a relay and ANNA-PC offline.
func devicesEnv(t *testing.T, yaml string) *env {
	e := adminEnv(t, yaml)
	e.hub.Update("akka", func(s *model.Server) {
		s.Status, s.Loaded, s.MyID, s.LastContact = model.StatusUp, true, "AKKAAAA-1111111", time.Now()
		s.Devices = map[string]*model.Device{relayPC: {ID: relayPC, Name: "LAPTOP-DANI"}, lanPC: {ID: lanPC, Name: "ANNA-PC", LastSeen: time.Now().Add(-time.Hour)}}
		s.Connections = map[string]*model.Connection{relayPC: {Connected: true, Type: "relay-client", StartedAt: time.Now()}}
	})
	e.eng.Evaluate()
	return e
}

// W-047: the card Devices marks the PCs that always connect through a relay
// (no H11, H12 after relay_lost); the Checks form has the threshold of H12.
func TestDevicesForm(t *testing.T) {
	e := devicesEnv(t, twoServers+"thresholds: {relay_lost: 45m}\ndevices: [{id: "+gonePC+", relay: true}]\n")
	csrf := e.csrf("/settings")
	_, page := e.get("/settings")
	if !strings.Contains(page, `<a href="#servers">Servers</a><a href="#devices">Devices</a>`) {
		t.Error("no link to the card Devices")
	}
	card := section(t, page, "devices")
	for _, want := range []string{
		"For a device that connects only through a relay: syncwatch does not report H11 for it. H12 opens when the device has no connection for more than 45 minutes.",
		`<tr><td>ANNA-PC</td><td class="mono">ANNAPCX</td><td>offline</td>`,
		`<input type="checkbox" name="relay" value="` + lanPC + `" > Always connects through a relay</label>`,
		`<tr><td>LAPTOP-DANI</td><td class="mono">LAPTOPD</td><td>connected</td>`,
		`<input type="checkbox" name="relay" value="` + relayPC + `" > Always connects through a relay</label>`,
		`<tr><td>GONEPCX</td><td class="mono">GONEPCX</td><td>unknown</td>`,
		`<input type="checkbox" name="relay" value="` + gonePC + `" checked> Always connects through a relay</label>`,
	} {
		if !strings.Contains(card, want) {
			t.Errorf("card lacks %q", want)
		}
	}
	if strings.Contains(card, "AKKAAAA") {
		t.Error("the card lists a server")
	}
	save := func(ids ...string) (string, bool) {
		t.Helper()
		return flash(e.post("/settings/devices", url.Values{"csrf": {csrf}, "relay": ids}))
	}
	if msg, isErr := save(relayPC, strings.ToLower(relayPC)); isErr || msg != "The devices are saved." {
		t.Fatalf("save: %q %v", msg, isErr)
	}
	if d := e.cfg.Get().Devices; len(d) != 1 || d[0] != (config.Device{ID: relayPC, Relay: true}) {
		t.Fatalf("devices %+v", d)
	}
	nt := lastNotice(t, e, 1)
	if nt.Title != "Devices changed" || nt.Security || nt.Summary != "The device LAPTOP-DANI (LAPTOPD) always connects through a relay. H11 is off for it.\n"+
		"The device GONEPCX does not always connect through a relay now. H11 is on again for it.\nChanged from 127.0.0.1." {
		t.Fatalf("notice %+v", nt)
	}
	if _, page = e.get("/settings"); !strings.Contains(section(t, page, "devices"), `value="`+relayPC+`" checked>`) {
		t.Fatal("the saved device is not selected")
	}
	if msg, isErr := save(); isErr || len(e.cfg.Get().Devices) != 0 {
		t.Fatalf("clear: %q %v", msg, isErr)
	}
	if nt := lastNotice(t, e, 2); !strings.HasPrefix(nt.Summary, "The device LAPTOP-DANI (LAPTOPD) does not always connect through a relay now. H11 is on again for it.\n") {
		t.Fatalf("notice %+v", nt)
	}
	if msg, isErr := save("ABC"); !isErr || msg != `Not saved. Devices (devices): "ABC" is not a Syncthing device ID. Use the complete device ID, 8 groups of 7 characters.` {
		t.Fatalf("a device ID that is not valid: %q %v", msg, isErr)
	}
	if msg, isErr := flash(e.post("/settings/devices", url.Values{"csrf": {csrf}, "rev": {"stale"}, "relay": {relayPC}})); !isErr || !strings.Contains(msg, "Reload the page") || len(e.cfg.Get().Devices) != 0 {
		t.Fatalf("stale: %q %v", msg, isErr)
	}

	// The threshold of H12 in the Checks form.
	if !strings.Contains(section(t, page, "checks"), `<label>H12 no relay connection for more than <input name="relay_lost" value="45m"></label>`) {
		t.Fatal("no input relay_lost")
	}
	f := checksForm(e.cfg.Get(), csrf)
	if msg, isErr := flash(e.post("/settings/checks", f)); isErr || e.cfg.Get().Thresholds.RelayLost.String() != "45m" {
		t.Fatalf("a form without relay_lost keeps it: %q %v", msg, isErr)
	}
	f.Set("relay_lost", "1h")
	if msg, isErr := flash(e.post("/settings/checks", f)); isErr || e.cfg.Get().Thresholds.RelayLost.String() != "1h" {
		t.Fatalf("relay_lost 1h: %q %v", msg, isErr)
	}
	if nt := lastNotice(t, e, 3); nt.Security || !strings.HasPrefix(nt.Summary, "H12 no relay connection for more than: 45m → 1h\n") {
		t.Fatalf("notice %+v", nt)
	}
	for value, want := range map[string]string{
		"5s":     "H12 no relay connection for more than (thresholds.relay_lost): the value must be between 10 seconds and 3650 days.",
		"3 days": "H12 no relay connection for more than: the value &#34;3 days&#34; is not a duration.",
		"0":      "H12 no relay connection for more than: the value must be more than 0.",
	} {
		f.Set("relay_lost", value)
		resp, body := e.postPage("/settings/checks", f)
		if checks := section(t, body, "checks"); resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(checks, want) ||
			!strings.Contains(checks, `name="relay_lost" value="`+html.EscapeString(value)+`" aria-invalid="true"`) {
			t.Errorf("relay_lost %q: %d", value, resp.StatusCode)
		}
	}
	if e.cfg.Get().Thresholds.RelayLost.String() != "1h" {
		t.Fatal("a refused save changed relay_lost")
	}
}

// W-039: the settings notices name each setting by its label. The settings
// that only the YAML has are named by their part of the Settings page.
func TestNoticeLinesNameEverySetting(t *testing.T) {
	parse := func(y string) *config.Config {
		t.Helper()
		c, err := config.ParseYAML([]byte(y))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	old := parse(`servers: [{name: AKKA, url: 'https://a:1'}]
structure: {ignore_dirs: ['.*', '.streams'], nested_scan_depth: 3}
shares: {add: {AKKA: [/x/LIVE-1]}, exclude: {AKKA: [/x/OLD]}}
devices: [{id: ` + relayPC + `, relay: true}]
access: {session_ttl: 30d}
`)
	now := parse(`servers: [{name: AKKA, url: 'https://a:1', site: IVAR Office}]
site: IVAR Office
structure: {ignore_dirs: ['.*', '#snapshot'], nested_scan_depth: 5, case_insensitive: true}
shares: {add: {AKKA: [/x/LIVE-2]}, exclude: {SOL: [/y/OLD]}}
devices: [{id: ` + lanPC + `, relay: true}]
access: {session_ttl: 1d}
notifications: {repeat_urgent: 1h}
thresholds: {relay_lost: 2h}
`)
	got := configChanges(old, now)
	want := []string{
		"The site of the server AKKA changed from (none) to IVAR Office.",
		"The device ANNAPCX always connects through a relay. H11 is off for it.",
		"The device LAPTOPD does not always connect through a relay now. H11 is on again for it.",
		"Access (session_ttl): 30d → 1d",
		"Repeat urgent messages every: off → 1 hour",
		"Shares to add for AKKA (shares.add): added /x/LIVE-2 · removed /x/LIVE-1",
		"Shares to exclude for AKKA (shares.exclude): removed /x/OLD",
		"Shares to exclude for SOL (shares.exclude): added /y/OLD",
		"Site where syncwatch runs: (none) → IVAR Office",
		"Structure rules (case_insensitive): (none) → true",
		"Structure rules (ignore_dirs): added #snapshot · removed .streams",
		"Structure rules (nested_scan_depth): 3 → 5",
		"H12 no relay connection for more than: 30m → 2h",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, line := range got {
		for _, raw := range []string{"structure.", "shares.", "devices[", "access.", "notifications.", "thresholds.", "site"} {
			if strings.HasPrefix(line, raw) {
				t.Errorf("line %q starts with the key %q", line, raw)
			}
		}
	}
}

// RT-2: a value that is not a duration gets the message of the parser
// (with the format), and 0 gets its own message.
func TestDurationErrorsNameTheFormat(t *testing.T) {
	e := adminEnv(t, twoServers)
	csrf := e.csrf("/settings")
	const format = "Use a number and a unit, for example 30s, 15m, 24h, 3d or 2w."
	f := checksForm(e.cfg.Get(), csrf)
	f.Set("pc_offline", "3 days")
	f.Set("stuck_sync", "0")
	_, body := e.postPage("/settings/checks", f)
	msg := html.UnescapeString(section(t, body, "checks"))
	for _, want := range []string{`H7 PC offline for more than: the value "3 days" is not a duration. ` + format, "H5 stuck sync after: the value must be more than 0. " + format} {
		if !strings.Contains(msg, want) {
			t.Errorf("Checks: no %q", want)
		}
	}
	col := url.Values{"csrf": {csrf}, "mode": {"events"}, "event_timeout": {"3 days"}, "full_check_interval": {"4h"}, "poll_interval": {"0"},
		"structure_interval": {"1h"}, "baseline_interval": {"7d"}, "request_timeout": {"30s"}, "concurrency": {"3"}, "down_after_failures": {"2"}}
	_, body = e.postPage("/settings/collector", col)
	msg = html.UnescapeString(section(t, body, "collector"))
	for _, want := range []string{`Event long-poll timeout: the value "3 days" is not a duration. ` + format, "Poll interval (polled mode): the value must be more than 0. " + format} {
		if !strings.Contains(msg, want) {
			t.Errorf("Collector: no %q", want)
		}
	}
	if strings.Contains(msg, "duration of more than 0") {
		t.Error("the old message")
	}
}

// W-048: the Access card says what open access shows and that the JSON API
// needs an API token or a login.
func TestAccessCardSaysWhatOpenAccessShows(t *testing.T) {
	e := adminEnv(t, twoServers)
	_, page := e.get("/settings")
	if !strings.Contains(section(t, page, "access"), "With open access, each person who can reach syncwatch sees the dashboard: the names of servers, PCs and folders, the folder paths and the findings. The JSON API needs an API token or a login.") {
		t.Fatal("no sentence about open access")
	}
}
