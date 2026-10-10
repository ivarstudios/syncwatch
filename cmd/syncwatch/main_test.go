package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
