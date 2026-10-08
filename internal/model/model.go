// Package model holds the in-memory snapshot of every monitored Syncthing
// server and the Finding type produced by the rules.
package model

import (
	"maps"
	"slices"
	"sort"
	"strings"
	"time"
)

// ServerStatus is the monitor's view of whether a server can be reached.
type ServerStatus string

const (
	StatusUnknown     ServerStatus = "unknown" // not contacted yet
	StatusUp          ServerStatus = "up"
	StatusDown        ServerStatus = "down"
	StatusCertChanged ServerStatus = "cert-changed"
)

// Snapshot is a consistent copy of all server states.
type Snapshot struct {
	At      time.Time
	Servers []*Server // in configuration order
}

// Server returns the server with the given ID, or nil.
func (s *Snapshot) Server(id string) *Server {
	for _, sv := range s.Servers {
		if sv.ID == id {
			return sv
		}
	}
	return nil
}

// ServerByDeviceID returns the polled server with the given Syncthing device ID, or nil.
func (s *Snapshot) ServerByDeviceID(dev string) *Server {
	for _, sv := range s.Servers {
		if sv.MyID != "" && sv.MyID == dev {
			return sv
		}
	}
	return nil
}

// IsServerDevice reports whether a device ID belongs to a polled server.
func (s *Snapshot) IsServerDevice(dev string) bool { return s.ServerByDeviceID(dev) != nil }

// Server is everything the monitor knows about one Syncthing instance.
type Server struct {
	ID     string
	Name   string
	URL    string
	GUIURL string // where "Open in Syncthing" links point

	Status      ServerStatus
	LastError   string
	LastContact time.Time
	DownSince   time.Time
	Cert        CertInfo
	CertChange  *CertChange
	Mode        string // "events" or "polled"

	MyID      string
	Version   string
	OS        string
	StartTime time.Time
	PathSep   string

	Loaded            bool // at least one full re-check completed
	LastFullCheck     time.Time
	LastEvent         time.Time
	LastStructureScan time.Time

	Folders        map[string]*Folder     // by folder ID
	Devices        map[string]*Device     // configured devices, including itself
	Connections    map[string]*Connection // by device ID
	PendingDevices []PendingDevice
	PendingFolders []PendingFolder
	Conflicts      map[string]int64 // raw conflict counter per folder ID
	Structure      *Structure
	Rate           Rate // current transfer rate over all connections
}

// Rate is a server's transfer rate between its two latest traffic readings.
type Rate struct {
	In, Out float64 // bytes per second received and sent
	At      time.Time
}

// Overall sync states of a server, from all its folders, worst first.
const (
	SyncUnknown  = "unknown" // not reachable, or not read yet
	SyncError    = "error"
	SyncSyncing  = "syncing"
	SyncScanning = "scanning"
	SyncPaused   = "paused" // every folder is paused
	SyncUpToDate = "up to date"
)

// FolderSync is one folder's state for the overall status: SyncError,
// SyncSyncing, SyncScanning, SyncPaused or SyncUpToDate.
func (f *Folder) FolderSync() string {
	switch {
	case f.Paused:
		return SyncPaused
	case f.State == "error" || f.Error != "" || f.Errors > 0 || f.PullErrors > 0:
		return SyncError
	case f.State == "scanning" || f.State == "scan-waiting" || f.State == "starting":
		return SyncScanning
	case f.State == "idle" && f.NeedItems == 0:
		return SyncUpToDate
	}
	return SyncSyncing
}

// SyncStatus is the overall sync state of a server: the worst of its
// folders (error, syncing, scanning, up to date), "paused" when every
// folder is paused, "unknown" while it can't be reached.
func (s *Server) SyncStatus() string {
	if s.Status != StatusUp || !s.Loaded {
		return SyncUnknown
	}
	rank := map[string]int{SyncUpToDate: 0, SyncScanning: 1, SyncSyncing: 2, SyncError: 3}
	worst, active := SyncUpToDate, 0
	for _, f := range s.Folders {
		st := f.FolderSync()
		if st == SyncPaused {
			continue
		}
		active++
		if rank[st] > rank[worst] {
			worst = st
		}
	}
	if active == 0 && len(s.Folders) > 0 {
		return SyncPaused
	}
	return worst
}

// CertInfo is the certificate the server presented last.
type CertInfo struct {
	Fingerprint string
	NotAfter    time.Time
	CASigned    bool
}

// CertChange describes a changed self-signed certificate waiting for acceptance.
type CertChange struct {
	Old, New string
	NotAfter time.Time
	SeenAt   time.Time
}

