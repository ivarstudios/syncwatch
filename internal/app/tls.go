package app

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/stclient"
)

// Self-signed certificates are valid for 825 days (the longest Apple
// platforms accept) and replaced, on start or while running, when fewer than
// 30 days are left. Own certificate files are reloaded when they change.
const (
	selfSignedValidity = 825 * 24 * time.Hour
	selfSignedRenew    = 30 * 24 * time.Hour
	selfSignedCheck    = 24 * time.Hour // how often a running server looks at the expiry
	certCheckInterval  = time.Minute    // how often it looks at the certificate files
)

// tlsConfig returns the dashboard's TLS configuration, or nil for plain HTTP.
func tlsConfig(o Options, cfg *config.Config, log *slog.Logger) (*tls.Config, error) {
	src := &certSource{certFile: o.TLSCert, keyFile: o.TLSKey, log: log, now: time.Now}
	switch {
	case src.certFile != "" || src.keyFile != "":
		// Your own certificate wins over the self-signed one the image
		// turns on by default.
		if src.certFile == "" || src.keyFile == "" {
			return nil, errors.New("HTTPS needs both a certificate file and a private key file")
		}
	case o.TLSSelfSigned:
		src.certFile, src.keyFile = filepath.Join(o.DataDir, "tls-cert.pem"), filepath.Join(o.DataDir, "tls-key.pem")
		src.hosts = tlsHosts(cfg, o.Listen, o.TLSHosts, log)
		made, err := ensureSelfSigned(src.certFile, src.keyFile, src.hosts, time.Now())
		if err != nil {
			return nil, fmt.Errorf("cannot make the self-signed certificate: %w", err)
		}
		if made {
			log.Info("self-signed certificate generated for the dashboard: accept it in each browser", "file", src.certFile, "names", src.hosts)
		}
	default:
		return nil, nil
	}
	if err := src.load(time.Now()); err != nil {
		return nil, fmt.Errorf("cannot read the TLS certificate and key files: %w", err)
	}
	log.Info("syncwatch serves HTTPS", "certificate", src.certFile, "sha256", fingerprint(src.cert))
	// h2: browsers allow about six HTTP/1.1 connections per host, and every
	// open dashboard tab keeps one for its event stream.
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: src.get, NextProtos: []string{"h2", "http/1.1"}}, nil
}

// certSource hands the certificate to TLS handshakes and keeps it current
// without a restart: own certificate files are reloaded when they change
// (certbot and friends renew them in place), and the self-signed one is
// renewed before it expires.
type certSource struct {
	certFile, keyFile string
	hosts             []string // names of the self-signed certificate; nil for own files
	log               *slog.Logger
	now               func() time.Time

	mu      sync.Mutex
	cert    *tls.Certificate
	stamp   [2]time.Time // modification times of the files cert came from
	checked time.Time    // last look at the files
	renewed time.Time    // last look at the self-signed certificate's expiry
}

// load reads the certificate files.
func (c *certSource) load(now time.Time) error {
	cert, err := tls.LoadX509KeyPair(c.certFile, c.keyFile)
	if err != nil {
		return err
	}
	c.cert, c.stamp, c.checked, c.renewed = &cert, c.fileStamp(), now, now
	return nil
}

func (c *certSource) fileStamp() (st [2]time.Time) {
	for i, f := range []string{c.certFile, c.keyFile} {
		if fi, err := os.Stat(f); err == nil {
			st[i] = fi.ModTime()
		}
	}
	return st
}

// get is tls.Config.GetCertificate. The expiry is checked once a day and the
// files once a minute at most, so handshakes stay cheap.
func (c *certSource) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.hosts != nil && now.Sub(c.renewed) >= selfSignedCheck {
		c.renewed = now
		made, err := ensureSelfSigned(c.certFile, c.keyFile, c.hosts, now)
		switch {
		case err != nil:
			c.log.Warn("cannot replace the self-signed certificate", "err", err)
		case made:
			c.log.Info("self-signed certificate replaced: accept it again in each browser", "file", c.certFile, "names", c.hosts)
			c.reload(now)
		}
	}
	if now.Sub(c.checked) >= certCheckInterval {
		c.checked = now
		if c.fileStamp() != c.stamp {
			c.reload(now)
		}
	}
	return c.cert, nil
}

