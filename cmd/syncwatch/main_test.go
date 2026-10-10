package main

import (
	"slices"
	"testing"
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
