package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/settings"
	"github.com/ivarstudios/syncwatch/internal/store"
)

func TestServerURLChangeNeedsKey(t *testing.T) {
	e := adminEnv(t, twoServers)
	csrf := e.csrf("/settings")
	_ = e.st.SetPin("akka", "pinned-fingerprint")
	save := func(f url.Values) (string, bool) {
		f.Set("csrf", csrf)
		f.Set("orig_id", "akka")
		f.Set("name", "AKKA")
		return flash(e.post("/settings/servers", f))
	}

	// Pointing the server elsewhere without entering the key is refused, so
	// the stored key can't be redirected to another host.
	if msg, isErr := save(url.Values{"url": {"https://evil.example:8384"}}); !isErr || !strings.Contains(msg, "API key") {
		t.Fatalf("URL change without a key: %q %v", msg, isErr)
	}
	if sv, _ := e.cfg.Get().ServerByKey("akka"); sv.URL != "https://a:1" {
		t.Fatalf("URL changed to %s", sv.URL)
	}
	// Plain http:// needs the explicit opt-in, even with a key.
	if _, isErr := save(url.Values{"url": {"http://a:1"}, "api_key": {"k"}}); !isErr {
		t.Fatal("http:// accepted without allow_http")
	}
	// Other edits without a key are fine.
	if msg, isErr := save(url.Values{"url": {"https://A:1/"}, "disabled": {"on"}}); isErr {
		t.Fatalf("edit without URL change refused: %s", msg)
	}
	if key, bound, _ := e.st.ServerKey("akka"); key != "secret-key-akka" || bound != "https://a:1" {
		t.Fatalf("key or binding changed: %q %q", key, bound)
	}
	if pin, _ := e.st.Pin("akka"); pin != "pinned-fingerprint" {
		t.Fatal("pin changed by an unrelated edit")
	}

	// With a new key the server moves; the key is bound to the new URL and
	// the old URL's certificate pin is forgotten.
	if msg, isErr := save(url.Values{"url": {"https://192.0.2.7:8384"}, "api_key": {"new-key"}}); isErr {
		t.Fatalf("URL change with key: %s", msg)
	}
	if key, bound, _ := e.st.ServerKey("akka"); key != "new-key" || bound != "https://192.0.2.7:8384" {
		t.Fatalf("new key: %q %q", key, bound)
	}
	if pin, _ := e.st.Pin("akka"); pin != "" {
		t.Fatal("old pin kept for the new URL")
	}
	if n := e.noticesText(); !strings.Contains(n, "API URL of the server AKKA changed") || !strings.Contains(n, "https://192.0.2.7:8384") {
		t.Fatalf("no notice for the URL change: %q", n)
	}
	if _, body := e.get("/settings"); strings.Contains(body, "new-key") {
		t.Fatal("settings page shows the key")
	}
}

func TestPinChangesNeedConfirmation(t *testing.T) {
	e := adminEnv(t, twoServers)
	csrf := e.csrf("/settings")
	accept := func(confirm string) (string, bool) {
		return flash(e.post("/settings/servers/akka/accept-cert", url.Values{"csrf": {csrf}, "fingerprint": {"ff"}, "confirm": {confirm}}))
	}
	reset := func(confirm string) (string, bool) {
		return flash(e.post("/settings/servers", url.Values{"csrf": {csrf}, "orig_id": {"akka"}, "name": {"AKKA"},
			"url": {"https://a:1"}, "reset_pin": {"on"}, "confirm": {confirm}}))
	}
	for _, bad := range []string{"", "wrong"} {
		if _, isErr := accept(bad); !isErr {
			t.Fatalf("certificate accepted with confirmation %q", bad)
		}
		if _, isErr := reset(bad); !isErr {
			t.Fatalf("pin reset with confirmation %q", bad)
		}
	}
	e.mu.Lock()
	n := len(e.accepted) + len(e.resets)
	e.mu.Unlock()
	if n != 0 {
		t.Fatal("hooks called without confirmation")
	}
	// The admin password or the server's own API key confirms.
	if msg, isErr := accept("correct horse battery"); isErr {
		t.Fatalf("accept with the admin password: %s", msg)
	}
	if msg, isErr := reset("secret-key-akka"); isErr {
		t.Fatalf("reset with the API key: %s", msg)
	}
	e.mu.Lock()
	accepted, resets := len(e.accepted), len(e.resets)
	e.mu.Unlock()
	if accepted != 1 || resets != 1 {
		t.Fatalf("accepted %d, reset %d", accepted, resets)
	}
	notices := e.noticesText()
	if !strings.Contains(notices, "new certificate accepted") || !strings.Contains(notices, "pinned certificate is removed") {
		t.Fatalf("notices: %q", notices)
	}
}

