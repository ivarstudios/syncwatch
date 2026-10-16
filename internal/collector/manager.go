package collector

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"sync"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/store"
)

// Manager runs one collector per enabled server and restarts them when the
// configuration changes.
type Manager struct {
	hub *model.Hub
	st  *store.Store
	cfg *config.Holder
	log *slog.Logger

	// OnPin, when set, is called after the collector pinned the certificate
	// of a server without an administrator: at the first contact (renewal
	// false) or for a CA-signed renewal for the same host name (renewal true).
	// A certificate that the collector pinned in the last 24 hours gives no
	// call (2 front ends that alternate their certificates, see pinQuiet).
	// It runs in its own goroutine, after the pin is saved. Set it before Run.
	OnPin func(serverID, fingerprint string, renewal bool)

	mu      sync.Mutex
	ctx     context.Context
	running map[string]*running
	lastCol config.Collector
	keyErr  bool // the last Sync couldn't read some server's API key
}

type running struct {
	c      *Collector
	cancel context.CancelFunc
	done   chan struct{}
	conf   config.Server
	keyRev string
}

// NewManager creates a manager.
func NewManager(hub *model.Hub, st *store.Store, cfg *config.Holder, log *slog.Logger) *Manager {
	return &Manager{hub: hub, st: st, cfg: cfg, log: log, running: map[string]*running{}}
}

// keyRetry is how often Sync runs on its own while a server's API key can't
// be read (a database error, a secret key being restored), so monitoring
// resumes without a settings save.
var keyRetry = 5 * time.Minute

// Run starts collectors and keeps them in line with the configuration until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	m.Sync()
	t := time.NewTicker(keyRetry)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			m.mu.Lock()
			defer m.mu.Unlock()
			for _, r := range m.running {
				r.cancel()
				<-r.done
			}
			return
		case <-t.C:
			m.mu.Lock()
			retry := m.keyErr
			m.mu.Unlock()
			if retry {
				m.Sync()
			}
		}
	}
}

// Sync starts, stops and restarts collectors to match the configuration.
func (m *Manager) Sync() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ctx == nil {
		return
	}
	cfg := m.cfg.Get()
	colChanged := !reflect.DeepEqual(cfg.Collector, m.lastCol)
	m.lastCol = cfg.Collector

	var hubServers []*model.Server
	want := map[string]config.Server{}
	for _, s := range cfg.Servers {
		if s.Disabled {
			continue
		}
		want[s.ID] = s
		sv := &model.Server{ID: s.ID, Name: s.Name, URL: s.URL, GUIURL: s.LinkURL()}
		if !m.hub.View(s.ID, func(*model.Server) {}) {
			// New in the hub (a server without a known device ID at the
			// start, a new server, or one enabled again): the deleted
			// folders that the store keeps, before an evaluation sees the
			// server, so that their S8 findings stay open. The start of
			// the application gives them to the other servers.
			if d, err := m.st.DeletedDirs(s.ID); err != nil {
				m.log.Error("cannot read the deleted project folders", "server", s.Name, "err", err)
			} else if len(d) > 0 {
				sv.Deleted = d
			}
		}
		hubServers = append(hubServers, sv)
	}
	m.hub.SetServers(hubServers)

	m.keyErr = false
	for id, r := range m.running {
		w, ok := want[id]
		key, bound, err := m.st.ServerKey(id)
		// A key that can't be read now hasn't changed as far as anyone
		// knows; the running collector keeps the one it started with.
		keyChanged := err == nil && (key != r.keyRev || !config.SameEndpoint(bound, w.URL))
		if !ok || colChanged || w.URL != r.conf.URL || keyChanged {
			r.cancel()
			<-r.done
			delete(m.running, id)
		}
	}
	for id, s := range want {
		if _, ok := m.running[id]; ok {
			continue
		}
		key, bound, err := m.st.ServerKey(id)
		if err != nil {
			m.log.Error("cannot read the API key; trying again in "+keyRetry.String(), "server", s.Name, "err", err)
			m.keyErr = true
			m.hub.Update(id, func(sv *model.Server) {
				sv.Status = model.StatusDown
				sv.LastError = "cannot read the API key (check the secret key)"
			})
			continue
		}
		if key == "" {
			m.hub.Update(id, func(sv *model.Server) {
				sv.Status = model.StatusDown
				sv.LastError = "no API key configured"
			})
			continue
		}
		// A key is only ever sent to the URL it was entered for, so changing
		// a server's URL (in the settings or by an import) can't redirect it.
		if !config.SameEndpoint(bound, s.URL) {
			m.log.Warn("cannot connect, the API key is for a different API URL: type the API key again", "server", s.Name, "url", s.URL)
			m.hub.Update(id, func(sv *model.Server) {
				sv.Status = model.StatusDown
				sv.LastError = "the API URL changed: type the API key again in Settings → Servers"
			})
			continue
		}
		c, err := newCollector(s, key, m.hub, m.st, m.cfg, m.log, m.OnPin)
		if err != nil {
			m.log.Error("cannot start the collector", "server", s.Name, "err", err)
			m.hub.Update(id, func(sv *model.Server) {
				sv.Status = model.StatusDown
				sv.LastError = err.Error()
			})
			continue
		}
		ctx, cancel := context.WithCancel(m.ctx)
		r := &running{c: c, cancel: cancel, done: make(chan struct{}), conf: s, keyRev: key}
		m.running[id] = r
		go func() {
			defer close(r.done)
			c.Run(ctx)
		}()
	}
}

