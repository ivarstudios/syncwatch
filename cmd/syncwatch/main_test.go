package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/app"
)

// The gate has its own listen variable, so a gate container doesn't inherit
// the image's SYNCWATCH_LISTEN=:8080 meant for serve.
func TestGateListen(t *testing.T) {
	t.Setenv("SYNCWATCH_LISTEN", ":8080")
	t.Setenv("SYNCWATCH_GATE_LISTEN", "")
	t.Setenv("SYNCWATCH_GATE_BROWSE_ROOTS", "")
	if o, _ := gateOptions(nil); o.Listen != "127.0.0.1:8385" || o.BrowseRoots != nil {
		t.Fatalf("defaults: listen %q, browse roots %q", o.Listen, o.BrowseRoots)
	}
	t.Setenv("SYNCWATCH_GATE_LISTEN", ":8385")
	t.Setenv("SYNCWATCH_GATE_BROWSE_ROOTS", "/srv/extra, D:\\Extra")
	if o, _ := gateOptions(nil); o.Listen != ":8385" || !slices.Equal(o.BrowseRoots, []string{"/srv/extra", `D:\Extra`}) {
		t.Fatalf("from env: listen %q, browse roots %q", o.Listen, o.BrowseRoots)
	}
	if o, level := gateOptions([]string{"--listen", "192.0.2.1:9", "--log-level", "debug"}); o.Listen != "192.0.2.1:9" || level != "debug" {
		t.Fatalf("from flags: listen %q, level %q", o.Listen, level)
	}
}

func TestHealthURL(t *testing.T) {
	cases := []struct {
		listen, tls, want string
	}{
		{"", "", "http://127.0.0.1:8080/healthz"},
		{":8080", "", "http://127.0.0.1:8080/healthz"},
		{"0.0.0.0:9000", "", "http://127.0.0.1:9000/healthz"},
		{"[::]:9000", "", "http://127.0.0.1:9000/healthz"},
		{"192.168.1.10:8080", "", "http://192.168.1.10:8080/healthz"},
		{":8080", "true", "https://127.0.0.1:8080/healthz"},
		{":8080", "yes", "https://127.0.0.1:8080/healthz"},
	}
	for _, c := range cases {
		t.Setenv("SYNCWATCH_LISTEN", c.listen)
		t.Setenv("SYNCWATCH_TLS_SELF_SIGNED", c.tls)
		t.Setenv("SYNCWATCH_TLS_CERT", "")
		if got := healthURL(); got != c.want {
			t.Errorf("listen %q tls %q: got %s, want %s", c.listen, c.tls, got, c.want)
		}
	}
	if defaultListen != "127.0.0.1:8080" {
		t.Fatal("the binary must listen on loopback by default")
	}
}

func TestCommandRouting(t *testing.T) {
	cases := []struct {
		args []string
		cmd  string
		rest int
	}{
		{nil, "serve", 0},
		{[]string{"--listen", ":1"}, "serve", 2},
		{[]string{"-h"}, "help", 0},
		{[]string{"--help"}, "help", 0},
		{[]string{"-help"}, "help", 0},
		{[]string{"--version"}, "version", 0},
		{[]string{"-version"}, "version", 0},
		{[]string{"version"}, "version", 0},
		{[]string{"probe", "--url", "x"}, "probe", 2},
		{[]string{"serve", "-h"}, "serve", 1},
	}
	for _, c := range cases {
		cmd, rest := command(c.args)
		if cmd != c.cmd || len(rest) != c.rest {
			t.Errorf("%q: got %s %q", c.args, cmd, rest)
		}
	}
}

func TestEnvBool(t *testing.T) {
	cases := []struct {
		val  string
		want bool
		bad  bool
	}{
		{"", false, false}, {"true", true, false}, {"TRUE", true, false}, {"1", true, false},
		{"yes", true, false}, {"Yes", true, false}, {"on", true, false}, {"ON", true, false},
		{"false", false, false}, {"0", false, false}, {"no", false, false}, {"off", false, false},
		{" true ", true, false},
		{"maybe", false, true}, {"2", false, true}, {"ja", false, true},
	}
	for _, c := range cases {
		t.Setenv("SYNCWATCH_TEST_BOOL", c.val)
		got, err := envBool("SYNCWATCH_TEST_BOOL")
		if got != c.want || (err != nil) != c.bad {
			t.Errorf("%q: got %v %v", c.val, got, err)
		}
		if err != nil && !strings.Contains(err.Error(), "SYNCWATCH_TEST_BOOL") {
			t.Errorf("%q: the error must name the variable: %v", c.val, err)
		}
	}
}

