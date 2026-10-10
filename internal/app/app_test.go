package app

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
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
  discord_role_id: "123"
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
	for _, keep := range []string{"# IVAR bootstrap", "https://192.168.2.10:8384", "discord_role_id", "imported into syncwatch's encrypted store"} {
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
	if !strings.Contains(logs.String(), "still contains secrets") {
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
	if strings.Contains(logs.String(), "still contains secrets") {
		t.Fatalf("null values reported as secrets:\n%s", logs.String())
	}
	if b, _ := os.ReadFile(path); string(b) != yaml {
		t.Fatalf("file changed:\n%s", b)
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
	c.Notify.DiscordRoleID = "1"
	if err := w.save(c); err != nil {
		t.Fatal(err)
	}
	stored, err := settings.Load(st)
	if err != nil || len(stored.Servers) != 2 || stored.Notify.DiscordRoleID != "1" {
		t.Fatalf("UI save undid the import: %+v %v", stored, err)
	}
	if key, _, _ := st.ServerKey("b"); key != "key-b" {
		t.Fatalf("B's key: %q", key)
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
	leaf, err := x509.ParseCertificate(tc.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
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
	if made, err := ensureSelfSigned(certFile, keyFile, tlsHosts(cfg, "", nil), time.Now()); err != nil || made {
		t.Fatalf("valid certificate replaced: %v %v", made, err)
	}
	if made, err := ensureSelfSigned(certFile, keyFile, tlsHosts(cfg, "", nil), time.Now().Add(selfSignedValidity-time.Hour)); err != nil || !made {
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
	leaf, _ = x509.ParseCertificate(tc.Certificates[0].Certificate[0])
	for _, h := range []string{"syncwatch.ivar.lan", "192.168.1.20", "localhost"} {
		if err := leaf.VerifyHostname(h); err != nil {
			t.Errorf("new certificate doesn't cover %s: %v", h, err)
		}
	}
	if after, _ := os.ReadFile(certFile); bytes.Equal(before, after) {
		t.Fatal("certificate kept although public_url changed")
	}
	if made, err := ensureSelfSigned(certFile, keyFile, tlsHosts(cfg, "192.168.1.20:8080", nil), time.Now()); err != nil || made {
		t.Fatalf("unchanged setup replaced its certificate: %v %v", made, err)
	}
	if got := tlsHosts(cfg, ":8080", nil); len(got) != 4 {
		t.Fatalf("listening on all interfaces adds no name: %v", got)
	}
	if got := tlsHosts(cfg, "0.0.0.0:8080", nil); len(got) != 4 {
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
	if leaf, _ := x509.ParseCertificate(tc.Certificates[0].Certificate[0]); leaf.VerifyHostname("own.example") != nil {
		t.Fatal("the self-signed certificate was used instead of the given one")
	}

	// Extra names (SYNCWATCH_TLS_HOSTS), e.g. the NAS's LAN address.
	tc, err = tlsConfig(Options{DataDir: dir, TLSSelfSigned: true, Listen: ":8080", TLSHosts: []string{"192.168.1.10", "akka.ivar.lan"}}, cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ = x509.ParseCertificate(tc.Certificates[0].Certificate[0])
	for _, h := range []string{"192.168.1.10", "akka.ivar.lan", "syncwatch.ivar.lan"} {
		if err := leaf.VerifyHostname(h); err != nil {
			t.Errorf("certificate doesn't cover %s: %v", h, err)
		}
	}
}
