package collector

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"sync"

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

	mu      sync.Mutex
	ctx     context.Context
	running map[string]*running
	lastCol config.Collector
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

// Run starts collectors and keeps them in line with the configuration until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	m.mu.Lock()
	m.ctx = ctx
	m.mu.Unlock()
	m.Sync()
	<-ctx.Done()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.running {
		r.cancel()
		<-r.done
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
		hubServers = append(hubServers, &model.Server{ID: s.ID, Name: s.Name, URL: s.URL, GUIURL: s.LinkURL()})
	}
	m.hub.SetServers(hubServers)

	for id, r := range m.running {
		key, bound, _ := m.st.ServerKey(id)
		w, ok := want[id]
		if !ok || colChanged || w.URL != r.conf.URL || key != r.keyRev || !config.SameEndpoint(bound, w.URL) {
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
			m.log.Error("reading API key", "server", s.Name, "err", err)
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
			m.log.Warn("not connecting: the API key was entered for a different URL; enter it again", "server", s.Name, "url", s.URL)
			m.hub.Update(id, func(sv *model.Server) {
				sv.Status = model.StatusDown
				sv.LastError = "the API URL changed after the API key was entered; enter the API key again in Settings"
			})
			continue
		}
		c, err := newCollector(s, key, m.hub, m.st, m.cfg, m.log)
		if err != nil {
			m.log.Error("starting collector", "server", s.Name, "err", err)
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
		return errors.New("this server has no changed certificate to accept")
	}
	if current.New != fp {
		return errors.New("the certificate changed again; reload the page and check the new fingerprint")
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
	m.log.Info("pinned certificate forgotten", "server", serverID)
	return nil
}

// RescanNow asks every collector for a full re-check (used after settings changes).
func (m *Manager) RescanNow() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.running {
		r.c.FullCheckNow()
	}
}