// Folder is one Syncthing folder on one server.
type Folder struct {
	ID         string
	Label      string
	Path       string
	Type       string
	Paused     bool
	SharedWith []string // device IDs, excluding the server itself

	State        string
	StateChanged time.Time
	Error        string
	WatchError   string
	Errors       int
	PullErrors   int
	FileErrors   []FileError // up to 5

	GlobalBytes, GlobalItems int64
	NeedBytes, NeedItems     int64
	LastScan                 time.Time
	LastFileAt               time.Time
	LastFileName             string

	Completion map[string]*Completion // by remote device ID
}

// DisplayName is the label, or the ID when the label is empty.
func (f *Folder) DisplayName() string {
	if f.Label != "" {
		return f.Label
	}
	return f.ID
}

// LocalPercent is how complete the server's own copy is.
func (f *Folder) LocalPercent() float64 {
	if f.GlobalBytes <= 0 || f.NeedBytes <= 0 {
		if f.NeedItems > 0 {
			return 99.9
		}
		return 100
	}
	p := 100 * float64(f.GlobalBytes-f.NeedBytes) / float64(f.GlobalBytes)
	if p < 0 {
		return 0
	}
	return p
}

// SharedWithDevice reports whether the folder is shared with a device.
func (f *Folder) SharedWithDevice(dev string) bool { return slices.Contains(f.SharedWith, dev) }

// FileError is one failing file.
type FileError struct {
	Path  string
	Error string
}

// Completion is a remote device's progress on a folder, as reported by a server.
type Completion struct {
	Percent     float64
	NeedBytes   int64
	NeedItems   int64
	RemoteState string // valid, paused, notSharing, unknown
	UpdatedAt   time.Time
}

// Device is a device configured on a server.
type Device struct {
	ID        string
	Name      string
	Paused    bool
	Addresses []string
	LastSeen  time.Time // zero if never seen
}

// Connection is a server's connection to a device.
type Connection struct {
	Connected     bool
	Paused        bool
	Address       string
	Type          string
	ClientVersion string
	StartedAt     time.Time
}

// IsRelay reports whether the connection goes through a relay.
func (c *Connection) IsRelay() bool { return strings.Contains(strings.ToLower(c.Type), "relay") }

// PendingDevice is a device asking to connect.
type PendingDevice struct {
	DeviceID string
	Name     string
	Address  string
	Time     time.Time
}

// PendingFolder is a folder offered by a device.
type PendingFolder struct {
	FolderID  string
	Label     string
	OfferedBy string
	Time      time.Time
}

// Structure is the result of the structure scan of a server's shares.
type Structure struct {
	ScannedAt  time.Time
	BaselineAt time.Time
	Shares     []*Share
	Nested     map[string]*NestedDir // by full path
	// What the last scan cost the server: directory listings and time.
	ScanListings int
	ScanTook     time.Duration
	ScanBaseline bool
}

// Share is a directory whose children are project folders.
type Share struct {
	Path      string
	Name      string
	Attrs     map[string]string
	Manual    bool // added by configuration rather than detected
	Dirs      []*Dir
	Error     string
	ScannedAt time.Time
}

// Dir is a top-level directory in a share.
type Dir struct {
	Name      string
	Path      string
	FolderIDs []string // Syncthing folders at, above or below this path
}

// NestedDir is a directory matching the project pattern inside a project folder.
type NestedDir struct {
	Path     string
	Parent   string // the top-level project folder containing it
	Share    string
	FolderID string // set when it lies inside a Syncthing folder
	Source   string // "scan" or "event"
	FoundAt  time.Time
}

// Clone returns a deep copy.
func (s *Snapshot) Clone() *Snapshot {
	out := &Snapshot{At: s.At, Servers: make([]*Server, len(s.Servers))}
	for i, sv := range s.Servers {
		out.Servers[i] = sv.Clone()
	}
	return out
}

// Clone returns a deep copy.
func (s *Server) Clone() *Server {
	if s == nil {
		return nil
	}
	c := *s
	if s.CertChange != nil {
		cc := *s.CertChange
		c.CertChange = &cc
	}
	c.Folders = make(map[string]*Folder, len(s.Folders))
	for k, f := range s.Folders {
		c.Folders[k] = f.Clone()
	}
	c.Devices = make(map[string]*Device, len(s.Devices))
	for k, d := range s.Devices {
		dc := *d
		dc.Addresses = slices.Clone(d.Addresses)
		c.Devices[k] = &dc
	}
	c.Connections = make(map[string]*Connection, len(s.Connections))
	for k, cn := range s.Connections {
		cc := *cn
		c.Connections[k] = &cc
	}
	c.PendingDevices = slices.Clone(s.PendingDevices)
	c.PendingFolders = slices.Clone(s.PendingFolders)
	c.Conflicts = maps.Clone(s.Conflicts)
	c.Structure = s.Structure.Clone()
	return &c
}

