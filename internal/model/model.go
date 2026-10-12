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
	// Disabled are the servers that are disabled in the configuration. The
	// collector does not poll them, but their devices are servers, not PCs.
	Disabled []DisabledServer
	// Links are the connections between the polled servers that have each
	// other configured, in configuration order; nil until the engine sets them.
	Links []ServerLink
	// SiteLinks are the connections between the sites, when 2 or more sites
	// are set.
	SiteLinks []SiteLink
}

// States of a link between 2 servers or 2 sites.
const (
	LinkConnected    = "connected"    // the 2 servers are connected to each other
	LinkDisconnected = "disconnected" // both servers are up and loaded, and not connected to each other
	LinkUnreachable  = "unreachable"  // a server of the pair is not up or not loaded: syncwatch cannot tell
)

// ServerLink is the connection between 2 polled servers.
type ServerLink struct {
	A, B  string    // server IDs, A before B in configuration order
	State string    // LinkConnected, LinkDisconnected or LinkUnreachable
	Since time.Time // connected: the start of the connection; disconnected: the last time they saw each other; zero when not known
}

// SiteLink is the connection between 2 sites: the server links between them.
type SiteLink struct {
	A, B    string      // site names, A before B in the order of their first server in the configuration
	State   string      // LinkConnected when 1 server link is connected; else LinkUnreachable when 1 is unreachable; else LinkDisconnected
	Since   time.Time   // connected: the earliest Since of the connected links; disconnected: the latest Since of the links; else zero
	Servers [][2]string // the server links (A, B server IDs) between the 2 sites
}

// DisabledServer is a server that is disabled in the configuration, with
// the device ID it had when syncwatch polled it last ("" if never).
type DisabledServer struct {
	ID, Name, MyID string
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

// DisabledByDeviceID returns the disabled server with the given Syncthing device ID, or nil.
func (s *Snapshot) DisabledByDeviceID(dev string) *DisabledServer {
	for i := range s.Disabled {
		if s.Disabled[i].MyID != "" && s.Disabled[i].MyID == dev {
			return &s.Disabled[i]
		}
	}
	return nil
}

// IsServerDevice reports whether a device ID belongs to a polled server or
// to a server that is disabled in the configuration.
func (s *Snapshot) IsServerDevice(dev string) bool {
	return s.ServerByDeviceID(dev) != nil || s.DisabledByDeviceID(dev) != nil
}

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
	UpSince     time.Time // when the status last became up (after the start of syncwatch or a failure); set by the hub
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
	LastStructureScan time.Time // the end of the last completed structure scan; zero before the first one

	Folders        map[string]*Folder     // by folder ID
	Devices        map[string]*Device     // configured devices, including itself
	Connections    map[string]*Connection // by device ID
	PendingDevices []PendingDevice
	PendingFolders []PendingFolder
	Conflicts      map[string]int64 // raw conflict counter per folder ID
	Structure      *Structure
	Rate           Rate // current transfer rate over all connections

	RemoteStates    map[string]map[string]string     // last known remoteState by folder ID, then device ID; see RemoteState
	LastCompletions map[string]map[string]Completion // last completion of a connected, valid device by folder ID, then device ID; see LastCompletion

	NeedLists      map[string]map[string]NeedList         // the files that a device still needs, by folder ID, then device ID; replaced at each full check
	Progress       map[string]map[string][]ProgressSample // by folder ID, then device ID ("" for the server's own copy), oldest first; see AddProgress
	RelayListeners []RelayListener                        // the relay listeners of the server (connectionServiceStatus)
}

// NeedList is a part of the files that a device still needs for a folder.
type NeedList struct {
	Names []string  // at most 20 file names (paths in the folder) that the device still needs
	Items int64     // the device's need items when the list was read
	At    time.Time // when the list was read
}

// ProgressSample is the progress of a folder for a device at one time.
type ProgressSample struct {
	At          time.Time
	NeedBytes   int64
	GlobalBytes int64
}

// Limits of the progress samples of one folder and device.
const (
	progressSpacing = time.Minute   // the shortest time between a sample and the sample after the next one
	progressWindow  = 2 * time.Hour // samples older than this before the newest one go
	maxProgress     = 120
)

