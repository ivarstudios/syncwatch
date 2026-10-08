package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/gate"
)

// GateOptions configure `syncwatch gate`.
type GateOptions struct {
	DataDir    string // the gate's own token, certificate and Syncthing pin
	Listen     string
	Syncthing  string // Syncthing's GUI/API URL, seen from the gate
	KeyFile    string // a file holding Syncthing's API key
	Key        string // or the key itself
	ConfigXML  string // or Syncthing's config.xml, to read the key from
	Token      string // the token syncwatch presents; default: <data>/gate-token
	TLSCert    string
	TLSKey     string
	TLSHosts   []string
	Log        *slog.Logger
	OnListen   func(addr string) // for tests
	TokenReady func(tok string)  // for tests
}

// RunGate serves the read-only gate until ctx is cancelled.
func RunGate(ctx context.Context, o GateOptions) error {
	log := o.Log
	if err := os.MkdirAll(o.DataDir, 0o700); err != nil {
		return fmt.Errorf("creating data directory: %w", err)
	}
	key, err := gateSyncthingKey(o)
	if err != nil {
		return err
	}
	tok, made, err := gateToken(o)
	if err != nil {
		return err
	}
	if made {
		log.Warn("created the gate token; enter it as this server's API key in syncwatch (Settings → Servers), with the gate's address as the URL",
			"token", tok, "file", filepath.Join(o.DataDir, "gate-token"))
	}
	if o.TokenReady != nil {
		o.TokenReady(tok)
	}
	pinFile := filepath.Join(o.DataDir, "syncthing.pin")
	pin := ""
	if b, err := os.ReadFile(pinFile); err == nil {
		pin = strings.TrimSpace(string(b))
	}
	g, err := gate.New(gate.Options{
		Syncthing: o.Syncthing, APIKey: key, Token: tok, Pin: pin, Log: log,
		OnPin: func(fp string) {
			if err := os.WriteFile(pinFile, []byte(fp+"\n"), 0o600); err != nil {
				log.Error("saving Syncthing's certificate pin", "err", err)
			}
			log.Info("trusting Syncthing's self-signed certificate on first use", "sha256", fp[:16])
		},
	})
	if err != nil {
		return err
	}
	// The gate always serves HTTPS: the token must not cross the LAN in clear.
	tc, err := tlsConfig(Options{DataDir: o.DataDir, Listen: o.Listen, TLSCert: o.TLSCert, TLSKey: o.TLSKey, TLSSelfSigned: true, TLSHosts: o.TLSHosts}, config.Default(), log)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", o.Listen)
	if err != nil {
		return err
	}
	if o.OnListen != nil {
		o.OnListen(ln.Addr().String())
	}
	hs := &http.Server{
		Handler:           g,
		TLSConfig:         tc,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No write timeout: event long-polls stay open for minutes.
	}
	errc := make(chan error, 1)
	go func() { errc <- hs.ServeTLS(ln, "", "") }()
	log.Info("syncwatch gate started", "listen", ln.Addr().String(), "syncthing", o.Syncthing)
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = hs.Shutdown(sctx)
	return nil
}

// gateSyncthingKey reads Syncthing's API key: from a file, the option, or
// Syncthing's config.xml.
func gateSyncthingKey(o GateOptions) (string, error) {
	switch {
	case o.KeyFile != "":
		b, err := os.ReadFile(o.KeyFile)
		if err != nil {
			return "", fmt.Errorf("reading the Syncthing API key: %w", err)
		}
		return strings.TrimSpace(string(b)), nil
	case o.Key != "":
		return o.Key, nil
	case o.ConfigXML != "":
		b, err := os.ReadFile(o.ConfigXML)
		if err != nil {
			return "", fmt.Errorf("reading Syncthing's config.xml: %w", err)
		}
		var c struct {
			GUI struct {
				APIKey string `xml:"apikey"`
			} `xml:"gui"`
		}
		if err := xml.Unmarshal(b, &c); err != nil || strings.TrimSpace(c.GUI.APIKey) == "" {
			return "", fmt.Errorf("no API key in %s", o.ConfigXML)
		}
		return strings.TrimSpace(c.GUI.APIKey), nil
	}
	return "", errors.New("the gate needs Syncthing's API key: --syncthing-key-file, SYNCWATCH_GATE_SYNCTHING_KEY or --syncthing-config")
}

// gateToken returns the token syncwatch must present, creating and storing
// one on first start when none is given. made reports a new token.
func gateToken(o GateOptions) (tok string, made bool, err error) {
	if o.Token != "" {
		return o.Token, false, nil
	}
	path := filepath.Join(o.DataDir, "gate-token")
	if b, err := os.ReadFile(path); err == nil {
		if t := strings.TrimSpace(string(b)); t != "" {
			return t, false, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, err
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", false, err
	}
	tok = "swg_" + base64.RawURLEncoding.EncodeToString(raw)
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", false, fmt.Errorf("writing %s: %w", path, err)
	}
	return tok, true, nil
}
