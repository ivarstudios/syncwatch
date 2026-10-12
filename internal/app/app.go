// Package app wires the collector, engine, notifier and web server together.
package app

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ivarstudios/syncwatch/internal/collector"
	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/engine"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/notify"
	"github.com/ivarstudios/syncwatch/internal/settings"
	"github.com/ivarstudios/syncwatch/internal/stclient"
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
	// TrustedProxies are the reverse proxies whose X-Forwarded-Proto header
	// counts (SYNCWATCH_TRUSTED_PROXIES).
	TrustedProxies []netip.Prefix
	// NotifyLimit is how long notifications can fail before /healthz
	// answers 503 (SYNCWATCH_HEALTH_NOTIFY_LIMIT); 0 = this rule is off.
	NotifyLimit time.Duration
	// DeadmanURL is the address of an outside monitor service that
	// syncwatch calls while it is healthy (SYNCWATCH_DEADMAN_URL); "" = off.
	DeadmanURL string
	Version    string
	Log        *slog.Logger
	OnListen   func(addr string) // for tests
	// onManager gets the collector manager after Run wired it (for tests).
	onManager func(*collector.Manager)
}

// evalLimit is how long syncwatch can be without an evaluation of the checks
// before /healthz answers 503. The engine evaluates at least every 30 seconds.
const evalLimit = 5 * time.Minute

// OpenStore opens the database in a data directory.
func OpenStore(dataDir, secretKey string) (*store.Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("cannot make the data directory: %w", err)
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
		return nil, fmt.Errorf("there is no syncwatch data in %s", dataDir)
	}
	if strings.TrimSpace(secretKey) == "" {
		if _, err := os.Stat(filepath.Join(dataDir, "secret.key")); err != nil {
			return nil, fmt.Errorf("there is no secret.key in %s and SYNCWATCH_SECRET_KEY is not set. Use the secret key of the syncwatch that runs", dataDir)
		}
	}
	return OpenStore(dataDir, secretKey)
}

