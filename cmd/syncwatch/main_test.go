package main

import "testing"

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