// AddProgress records a progress sample of a folder for a device ("" for the
// server's own copy). The newest sample always has the newest need, thus
// the end of a change is not lost (W-059: a PC that received its files in
// 10 seconds and then stopped): a sample less than 1 minute after the
// sample before the newest one replaces the newest one. A sample with the
// need of the newest one does not replace it: the newest sample keeps the
// time when its need began. Thus a minute has at most 2 samples. A sample
// that is not after the newest one is not kept. Samples more than 2 hours
// older than p go, and at most 120 samples stay.
func (s *Server) AddProgress(folderID, deviceID string, p ProgressSample) {
	samples := s.Progress[folderID][deviceID]
	n := len(samples)
	switch {
	case n > 0 && !p.At.After(samples[n-1].At):
		return
	case n > 1 && p.At.Sub(samples[n-2].At) < progressSpacing:
		if p.NeedBytes != samples[n-1].NeedBytes {
			samples[n-1] = p
		}
		return
	}
	cut := p.At.Add(-progressWindow)
	i := 0
	for i < len(samples) && samples[i].At.Before(cut) {
		i++
	}
	samples = append(samples[i:], p)
	if len(samples) > maxProgress {
		samples = samples[len(samples)-maxProgress:]
	}
	if s.Progress == nil {
		s.Progress = map[string]map[string][]ProgressSample{}
	}
	if s.Progress[folderID] == nil {
		s.Progress[folderID] = map[string][]ProgressSample{}
	}
	s.Progress[folderID][deviceID] = samples
}

// RelayListener is the state of one relay listener of a server.
type RelayListener struct {
	URI       string    // the listen address without its query: relay://10.115.1.60:22067 or dynamic+https://relays.syncthing.net/endpoint
	Connected bool      // Syncthing reports a WAN address and no error for it
	Error     string    // Syncthing's error text, "" when none
	Since     time.Time // when Connected last changed (the first sample when not known)
}

// Rate is a server's transfer rate between its two latest traffic readings.
type Rate struct {
	In, Out float64 // bytes per second received and sent
	At      time.Time
}

// Sync states of a folder, and of a server from all its folders, worst first.
const (
	SyncUnknown    = "unknown" // not reachable, or not read yet
	SyncError      = "error"
	SyncWatchError = "watch error" // the file watcher failed: a warning, the folder still syncs on rescans
	SyncSyncing    = "syncing"
	SyncScanning   = "scanning"
	SyncPaused     = "paused" // every folder is paused
	SyncUpToDate   = "up to date"
)

// FolderSync is one folder's state: SyncPaused, SyncError, SyncWatchError,
// SyncSyncing, SyncScanning or SyncUpToDate. The grid, the overview tile
// and both APIs count folders by it, so they always agree.
func (f *Folder) FolderSync() string {
	switch {
	case f.Paused:
		return SyncPaused
	case f.State == "error" || f.Error != "" || f.Errors > 0 || f.PullErrors > 0:
		return SyncError
	case f.WatchError != "":
		return SyncWatchError
	case f.State == "scanning" || f.State == "scan-waiting" || f.State == "starting":
		return SyncScanning
	case f.State == "idle" && f.NeedItems == 0:
		return SyncUpToDate
	}
	return SyncSyncing
}

