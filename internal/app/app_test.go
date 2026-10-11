package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/notify"
	"github.com/ivarstudios/syncwatch/internal/settings"
	"github.com/ivarstudios/syncwatch/internal/store"
)

const bootstrapYAML = `# IVAR bootstrap
public_url: http://192.168.1.10:8080
servers:
  - name: AKKA
    url: https://192.168.1.10:8384
    api_key: secret-akka # from Syncthing → Settings
  - name: SOL
    url: https://192.168.2.10:8384
notifications:
  discord_webhook: https://discord.com/api/webhooks/1/secret-hook
  discord_role_id: "123456789012345678"
api:
  token: swt_secret_token
`

func TestBootstrapSecretsRemovedAfterImport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "syncwatch.yaml")
	if err := os.WriteFile(path, []byte(bootstrapYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	st, _ := store.OpenMemory(make([]byte, 32))
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	c, err := LoadConfig(st, path, log)
	if err != nil {
		t.Fatal(err)
	}
	if key, bound, _ := st.ServerKey("akka"); key != "secret-akka" || bound != "https://192.168.1.10:8384" {
		t.Fatalf("key not imported: %q %q", key, bound)
	}
	if len(c.Servers) != 2 {
		t.Fatalf("servers: %d", len(c.Servers))
	}

	b, _ := os.ReadFile(path)
	for _, secret := range []string{"secret-akka", "secret-hook", "swt_secret_token"} {
		if strings.Contains(string(b), secret) {
			t.Fatalf("bootstrap file still contains %q:\n%s", secret, b)
		}
	}
	// Everything else is kept, comments included, and the file still parses.
	for _, keep := range []string{"# IVAR bootstrap", "https://192.168.2.10:8384", "discord_role_id", "syncwatch imported this secret into its encrypted store"} {
		if !strings.Contains(string(b), keep) {
			t.Fatalf("bootstrap file lost %q:\n%s", keep, b)
		}
	}
	if _, err := config.ParseYAML(b); err != nil {
		t.Fatalf("scrubbed file doesn't parse: %v", err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 && os.PathSeparator == '/' {
		t.Fatalf("file mode changed to %v", fi.Mode().Perm())
	}

	// On later starts the file isn't read; one that still holds secrets
	// (e.g. replaced by hand, or on a read-only mount) is reported.
	_ = os.WriteFile(path, []byte(bootstrapYAML), 0o600)
	logs.Reset()
	if _, err := LoadConfig(st, path, log); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "still contains unencrypted secrets") {
		t.Fatalf("no warning: %s", logs.String())
	}
	if b2, _ := os.ReadFile(path); !bytes.Equal(b2, []byte(bootstrapYAML)) {
		t.Fatal("a bootstrap file that wasn't imported must not be changed")
	}
}

// api_key: null (or ~) is an empty value, not a secret: nothing is imported
// from it, so it must not be reported as still in the file on every start.
func TestBootstrapNullIsNotASecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "syncwatch.yaml")
	const yaml = "servers:\n  - name: A\n    url: https://10.0.0.1:8384\n    api_key: null\nnotifications:\n  discord_webhook: ~\napi:\n  token:\n"
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	st, _ := store.OpenMemory(make([]byte, 32))
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	for i := 0; i < 2; i++ {
		if _, err := LoadConfig(st, path, log); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Contains(logs.String(), "still contains unencrypted secrets") {
		t.Fatalf("null values reported as secrets:\n%s", logs.String())
	}
	if b, _ := os.ReadFile(path); string(b) != yaml {
		t.Fatalf("file changed:\n%s", b)
	}
}

// The webhook for settings notices is a secret of the bootstrap file too: it
// goes into the encrypted store, and the scrub removes it from the file.
func TestBootstrapNoticesWebhookScrubbed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "syncwatch.yaml")
	const hook = "https://discord.com/api/webhooks/2/secret-notices"
	if err := os.WriteFile(path, []byte("servers:\n  - name: A\n    url: https://10.0.0.1:8384\nnotifications:\n  discord_notices_webhook: "+hook+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !bootstrapHasSecrets(path) {
		t.Fatal("the webhook for settings notices is not a secret of the bootstrap file")
	}
	st, _ := store.OpenMemory(make([]byte, 32))
	var logs bytes.Buffer
	if _, err := LoadConfig(st, path, slog.New(slog.NewTextHandler(&logs, nil))); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.Secret(store.SecretNoticesWebhook); v != hook {
		t.Fatalf("stored webhook for settings notices %q", v)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "secret-notices") || !strings.Contains(string(b), scrubbedNote) || bootstrapHasSecrets(path) {
		t.Fatalf("the bootstrap file still contains the webhook for settings notices:\n%s", b)
	}
	if _, err := config.ParseYAML(b); err != nil {
		t.Fatalf("scrubbed file does not parse: %v", err)
	}
}

// A symlinked bootstrap file (Kubernetes-style mounts, hand-made links) is
// scrubbed where the plaintext lives: in the target, with the link kept.
func TestBootstrapScrubFollowsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.yaml")
	link := filepath.Join(dir, "syncwatch.yaml")
	if err := os.WriteFile(target, []byte(bootstrapYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks not available: %v", err)
	}
	st, _ := store.OpenMemory(make([]byte, 32))
	var logs bytes.Buffer
	if _, err := LoadConfig(st, link, slog.New(slog.NewTextHandler(&logs, nil))); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "removed the imported secrets") {
		t.Fatalf("scrub not reported:\n%s", logs.String())
	}
	b, _ := os.ReadFile(target)
	if strings.Contains(string(b), "secret-akka") || !strings.Contains(string(b), scrubbedNote) {
		t.Fatalf("symlink target still holds the secrets:\n%s", b)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced by a file")
	}
}