// openServeStore opens the database for serve and logs the secret key
// problems at start. Without this they show only later, as API keys and a
// webhook that syncwatch cannot read. A missing secret.key is still made
// new, as before.
func openServeStore(dataDir, secretKey string, log *slog.Logger) (*store.Store, error) {
	keyFile := filepath.Join(dataDir, "secret.key")
	_, statErr := os.Stat(keyFile)
	fromEnv := strings.TrimSpace(secretKey) != ""
	st, err := OpenStore(dataDir, secretKey)
	if err != nil {
		return nil, err
	}
	made := !fromEnv && errors.Is(statErr, fs.ErrNotExist)
	if fromEnv && statErr == nil {
		log.Warn("SYNCWATCH_SECRET_KEY is set, syncwatch does not use secret.key", "file", keyFile)
	}
	source := keyFile
	if fromEnv {
		source = "SYNCWATCH_SECRET_KEY"
	}
	bad, total, err := st.UnreadableSecrets()
	switch {
	case err != nil:
		log.Error("cannot read the stored secrets", "err", err)
	case len(bad) > 0 && made:
		log.Error("cannot decrypt the stored secrets: restore the old secret.key", "cause", "secret.key was missing, syncwatch made a new key", "file", keyFile, "secrets", strings.Join(bad, ", "))
	case len(bad) > 0:
		log.Error("cannot decrypt the stored secrets: restore the old secret key", "key", source, "secrets", strings.Join(bad, ", "), "stored", total)
	case made:
		log.Info("new secret key made", "file", keyFile)
	}
	return st, nil
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
			log.Warn("syncwatch does not read the bootstrap file, but the file still contains unencrypted secrets: delete it", "file", bootstrap)
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
			log.Info("bootstrap file imported", "file", bootstrap, "servers", len(c.Servers), "secrets", n)
			if n > 0 {
				// The secrets are encrypted in the database now; don't leave
				// them in plaintext next to it.
				if err := scrubBootstrap(bootstrap); err != nil {
					log.Warn("cannot remove the imported secrets from the bootstrap file, the file still contains them unencrypted: delete the file or remove the secrets manually", "file", bootstrap, "err", err)
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

// errLocksEveryoneOut refuses a file that sets built-in login to on while the
// stored configuration is in proxy mode and no administrator password is set.
// A server that runs in proxy mode did not log a setup token: after the
// reload, each page asks for a setup token that nobody has. The dashboard
// refuses the same file (web: locksEveryoneOut). In built-in mode without an
// administrator password (the first start), the start logged the setup token
// and the import is permitted. A file without an access section has built-in
// login.
var errLocksEveryoneOut = errors.New("Not imported. The file sets built-in login to on, but no administrator password is set. " +
	"First set an administrator password with syncwatch reset-admin-password. Or add access: {mode: proxy} to the file.")

// Imported is the result of ImportFile.
type Imported struct {
	Config  *config.Config
	Secrets int      // the secrets that the file had
	Removed []string // the names of the servers that the file removed, with their API keys and pinned certificates
}

// ImportFile replaces the stored configuration with a YAML file. A running
// server applies it within configCheckInterval and sends a settings notice.
// The command line asks for no confirmation: a person who can run it can
// also change the database. A file that locks everyone out is refused
// (errLocksEveryoneOut), as in the dashboard.
func ImportFile(st *store.Store, path string) (*Imported, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := config.ParseYAML(b)
	if err != nil {
		return nil, err
	}
	old, err := settings.Load(st)
	if err != nil {
		return nil, err
	}
	if old != nil && old.Access.Mode == config.AccessProxy && c.Access.Mode == config.AccessBuiltin {
		h, err := st.Setting(web.KeyAdminHash)
		if err != nil {
			return nil, err
		}
		if h == "" {
			return nil, errLocksEveryoneOut
		}
	}
	removed := removedServers(old, c)
	changes := importChanges(st, old, c)
	if line := RemovedLine(removed); line != "" {
		changes = append(changes, line)
	}
	n, err := settings.ImportSecrets(st, c)
	if err != nil {
		return nil, err
	}
	// Recorded before the revision changes: the server that runs sends the
	// notice to the channels of the configuration that it has, thus a
	// webhook that the file replaces also gets it.
	if len(changes) > 0 {
		if err := recordCLINotice(st, titleCLIImport, append([]string{"syncwatch import-config replaced the configuration."}, changes...)); err != nil {
			return nil, err
		}
	}
	if _, err := saveConfig(st, c); err != nil {
		return nil, err
	}
	return &Imported{Config: c, Secrets: n, Removed: removed}, settings.DropRemovedServers(st, old, c)
}

// removedServers returns the names of the servers of old that c does not
// have (settings.DropRemovedServers deletes their API keys and pinned
// certificates).
func removedServers(old, c *config.Config) []string {
	if old == nil {
		return nil
	}
	keep := map[string]bool{}
	for _, sv := range c.Servers {
		keep[sv.ID] = true
	}
	var out []string
	for _, sv := range old.Servers {
		if !keep[sv.ID] {
			out = append(out, sv.Name)
		}
	}
	return out
}

// RemovedLine words the servers that an import removed, for the settings
// notice and the output of import-config.
func RemovedLine(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return "The import removed the server " + names[0] + " and deleted its API key and pinned certificate."
	}
	return "The import removed the servers " + strings.Join(names, ", ") + " and deleted their API keys and pinned certificates."
}

// importChanges words what an import changes, for its settings notice: the
// sections of the Settings page and the secrets in the file. It runs before
// the secrets are stored, to tell a replaced secret from a new one.
func importChanges(st *store.Store, old, c *config.Config) []string {
	if old == nil {
		old = config.Default()
	}
	var out []string
	if s := config.ChangedSections(old, c); len(s) > 0 {
		out = append(out, "The import changed these sections of Settings: "+strings.Join(s, ", ")+".")
	}
	secret := func(had bool, what string) {
		if had {
			out = append(out, "The "+what+" is replaced.")
		} else {
			out = append(out, "The "+what+" is added.")
		}
	}
	for _, sv := range c.Servers {
		if sv.APIKey != "" {
			secret(st.HasSecret(store.ServerAPIKeySecret(sv.ID)), "API key of the server "+sv.Name)
		}
	}
	if c.Notify.DiscordWebhook != "" {
		secret(st.HasSecret(store.SecretDiscordWebhook), "Discord webhook")
	}
	if c.Notify.DiscordNoticesWebhook != "" {
		secret(st.HasSecret(store.SecretNoticesWebhook), "Discord webhook for settings notices")
	}
	if c.API.Token != "" {
		// The API token of the file replaces the value of the API token with
		// the name of the single API token of an older version, or adds it.
		secret(hasLegacyToken(st), `API token "`+store.LegacyTokenName+`"`)
	}
	return out
}

// hasLegacyToken reports whether an API token has the name of the single API
// token of an older version: the API token whose value an imported api.token
// replaces (settings.ImportSecrets uses the same lookup).
func hasLegacyToken(st *store.Store) bool {
	t, err := st.APITokenByName(store.LegacyTokenName)
	return err == nil && t != nil
}

// keyCLINotice is the prefix of the settings that keep a settings notice for
// a change made with the command line (import-config, reset-admin-password).
// The server that runs sends them in less than configCheckInterval and
// deletes them. A server that does not run sends them when it starts.
const keyCLINotice = "cli.notice."

// Titles of the settings notices for changes made with the command line.
const (
	titleCLIImport = "Configuration changed with the command line"
	titleCLIReset  = "Administrator password changed with the command line"
)

// cliNotice is a settings notice that a command recorded for the server.
type cliNotice struct {
	At      time.Time `json:"at"`
	Title   string    `json:"title"`
	Summary string    `json:"summary"`
}

// recordCLINotice stores a settings notice for the server. Each notice has a
// setting of its own, thus the notices of 2 commands at the same time stay.
func recordCLINotice(st *store.Store, title string, lines []string) error {
	now := time.Now()
	id := make([]byte, 4)
	_, _ = rand.Read(id)
	return st.SetJSONSetting(fmt.Sprintf("%s%019d-%x", keyCLINotice, now.UnixNano(), id), cliNotice{At: now, Title: title, Summary: strings.Join(lines, "\n")})
}

// RecordPasswordReset records a reset of the administrator password with the
// command line, for the settings notice that the server sends.
func RecordPasswordReset(st *store.Store) error {
	return recordCLINotice(st, titleCLIReset, []string{"The new administrator password is set. syncwatch logged out all sessions."})
}

// pinNotice words the settings notice for a certificate that the collector
// pinned without an administrator: at the first contact with the server, or
// a CA-signed certificate that replaced the pinned one for the same host
// name (renewal). The URL is the API URL of the configuration.
func pinNotice(cfg *config.Config, serverID, fingerprint string, renewal bool) notify.Notice {
	name, where := serverID, "the server"
	for _, sv := range cfg.Servers {
		if sv.ID == serverID {
			name, where = sv.Name, sv.URL
			break
		}
	}
	fp := stclient.ShortFP(fingerprint)
	if renewal {
		return notify.Notice{Title: "Server " + name + ": new CA-signed certificate", Security: true,
			Summary: "The certificate of " + where + " changed to a certificate from a certificate authority for the same host name. syncwatch pinned it: " + fp + "."}
	}
	return notify.Notice{Title: "Server " + name + ": certificate pinned", Security: true,
		Summary: "syncwatch pinned the certificate " + fp + " of " + where + " at the first contact."}
}

// pinNotices returns the function for collector.Manager.OnPin: a settings
// notice through send for each certificate that the collector pins.
func pinNotices(holder *config.Holder, send func(notify.Notice)) func(serverID, fingerprint string, renewal bool) {
	return func(serverID, fingerprint string, renewal bool) {
		send(pinNotice(holder.Get(), serverID, fingerprint, renewal))
	}
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
	// noticeLater captures the notification channels configured now and
	// returns a function that sends a settings notice to them
	// (notify.Notifier.NoticeLater).
	noticeLater func() func(notify.Notice)

	mu      sync.Mutex
	applied int64               // the revision the holder has
	notice  func(notify.Notice) // the channels of that configuration
}

// capture keeps the notification channels of the configuration that runs.
// A command writes a new webhook before the watcher sees the change, thus
// the channels are captured before the change, not when it is found.
func (w *configWatcher) capture() {
	if w.noticeLater != nil {
		w.notice = w.noticeLater()
	}
}

// channelsChanged captures the channels again after a change in this
// process (a new webhook in Settings).
func (w *configWatcher) channelsChanged() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.capture()
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
	if err := w.holder.Set(c); err != nil {
		return err
	}
	w.capture()
	return nil
}

// sendCLINotices sends the settings notices that commands recorded, and
// deletes them.
func (w *configWatcher) sendCLINotices() {
	pending, err := w.st.SettingsWithPrefix(keyCLINotice)
	if err != nil {
		w.log.Error("cannot read the settings notices of the command line", "err", err)
		return
	}
	for _, key := range slices.Sorted(maps.Keys(pending)) {
		var n cliNotice
		if err := json.Unmarshal([]byte(pending[key]), &n); err == nil && n.Title != "" {
			summary := n.Summary
			if time.Since(n.At) > time.Minute {
				// Sent at the next start, or a long time after the change.
				summary += "\nChanged at " + n.At.In(w.holder.Get().Location()).Format("2006-01-02 15:04") + "."
			}
			w.log.Warn("settings changed", "change", n.Title, "detail", summary, "source", "command line")
			if w.notice != nil {
				// The commands change the administrator password or import a
				// configuration: security notices (kept until a channel
				// accepts them).
				w.notice(notify.Notice{Title: n.Title, Summary: summary, Security: true})
			}
		}
		if err := w.st.DeleteSetting(key); err != nil {
			w.log.Error("cannot delete a settings notice of the command line", "err", err)
		}
	}
}

// check reloads the configuration if another process changed it, and sends
// the settings notices of the command line.
func (w *configWatcher) check() {
	w.mu.Lock()
	defer w.mu.Unlock()
	rev, err := w.st.ConfigRevision()
	// After the revision: a command records its notice before it saves, thus
	// the notice of a change found now goes to the channels that were
	// configured before the change.
	w.sendCLINotices()
	if err != nil || rev == w.applied {
		return
	}
	w.applied = rev // reported once, even when it can't be applied
	c, err := settings.Load(w.st)
	if err == nil && c == nil {
		err = errors.New("there is no stored configuration")
	}
	if err == nil {
		err = w.holder.Set(c)
	}
	if err != nil {
		w.log.Error("cannot use the configuration that another process changed", "revision", rev, "err", err)
		return
	}
	w.log.Info("configuration reloaded after a change by another process (syncwatch import-config)", "revision", rev, "servers", len(c.Servers))
	w.capture()
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
	started := time.Now()
	o.DataDir = absPath(o.DataDir)
	st, err := openServeStore(o.DataDir, o.SecretKey, log)
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
		return fmt.Errorf("the stored configuration is not valid: %w", err)
	}
	if o.AdminPassword != "" {
		if h, _ := st.Setting(web.KeyAdminHash); h == "" {
			if len([]rune(o.AdminPassword)) < web.MinPasswordLength {
				return fmt.Errorf("SYNCWATCH_ADMIN_PASSWORD must have at least %d characters", web.MinPasswordLength)
			}
			hash, err := web.HashPassword(o.AdminPassword)
			if err != nil {
				return err
			}
			if err := st.SetSetting(web.KeyAdminHash, hash); err != nil {
				return err
			}
			log.Info("administrator password set from SYNCWATCH_ADMIN_PASSWORD")
		}
	}

	hub := model.NewHub(nil)
	// A server keeps the device ID it had last time, so that one that can't
	// be reached at startup still counts as a server rather than as a PC.
	var known []*model.Server
	for _, s := range cfg.Servers {
		if id, _ := st.ServerDeviceID(s.ID); id != "" && !s.Disabled {
			known = append(known, &model.Server{ID: s.ID, Name: s.Name, URL: s.URL, GUIURL: s.LinkURL(), MyID: id})
		}
	}
	hub.SetServers(known)
	eng, err := engine.New(st, hub, holder, nil, log)
	if err != nil {
		return err
	}
	ntf := notify.New(eng, st, holder, log)
	eng.OnEvaluate(ntf.Wake)
	mgr := collector.NewManager(hub, st, holder, log)
	// A certificate that the collector pins without an administrator (first
	// contact, CA-signed renewal) gets a settings notice.
	mgr.OnPin = pinNotices(holder, ntf.Notice)
	holder.OnChange(func() {
		mgr.Sync()
		eng.Trigger()
	})
	// An import may bring a new webhook; retry delivery at once (as the UI's
	// import does).
	watcher := &configWatcher{st: st, holder: holder, log: log, onReload: ntf.ChannelsChanged, noticeLater: ntf.NoticeLater}
	if watcher.applied, err = st.ConfigRevision(); err != nil {
		return err
	}
	watcher.capture()
	// A new webhook in Settings: the watcher keeps the new channels for the
	// notices of the command line.
	channelsChanged := func() {
		ntf.ChannelsChanged()
		watcher.channelsChanged()
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
		SendNoticesTest: ntf.SendNoticesTest,
		Notice:          ntf.Notice,
		NoticeLater:     ntf.NoticeLater,
		ChannelsChanged: channelsChanged,
		Delivery:        ntf.Delivery,
	}, o.Version, log)
	if err != nil {
		return err
	}
	srvWeb.AllowHosts(o.Listen, o.TLSHosts...)
	srvWeb.TrustProxies(o.TrustedProxies)
	srvWeb.SetHealth(web.HealthOptions{Started: started, NotifyLimit: o.NotifyLimit, EvalLimit: evalLimit})
	srvWeb.LogSetupToken(os.Stderr)
	workerFuncs := []func(context.Context){mgr.Run, eng.Run, ntf.Run, watcher.run}
	deadmanAt := "off"
	if o.DeadmanURL != "" {
		dm, err := newDeadman(o.DeadmanURL, srvWeb.Ready, o.Version, log)
		if err != nil {
			return err
		}
		eng.OnEvaluate(dm.Wake)
		workerFuncs = append(workerFuncs, dm.run)
		deadmanAt = dm.target()
	}
	if o.onManager != nil {
		o.onManager(mgr)
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
	// The time when syncwatch did not run, before the first heartbeat of
	// this start replaces the last one. Only a start that listens counts: a
	// start that fails before this point (the TLS files, an address in use)
	// did not run. Docker starts such a container again and again, and a
	// heartbeat of each failed start would cut the gap.
	prev, err := noteGap(st, started, holder.Get().Location(), log)
	if err != nil {
		log.Warn("cannot record the time when syncwatch did not run", "err", err)
	}
	if err := st.SetAliveAt(started); err != nil {
		log.Warn("cannot record the heartbeat", "err", err)
	}
	// The heartbeat corrects the gap when the clock was behind at the start.
	clock := &clockCheck{st: st, log: log, loc: holder.Get().Location(), prev: prev, started: started}
	workerFuncs = append(workerFuncs, heartbeat(st, log, clock))
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
	for _, run := range workerFuncs {
		workers.Add(1)
		go func() { defer workers.Done(); run(ctx) }()
	}
	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	if o.OnListen != nil {
		o.OnListen(ln.Addr().String())
	}
	notifyLimit := "off"
	if o.NotifyLimit > 0 {
		notifyLimit = config.Duration(o.NotifyLimit).String()
	}
	log.Info("syncwatch started", "version", o.Version, "listen", ln.Addr().String(), "https", tc != nil, "data", o.DataDir, "servers", len(holder.Get().Servers),
		"trusted_proxies", proxyList(o.TrustedProxies), "health_notify_limit", notifyLimit, "deadman", deadmanAt)

	var serveErr error
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr = err
			log.Error("the web server stopped", "err", err)
		}
	}
	log.Info("syncwatch stops")
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
		log.Warn("some workers did not stop in time")
	}
	// A clean stop: the next start counts the gap from now.
	if err := st.SetAliveAt(time.Now()); err != nil {
		log.Warn("cannot record the heartbeat", "err", err)
	}
	return serveErr
}

// proxyList writes the trusted proxies for the start log line.
func proxyList(p []netip.Prefix) string {
	if len(p) == 0 {
		return "none"
	}
	out := make([]string, len(p))
	for i, x := range p {
		out[i] = x.String()
	}
	return strings.Join(out, ",")
}
