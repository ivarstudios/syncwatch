//go:build integration

// Package integration runs the acceptance scenarios against real Syncthing
// processes: three servers and two PCs, a fake Discord webhook and syncwatch.
//
//	SYNCTHING_BIN=/path/to/syncthing go test -tags integration -v -timeout 30m ./test/integration
package integration

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/test/harness"
)

const (
	role     = "777000111"
	token    = "swt_integration_token_abcdef0123"
	password = "integration-admin-pw"
)

type suite struct {
	t   *testing.T
	ctx context.Context
	env *harness.Env
	fd  *harness.FakeDiscord
	sw  *harness.Syncwatch
}

func (s *suite) logf(f string, a ...any) { s.t.Logf(f, a...) }

func (s *suite) waitFinding(timeout time.Duration, check, part string) harness.Finding {
	s.t.Helper()
	st, err := s.sw.WaitStatus(s.ctx, timeout, func(st *harness.Status) bool { return st.Find(check, part) != nil })
	if err != nil {
		s.dump(st)
		s.t.Fatalf("waiting for %s %q: %v", check, part, err)
	}
	f := *st.Find(check, part)
	s.logf("✔ %s open: %s", strings.Join(f.Checks, "+"), f.Message)
	return f
}

func (s *suite) waitGone(timeout time.Duration, check, part string) {
	s.t.Helper()
	st, err := s.sw.WaitStatus(s.ctx, timeout, func(st *harness.Status) bool { return st.Find(check, part) == nil })
	if err != nil {
		s.dump(st)
		s.t.Fatalf("waiting for %s %q to resolve: %v", check, part, err)
	}
	s.logf("✔ %s %q resolved", check, part)
}

func (s *suite) waitDiscord(since time.Time, timeout time.Duration, what string, pred func(harness.DiscordMessage) bool) harness.DiscordMessage {
	s.t.Helper()
	m, ok := s.fd.WaitFor(since, timeout, pred)
	if !ok {
		for _, m := range s.fd.Messages() {
			s.logf("discord @%s: %s", m.At.Format("15:04:05"), m.Text())
		}
		s.t.Fatalf("no Discord message: %s", what)
	}
	s.logf("✔ Discord: %s", strings.ReplaceAll(m.Content, "\n", " "))
	return m
}

func (s *suite) dump(st *harness.Status) {
	if st == nil {
		return
	}
	s.logf("health: %s", st.Health)
	for _, f := range st.Findings {
		s.logf("  %s %s stale=%v urgent=%v: %s", strings.Join(f.Checks, "+"), f.Severity, f.Stale, f.Urgent, f.Message)
	}
	if b, err := os.ReadFile(s.sw.LogPath()); err == nil {
		lines := strings.Split(string(b), "\n")
		if len(lines) > 40 {
			lines = lines[len(lines)-40:]
		}
		s.logf("syncwatch log tail:\n%s", strings.Join(lines, "\n"))
	}
}

func mentionsCheck(title string) func(harness.DiscordMessage) bool {
	return func(m harness.DiscordMessage) bool { return m.Mentions(role) && strings.Contains(m.Text(), title) }
}