// A single-file bind mount can't be replaced by rename; the file is then
// rewritten in place rather than left with the secrets.
func TestBootstrapScrubInPlaceWhenRenameFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "syncwatch.yaml")
	if err := os.WriteFile(path, []byte(bootstrapYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	orig := renameFile
	renameFile = func(string, string) error { return errors.New("device or resource busy") }
	t.Cleanup(func() { renameFile = orig })
	st, _ := store.OpenMemory(make([]byte, 32))
	var logs bytes.Buffer
	if _, err := LoadConfig(st, path, slog.New(slog.NewTextHandler(&logs, nil))); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "removed the imported secrets") {
		t.Fatalf("scrub not reported:\n%s", logs.String())
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "secret-hook") || !strings.Contains(string(b), "# IVAR bootstrap") {
		t.Fatalf("file not rewritten in place:\n%s", b)
	}
	if _, err := config.ParseYAML(b); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(path)); len(entries) != 1 {
		t.Fatalf("temp file left behind: %v", entries)
	}
}

// syncwatch import-config while serving: the running server applies the
// imported configuration, and the next save in the UI keeps it.
func TestConfigReloadedAfterExternalImport(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	cfg, err := LoadConfig(st, "", log)
	if err != nil {
		t.Fatal(err)
	}
	holder, err := config.NewHolder(cfg)
	if err != nil {
		t.Fatal(err)
	}
	reloads := 0
	w := &configWatcher{st: st, holder: holder, log: log, onReload: func() { reloads++ }}
	if w.applied, err = st.ConfigRevision(); err != nil || w.applied == 0 {
		t.Fatalf("revision after the first save: %d %v", w.applied, err)
	}

	// Saves from this process (the UI) are not reloaded.
	c := holder.Get().Clone()
	c.Servers = append(c.Servers, config.Server{Name: "A", URL: "https://a.invalid:8384"})
	if err := w.save(c); err != nil {
		t.Fatal(err)
	}
	w.check()
	if reloads != 0 || len(holder.Get().Servers) != 1 {
		t.Fatal("own save treated as a change from outside")
	}

	// Another process replaces the configuration.
	yaml := filepath.Join(dir, "new.yaml")
	if err := os.WriteFile(yaml, []byte("servers:\n  - name: A\n    url: https://a.invalid:8384\n  - name: B\n    url: https://b.invalid:8384\n    api_key: key-b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	other, err := OpenExistingStore(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ImportFile(other, yaml); err != nil {
		t.Fatal(err)
	}
	other.Close()
	if len(holder.Get().Servers) != 1 {
		t.Fatal("applied before the check")
	}
	w.check()
	if got := holder.Get().Servers; len(got) != 2 || got[1].ID != "b" {
		t.Fatalf("not reloaded: %+v", got)
	}
	if reloads != 1 || !strings.Contains(logs.String(), "configuration reloaded") {
		t.Fatalf("reloads=%d logs:\n%s", reloads, logs.String())
	}
	w.check()
	if reloads != 1 {
		t.Fatal("reloaded again without a change")
	}

	// The next UI save starts from the imported configuration.
	c = holder.Get().Clone()
	c.Notify.DiscordRoleID = "100000000000000001"
	if err := w.save(c); err != nil {
		t.Fatal(err)
	}
	stored, err := settings.Load(st)
	if err != nil || len(stored.Servers) != 2 || stored.Notify.DiscordRoleID != "100000000000000001" {
		t.Fatalf("UI save undid the import: %+v %v", stored, err)
	}
	if key, _, _ := st.ServerKey("b"); key != "key-b" {
		t.Fatalf("B's key: %q", key)
	}
}

// recSender is a notification channel that records what reaches it, with
// the webhook it was made for (read from the store, as the notifier does).
type recSender struct {
	hook string
	got  chan<- sentNotice
}

type sentNotice struct {
	hook string
	m    notify.Message
}

func (s *recSender) Name() string { return "discord" }

func (s *recSender) Send(_ context.Context, m notify.Message) error {
	s.got <- sentNotice{s.hook, m}
	return nil
}

func nextNotice(t *testing.T, got <-chan sentNotice) sentNotice {
	t.Helper()
	select {
	case n := <-got:
		return n
	case <-time.After(5 * time.Second):
		t.Fatal("no settings notice")
	}
	return sentNotice{}
}

func noMoreNotices(t *testing.T, got <-chan sentNotice) {
	t.Helper()
	select {
	case n := <-got:
		t.Fatalf("one more settings notice: %q %q", n.m.Title, n.m.Summary)
	case <-time.After(300 * time.Millisecond):
	}
}

// syncwatch import-config and reset-admin-password while serving: the
// server that runs sends one settings notice for each and logs a warning.
// An import that replaces the webhook is announced to the old webhook, as a
// change in Settings is. A command run while no server runs is announced
// at the next start, one time.
func TestCLIChangesAnnounced(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	cfg, err := LoadConfig(st, "", log)
	if err != nil {
		t.Fatal(err)
	}
	holder, err := config.NewHolder(cfg)
	if err != nil {
		t.Fatal(err)
	}
	const oldHook, newHook = "https://discord.com/api/webhooks/1/old", "https://discord.com/api/webhooks/2/new"
	if err := st.SetSecret(store.SecretDiscordWebhook, oldHook); err != nil {
		t.Fatal(err)
	}
	got := make(chan sentNotice, 10)
	ntf := notify.New(nil, st, holder, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ntf.SetSenders(func() []notify.Sender {
		hook, _ := st.Secret(store.SecretDiscordWebhook)
		return []notify.Sender{&recSender{hook: hook, got: got}}
	})
	start := func() *configWatcher {
		w := &configWatcher{st: st, holder: holder, log: log, onReload: ntf.ChannelsChanged, noticeLater: ntf.NoticeLater}
		if w.applied, err = st.ConfigRevision(); err != nil {
			t.Fatal(err)
		}
		w.capture()
		return w
	}
	w := start()
	w.check()
	noMoreNotices(t, got)

	// The import brings a server and a new webhook.
	yaml := filepath.Join(dir, "new.yaml")
	if err := os.WriteFile(yaml, []byte("servers:\n  - name: A\n    url: https://a.invalid:8384\nnotifications:\n  discord_webhook: "+newHook+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	other, err := OpenExistingStore(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := ImportFile(other, yaml); err != nil {
		t.Fatal(err)
	}
	other.Close()
	w.check()
	n := nextNotice(t, got)
	if n.hook != oldHook {
		t.Fatalf("the import was announced to %s, not to the webhook it replaced", n.hook)
	}
	if !strings.Contains(n.m.Title, titleCLIImport) || n.m.Summary != "syncwatch import-config replaced the configuration.\nThe import changed these sections of Settings: Servers.\nThe Discord webhook is replaced." {
		t.Fatalf("import notice: %q %q", n.m.Title, n.m.Summary)
	}
	noMoreNotices(t, got)
	if len(holder.Get().Servers) != 1 {
		t.Fatal("the import was not applied")
	}
	for _, want := range []string{`level=WARN msg="settings changed" change="` + titleCLIImport + `"`, `source="command line"`} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("no %q in the log:\n%s", want, logs.String())
		}
	}
	w.check()
	noMoreNotices(t, got)

	// The password reset of the command line, after the import: to the new
	// webhook, the channel of the configuration that runs now.
	logs.Reset()
	other, err = OpenExistingStore(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := RecordPasswordReset(other); err != nil {
		t.Fatal(err)
	}
	other.Close()
	w.check()
	n = nextNotice(t, got)
	if n.hook != newHook || !strings.Contains(n.m.Title, titleCLIReset) || n.m.Summary != "The new administrator password is set. syncwatch logged out all sessions." {
		t.Fatalf("reset notice to %s: %q %q", n.hook, n.m.Title, n.m.Summary)
	}
	if !strings.Contains(logs.String(), `level=WARN msg="settings changed" change="`+titleCLIReset+`"`) {
		t.Fatalf("reset not logged:\n%s", logs.String())
	}
	w.check()
	noMoreNotices(t, got)

	// An import that changes nothing has no notice.
	other, _ = OpenExistingStore(dir, "")
	if err := os.WriteFile(yaml, []byte("servers:\n  - name: A\n    url: https://a.invalid:8384\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ImportFile(other, yaml); err != nil {
		t.Fatal(err)
	}
	other.Close()
	w.check()
	noMoreNotices(t, got)

	// A reset while no server ran: the next start sends it one time, with
	// the time of the change.
	if err := st.SetJSONSetting(keyCLINotice+"1", cliNotice{At: time.Now().Add(-2 * time.Hour), Title: titleCLIReset, Summary: "The new administrator password is set."}); err != nil {
		t.Fatal(err)
	}
	w = start()
	w.check()
	n = nextNotice(t, got)
	if !strings.Contains(n.m.Title, titleCLIReset) || !strings.Contains(n.m.Summary, "\nChanged at ") {
		t.Fatalf("notice at the next start: %q %q", n.m.Title, n.m.Summary)
	}
	w.check()
	noMoreNotices(t, got)
	if left, _ := st.SettingsWithPrefix(keyCLINotice); len(left) != 0 {
		t.Fatalf("notices left in the store: %v", left)
	}
}

// A lost secret.key (or a changed SYNCWATCH_SECRET_KEY) makes the stored
// secrets unreadable. syncwatch still starts and still makes a new key, but
// the log says why at start. SYNCWATCH_SECRET_KEY next to a secret.key gets
// a warning: syncwatch does not use the file.
func TestSecretKeyProblemsLogged(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "secret.key")
	st, err := openServeStore(dir, "", log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), `level=INFO msg="new secret key made"`) {
		t.Fatalf("first start:\n%s", logs.String())
	}
	if err := st.SetSecret(store.SecretDiscordWebhook, "https://discord.com/api/webhooks/1/x"); err != nil {
		t.Fatal(err)
	}
	st.Close()

	logs.Reset()
	if st, err = openServeStore(dir, "", log); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if logs.Len() != 0 {
		t.Fatalf("a normal start logged:\n%s", logs.String())
	}

	if err := os.Remove(keyFile); err != nil {
		t.Fatal(err)
	}
	logs.Reset()
	if st, err = openServeStore(dir, "", log); err != nil {
		t.Fatalf("a start without secret.key failed: %v", err)
	}
	st.Close()
	for _, want := range []string{`level=ERROR msg="cannot decrypt the stored secrets: restore the old secret.key"`, "secret.key was missing", store.SecretDiscordWebhook} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("no %q in the log:\n%s", want, logs.String())
		}
	}
	if _, err := os.Stat(keyFile); err != nil {
		t.Fatalf("no new secret.key: %v", err)
	}

	logs.Reset()
	env := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
	if st, err = openServeStore(dir, env, log); err != nil {
		t.Fatal(err)
	}
	st.Close()
	for _, want := range []string{`level=WARN msg="SYNCWATCH_SECRET_KEY is set, syncwatch does not use secret.key"`, `level=ERROR msg="cannot decrypt the stored secrets: restore the old secret key" key=SYNCWATCH_SECRET_KEY`} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("no %q in the log:\n%s", want, logs.String())
		}
	}
}

