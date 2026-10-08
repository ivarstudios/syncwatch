package config

import (
	"sync"
	"sync/atomic"
)

type holderVal struct {
	cfg *Config
	cs  *CompiledStructure
}

// Holder gives concurrent access to the current configuration.
type Holder struct {
	v    atomic.Pointer[holderVal]
	mu   sync.Mutex
	subs []func()
}

// NewHolder validates c and returns a holder for it.
func NewHolder(c *Config) (*Holder, error) {
	h := &Holder{}
	if err := h.store(c); err != nil {
		return nil, err
	}
	return h, nil
}

func (h *Holder) store(c *Config) error {
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		return err
	}
	cs, err := c.Structure.Compile()
	if err != nil {
		return err
	}
	h.v.Store(&holderVal{cfg: c, cs: cs})
	return nil
}

// Get returns the current configuration. Callers must not modify it; use
// Clone and Set to change it.
func (h *Holder) Get() *Config { return h.v.Load().cfg }

// Structure returns the compiled structure rules.
func (h *Holder) Structure() *CompiledStructure { return h.v.Load().cs }

// Set validates and installs a new configuration, then notifies subscribers.
func (h *Holder) Set(c *Config) error {
	if err := h.store(c); err != nil {
		return err
	}
	h.mu.Lock()
	subs := append([]func(){}, h.subs...)
	h.mu.Unlock()
	for _, fn := range subs {
		fn()
	}
	return nil
}

// OnChange registers fn to run after every Set.
func (h *Holder) OnChange(fn func()) {
	h.mu.Lock()
	h.subs = append(h.subs, fn)
	h.mu.Unlock()
}