// A stolen admin session must not be able to guess the admin password
// through the password-change form.
func TestPasswordChangeIsRateLimited(t *testing.T) {
	e := adminEnv(t, twoServers)
	csrf := e.csrf("/settings")
	change := func(current string) (string, bool) {
		return flash(e.post("/settings/password", url.Values{"csrf": {csrf}, "current": {current},
			"password": {"a new long password"}, "password2": {"a new long password"}}))
	}
	for i := 0; i < 5; i++ {
		if msg, isErr := change(fmt.Sprintf("guess %d", i)); !isErr || !strings.Contains(msg, "not correct") {
			t.Fatalf("guess %d: %q %v", i, msg, isErr)
		}
	}
	if msg, isErr := change("correct horse battery"); !isErr || !strings.Contains(msg, "Too many") {
		t.Fatalf("right password after 5 guesses: %q %v", msg, isErr)
	}
	if h, _ := e.st.Setting(KeyAdminHash); !CheckPassword("correct horse battery", h) {
		t.Fatal("password changed while rate limited")
	}
}

// The webhook only goes to Discord, and changes that can silence alerting
// are announced.
func TestNotificationChangesAreAnnounced(t *testing.T) {
	e := adminEnv(t, `servers: [{name: AKKA, url: "https://a:1", api_key: secret-key-akka}, {name: SOL, url: "https://b:1"}]
notifications: {discord_webhook: "https://discord.com/api/webhooks/1/old", discord_role_id: "420000000000000000"}
`)
	csrf := e.csrf("/settings")
	notify := func(f url.Values) (int, string) {
		f.Set("csrf", csrf)
		f.Set("weekly_day", "monday")
		f.Set("weekly_time", "08:00")
		f.Set("timezone", "Local")
		resp, body := e.postPage("/settings/notifications", f)
		return resp.StatusCode, body
	}
	// A refused save shows the form again with the error (W-016).
	if code, body := notify(url.Values{"discord_webhook": {"http://10.0.0.5:8080/api/webhooks/1/x"}, "discord_role_id": {"420000000000000000"}}); code != http.StatusUnprocessableEntity || !strings.Contains(body, "discord.com/api/webhooks/…).") {
		t.Fatalf("internal webhook URL: %d", code)
	}
	if v, _ := e.st.Secret(store.SecretDiscordWebhook); v != "https://discord.com/api/webhooks/1/old" {
		t.Fatalf("webhook changed to %q", v)
	}
	if code, _ := notify(url.Values{"discord_webhook": {"https://discord.com/api/webhooks/2/new"}}); code != http.StatusSeeOther {
		t.Fatalf("Discord webhook refused: %d", code)
	}
	if n := e.noticesText(); !strings.Contains(n, "Discord webhook is replaced") || !strings.Contains(n, "do not mention a role now") {
		t.Fatalf("notices: %q", n)
	}
	notify(url.Values{"discord_webhook_clear": {"on"}})
	if n := e.noticesText(); !strings.Contains(n, "Discord webhook is removed") {
		t.Fatalf("notices: %q", n)
	}

	form := url.Values{"csrf": {csrf}}
	for _, c := range config.Catalog {
		if c.ID != "H1" {
			form.Set("enabled_"+c.ID, "on")
		}
		if c.Urgent && c.ID != "H4" {
			form.Set("urgent_"+c.ID, "on")
		}
		form.Set("debounce_"+c.ID, config.Duration(c.Debounce).String())
	}
	form.Set("debounce_S1", "30d")
	for _, k := range []string{"failing_files", "stuck_sync", "paused", "pc_offline", "conflict_window", "inactive_project", "old_device", "dead_share"} {
		form.Set(k, "2d")
	}
	e.post("/settings/checks", form)
	n := e.noticesText()
	for _, want := range []string{"The check H1 (Server unreachable) is disabled.", "The check H4 (Folder sync stopped) does not send urgent messages now.", "The check S1 (Syncthing folder inside another) waits 30 days instead of"} {
		if !strings.Contains(n, want) {
			t.Errorf("no notice %q in %q", want, n)
		}
	}

	if msg, _ := flash(e.post("/settings/import", url.Values{"csrf": {csrf}, "yaml": {"servers: [{name: AKKA, url: 'https://a:1'}, {name: SOL, url: 'https://b:1'}]\nchecks: {H3: {enabled: false}}\nnotifications: {discord_webhook: 'https://evil.example/hook'}\n"}})); !strings.Contains(msg, "discord_webhook") {
		t.Fatalf("import with an internal webhook: %q", msg)
	}
	if msg, isErr := impFile(e, csrf, "servers: [{name: AKKA, url: 'https://a:1'}, {name: SOL, url: 'https://b:1'}]\nchecks: {H3: {enabled: false}}\n", nil); isErr {
		t.Fatalf("import: %s", msg)
	}
	if n := e.noticesText(); !strings.Contains(n, "The check H3 (Server certificate changed) is disabled.") {
		t.Fatalf("import notice: %q", n)
	}
}