// Stopping with a dashboard tab open: its event stream is closed first, so
// shutdown doesn't sit out the 5 s Shutdown budget (Docker kills at 10 s).
func TestShutdownClosesEventStreams(t *testing.T) {
	dir := t.TempDir()
	// viewer: open lets the stream be opened without a login.
	boot := filepath.Join(dir, "syncwatch.yaml")
	if err := os.WriteFile(boot, []byte("access:\n  viewer: open\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	addr := make(chan string, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{DataDir: dir, Listen: "127.0.0.1:0", ConfigFile: boot,
			Log: slog.New(slog.NewTextHandler(io.Discard, nil)), OnListen: func(a string) { addr <- a }})
	}()
	var a string
	select {
	case a = <-addr:
	case err := <-done:
		t.Fatalf("Run returned: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("no listener")
	}
	resp, err := http.Get("http://" + a + "/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("HTTP %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if _, err := resp.Body.Read(make([]byte, 64)); err != nil { // the handler is in its loop
		t.Fatal(err)
	}
	start := time.Now()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("shutdown took %v with an event stream open", d)
	}
	ended := make(chan struct{})
	go func() { _, _ = io.ReadAll(resp.Body); close(ended) }()
	select {
	case <-ended:
	case <-time.After(3 * time.Second):
		t.Fatal("the event stream was not closed")
	}
}