// AcceptCert pins the new certificate a server presented. fp must match the
// fingerprint shown to the admin, so a certificate that changed again in the
// meantime isn't accepted by accident.
func (m *Manager) AcceptCert(serverID, fp string) error {
	var current *model.CertChange
	m.hub.View(serverID, func(s *model.Server) {
		if s.CertChange != nil {
			cc := *s.CertChange
			current = &cc
		}
	})
	if current == nil {
		return errors.New("This server has no new certificate to accept.")
	}
	if current.New != fp {
		return errors.New("The certificate changed again. Reload the page. Then check the new fingerprint.")
	}
	if err := m.st.SetPin(serverID, fp); err != nil {
		return err
	}
	m.mu.Lock()
	r := m.running[serverID]
	m.mu.Unlock()
	if r != nil {
		r.c.client.SetPin(fp)
		r.c.Wake()
	}
	m.log.Info("new certificate accepted", "server", serverID)
	return nil
}

// ResetPin forgets a server's pinned certificate, so the next one it presents
// is trusted on first use. A running collector picks this up immediately.
func (m *Manager) ResetPin(serverID string) error {
	if err := m.st.SetPin(serverID, ""); err != nil {
		return err
	}
	m.mu.Lock()
	r := m.running[serverID]
	m.mu.Unlock()
	if r != nil {
		r.c.client.SetPin("")
		r.c.Wake()
	}
	m.log.Info("pinned certificate removed", "server", serverID)
	return nil
}

// ScanNow starts a structure scan of one server for an administrator (see
// Collector.ScanNow). A server without a running collector (disabled, no
// API key, a key for another URL) is not up.
func (m *Manager) ScanNow(serverID string) ScanStart {
	m.mu.Lock()
	r := m.running[serverID]
	m.mu.Unlock()
	if r == nil {
		return ScanStart{State: ScanNotUp}
	}
	return r.c.ScanNow()
}

// MeasureSize asks the gate of one server to measure the size of path, a
// project folder that is not in Syncthing, for an administrator (see
// Collector.MeasureNow).
func (m *Manager) MeasureSize(serverID, path string) error {
	m.mu.Lock()
	r := m.running[serverID]
	m.mu.Unlock()
	if r == nil {
		return ErrSizeNotUp
	}
	return r.c.MeasureNow(path)
}

// RescanNow asks every collector for a full re-check (used after settings changes).
func (m *Manager) RescanNow() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.running {
		r.c.FullCheckNow()
	}
}
