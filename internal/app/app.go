// Package app wires the collector, engine, notifier and web server together.
package app

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/ivarstudios/syncwatch/internal/collector"
	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/engine"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/notify"
	"github.com/ivarstudios/syncwatch/internal/settings"
	"github.com/ivarstudios/syncwatch/internal/store"
	"github.com/ivarstudios/syncwatch/internal/web"
)

// Options configure a server run.
type Options struct {
	DataDir       string
	Listen        string
	ConfigFile    string // bootstrap YAML, imported when the database has no configuration
	SecretKey     string // SYNCWATCH_SECRET_KEY
	AdminPassword string // SYNCWATCH_ADMIN_PASSWORD, used only if no admin password is set
	TLSCert       string // serve HTTPS with this certificate and key
	TLSKey        string
	TLSSelfSigned bool     // serve HTTPS with a self-signed certificate kept in DataDir
	TLSHosts      []string // extra names for the self-signed certificate
	Version       string
	Log           *slog.Logger
}

// OpenStore opens the database in a data directory.
func OpenStore(dataDir, secretKey string) (*store.Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating data directory: %w", err)
	}
	key, err := store.LoadKey(dataDir, secretKey)
	if err != nil {
		return nil, err
	}
	return store.Open(filepath.Join(dataDir, "syncwatch.db"), key)
}

// LoadConfig returns the stored configuration, importing the bootstrap file
// (with any secrets in it) the first time.
func LoadConfig(st *store.Store, bootstrap string, log *slog.Logger) (*config.Config, error) {
	cfg, err := settings.Load(st)
	if err != nil {
		return nil, err
	}
	if cfg != nil {
		if bootstrap != "" && bootstrapHasSecrets(bootstrap) {
			log.Warn("the bootstrap file is no longer read but still contains secrets (API keys, webhook, token) in plaintext; delete it", "file", bootstrap)
		}
		return cfg, nil
	}
	if bootstrap != "" {
		b, err := os.ReadFile(bootstrap)
		switch {
		case err == nil:
			c, err := config.ParseYAML(b)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", bootstrap, err)
			}
			n, err := settings.ImportSecrets(st, c)
			if err != nil {
				return nil, err
			}
			if err := settings.Save(st, c); err != nil {
				return nil, err
			}
			log.Info("imported bootstrap configuration", "file", bootstrap, "servers", len(c.Servers), "secrets", n)
			if n > 0 {
				// The secrets are encrypted in the database now; don't leave
				// them in plaintext next to it.
				if err := scrubBootstrap(bootstrap); err != nil {
					log.Warn("could not remove the imported secrets from the bootstrap file; delete it", "file", bootstrap, "err", err)
				} else {
					log.Info("removed the imported secrets from the bootstrap file", "file", bootstrap)
				}
			}
			return c, nil
		case !errors.Is(err, os.ErrNotExist):
			return nil, err
		}
	}
	c := config.Default()
	if err := settings.Save(st, c); err != nil {
		return nil, err
	}
	return c, nil
}

// ImportFile replaces the stored configuration with a YAML file.
func ImportFile(st *store.Store, path string) (*config.Config, int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, err
	}
	c, err := config.ParseYAML(b)
	if err != nil {
		return nil, 0, err
	}
	old, err := settings.Load(st)
	if err != nil {
		return nil, 0, err
	}
	n, err := settings.ImportSecrets(st, c)
	if err != nil {
		return nil, 0, err
	}
	if err := settings.Save(st, c); err != nil {
		return nil, 0, err
	}
	return c, n, settings.DropRemovedServers(st, old, c)
}

// Run serves until ctx is cancelled.
func Run(ctx context.Context, o Options) error {
	log := o.Log
	st, err := OpenStore(o.DataDir, o.SecretKey)
	if err != nil {
		return err
	}
	defer st.Close()

	cfg, err := LoadConfig(st, o.ConfigFile, log)
	if err != nil {
		return err
	}
	holder, err := config.NewHolder(cfg)
	if err != nil {
		return fmt.Errorf("stored configuration is invalid: %w", err)
	}
	if o.AdminPassword != "" {
		if h, _ := st.Setting(web.KeyAdminHash); h == "" {
			if len([]rune(o.AdminPassword)) < web.MinPasswordLength {
				return fmt.Errorf("SYNCWATCH_ADMIN_PASSWORD must be at least %d characters", web.MinPasswordLength)
			}
			hash, err := web.HashPassword(o.AdminPassword)
			if err != nil {
				return err
			}
			if err := st.SetSetting(web.KeyAdminHash, hash); err != nil {
				return err
			}
			log.Info("admin password set from SYNCWATCH_ADMIN_PASSWORD")
		}
	}

	hub := model.NewHub(nil)
	eng, err := engine.New(st, hub, holder, nil, log)
	if err != nil {
		return err
	}
	ntf := notify.New(eng, st, holder, log)
	eng.OnEvaluate(ntf.Wake)
	mgr := collector.NewManager(hub, st, holder, log)
	holder.OnChange(func() {
		mgr.Sync()
		eng.Trigger()
	})

	srvWeb, err := web.New(holder, st, eng, web.Hooks{
		SaveConfig: func(c *config.Config) error {
			c.ApplyDefaults()
			if err := c.Validate(); err != nil {
				return err
			}
			if err := settings.Save(st, c); err != nil {
				return err
			}
			return holder.Set(c)
		},
		AcceptCert:      mgr.AcceptCert,
		ResetPin:        mgr.ResetPin,
		SendTest:        ntf.SendTest,
		Notice:          ntf.Notice,
		NoticeLater:     ntf.NoticeLater,
		ChannelsChanged: ntf.ChannelsChanged,
		Delivery:        ntf.Delivery,
	}, o.Version, log)
	if err != nil {
		return err
	}
	if tok := srvWeb.SetupToken(); tok != "" && holder.Get().Access.Mode == config.AccessBuiltin {
		log.Warn("no admin password is set yet: open the dashboard and enter this one-time setup token", "setup_token", tok)
	}

	tc, err := tlsConfig(o, holder.Get(), log)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", o.Listen)
	if err != nil {
		return err
	}
	if tc != nil {
		ln = tls.NewListener(ln, tc)
	}
	hs := &http.Server{
		Handler:           srvWeb,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{}, 3)
	go func() { mgr.Run(ctx); done <- struct{}{} }()
	go func() { eng.Run(ctx); done <- struct{}{} }()
	go func() { ntf.Run(ctx); done <- struct{}{} }()
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	log.Info("syncwatch started", "version", o.Version, "listen", ln.Addr().String(), "https", tc != nil, "data", o.DataDir, "servers", len(holder.Get().Servers))

	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	log.Info("shutting down")
	shCtx, shCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shCancel()
	_ = hs.Shutdown(shCtx)
	cancel()
	for i := 0; i < 3; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}
	return nil
}