func TestImportCannotRedirectKeys(t *testing.T) {
	e := adminEnv(t, twoServers)
	csrf := e.csrf("/settings")
	// The imports below remove servers: the checkbox and the administrator
	// password confirm it in step 2 (W-055).
	imp := func(yaml string) string {
		msg, _ := impFile(e, csrf, yaml, url.Values{"delete_servers": {"on"}, "confirm": {"correct horse battery"}})
		return msg
	}
	// AKKA moves to another host without an api_key in the file; SOL is dropped.
	// The preview says that AKKA needs its API key again.
	if _, body, _ := impPreview(e, csrf, "servers: [{name: AKKA, url: 'https://evil.example:8384'}]\n"); !strings.Contains(body, "The API URL of the server AKKA changes. Type the API key again for AKKA.") {
		t.Fatalf("preview: %s", section(t, body, "import"))
	}
	if msg := imp("servers: [{name: AKKA, url: 'https://evil.example:8384'}]\n"); !strings.Contains(msg, "Type the API key again for AKKA") {
		t.Fatalf("import message: %q", msg)
	}
	sv, _ := e.cfg.Get().ServerByKey("akka")
	if settings.KeyUsable(e.st, sv) {
		t.Fatal("the stored key is usable for the imported URL")
	}
	if n := e.noticesText(); !strings.Contains(n, "API URL of the server AKKA changed") || !strings.Contains(n, "The server SOL is removed.") {
		t.Fatalf("notices: %q", n)
	}
	// A removed server's key doesn't survive for a later server with the same ID.
	imp("servers: [{name: AKKA, url: 'https://a:1'}, {name: SOL, url: 'https://b:1'}]\n")
	_ = e.st.SetServerKey("sol", "sol-key", "https://b:1")
	imp("servers: [{name: AKKA, url: 'https://a:1'}]\n")
	if e.st.HasSecret(store.ServerAPIKeySecret("sol")) {
		t.Fatal("a removed server's key was kept")
	}
	// Back at the URL it was entered for, AKKA's key works again.
	sv, _ = e.cfg.Get().ServerByKey("akka")
	if !settings.KeyUsable(e.st, sv) {
		t.Fatal("key not usable at its own URL")
	}
	// Switching login off by import is announced too.
	if msg, isErr := impFile(e, csrf, "servers: [{name: AKKA, url: 'https://a:1'}]\naccess: {mode: proxy}\n", url.Values{"confirm": {"correct horse battery"}}); isErr {
		t.Fatalf("import to proxy mode: %s", msg)
	}
	if !strings.Contains(e.noticesText(), "Built-in login is off") {
		t.Fatalf("notices: %q", e.noticesText())
	}
}

