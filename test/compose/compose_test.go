//go:build compose

// Package compose runs the end-to-end scenarios against the Docker image:
// Syncthing containers (two servers, one PC), a fake Discord webhook and
// syncwatch running non-root with a read-only root filesystem.
//
//	go test -tags compose -v -timeout 30m ./test/compose
package compose

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/test/harness"
)

const (
	role     = "555"
	token    = "swt_compose_token_0123456789"
	password = "compose-admin-password"
	swURL    = "https://127.0.0.1:18780" // the image serves HTTPS by default
	fakeURL  = "http://127.0.0.1:18799/messages"
)

func dc(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("docker", append([]string{"compose", "-f", "docker-compose.yml"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("docker compose %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// sh runs a command inside a Syncthing container as the syncthing user.
func sh(t *testing.T, svc string, args ...string) {
	t.Helper()
	dc(t, append([]string{"exec", "-T", "-u", "1000:1000", svc}, args...)...)
}

type msgs []harness.DiscordMessage

func discord(t *testing.T) msgs {
	resp, err := http.Get(fakeURL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out msgs
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func waitDiscord(t *testing.T, since time.Time, timeout time.Duration, what string, pred func(harness.DiscordMessage) bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, m := range discord(t) {
			if !m.At.Before(since) && pred(m) {
				t.Logf("✔ Discord: %s", m.Content)
				return
			}
		}
		time.Sleep(time.Second)
	}
	for _, m := range discord(t) {
		t.Logf("discord: %s", m.Text())
	}
	t.Fatalf("no Discord message: %s", what)
}

func TestCompose(t *testing.T) {
	if os.Getenv("SKIP_COMPOSE") != "" {
		t.Skip("SKIP_COMPOSE set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	dc(t, "down", "-v", "--remove-orphans")
	dc(t, "up", "-d", "--build")
	defer func() {
		if t.Failed() {
			t.Log(dc(t, "logs", "--no-color", "--tail", "120", "syncwatch"))
		}
		if os.Getenv("KEEP_COMPOSE") == "" {
			dc(t, "down", "-v", "--remove-orphans")
		}
	}()

	insts := map[string]*harness.Instance{
		"e2e-akka":   harness.Remote("AKKA", true, "https://127.0.0.1:18701", "key-akka-compose-0123456789", "tcp://akka.e2e.invalid:22000"),
		"e2e-sol":    harness.Remote("SOL", true, "https://127.0.0.1:18702", "key-sol-compose-0123456789", "tcp://sol.e2e.invalid:22000"),
		"e2e-mortis": harness.Remote("MORTIS", false, "https://127.0.0.1:18703", "key-mortis-compose-0123456789", "tcp://mortis.e2e.invalid:22000"),
	}
	for _, i := range insts {
		if err := i.WaitReady(ctx, 120*time.Second); err != nil {
			t.Fatal(err)
		}
		if err := i.Call(ctx, http.MethodPatch, "/rest/config/options", map[string]any{
			"globalAnnounceEnabled": false, "localAnnounceEnabled": false, "relaysEnabled": false,
			"natEnabled": false, "urAccepted": -1, "crashReportingEnabled": false,
		}, nil); err != nil {
			t.Fatal(err)
		}
		if err := i.SetName(ctx); err != nil {
			t.Fatal(err)
		}
	}
	for an, a := range insts {
		for bn, b := range insts {
			if an == bn || (!a.Server && !b.Server) {
				continue
			}
			if err := a.AddDevice(ctx, b); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk := func(svc string, path string) string {
		sh(t, svc, "mkdir", "-p", path)
		return path
	}
	share := func(name string) string { return "/var/syncthing/share/" + name }
	mk("e2e-akka", share("AKKA-LIVE-1SOURCE"))
	mk("e2e-sol", share("SOL-LIVE-1SOURCE"))
	folder := func(id, label string, places map[string]string) {
		var with []*harness.Instance
		for svc := range places {
			with = append(with, insts[svc])
		}
		for svc, p := range places {
			mk(svc, p)
			if err := insts[svc].AddFolder(ctx, id, label, p, with); err != nil {
				t.Fatal(err)
			}
		}
	}
	folder("p-alpha", "LIVE-ALPHA-1SOURCE", map[string]string{
		"e2e-akka": share("AKKA-LIVE-1SOURCE/LIVE-ALPHA-1SOURCE"), "e2e-sol": share("SOL-LIVE-1SOURCE/LIVE-ALPHA-1SOURCE"), "e2e-mortis": "/var/syncthing/LIVE-ALPHA-1SOURCE",
	})
	folder("p-beta", "LIVE-BETA-1SOURCE", map[string]string{
		"e2e-akka": share("AKKA-LIVE-1SOURCE/LIVE-BETA-1SOURCE"), "e2e-sol": share("SOL-LIVE-1SOURCE/LIVE-BETA-1SOURCE"),
	})
	mk("e2e-akka", share("AKKA-LIVE-1SOURCE/random stuff"))

	sw, _ := harness.StartSyncwatchRemote(swURL, password, token)
	st, err := sw.WaitStatus(ctx, 3*time.Minute, func(st *harness.Status) bool {
		up := 0
		for _, s := range st.Servers {
			if s.Status == "up" && s.Folders.Total == 2 {
				up++
			}
		}
		return up == 2 && st.Find("S4", "random stuff") != nil
	})
	if err != nil {
		t.Fatalf("syncwatch never saw both servers: %v %+v", err, st)
	}
	t.Logf("health: %s", st.Health)

	t.Run("container-hardening", func(t *testing.T) {
		out := strings.TrimSpace(dc(t, "exec", "-T", "syncwatch", "/usr/local/bin/syncwatch", "version"))
		if !strings.Contains(out, "e2e") {
			t.Fatalf("version: %q", out)
		}
		id := strings.TrimSpace(dc(t, "ps", "-q", "syncwatch"))
		insp, err := exec.Command("docker", "inspect", "--format", "{{.Config.User}} {{.HostConfig.ReadonlyRootfs}}", id).CombinedOutput()
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(insp)); got != "65532:65532 true" {
			t.Fatalf("expected non-root, read-only: %q", got)
		}
	})

	t.Run("nested", func(t *testing.T) {
		since := time.Now()
		mk("e2e-akka", share("AKKA-LIVE-1SOURCE/LIVE-ALPHA-1SOURCE/work/LIVE-INNER-1SOURCE"))
		_ = insts["e2e-akka"].Rescan(ctx, "p-alpha")
		if _, err := sw.WaitStatus(ctx, 2*time.Minute, func(st *harness.Status) bool { return st.Find("S3", "LIVE-INNER-1SOURCE") != nil }); err != nil {
			t.Fatal(err)
		}
		waitDiscord(t, since, 90*time.Second, "S3 ping", func(m harness.DiscordMessage) bool {
			return m.Mentions(role) && strings.Contains(m.Text(), "LIVE-INNER")
		})
	})

	t.Run("overlap-and-share-root", func(t *testing.T) {
		since := time.Now()
		if err := insts["e2e-sol"].AddFolder(ctx, "root", "SOL source share", share("SOL-LIVE-1SOURCE"), nil); err != nil {
			t.Fatal(err)
		}
		if _, err := sw.WaitStatus(ctx, 2*time.Minute, func(st *harness.Status) bool {
			return st.Find("S2", "SOL-LIVE-1SOURCE") != nil && st.Find("S1", "inside SOL source share") != nil
		}); err != nil {
			t.Fatal(err)
		}
		waitDiscord(t, since, 90*time.Second, "S1/S2 ping", func(m harness.DiscordMessage) bool { return m.Mentions(role) && strings.Contains(m.Text(), "SOL") })
	})

	// SOL is watched through the read-only gate. From outside, the gate
	// refuses everything but the allowlisted GETs with its token, and the
	// configuration changes made above come out of its event stream without
	// SOL's API key.
	t.Run("read-only-gate", func(t *testing.T) {
		const gateURL, gateToken, solKey = "https://127.0.0.1:18704", "gate-token-sol-compose-0123456789", "key-sol-compose-0123456789"
		hc := &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // test stack
		call := func(method, path, tok string) (int, string) {
			req, _ := http.NewRequest(method, gateURL+path, nil)
			if tok != "" {
				req.Header.Set("X-API-Key", tok)
			}
			resp, err := hc.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			return resp.StatusCode, string(b)
		}
		for _, c := range []struct {
			method, path, tok string
			want              int
		}{
			{http.MethodGet, "/rest/system/status", "", http.StatusForbidden},
			{http.MethodGet, "/rest/system/status", solKey, http.StatusForbidden}, // SOL's own key is no use at the gate
			{http.MethodPost, "/rest/system/restart", gateToken, http.StatusMethodNotAllowed},
			{http.MethodPost, "/rest/config/folders", gateToken, http.StatusMethodNotAllowed},
			{http.MethodGet, "/rest/config", gateToken, http.StatusNotFound},
			{http.MethodGet, "/rest/system/status", gateToken, http.StatusOK},
		} {
			if code, body := call(c.method, c.path, c.tok); code != c.want {
				t.Errorf("%s %s: %d, want %d (%s)", c.method, c.path, code, c.want, body)
			}
		}
		code, body := call(http.MethodGet, "/rest/events?since=0&timeout=1", gateToken)
		if code != http.StatusOK || strings.Contains(body, solKey) {
			t.Fatalf("events through the gate: %d, key leaked: %v", code, strings.Contains(body, solKey))
		}
		var evs []struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		_ = json.Unmarshal([]byte(body), &evs)
		saved := 0
		for _, e := range evs {
			if e.Type == "ConfigSaved" {
				saved++
				if len(e.Data) > 0 && string(e.Data) != "null" {
					t.Fatalf("ConfigSaved passed with its payload: %s", e.Data)
				}
			}
		}
		t.Logf("✔ gate: %d events, %d ConfigSaved without payload", len(evs), saved)
		if saved == 0 {
			t.Error("no ConfigSaved event to check; SOL's configuration was changed above")
		}
	})

	t.Run("paused-folder", func(t *testing.T) {
		if err := insts["e2e-akka"].PauseFolder(ctx, "p-beta", true); err != nil {
			t.Fatal(err)
		}
		if _, err := sw.WaitStatus(ctx, 2*time.Minute, func(st *harness.Status) bool { return st.Find("H6", "LIVE-BETA-1SOURCE on AKKA") != nil }); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("pc-offline", func(t *testing.T) {
		dc(t, "stop", "e2e-mortis")
		if _, err := sw.WaitStatus(ctx, 3*time.Minute, func(st *harness.Status) bool { return st.Find("H7", "MORTIS") != nil }); err != nil {
			t.Fatal(err)
		}
		dc(t, "start", "e2e-mortis")
	})

	t.Run("server-stopped", func(t *testing.T) {
		since := time.Now()
		dc(t, "stop", "e2e-sol")
		if _, err := sw.WaitStatus(ctx, 2*time.Minute, func(st *harness.Status) bool { return st.Find("H1", "SOL") != nil }); err != nil {
			t.Fatal(err)
		}
		waitDiscord(t, since, 90*time.Second, "H1 ping", func(m harness.DiscordMessage) bool {
			return m.Mentions(role) && strings.Contains(m.Text(), "unreachable") && strings.Contains(m.Text(), "SOL")
		})
		since = time.Now()
		dc(t, "start", "e2e-sol")
		if _, err := sw.WaitStatus(ctx, 2*time.Minute, func(st *harness.Status) bool { return st.Find("H1", "SOL") == nil }); err != nil {
			t.Fatal(err)
		}
		waitDiscord(t, since, 90*time.Second, "resolved", func(m harness.DiscordMessage) bool {
			return !m.Mentions(role) && strings.Contains(m.Text(), "Resolved") && strings.Contains(m.Text(), "SOL")
		})
	})

	t.Run("certificate-replaced", func(t *testing.T) {
		since := time.Now()
		dc(t, "exec", "-T", "e2e-akka", "rm", "-f", "/var/syncthing/config/https-cert.pem", "/var/syncthing/config/https-key.pem")
		dc(t, "restart", "e2e-akka")
		st, err := sw.WaitStatus(ctx, 2*time.Minute, func(st *harness.Status) bool { return st.Find("H3", "AKKA") != nil })
		if err != nil {
			t.Fatal(err)
		}
		waitDiscord(t, since, 90*time.Second, "H3 ping", func(m harness.DiscordMessage) bool {
			return m.Mentions(role) && strings.Contains(m.Text(), "certificate")
		})
		fp := st.Find("H3", "AKKA").Detail("New fingerprint (full)")
		if err := sw.Login(); err != nil {
			t.Fatal(err)
		}
		if _, err := sw.PostForm("/settings", "/settings/servers/akka/accept-cert", url.Values{"fingerprint": {fp}, "confirm": {password}}); err != nil {
			t.Fatal(err)
		}
		if _, err := sw.WaitStatus(ctx, 2*time.Minute, func(st *harness.Status) bool { return st.Find("H3", "AKKA") == nil }); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("payload-limits", func(t *testing.T) {
		for _, m := range discord(t) {
			if len(m.Problems) > 0 {
				t.Fatalf("Discord limits violated: %v", m.Problems)
			}
		}
	})
}