// SyncStatus is the overall sync state of a server: the worst of its
// folders (error, watch error, syncing, scanning, up to date), "paused"
// when every folder is paused, "unknown" while it can't be reached.
func (s *Server) SyncStatus() string {
	if s.Status != StatusUp || !s.Loaded {
		return SyncUnknown
	}
	rank := map[string]int{SyncUpToDate: 0, SyncScanning: 1, SyncSyncing: 2, SyncWatchError: 3, SyncError: 4}
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

// knownRemoteState reports whether a remoteState says anything about the
// device: Syncthing answers "unknown" for every disconnected device.
func knownRemoteState(s string) bool { return s != "" && s != "unknown" }

// RemoteState is a device's state for a folder (valid, paused or notSharing):
// the one reported now, or the last known one while Syncthing reports
// "unknown", which it does as soon as the device disconnects. The checks that
// wait for a paused or unshared folder (H6, C3) would otherwise start over at
// every disconnect and never open for a PC that is off every night.
func (s *Server) RemoteState(folderID, dev string) string {
	if f := s.Folders[folderID]; f != nil {
		if c := f.Completion[dev]; c != nil && knownRemoteState(c.RemoteState) {
			return c.RemoteState
		}
	}
	return s.RemoteStates[folderID][dev]
}

// LastCompletion is a device's completion of a folder as the server reported
// it last while the device was connected and its remote state was valid.
// Syncthing reports "unknown" as soon as the device disconnects, and the
// stuck sync check (H5) keeps the view of the last connection while the
// device is away instead of starting again at every disconnect.
func (s *Server) LastCompletion(folderID, dev string) (Completion, bool) {
	c, ok := s.LastCompletions[folderID][dev]
	return c, ok
}

// RememberRemoteStates records the remote states known now for RemoteState
// and the completion of connected devices with a valid state for
// LastCompletion, and forgets both for folders and devices that are no
// longer shared. The hub calls it after every update.
func (s *Server) RememberRemoteStates() {
	for id, f := range s.Folders {
		for dev, c := range f.Completion {
			if !knownRemoteState(c.RemoteState) || !f.SharedWithDevice(dev) {
				continue
			}
			if s.RemoteStates == nil {
				s.RemoteStates = map[string]map[string]string{}
			}
			if s.RemoteStates[id] == nil {
				s.RemoteStates[id] = map[string]string{}
			}
			s.RemoteStates[id][dev] = c.RemoteState
			if cn := s.Connections[dev]; c.RemoteState != "valid" || cn == nil || !cn.Connected {
				continue
			}
			if s.LastCompletions == nil {
				s.LastCompletions = map[string]map[string]Completion{}
			}
			if s.LastCompletions[id] == nil {
				s.LastCompletions[id] = map[string]Completion{}
			}
			s.LastCompletions[id][dev] = *c
		}
	}
	forgetUnshared(s.RemoteStates, s.Folders)
	forgetUnshared(s.LastCompletions, s.Folders)
}

// forgetUnshared deletes the entries of folders and devices that are no
// longer shared from a map by folder ID, then device ID.
func forgetUnshared[V any](m map[string]map[string]V, folders map[string]*Folder) {
	for id, devs := range m {
		f := folders[id]
		for dev := range devs {
			if f == nil || !f.SharedWithDevice(dev) {
				delete(devs, dev)
			}
		}
		if len(devs) == 0 {
			delete(m, id)
		}
	}
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

	LastLocalChange time.Time // newest LocalChangeDetected seen by the collector
	FirstSeen       time.Time // when syncwatch saw the folder on this server for the first time (store folder_activity)
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
	GlobalBytes int64  // the size of the folder for the device
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
	out := &Snapshot{At: s.At, Servers: make([]*Server, len(s.Servers)), Disabled: slices.Clone(s.Disabled), Links: slices.Clone(s.Links), SiteLinks: slices.Clone(s.SiteLinks)}
	for i, sv := range s.Servers {
		out.Servers[i] = sv.Clone()
	}
	for i := range out.SiteLinks {
		out.SiteLinks[i].Servers = slices.Clone(s.SiteLinks[i].Servers)
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
	c.RemoteStates = make(map[string]map[string]string, len(s.RemoteStates))
	for k, m := range s.RemoteStates {
		c.RemoteStates[k] = maps.Clone(m)
	}
	c.LastCompletions = make(map[string]map[string]Completion, len(s.LastCompletions))
	for k, m := range s.LastCompletions {
		c.LastCompletions[k] = maps.Clone(m)
	}
	c.NeedLists = make(map[string]map[string]NeedList, len(s.NeedLists))
	for k, m := range s.NeedLists {
		lists := make(map[string]NeedList, len(m))
		for dev, nl := range m {
			nl.Names = slices.Clone(nl.Names)
			lists[dev] = nl
		}
		c.NeedLists[k] = lists
	}
	c.Progress = make(map[string]map[string][]ProgressSample, len(s.Progress))
	for k, m := range s.Progress {
		samples := make(map[string][]ProgressSample, len(m))
		for dev, ps := range m {
			samples[dev] = slices.Clone(ps)
		}
		c.Progress[k] = samples
	}
	c.RelayListeners = slices.Clone(s.RelayListeners)
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
	Disabled  bool      // a server that is disabled in the configuration (IsServer is set)
	ServerID  string    // set when IsServer
	LastSeen  time.Time // latest across servers
	Connected bool      // connected to at least one reachable server
	Version   string    // client version from a connection, or the server version
	Relay     bool
	SeenBy    []string // server IDs that have the device configured
}

// Devices unifies devices by ID across all servers. Servers come first, in
// configuration order, then the disabled servers, then PCs sorted by name. A
// disabled server is not polled: like a PC, it is connected when a polled
// server has a connection to it.
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
	for _, ds := range s.Disabled {
		if ds.MyID == "" || byID[ds.MyID] != nil {
			continue
		}
		dv := add(ds.MyID)
		dv.IsServer, dv.Disabled, dv.ServerID, dv.Name = true, true, ds.ID, ds.Name
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
			polled := dv.IsServer && !dv.Disabled
			if d.LastSeen.After(dv.LastSeen) && !polled {
				dv.LastSeen = d.LastSeen
			}
			if cn, ok := sv.Connections[id]; ok && cn.Connected && sv.Status == StatusUp {
				if !polled {
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
	if ds := s.DisabledByDeviceID(id); ds != nil {
		return ds.Name
	}
	for _, sv := range s.Servers {
		if d, ok := sv.Devices[id]; ok && d.Name != "" {
			return d.Name
		}
	}
	return ShortID(id)
}