func TestSelfSignedCertificate(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.PublicURL = "https://monitor.lan:8080"
	log := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	tc, err := tlsConfig(Options{DataDir: dir, TLSSelfSigned: true}, cfg, log)
	if err != nil || tc == nil {
		t.Fatalf("tls config: %v", err)
	}
	leaf := leafOf(t, tc)
	if err := leaf.VerifyHostname("monitor.lan"); err != nil {
		t.Fatal(err)
	}
	if err := leaf.VerifyHostname("127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if d := time.Until(leaf.NotAfter); d > selfSignedValidity || d < selfSignedValidity-2*time.Hour {
		t.Fatalf("validity %v", d)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "tls-key.pem")); os.PathSeparator == '/' && fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v", fi.Mode().Perm())
	}

	// Kept on the next start, replaced when it is about to expire.
	certFile, keyFile := filepath.Join(dir, "tls-cert.pem"), filepath.Join(dir, "tls-key.pem")
	if made, err := ensureSelfSigned(certFile, keyFile, tlsHosts(cfg, "", nil, log), time.Now()); err != nil || made {
		t.Fatalf("valid certificate replaced: %v %v", made, err)
	}
	if made, err := ensureSelfSigned(certFile, keyFile, tlsHosts(cfg, "", nil, log), time.Now().Add(selfSignedValidity-time.Hour)); err != nil || !made {
		t.Fatalf("expiring certificate kept: %v %v", made, err)
	}
	b, _ := os.ReadFile(certFile)
	if blk, _ := pem.Decode(b); blk == nil {
		t.Fatal("certificate file is not PEM")
	}

	// A new public_url host or a specific listen address gets a new
	// certificate on start; an unchanged setup keeps its own.
	before := b
	cfg.PublicURL = "https://syncwatch.ivar.lan"
	tc, err = tlsConfig(Options{DataDir: dir, TLSSelfSigned: true, Listen: "192.168.1.20:8080"}, cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	leaf = leafOf(t, tc)
	for _, h := range []string{"syncwatch.ivar.lan", "192.168.1.20", "localhost"} {
		if err := leaf.VerifyHostname(h); err != nil {
			t.Errorf("new certificate doesn't cover %s: %v", h, err)
		}
	}
	if after, _ := os.ReadFile(certFile); bytes.Equal(before, after) {
		t.Fatal("certificate kept although public_url changed")
	}
	if made, err := ensureSelfSigned(certFile, keyFile, tlsHosts(cfg, "192.168.1.20:8080", nil, log), time.Now()); err != nil || made {
		t.Fatalf("unchanged setup replaced its certificate: %v %v", made, err)
	}
	if got := tlsHosts(cfg, ":8080", nil, log); len(got) != 4 {
		t.Fatalf("listening on all interfaces adds no name: %v", got)
	}
	if got := tlsHosts(cfg, "0.0.0.0:8080", nil, log); len(got) != 4 {
		t.Fatalf("0.0.0.0 adds no name: %v", got)
	}

	// Plain HTTP unless asked; a certificate needs its key; not both modes.
	if tc, err := tlsConfig(Options{DataDir: dir}, cfg, log); err != nil || tc != nil {
		t.Fatal("TLS without being asked")
	}
	if _, err := tlsConfig(Options{DataDir: dir, TLSCert: certFile}, cfg, log); err == nil {
		t.Fatal("certificate without key accepted")
	}
	if tc, err := tlsConfig(Options{DataDir: dir, TLSCert: certFile, TLSKey: keyFile}, cfg, log); err != nil || tc.MinVersion != tls.VersionTLS12 {
		t.Fatalf("certificate files: %v", err)
	}

	// The image turns the self-signed certificate on by default; your own
	// certificate files win over it.
	own := filepath.Join(t.TempDir(), "own")
	if _, err := ensureSelfSigned(own+"-cert.pem", own+"-key.pem", []string{"own.example"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	tc, err = tlsConfig(Options{DataDir: dir, TLSCert: own + "-cert.pem", TLSKey: own + "-key.pem", TLSSelfSigned: true}, cfg, log)
	if err != nil {
		t.Fatalf("own certificate with self-signed on: %v", err)
	}
	if leafOf(t, tc).VerifyHostname("own.example") != nil {
		t.Fatal("the self-signed certificate was used instead of the given one")
	}

	// Extra names (SYNCWATCH_TLS_HOSTS), e.g. the NAS's LAN address.
	tc, err = tlsConfig(Options{DataDir: dir, TLSSelfSigned: true, Listen: ":8080", TLSHosts: []string{"192.168.1.10", "akka.ivar.lan"}}, cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	leaf = leafOf(t, tc)
	for _, h := range []string{"192.168.1.10", "akka.ivar.lan", "syncwatch.ivar.lan"} {
		if err := leaf.VerifyHostname(h); err != nil {
			t.Errorf("certificate doesn't cover %s: %v", h, err)
		}
	}
	if !slices.Contains(tc.NextProtos, "h2") {
		t.Fatal("HTTP/2 not offered")
	}
}

// leafOf is the certificate a TLS handshake gets from tc.
func leafOf(t *testing.T, tc *tls.Config) *x509.Certificate {
	t.Helper()
	cert, err := tc.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil || cert == nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return leaf
}

// SYNCWATCH_TLS_HOSTS entries with brackets or a port are normalised and
// nonsense is skipped with a warning; the certificate made for them is kept
// on the next start instead of being made anew every time.
func TestTLSHostsNormalised(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	extra := []string{"[fd00::1]", "[fd00::2]:8080", "192.168.1.10:8080", "nas.lan:8080", " Nas.Lan ", "http://nas", "bad host", "*.lan", "a_b.lan"}
	got := tlsHosts(config.Default(), ":8080", extra, log)
	want := []string{"localhost", "127.0.0.1", "::1", "fd00::1", "fd00::2", "192.168.1.10", "nas.lan", "a_b.lan"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	for _, bad := range []string{"http://nas", "bad host", "*.lan"} {
		if !strings.Contains(logs.String(), bad) {
			t.Errorf("no warning for %q:\n%s", bad, logs.String())
		}
	}
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "tls-cert.pem"), filepath.Join(dir, "tls-key.pem")
	if made, err := ensureSelfSigned(certFile, keyFile, got, time.Now()); err != nil || !made {
		t.Fatalf("first start: %v %v", made, err)
	}
	if made, err := ensureSelfSigned(certFile, keyFile, got, time.Now()); err != nil || made {
		t.Fatalf("the next start made a new certificate: %v %v", made, err)
	}
}

// Own certificate files are reloaded when they change (looked at once a
// minute), so a renewal by certbot needs no restart.
func TestCertificateFilesReloaded(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if _, err := ensureSelfSigned(certFile, keyFile, []string{"first.example"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	clock := time.Now()
	src := &certSource{certFile: certFile, keyFile: keyFile, log: slog.New(slog.NewTextHandler(&logs, nil)), now: func() time.Time { return clock }}
	if err := src.load(clock); err != nil {
		t.Fatal(err)
	}
	tc := &tls.Config{GetCertificate: src.get}
	if leafOf(t, tc).VerifyHostname("first.example") != nil {
		t.Fatal("the first certificate isn't served")
	}
	// Coarse file system clocks: give rewritten files a later mtime by hand.
	mtime := time.Now()
	touch := func(f string) {
		mtime = mtime.Add(2 * time.Second)
		if err := os.Chtimes(f, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}

	// Renewed in place (a new name forces new files).
	if made, err := ensureSelfSigned(certFile, keyFile, []string{"second.example"}, time.Now()); err != nil || !made {
		t.Fatalf("renewal: %v %v", made, err)
	}
	touch(certFile)
	touch(keyFile)
	clock = clock.Add(30 * time.Second)
	if leafOf(t, tc).VerifyHostname("second.example") == nil {
		t.Fatal("files read again before a minute passed")
	}
	clock = clock.Add(31 * time.Second)
	if leafOf(t, tc).VerifyHostname("second.example") != nil {
		t.Fatal("the renewed certificate isn't served")
	}
	if !strings.Contains(logs.String(), "TLS certificate reloaded") {
		t.Fatalf("reload not logged:\n%s", logs.String())
	}

	// certbot writes the certificate and the key one after the other: a pair
	// that doesn't match yet keeps the current certificate, and the key's
	// arrival completes the change.
	other := filepath.Join(dir, "third")
	if _, err := ensureSelfSigned(other+"-cert.pem", other+"-key.pem", []string{"third.example"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(other + "-cert.pem")
	if err := os.WriteFile(certFile, b, 0o644); err != nil {
		t.Fatal(err)
	}
	touch(certFile)
	clock = clock.Add(time.Minute)
	if leafOf(t, tc).VerifyHostname("second.example") != nil {
		t.Fatal("a certificate without its key replaced the current one")
	}
	if !strings.Contains(logs.String(), "cannot read the changed TLS certificate files") {
		t.Fatalf("mismatch not logged:\n%s", logs.String())
	}
	b, _ = os.ReadFile(other + "-key.pem")
	if err := os.WriteFile(keyFile, b, 0o600); err != nil {
		t.Fatal(err)
	}
	touch(keyFile)
	clock = clock.Add(time.Minute)
	if leafOf(t, tc).VerifyHostname("third.example") != nil {
		t.Fatal("the completed pair isn't served")
	}
}

// The self-signed certificate is renewed while running (looked at daily,
// 30 days before it expires), not only on the next start.
func TestSelfSignedRenewedWhileRunning(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "tls-cert.pem"), filepath.Join(dir, "tls-key.pem")
	hosts := []string{"localhost", "127.0.0.1", "nas.lan"}
	clock := time.Now()
	if _, err := ensureSelfSigned(certFile, keyFile, hosts, clock); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	src := &certSource{certFile: certFile, keyFile: keyFile, hosts: hosts, log: slog.New(slog.NewTextHandler(&logs, nil)), now: func() time.Time { return clock }}
	if err := src.load(clock); err != nil {
		t.Fatal(err)
	}
	tc := &tls.Config{GetCertificate: src.get}
	first := leafOf(t, tc)

	// Long before the end nothing changes, however many days pass.
	clock = clock.Add(100 * 24 * time.Hour)
	if !leafOf(t, tc).NotAfter.Equal(first.NotAfter) {
		t.Fatal("renewed with more than 30 days left")
	}
	// In the last 30 days the next daily look renews it.
	clock = clock.Add(selfSignedValidity - 120*24*time.Hour)
	leaf := leafOf(t, tc)
	if !leaf.NotAfter.After(first.NotAfter.Add(700 * 24 * time.Hour)) {
		t.Fatalf("not renewed: valid until %v", leaf.NotAfter)
	}
	if leaf.VerifyHostname("nas.lan") != nil {
		t.Fatal("the renewed certificate lost its names")
	}
	if !strings.Contains(logs.String(), "self-signed certificate replaced") {
		t.Fatalf("renewal not logged:\n%s", logs.String())
	}
	// The files hold the new one, for the next start.
	if made, err := ensureSelfSigned(certFile, keyFile, hosts, clock); err != nil || made {
		t.Fatalf("files not updated: %v %v", made, err)
	}
}

// HTTP/2 is offered, so several open tabs (each holding an event stream)
// don't exhaust the browser's per-host connection limit.
func TestHTTP2Served(t *testing.T) {
	tc, err := tlsConfig(Options{DataDir: t.TempDir(), TLSSelfSigned: true}, config.Default(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hs := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, r.Proto) })}
	go hs.Serve(tls.NewListener(ln, tc)) //nolint:errcheck
	defer hs.Close()
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // the test's own self-signed certificate
		ForceAttemptHTTP2: true,
	}}
	resp, err := client.Get("https://" + ln.Addr().String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.ProtoMajor != 2 || string(b) != "HTTP/2.0" {
		t.Fatalf("served over %s (handler saw %s)", resp.Proto, b)
	}
}
