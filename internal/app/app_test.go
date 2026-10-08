package app

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
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