// reset-admin-password records a settings notice for the server, which sends
// it (internal/app TestCLIChangesAnnounced).
func TestResetPasswordRecordsNotice(t *testing.T) {
	t.Setenv("SYNCWATCH_SECRET_KEY", "")
	dir := t.TempDir()
	st, err := app.OpenStore(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	in := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(in, []byte("a-new-password-123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(in)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	stdin := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = stdin }()
	if err := resetPassword([]string{"--data", dir}); err != nil {
		t.Fatal(err)
	}
	st, err = app.OpenExistingStore(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	notices, err := st.SettingsWithPrefix("cli.notice.")
	if err != nil || len(notices) != 1 {
		t.Fatalf("notices %v %v", notices, err)
	}
	for _, v := range notices {
		if !strings.Contains(v, "Administrator password changed with the command line") {
			t.Fatalf("notice %s", v)
		}
	}
}

// SYNCWATCH_HEALTH_NOTIFY_LIMIT: a duration from 1 minute to 1 day, 15
// minutes when not set. A value that is not valid stops the start with a
// message that names the variable.
func TestEnvNotifyLimit(t *testing.T) {
	for v, want := range map[string]time.Duration{"": 15 * time.Minute, "1m": time.Minute, " 90s ": 90 * time.Second, "2h": 2 * time.Hour, "1d": 24 * time.Hour} {
		t.Setenv("SYNCWATCH_HEALTH_NOTIFY_LIMIT", v)
		if got, err := envNotifyLimit(); err != nil || got != want {
			t.Errorf("%q: %v %v, want %v", v, got, err, want)
		}
	}
	for v, want := range map[string]string{
		"30s":    "SYNCWATCH_HEALTH_NOTIFY_LIMIT: the value must be between 1 minute and 1 day.",
		"25h":    "SYNCWATCH_HEALTH_NOTIFY_LIMIT: the value must be between 1 minute and 1 day.",
		"0":      "SYNCWATCH_HEALTH_NOTIFY_LIMIT: the value must be between 1 minute and 1 day.",
		"3 days": `SYNCWATCH_HEALTH_NOTIFY_LIMIT: the value "3 days" is not a duration. Use a number and a unit, for example 30s, 15m, 24h, 3d or 2w.`,
	} {
		t.Setenv("SYNCWATCH_HEALTH_NOTIFY_LIMIT", v)
		if _, err := envNotifyLimit(); err == nil || err.Error() != want {
			t.Errorf("%q: %v, want %q", v, err, want)
		}
	}
}

// SYNCWATCH_DEADMAN_URL: an http or https URL with a host, without a user
// name or password; not set = off. The error does not repeat the value.
func TestEnvDeadmanURL(t *testing.T) {
	t.Setenv("SYNCWATCH_DEADMAN_URL", "")
	if v, err := envDeadmanURL(); v != "" || err != nil {
		t.Fatalf("not set: %q %v", v, err)
	}
	t.Setenv("SYNCWATCH_DEADMAN_URL", " https://hc-ping.invalid/0b7c-uuid ")
	if v, err := envDeadmanURL(); v != "https://hc-ping.invalid/0b7c-uuid" || err != nil {
		t.Fatalf("valid: %q %v", v, err)
	}
	for _, bad := range []string{"hc-ping.invalid/secret-1", "ftp://hc-ping.invalid/secret-2", "https://user:secret-3@hc-ping.invalid/x"} {
		t.Setenv("SYNCWATCH_DEADMAN_URL", bad)
		_, err := envDeadmanURL()
		if err == nil || !strings.HasPrefix(err.Error(), "SYNCWATCH_DEADMAN_URL: ") || strings.Contains(err.Error(), "secret") {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

// SYNCWATCH_TRUSTED_PROXIES: IP addresses and CIDR prefixes, comma-separated.
func TestEnvTrustedProxies(t *testing.T) {
	t.Setenv("SYNCWATCH_TRUSTED_PROXIES", "")
	if p, err := envTrustedProxies(); p != nil || err != nil {
		t.Fatalf("not set: %v %v", p, err)
	}
	t.Setenv("SYNCWATCH_TRUSTED_PROXIES", "172.18.0.2, 10.1.2.3/8,::1,2001:db8::1/32 , ::ffff:192.0.2.7,::ffff:198.51.100.0/120")
	p, err := envTrustedProxies()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, x := range p {
		got = append(got, x.String())
	}
	if want := []string{"172.18.0.2/32", "10.0.0.0/8", "::1/128", "2001:db8::/32", "192.0.2.7/32", "198.51.100.0/24"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for _, bad := range []string{"proxy.lan.invalid", "10.0.0.0/33", "fe80::1%eth0", "10.0.0.1:8080", "*"} {
		t.Setenv("SYNCWATCH_TRUSTED_PROXIES", "10.0.0.1,"+bad)
		if _, err := envTrustedProxies(); err == nil || !strings.HasPrefix(err.Error(), "SYNCWATCH_TRUSTED_PROXIES: ") || !strings.Contains(err.Error(), bad) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

// A value that is not valid stops serve before it opens the data directory.
func TestServeRefusesBadEnvironment(t *testing.T) {
	for key, val := range map[string]string{"SYNCWATCH_HEALTH_NOTIFY_LIMIT": "soon", "SYNCWATCH_DEADMAN_URL": "ftp://x.invalid/", "SYNCWATCH_TRUSTED_PROXIES": "proxy.invalid"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv("SYNCWATCH_HEALTH_NOTIFY_LIMIT", "")
			t.Setenv("SYNCWATCH_DEADMAN_URL", "")
			t.Setenv("SYNCWATCH_TRUSTED_PROXIES", "")
			t.Setenv(key, val)
			data := filepath.Join(t.TempDir(), "data")
			if err := serve([]string{"--data", data}); err == nil || !strings.HasPrefix(err.Error(), key+": ") {
				t.Fatalf("%s=%q: %v", key, val, err)
			}
			if _, err := os.Stat(data); err == nil {
				t.Fatal("serve made the data directory")
			}
		})
	}
}

// syncwatch healthcheck gives the first line of an answer that is not 200:
// the image healthcheck and docker inspect show the reason.
func TestHealthcheckGivesTheReason(t *testing.T) {
	var status int
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	for _, c := range []struct {
		status int
		body   string
		want   string
	}{
		{200, "ok\n", ""},
		{503, "notifications fail from 2026-10-11 21:04\nno evaluation of the checks from 2026-10-11 21:10\n", "the health check returned HTTP 503: notifications fail from 2026-10-11 21:04"},
		{503, "", "the health check returned HTTP 503"},
		{503, "database\x07 not available\r\n", "the health check returned HTTP 503: database not available"},
		{421, strings.Repeat("x", 300), "the health check returned HTTP 421: " + strings.Repeat("x", 200) + "…"},
	} {
		status, body = c.status, c.body
		err := healthcheck([]string{"--url", srv.URL + "/healthz"})
		if (c.want == "" && err != nil) || (c.want != "" && (err == nil || err.Error() != c.want)) {
			t.Errorf("HTTP %d %q: %v, want %q", c.status, c.body, err, c.want)
		}
	}
}

// The maintenance commands work on an existing instance; a wrong --data must
// not become a fresh, empty one that reports success.
func TestMaintenanceCommandsNeedExistingData(t *testing.T) {
	t.Setenv("SYNCWATCH_SECRET_KEY", "")
	missing := filepath.Join(t.TempDir(), "nothing-here")
	yaml := filepath.Join(t.TempDir(), "new.yaml")
	if err := os.WriteFile(yaml, []byte("timezone: UTC\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, fn := range map[string]func() error{
		"reset-admin-password": func() error { return resetPassword([]string{"--data", missing}) },
		"import-config":        func() error { return importConfig([]string{"--data", missing, yaml}) },
		"export-config":        func() error { return exportConfig([]string{"--data", missing}) },
	} {
		err := fn()
		if err == nil || !strings.Contains(err.Error(), "no syncwatch data in "+missing) {
			t.Errorf("%s with a wrong --data: %v", name, err)
		}
		if _, err := os.Stat(missing); err == nil {
			t.Errorf("%s created %s", name, missing)
		}
	}

	// An existing instance is fine. A secret key must be given or found in
	// the data directory: a new one would encrypt imports with a key the
	// server doesn't have.
	dir := t.TempDir()
	st, err := app.OpenStore(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if err := exportConfig([]string{"--data", dir}); err == nil || !strings.Contains(err.Error(), "there is no stored configuration") {
		t.Fatalf("export-config on an empty instance: %v", err)
	}
	if err := importConfig([]string{"--data", dir, yaml}); err != nil {
		t.Fatalf("import-config: %v", err)
	}
	if err := exportConfig([]string{"--data", dir}); err != nil {
		t.Fatalf("export-config: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "secret.key")); err != nil {
		t.Fatal(err)
	}
	if err := importConfig([]string{"--data", dir, yaml}); err == nil || !strings.Contains(err.Error(), "secret.key") {
		t.Fatalf("import-config without a key: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "secret.key")); err == nil {
		t.Fatal("import-config created a secret key")
	}
}