// Switching login off, opening up viewing or setting a viewer password needs
// the admin password, on the Access form and by import.
func TestWeakeningAccessNeedsAdminPassword(t *testing.T) {
	e := adminEnv(t, twoServers)
	csrf := e.csrf("/settings")
	access := func(f url.Values) (string, bool) {
		f.Set("csrf", csrf)
		return flash(e.post("/settings/access", f))
	}
	viewer := func() string { return e.cfg.Get().Access.Viewer }

	for _, confirm := range []string{"", "wrong password"} {
		if msg, isErr := access(url.Values{"mode": {"builtin"}, "viewer": {"open"}, "confirm": {confirm}}); !isErr || !strings.Contains(msg, "administrator password") {
			t.Fatalf("open viewing with confirm %q: %q %v", confirm, msg, isErr)
		}
	}
	if msg, isErr := access(url.Values{"mode": {"proxy"}, "confirm_proxy": {"on"}, "viewer": {"none"}}); !isErr || !strings.Contains(msg, "administrator password") {
		t.Fatalf("proxy mode without the password: %q %v", msg, isErr)
	}
	if msg, isErr := access(url.Values{"mode": {"builtin"}, "viewer": {"password"}, "viewer_password": {"viewer password 1"}}); !isErr {
		t.Fatalf("viewer password without the admin password: %q", msg)
	}
	if _, ok := e.web.setting(KeyViewerHash); ok {
		t.Fatal("viewer password stored although the change was refused")
	}
	if e.cfg.Get().Access.Mode != "builtin" || viewer() != "none" {
		t.Fatalf("access changed: %+v", e.cfg.Get().Access)
	}

	if msg, isErr := access(url.Values{"mode": {"builtin"}, "viewer": {"open"}, "confirm": {"correct horse battery"}}); isErr || viewer() != "open" {
		t.Fatalf("open viewing with the password: %q %v", msg, isErr)
	}
	// Tightening access needs no password.
	if msg, isErr := access(url.Values{"mode": {"builtin"}, "viewer": {"none"}}); isErr || viewer() != "none" {
		t.Fatalf("closing viewing: %q %v", msg, isErr)
	}

	// Import: refused before anything from the file is stored. The preview
	// asks for the password; step 2 checks it (W-055).
	imp := func(confirm, yaml string) (string, bool) {
		return impFile(e, csrf, yaml, url.Values{"confirm": {confirm}})
	}
	file := "servers: [{name: AKKA, url: 'https://a:1'}, {name: SOL, url: 'https://b:1'}]\naccess: {mode: proxy}\napi: {token: swt_from_the_file}\n"
	if _, body, _ := impPreview(e, csrf, file); !strings.Contains(section(t, body, "import"), `name="confirm"`) ||
		!strings.Contains(body, "Built-in login is off (proxy mode). Each person who can reach syncwatch is an administrator.") {
		t.Fatalf("the preview does not ask for the password: %s", section(t, body, "import"))
	}
	for _, confirm := range []string{"", "wrong password"} {
		if msg, isErr := imp(confirm, file); !isErr || !strings.Contains(msg, "Not imported") || !strings.Contains(msg, "administrator password") {
			t.Fatalf("import to proxy mode with the password %q: %q %v", confirm, msg, isErr)
		}
	}
	if e.hasAPIToken("swt_from_the_file") {
		t.Fatal("secret from a refused import was stored")
	}
	if e.cfg.Get().Access.Mode != "builtin" || len(e.cfg.Get().Servers) != 2 {
		t.Fatal("refused import changed the configuration")
	}
	if msg, isErr := imp("", "servers: [{name: AKKA, url: 'https://a:1'}, {name: SOL, url: 'https://b:1'}]\n"); isErr {
		t.Fatalf("import that keeps access as it is: %q", msg)
	}
	if msg, isErr := imp("correct horse battery", file); isErr || e.cfg.Get().Access.Mode != "proxy" {
		t.Fatalf("import to proxy mode with the password: %q %v", msg, isErr)
	}
}
