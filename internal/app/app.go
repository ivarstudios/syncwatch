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
	"strings"
	"sync"
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
	OnListen      func(addr string) // for tests
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

// OpenExistingStore opens the database of an instance that exists already,
// for the maintenance commands. Unlike OpenStore it creates nothing: with a
// wrong --data they would otherwise set up a fresh, empty instance there and
// report success, and import-config would encrypt with a key the server
// doesn't have.
func OpenExistingStore(dataDir, secretKey string) (*store.Store, error) {
	dataDir = absPath(dataDir)
	if _, err := os.Stat(filepath.Join(dataDir, "syncwatch.db")); err != nil {
		return nil, fmt.Errorf("no syncwatch data in %s", dataDir)
	}
	if strings.TrimSpace(secretKey) == "" {
		if _, err := os.Stat(filepath.Join(dataDir, "secret.key")); err != nil {
			return nil, fmt.Errorf("no secret.key in %s and SYNCWATCH_SECRET_KEY is not set; use the key the server runs with", dataDir)
		}
	}
	return OpenStore(dataDir, secretKey)
}

// absPath resolves a relative path (the default data directory is ./data) so
// logs and errors say where it really is.
func absPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
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
			if _, err := saveConfig(st, c); err != nil {
				return nil, err
			}
			log.Info("imported bootstrap configuration", "file", bootstrap, "servers", len(c.Servers), "secrets", n)
			if n > 0 {
				// The secrets are encrypted in the database now; don't leave
				// them in plaintext next to it.
				if err := scrubBootstrap(bootstrap); err != nil {
					log.Warn("could not remove the imported secrets from the bootstrap file: it still holds them in plaintext; delete it or remove them by hand", "file", bootstrap, "err", err)
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
	if _, err := saveConfig(st, c); err != nil {
		return nil, err
	}
	return c, nil
}

// saveConfig stores c and bumps the configuration revision, which a running
// server watches (see configWatcher).
func saveConfig(st *store.Store, c *config.Config) (int64, error) {
	if err := settings.Save(st, c); err != nil {
		return 0, err
	}
	return st.BumpConfigRevision()
}

// ImportFile replaces the stored configuration with a YAML file. A running
// server applies it within configCheckInterval.
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
	if _, err := saveConfig(st, c); err != nil {
		return nil, 0, err
	}
	return c, n, settings.DropRemovedServers(st, old, c)
}

// configCheckInterval is how often a running server looks for a
// configuration saved by another process.
const configCheckInterval = 10 * time.Second

// configWatcher applies configuration changes another process made in the
// database (syncwatch import-config while serving), so they take effect
// without a restart and the next save in the UI doesn't write the old
// configuration back over them. Every save bumps a revision in the settings
// table; a revision the watcher didn't apply itself means a reload.
type configWatcher struct {
	st       *store.Store
	holder   *config.Holder
	log      *slog.Logger
	onReload func() // after a change from outside was applied

	mu      sync.Mutex
	applied int64 // the revision the holder has
}

// save stores a configuration changed in this process and installs it.
func (w *configWatcher) save(c *config.Config) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	rev, err := saveConfig(w.st, c)
	if err != nil {
		return err
	}
	w.applied = rev
	return w.holder.Set(c)
}

// check reloads the configuration if another process changed it.
func (w *configWatcher) check() {
	w.mu.Lock()
	defer w.mu.Unlock()
	rev, err := w.st.ConfigRevision()
	if err != nil || rev == w.applied {
		return
	}
	w.applied = rev // reported once, even when it can't be applied
	c, err := settings.Load(w.st)
	if err == nil && c == nil {
		err = errors.New("no configuration stored")
	}
	if err == nil {
		err = w.holder.Set(c)
	}
	if err != nil {
		w.log.Error("the configuration was changed by another process but can't be applied", "revision", rev, "err", err)
		return
	}
	w.log.Info("configuration reloaded: it was changed by another process (syncwatch import-config)", "revision", rev, "servers", len(c.Servers))
	if w.onReload != nil {
		w.onReload()
	}
}

func (w *configWatcher) run(ctx context.Context) {
	t := time.NewTicker(configCheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.check()
		}
	}
}

// Run serves until ctx is cancelled.
func Run(ctx context.Context, o Options) error {
	log := o.Log
	o.DataDir = absPath(o.DataDir)
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
	// An import may bring a new webhook; retry delivery at once (as the UI's
	// import does).
	watcher := &configWatcher{st: st, holder: holder, log: log, onReload: ntf.ChannelsChanged}
	if watcher.applied, err = st.ConfigRevision(); err != nil {
		return err
	}

	srvWeb, err := web.New(holder, st, eng, web.Hooks{
		SaveConfig: func(c *config.Config) error {
			c.ApplyDefaults()
			if err := c.Validate(); err != nil {
				return err
			}
			return watcher.save(c)
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
	// Requests, the dashboard's event streams among them, end with reqCtx:
	// shutdown cancels it first, so open tabs don't hold Shutdown back.
	reqCtx, reqCancel := context.WithCancel(context.Background())
	defer reqCancel()
	hs := &http.Server{
		Handler:           srvWeb,
		BaseContext:       func(net.Listener) context.Context { return reqCtx },
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var workers sync.WaitGroup
	for _, run := range []func(context.Context){mgr.Run, eng.Run, ntf.Run, watcher.run} {
		workers.Add(1)
		go func() { defer workers.Done(); run(ctx) }()
	}
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	if o.OnListen != nil {
		o.OnListen(ln.Addr().String())
	}
	log.Info("syncwatch started", "version", o.Version, "listen", ln.Addr().String(), "https", tc != nil, "data", o.DataDir, "servers", len(holder.Get().Servers))

	var serveErr error
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr = err
			log.Error("the web server stopped", "err", err)
		}
	}
	log.Info("shutting down")
	reqCancel()
	shCtx, shCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shCancel()
	_ = hs.Shutdown(shCtx)
	// The workers stop before the store closes under them, whatever ended
	// the run.
	cancel()
	stopped := make(chan struct{})
	go func() { workers.Wait(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		log.Warn("not all workers stopped in time")
	}
	return serveErr
}
