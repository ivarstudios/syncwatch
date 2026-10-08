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
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/stclient"
)

// Self-signed certificates are valid for 825 days (the longest Apple
// platforms accept) and replaced on start when fewer than 30 days are left.
const (
	selfSignedValidity = 825 * 24 * time.Hour
	selfSignedRenew    = 30 * 24 * time.Hour
)

// tlsConfig returns the dashboard's TLS configuration, or nil for plain HTTP.
func tlsConfig(o Options, cfg *config.Config, log *slog.Logger) (*tls.Config, error) {
	certFile, keyFile := o.TLSCert, o.TLSKey
	switch {
	case certFile != "" || keyFile != "":
		// Your own certificate wins over the self-signed one the image
		// turns on by default.
		if certFile == "" || keyFile == "" {
			return nil, errors.New("HTTPS needs both a certificate and a key file")
		}
	case o.TLSSelfSigned:
		certFile, keyFile = filepath.Join(o.DataDir, "tls-cert.pem"), filepath.Join(o.DataDir, "tls-key.pem")
		hosts := tlsHosts(cfg, o.Listen, o.TLSHosts)
		made, err := ensureSelfSigned(certFile, keyFile, hosts, time.Now())
		if err != nil {
			return nil, fmt.Errorf("self-signed certificate: %w", err)
		}
		if made {
			log.Info("created a self-signed certificate for the dashboard; browsers ask to trust it once", "file", certFile, "names", hosts)
		}
	default:
		return nil, nil
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("loading the TLS certificate: %w", err)
	}
	sum := sha256.Sum256(cert.Certificate[0])
	log.Info("serving HTTPS", "certificate", certFile, "sha256", stclient.ShortFP(hex.EncodeToString(sum[:])))
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}, nil
}

// tlsHosts are the names a self-signed certificate is made for: loopback,
// the host of public_url, the listen address when it is a specific one, and
// any extra names (SYNCWATCH_TLS_HOSTS, e.g. the NAS's LAN address, which a
// container listening on :8080 can't know).
func tlsHosts(cfg *config.Config, listen string, extra []string) []string {
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
		add(strings.TrimSpace(h))
	}
	return hosts
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