// Clone returns a deep copy.
func (f *Folder) Clone() *Folder {
	c := *f
	c.SharedWith = slices.Clone(f.SharedWith)
	c.FileErrors = slices.Clone(f.FileErrors)
	c.Completion = make(map[string]*Completion, len(f.Completion))
	for k, v := range f.Completion {
		vc := *v
		c.Completion[k] = &vc
	}
	return &c
}

// Clone returns a deep copy.
func (st *Structure) Clone() *Structure {
	if st == nil {
		return nil
	}
	c := *st
	c.Shares = make([]*Share, len(st.Shares))
	for i, sh := range st.Shares {
		shc := *sh
		shc.Attrs = maps.Clone(sh.Attrs)
		shc.Dirs = make([]*Dir, len(sh.Dirs))
		for j, d := range sh.Dirs {
			dc := *d
			dc.FolderIDs = slices.Clone(d.FolderIDs)
			shc.Dirs[j] = &dc
		}
		c.Shares[i] = &shc
	}
	c.Nested = make(map[string]*NestedDir, len(st.Nested))
	for k, n := range st.Nested {
		nc := *n
		c.Nested[k] = &nc
	}
	return &c
}

// DeviceView is one device unified across all servers.
type DeviceView struct {
	ID        string
	Name      string
	IsServer  bool
	ServerID  string    // set when IsServer
	LastSeen  time.Time // latest across servers
	Connected bool      // connected to at least one reachable server
	Version   string    // client version from a connection, or the server version
	Relay     bool
	SeenBy    []string // server IDs that have the device configured
}

// Devices unifies devices by ID across all servers. Servers come first, in
// configuration order, then PCs sorted by name.
func (s *Snapshot) Devices() []*DeviceView {
	byID := map[string]*DeviceView{}
	var order []string
	add := func(id string) *DeviceView {
		if dv, ok := byID[id]; ok {
			return dv
		}
		dv := &DeviceView{ID: id}
		byID[id] = dv
		order = append(order, id)
		return dv
	}
	for _, sv := range s.Servers {
		if sv.MyID == "" {
			continue
		}
		dv := add(sv.MyID)
		dv.IsServer, dv.ServerID, dv.Name, dv.Version = true, sv.ID, sv.Name, sv.Version
		if sv.Status == StatusUp {
			dv.Connected = true
			dv.LastSeen = sv.LastContact
		}
	}
	for _, sv := range s.Servers {
		for id, d := range sv.Devices {
			if id == sv.MyID {
				continue
			}
			dv := add(id)
			if !slices.Contains(dv.SeenBy, sv.ID) {
				dv.SeenBy = append(dv.SeenBy, sv.ID)
			}
			if dv.Name == "" {
				dv.Name = d.Name
			}
			if d.LastSeen.After(dv.LastSeen) && !dv.IsServer {
				dv.LastSeen = d.LastSeen
			}
			if cn, ok := sv.Connections[id]; ok && cn.Connected && sv.Status == StatusUp {
				if !dv.IsServer {
					dv.Connected = true
					if cn.StartedAt.After(dv.LastSeen) || dv.LastSeen.IsZero() {
						dv.LastSeen = s.At
					}
				}
				if dv.Version == "" {
					dv.Version = cn.ClientVersion
				}
				if cn.IsRelay() {
					dv.Relay = true
				}
			}
		}
	}
	out := make([]*DeviceView, 0, len(order))
	var pcs []*DeviceView
	for _, id := range order {
		dv := byID[id]
		if dv.Name == "" {
			dv.Name = ShortID(id)
		}
		if dv.IsServer {
			out = append(out, dv)
		} else {
			pcs = append(pcs, dv)
		}
	}
	sort.SliceStable(pcs, func(i, j int) bool { return strings.ToLower(pcs[i].Name) < strings.ToLower(pcs[j].Name) })
	return append(out, pcs...)
}

// ShortID returns the first block of a device ID.
func ShortID(id string) string {
	if i := strings.IndexByte(id, '-'); i > 0 {
		return id[:i]
	}
	if len(id) > 7 {
		return id[:7]
	}
	return id
}

// DeviceName returns the name a server has configured for a device, falling back to the short ID.
func (s *Snapshot) DeviceName(id string) string {
	if sv := s.ServerByDeviceID(id); sv != nil {
		return sv.Name
	}
	for _, sv := range s.Servers {
		if d, ok := sv.Devices[id]; ok && d.Name != "" {
			return d.Name
		}
	}
	return ShortID(id)
}
