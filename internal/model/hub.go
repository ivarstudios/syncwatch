package model

import (
	"sync"
	"time"
)

// Hub holds the live state of every server. Collectors update their own
// server through Update; readers take consistent copies with Snapshot.
type Hub struct {
	mu      sync.RWMutex
	order   []string
	servers map[string]*Server
	changed chan struct{}
	now     func() time.Time
}

// NewHub creates an empty hub.
func NewHub(now func() time.Time) *Hub {
	if now == nil {
		now = time.Now
	}
	return &Hub{servers: map[string]*Server{}, changed: make(chan struct{}, 1), now: now}
}

// SetServers replaces the list of servers, keeping state for IDs that remain.
func (h *Hub) SetServers(list []*Server) {
	h.mu.Lock()
	defer h.mu.Unlock()
	next := map[string]*Server{}
	h.order = h.order[:0]
	for _, s := range list {
		if old, ok := h.servers[s.ID]; ok {
			old.Name, old.URL, old.GUIURL = s.Name, s.URL, s.GUIURL
			next[s.ID] = old
		} else {
			if s.Status == "" {
				s.Status = StatusUnknown
			}
			ensureMaps(s)
			next[s.ID] = s
		}
		h.order = append(h.order, s.ID)
	}
	h.servers = next
	h.signal()
}

func ensureMaps(s *Server) {
	if s.Folders == nil {
		s.Folders = map[string]*Folder{}
	}
	if s.Devices == nil {
		s.Devices = map[string]*Device{}
	}
	if s.Connections == nil {
		s.Connections = map[string]*Connection{}
	}
	if s.Conflicts == nil {
		s.Conflicts = map[string]int64{}
	}
}

// Update runs fn on a server's state under the lock and signals a change.
// It returns false if the server is unknown.
func (h *Hub) Update(id string, fn func(*Server)) bool {
	h.mu.Lock()
	s, ok := h.servers[id]
	if ok {
		fn(s)
		ensureMaps(s)
		h.signal()
	}
	h.mu.Unlock()
	return ok
}

// View runs fn on a server's state under a read lock.
func (h *Hub) View(id string, fn func(*Server)) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	s, ok := h.servers[id]
	if ok {
		fn(s)
	}
	return ok
}

// Snapshot returns a deep copy of all servers.
func (h *Hub) Snapshot() *Snapshot {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := &Snapshot{At: h.now()}
	for _, id := range h.order {
		out.Servers = append(out.Servers, h.servers[id].Clone())
	}
	return out
}

// Changed is signalled (coalesced) whenever any server state changes.
func (h *Hub) Changed() <-chan struct{} { return h.changed }

func (h *Hub) signal() {
	select {
	case h.changed <- struct{}{}:
	default:
	}
}