// reload picks up changed certificate files, keeping the previous
// certificate when the new files don't load (certbot writes them one after
// the other); the next change is tried again.
func (c *certSource) reload(now time.Time) {
	if err := c.load(now); err != nil { // load leaves cert as it was
		c.stamp = c.fileStamp()
		c.log.Warn("cannot read the changed TLS certificate files, the previous certificate stays in use", "certificate", c.certFile, "err", err)
		return
	}
	c.log.Info("TLS certificate reloaded", "certificate", c.certFile, "sha256", fingerprint(c.cert))
}

// fingerprint is the short SHA-256 of a certificate, as shown in the log.
func fingerprint(cert *tls.Certificate) string {
	sum := sha256.Sum256(cert.Certificate[0])
	return stclient.ShortFP(hex.EncodeToString(sum[:]))
}

// tlsHosts are the names a self-signed certificate is made for: loopback,
// the host of public_url, the listen address when it is a specific one, and
// any extra names (SYNCWATCH_TLS_HOSTS, e.g. the NAS's LAN address, which a
// container listening on :8080 can't know).
func tlsHosts(cfg *config.Config, listen string, extra []string, log *slog.Logger) []string {
	hosts := []string{"localhost", "127.0.0.1", "::1"}
	add := func(h string) {
		if h == "" || slices.ContainsFunc(hosts, func(x string) bool { return strings.EqualFold(x, h) }) {
			return
		}
		if ip := net.ParseIP(h); ip != nil && ip.IsUnspecified() {
			return
		}
		hosts = append(hosts, h)
	}
	if u, err := url.Parse(cfg.PublicURL); err == nil {
		add(u.Hostname())
	}
	if h, _, err := net.SplitHostPort(listen); err == nil {
		add(h)
	}
	for _, h := range extra {
		// Kept as given, a port or brackets would become a DNS name no
		// browser asks for, which the reuse check can't match either: a new
		// certificate on every start.
		if name, ok := hostName(h); ok {
			add(name)
		} else {
			log.Warn("SYNCWATCH_TLS_HOSTS entry ignored, it is not a host name or an IP address", "entry", h)
		}
	}
	return hosts
}

// hostName normalises a SYNCWATCH_TLS_HOSTS entry: brackets and a port are
// dropped ("[fd00::1]:8080" means fd00::1), and anything that is neither an
// IP address nor a DNS name is rejected.
func hostName(h string) (string, bool) {
	h = strings.TrimSpace(h)
	if host, port, err := net.SplitHostPort(h); err == nil && port != "" && strings.Trim(port, "0123456789") == "" {
		h = host
	} else if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1]
	}
	if net.ParseIP(h) != nil {
		return h, true
	}
	return h, validDNSName(h)
}

// validDNSName accepts the names a certificate can be made for: labels of
// letters, digits and hyphens (underscores too, which Go matches) joined by
// dots. No wildcards: VerifyHostname can't check them, so the certificate
// would be made anew on every start.
func validDNSName(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' || r == '-' || r == '_') {
				return false
			}
		}
	}
	return true
}

// ensureSelfSigned creates a self-signed certificate and key unless a valid
// one for every name in hosts exists already. It reports whether it created
// one, so a changed public_url gets a certificate for its host on restart.
func ensureSelfSigned(certFile, keyFile string, hosts []string, now time.Time) (bool, error) {
	if b, err := os.ReadFile(certFile); err == nil {
		if blk, _ := pem.Decode(b); blk != nil {
			if c, err := x509.ParseCertificate(blk.Bytes); err == nil && c.NotAfter.Sub(now) > selfSignedRenew && covers(c, hosts) {
				if _, err := tls.LoadX509KeyPair(certFile, keyFile); err == nil {
					return false, nil
				}
			}
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return false, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return false, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "syncwatch"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(selfSignedValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return false, err
	}
	kb, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb}), 0o600); err != nil {
		return false, err
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return false, err
	}
	return true, nil
}

// covers reports whether a certificate is valid for every host.
func covers(c *x509.Certificate, hosts []string) bool {
	for _, h := range hosts {
		if c.VerifyHostname(h) != nil {
			return false
		}
	}
	return true
}