func TestScenarios(t *testing.T) {
	bin := os.Getenv("SYNCTHING_BIN")
	if bin == "" {
		t.Skip("SYNCTHING_BIN not set")
	}
	base := 18600
	if p, err := strconv.Atoi(os.Getenv("SW_IT_PORT")); err == nil {
		base = p
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	dir := t.TempDir()
	if d := os.Getenv("SW_IT_DIR"); d != "" {
		dir = d
		_ = os.RemoveAll(dir)
	}
	s := &suite{t: t, ctx: ctx}

	env, err := harness.Start(ctx, harness.DefaultSpec(filepath.Join(dir, "st"), bin, base), s.logf)
	if err != nil {
		t.Fatal(err)
	}
	s.env = env
	defer env.Close()
	if err := env.BuildIVAR(ctx); err != nil {
		t.Fatal(err)
	}
	if err := env.WaitConnected(ctx, 90*time.Second); err != nil {
		t.Logf("warning: %v", err)
	}
	s.fd, err = harness.StartFakeDiscord("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer s.fd.Close()

	swBin := os.Getenv("SYNCWATCH_BIN")
	if swBin == "" {
		swBin = filepath.Join(dir, "syncwatch")
		if runtime.GOOS == "windows" {
			swBin += ".exe"
		}
		out, err := exec.Command("go", "build", "-o", swBin, "../../cmd/syncwatch").CombinedOutput()
		if err != nil {
			t.Fatalf("build: %v\n%s", err, out)
		}
	}
	data := filepath.Join(dir, "data")
	_ = os.MkdirAll(data, 0o755)
	if err := harness.WriteConfig(filepath.Join(data, "syncwatch.yaml"), env, s.fd.URL(), role, token, ""); err != nil {
		t.Fatal(err)
	}
	s.sw, err = harness.StartSyncwatch(swBin, data, "127.0.0.1:"+strconv.Itoa(base+90), password, token)
	if err != nil {
		t.Fatal(err)
	}
	defer s.sw.Stop()

	steps := []struct {
		name string
		fn   func(*testing.T)
	}{
		{"baseline", s.baseline}, {"dashboard", s.dashboard}, {"nested-synced", s.nestedSynced},
		{"nested-unsynced", s.nestedUnsynced}, {"overlapping-folders", s.overlap}, {"share-root-folder", s.shareRoot},
		{"paused-folder", s.paused}, {"folder-stopped", s.folderStopped}, {"pc-offline", s.pcOffline}, {"server-stopped", s.serverStopped},
		{"certificate-replaced", s.certReplaced}, {"snooze", s.snooze}, {"discord-payloads", s.payloads},
		{"no-secrets", s.noSecrets},
	}
	parent := s.t
	for _, st := range steps {
		ok := t.Run(st.name, func(t *testing.T) {
			s.t = t
			st.fn(t)
		})
		s.t = parent
		if !ok {
			break // later scenarios build on earlier ones
		}
	}
}

func (s *suite) baseline(t *testing.T) {
	st, err := s.sw.WaitStatus(s.ctx, 90*time.Second, func(st *harness.Status) bool {
		up := 0
		for _, sv := range st.Servers {
			if sv.Status == "up" && sv.Folders.Total > 0 {
				up++
			}
		}
		return up == 3 && st.Find("S7", "LIVE-X[1]") != nil
	})
	if err != nil {
		s.dump(st)
		t.Fatalf("servers not loaded: %v", err)
	}
	// IVAR acceptance cases.
	checks := []struct{ check, part string }{
		{"S7", "LIVE-NOTSYNCED-1SOURCE in AKKA-LIVE-1SOURCE"},
		{"S4", "random stuff"},
		{"S5", "ME-LIVE-WORKSHOP"},
		{"S5", "LIVE-KYRGYZ-2PROJECTFILES-BLENDER in AKKA-LIVE-1SOURCE"},
		{"S6", "LIVE-KYRGYZ-2PROJECTFILES-BLENDER in AKKA-LIVE-1SOURCE"},
		{"S6", "LIVE-KYRGYZ-2PROJECTFILES-BLENDER in AKKA-LIVE-2PROJECTFILES"},
		{"S7", "LIVE-X[1]-1SOURCE"},
	}
	for _, c := range checks {
		if st.Find(c.check, c.part) == nil {
			s.dump(st)
			t.Fatalf("missing %s %q", c.check, c.part)
		}
	}
	for _, f := range st.Findings {
		for _, bad := range []string{"SKÅNE", "@Recycle", ".streams", "LIVE-ALPHA", "LIVE-BETA", "ME-LIVE-DELTA"} {
			if strings.Contains(f.Subject, bad) {
				t.Fatalf("false positive: %s %s", f.Check, f.Message)
			}
		}
	}
	// Transient link drops while the test instances connect must settle without an H1.
	if st.Find("H1", "") != nil {
		t.Fatalf("unexpected H1: %+v", st.Find("H1", ""))
	}
	s.waitGone(2*time.Minute, "H2", "")
	if len(st.Devices) != 5 {
		t.Fatalf("expected 5 unified devices, got %d", len(st.Devices))
	}
}

func (s *suite) dashboard(t *testing.T) {
	if err := s.sw.Login(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/", "/grid", "/problems", "/settings", "/frag/overview", "/frag/grid", "/frag/problems?severity=error"} {
		body, err := s.sw.Get(p)
		if err != nil {
			t.Fatal(err)
		}
		if p == "/grid" {
			for _, want := range []string{"LIVE-ALPHA-2PROJECTFILES", "LIVE-SKÅNE-ÄÖ-1SOURCE", "MORTIS", "VENUS", "LIVE-1SOURCE"} {
				if !strings.Contains(body, want) {
					t.Fatalf("grid lacks %q", want)
				}
			}
		}
	}
}

func (s *suite) nestedSynced(t *testing.T) {
	since := time.Now()
	if _, err := s.env.Mkdir("AKKA", "AKKA-LIVE-2PROJECTFILES", "LIVE-ALPHA-2PROJECTFILES", "renders", "LIVE-NESTED-2PROJECTFILES"); err != nil {
		t.Fatal(err)
	}
	_ = s.env.Get("AKKA").Rescan(s.ctx, "p-alpha")
	f := s.waitFinding(90*time.Second, "S3", "LIVE-NESTED-2PROJECTFILES")
	if !f.Urgent || f.Severity != "ERROR" {
		t.Fatalf("S3 should be an urgent error: %+v", f)
	}
	s.waitDiscord(since, 90*time.Second, "urgent S3 ping", mentionsCheck("LIVE-NESTED-2PROJECTFILES"))
}

func (s *suite) nestedUnsynced(t *testing.T) {
	since := time.Now()
	if _, err := s.env.Mkdir("AKKA", "AKKA-LIVE-1SOURCE", "LIVE-NOTSYNCED-1SOURCE", "old", "LIVE-DEEP-1SOURCE"); err != nil {
		t.Fatal(err)
	}
	// Found by the hourly structure scan (1 minute in this test).
	s.waitFinding(150*time.Second, "S3", "LIVE-DEEP-1SOURCE")
	s.waitDiscord(since, 60*time.Second, "urgent S3 ping (scan)", mentionsCheck("LIVE-DEEP-1SOURCE"))
}

func (s *suite) overlap(t *testing.T) {
	since := time.Now()
	akka := s.env.Get("AKKA")
	sub, err := s.env.Mkdir("AKKA", "AKKA-LIVE-2PROJECTFILES", "LIVE-ALPHA-2PROJECTFILES", "sub")
	if err != nil {
		t.Fatal(err)
	}
	if err := akka.AddFolder(s.ctx, "overlap-sub", "ALPHA sub", sub, nil); err != nil {
		t.Fatal(err)
	}
	s.waitFinding(90*time.Second, "S1", "ALPHA sub inside LIVE-ALPHA-2PROJECTFILES")
	s.waitDiscord(since, 60*time.Second, "urgent S1 ping", mentionsCheck("ALPHA sub"))
	// Clean up: removing the folder resolves S1.
	if err := akka.RemoveFolder(s.ctx, "overlap-sub"); err != nil {
		t.Fatal(err)
	}
	s.waitGone(90*time.Second, "S1", "ALPHA sub")
	s.waitDiscord(since, 60*time.Second, "resolved S1", func(m harness.DiscordMessage) bool {
		return !m.Mentions(role) && strings.Contains(m.Text(), "Resolved") && strings.Contains(m.Text(), "ALPHA sub")
	})
}

func (s *suite) shareRoot(t *testing.T) {
	since := time.Now()
	sirius := s.env.Get("SIRIUS")
	root := sirius.ShareDir("SIRIUS-LIVE-2PROJECTFILES")
	if err := sirius.AddFolder(s.ctx, "share-root", "whole PROJECTFILES share", root, nil); err != nil {
		t.Fatal(err)
	}
	s.waitFinding(90*time.Second, "S2", "SIRIUS-LIVE-2PROJECTFILES on SIRIUS")
	s.waitFinding(30*time.Second, "S1", "LIVE-EPSILON-2PROJECTFILES inside whole PROJECTFILES share")
	s.waitDiscord(since, 60*time.Second, "urgent S2 ping", mentionsCheck("Share root"))
}

func (s *suite) paused(t *testing.T) {
	if err := s.env.Get("SIRIUS").PauseFolder(s.ctx, "p-gamma", true); err != nil {
		t.Fatal(err)
	}
	f := s.waitFinding(90*time.Second, "H6", "LIVE-GAMMA-1SOURCE on SIRIUS")
	if f.Urgent {
		t.Fatal("H6 is not urgent by default")
	}
}

// folderStopped removes a folder's .stfolder marker on a server, which makes
// Syncthing stop the folder with an error (as when a share isn't mounted).
func (s *suite) folderStopped(t *testing.T) {
	since := time.Now()
	akka := s.env.Get("AKKA")
	marker := filepath.Join(akka.ShareDir("AKKA-LIVE-1SOURCE"), "LIVE-BETA-1SOURCE", ".stfolder")
	if err := os.RemoveAll(marker); err != nil {
		t.Fatal(err)
	}
	_ = akka.Rescan(s.ctx, "p-beta")
	f := s.waitFinding(120*time.Second, "H4", "LIVE-BETA-1SOURCE on AKKA")
	if !f.Urgent || !strings.Contains(strings.ToLower(f.Message), "marker") {
		t.Fatalf("H4: %+v", f)
	}
	s.waitDiscord(since, 90*time.Second, "urgent H4 ping", mentionsCheck("Folder sync stopped"))
	since = time.Now()
	if err := os.MkdirAll(marker, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = akka.Rescan(s.ctx, "p-beta")
	s.waitGone(180*time.Second, "H4", "LIVE-BETA-1SOURCE on AKKA")
	s.waitDiscord(since, 60*time.Second, "resolved H4", func(m harness.DiscordMessage) bool {
		return !m.Mentions(role) && strings.Contains(m.Text(), "Resolved") && strings.Contains(m.Text(), "LIVE-BETA")
	})
}

func (s *suite) pcOffline(t *testing.T) {
	if err := s.env.Get("VENUS").Stop(); err != nil {
		t.Fatal(err)
	}
	s.waitFinding(150*time.Second, "H7", "VENUS")
	if err := s.env.Get("VENUS").Start(s.ctx); err != nil {
		t.Fatal(err)
	}
	s.waitGone(150*time.Second, "H7", "VENUS")
}

func (s *suite) serverStopped(t *testing.T) {
	since := time.Now()
	sirius := s.env.Get("SIRIUS")
	if err := sirius.Stop(); err != nil {
		t.Fatal(err)
	}
	f := s.waitFinding(90*time.Second, "H1", "SIRIUS")
	if !f.Urgent {
		t.Fatal("H1 must be urgent")
	}
	m := s.waitDiscord(since, 90*time.Second, "urgent H1 ping", mentionsCheck("SIRIUS"))
	if !strings.Contains(m.Text(), "Server unreachable") {
		t.Fatalf("H1 message: %s", m.Text())
	}
	// Findings from SIRIUS are frozen, not resolved, and AKKA/SOL keep working.
	st, err := s.sw.Status()
	if err != nil {
		t.Fatal(err)
	}
	h6 := st.Find("H6", "LIVE-GAMMA-1SOURCE on SIRIUS")
	if h6 == nil || !h6.Stale {
		s.dump(st)
		t.Fatal("SIRIUS's H6 should stay open and stale while it is down")
	}
	if st.Find("H2", "SIRIUS") != nil {
		t.Fatal("a stopped server must not also raise H2")
	}
	for _, sv := range st.Servers {
		if sv.Name != "SIRIUS" && sv.Status != "up" {
			t.Fatalf("%s affected by SIRIUS being down: %s", sv.Name, sv.Status)
		}
	}
	// Exactly one H1 ping for SIRIUS.
	n := 0
	for _, m := range s.fd.Messages() {
		if !m.At.Before(since) && m.Mentions(role) && strings.Contains(m.Text(), "SIRIUS") && strings.Contains(m.Text(), "unreachable") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly one H1 ping, got %d", n)
	}
	since = time.Now()
	if err := sirius.Start(s.ctx); err != nil {
		t.Fatal(err)
	}
	s.waitGone(90*time.Second, "H1", "SIRIUS")
	s.waitDiscord(since, 60*time.Second, "resolved H1", func(m harness.DiscordMessage) bool {
		return !m.Mentions(role) && strings.Contains(m.Text(), "Resolved") && strings.Contains(m.Text(), "SIRIUS")
	})
	st, _ = s.sw.WaitStatus(s.ctx, 60*time.Second, func(st *harness.Status) bool {
		f := st.Find("H6", "LIVE-GAMMA-1SOURCE on SIRIUS")
		return f != nil && !f.Stale
	})
	if f := st.Find("H6", "LIVE-GAMMA-1SOURCE on SIRIUS"); f == nil || f.Stale {
		t.Fatal("H6 should be live again after SIRIUS returns")
	}
}

func (s *suite) certReplaced(t *testing.T) {
	since := time.Now()
	sol := s.env.Get("SOL")
	if err := sol.ReplaceCert(s.ctx); err != nil {
		t.Fatal(err)
	}
	f := s.waitFinding(120*time.Second, "H3", "SOL")
	st, _ := s.sw.Status()
	if st.Find("H1", "SOL") != nil {
		t.Fatal("H3 replaces H1 for a changed certificate")
	}
	s.waitDiscord(since, 90*time.Second, "urgent H3 ping", mentionsCheck("certificate"))
	fp := f.Detail("New fingerprint (full)")
	if len(fp) != 64 {
		t.Fatalf("new fingerprint missing from details: %+v", f.Details)
	}
	// Accept it in the UI, like an admin would.
	since = time.Now()
	loc, err := s.sw.PostForm("/settings", "/settings/servers/sol/accept-cert", url.Values{"fingerprint": {fp}, "confirm": {password}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(loc, "/settings") {
		t.Fatalf("unexpected redirect %q", loc)
	}
	s.waitGone(90*time.Second, "H3", "SOL")
	s.waitDiscord(since, 60*time.Second, "resolved H3", func(m harness.DiscordMessage) bool {
		return !m.Mentions(role) && strings.Contains(m.Text(), "Resolved") && strings.Contains(m.Text(), "SOL")
	})
}

func (s *suite) snooze(t *testing.T) {
	st, _ := s.sw.Status()
	f := st.Find("S4", "random stuff")
	if f == nil {
		t.Fatal("S4 missing")
	}
	if _, err := s.sw.PostForm("/problems", "/problems/"+f.ID+"/snooze", url.Values{"for": {"1d"}, "reason": {"test"}}); err != nil {
		t.Fatal(err)
	}
	st, err := s.sw.WaitStatus(s.ctx, 10*time.Second, func(st *harness.Status) bool {
		g := st.Find("S4", "random stuff")
		return g != nil && g.Snoozed
	})
	if err != nil {
		t.Fatal("snooze not applied")
	}
	if st.Counts["snoozed"] < 1 {
		t.Fatalf("snoozed count: %v", st.Counts)
	}
}

func (s *suite) payloads(t *testing.T) {
	msgs := s.fd.Messages()
	if len(msgs) < 6 {
		t.Fatalf("expected several Discord messages, got %d", len(msgs))
	}
	for _, m := range msgs {
		if len(m.Problems) > 0 {
			t.Fatalf("Discord limits violated: %v", m.Problems)
		}
		if len(m.Embeds) == 0 {
			t.Fatal("message without embed")
		}
		if m.Mentions(role) && (len(m.AllowedMentions.Roles) != 1 || m.AllowedMentions.Roles[0] != role) {
			t.Fatal("mention without allowed_mentions")
		}
		if !m.Mentions(role) && len(m.AllowedMentions.Roles) > 0 {
			t.Fatal("allowed role without mention")
		}
		if strings.Contains(m.Text(), "Resolved") && m.Mentions(role) {
			t.Fatal("resolved messages must not mention the role")
		}
		if !strings.Contains(m.Embeds[0].URL, "dashboard.test") {
			t.Fatalf("missing dashboard link: %q", m.Embeds[0].URL)
		}
	}
}

func (s *suite) noSecrets(t *testing.T) {
	var texts []string
	for _, p := range []string{"/", "/grid", "/problems", "/settings", "/settings/export", "/api/v1/status"} {
		b, err := s.sw.Get(p)
		if err != nil {
			t.Fatal(err)
		}
		texts = append(texts, b)
	}
	for _, m := range s.fd.Messages() {
		texts = append(texts, m.Text())
	}
	if b, err := os.ReadFile(s.sw.LogPath()); err == nil {
		texts = append(texts, string(b))
	}
	secrets := []string{"fake-token", token}
	for _, inst := range s.env.Order {
		secrets = append(secrets, inst.APIKey)
	}
	for _, txt := range texts {
		for _, sec := range secrets {
			if strings.Contains(txt, sec) {
				t.Fatalf("secret %q leaked", sec)
			}
		}
	}
}
