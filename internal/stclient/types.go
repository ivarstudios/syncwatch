package stclient

import (
	"encoding/json"
	"time"
)

// The structs below decode only the fields syncwatch uses. Anything else in a
// response, such as folder encryption passwords, is dropped by the decoder.

// SystemStatus is the subset of /rest/system/status that syncwatch uses.
type SystemStatus struct {
	MyID          string    `json:"myID"`
	StartTime     time.Time `json:"startTime"`
	Uptime        int64     `json:"uptime"`
	PathSeparator string    `json:"pathSeparator"`
}

// SystemVersion is /rest/system/version.
type SystemVersion struct {
	Version     string `json:"version"`
	LongVersion string `json:"longVersion"`
	OS          string `json:"os"`
	Arch        string `json:"arch"`
	Container   bool   `json:"container"`
}

// Connection is one entry of /rest/system/connections.
type Connection struct {
	Connected     bool      `json:"connected"`
	Paused        bool      `json:"paused"`
	Address       string    `json:"address"`
	Type          string    `json:"type"`
	ClientVersion string    `json:"clientVersion"`
	IsLocal       bool      `json:"isLocal"`
	StartedAt     time.Time `json:"startedAt"`
}

// Connections is /rest/system/connections.
type Connections struct {
	Connections map[string]Connection `json:"connections"`
	Total       Traffic               `json:"total"`
}

// Traffic is the total of /rest/system/connections: bytes received and sent
// over all connections since Syncthing started.
type Traffic struct {
	At            time.Time `json:"at"`
	InBytesTotal  int64     `json:"inBytesTotal"`
	OutBytesTotal int64     `json:"outBytesTotal"`
}

// DeviceStat is one entry of /rest/stats/device.
type DeviceStat struct {
	LastSeen                time.Time `json:"lastSeen"`
	LastConnectionDurationS float64   `json:"lastConnectionDurationS"`
}

// FolderStat is one entry of /rest/stats/folder.
type FolderStat struct {
	LastFile struct {
		At       time.Time `json:"at"`
		Filename string    `json:"filename"`
		Deleted  bool      `json:"deleted"`
	} `json:"lastFile"`
	LastScan time.Time `json:"lastScan"`
}

// FolderDevice is a device a folder is shared with. The encryption password
// that Syncthing returns alongside it is deliberately not decoded.
type FolderDevice struct {
	DeviceID string `json:"deviceID"`
}

// FolderConfig is one entry of /rest/config/folders.
type FolderConfig struct {
	ID             string         `json:"id"`
	Label          string         `json:"label"`
	Path           string         `json:"path"`
	Type           string         `json:"type"`
	FilesystemType string         `json:"filesystemType"`
	Paused         bool           `json:"paused"`
	MarkerName     string         `json:"markerName"`
	Devices        []FolderDevice `json:"devices"`
}

// DeviceConfig is one entry of /rest/config/devices.
type DeviceConfig struct {
	DeviceID   string   `json:"deviceID"`
	Name       string   `json:"name"`
	Addresses  []string `json:"addresses"`
	Paused     bool     `json:"paused"`
	Introducer bool     `json:"introducer"`
}

// DBStatus is /rest/db/status for one folder.
type DBStatus struct {
	State            string    `json:"state"`
	StateChanged     time.Time `json:"stateChanged"`
	Error            string    `json:"error"`
	WatchError       string    `json:"watchError"`
	Invalid          string    `json:"invalid"`
	Errors           int       `json:"errors"`
	PullErrors       int       `json:"pullErrors"`
	GlobalBytes      int64     `json:"globalBytes"`
	GlobalTotalItems int64     `json:"globalTotalItems"`
	LocalBytes       int64     `json:"localBytes"`
	NeedBytes        int64     `json:"needBytes"`
	NeedTotalItems   int64     `json:"needTotalItems"`
	InSyncBytes      int64     `json:"inSyncBytes"`
	ReceiveOnlyItems int64     `json:"receiveOnlyTotalItems"`
	Sequence         int64     `json:"sequence"`
}

// FileError is one failing file in a folder.
type FileError struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// FolderErrors is /rest/folder/errors.
type FolderErrors struct {
	Folder string      `json:"folder"`
	Errors []FileError `json:"errors"`
}

// Completion is /rest/db/completion for one (folder, device) pair.
type Completion struct {
	Completion  float64 `json:"completion"`
	GlobalBytes int64   `json:"globalBytes"`
	GlobalItems int64   `json:"globalItems"`
	NeedBytes   int64   `json:"needBytes"`
	NeedItems   int64   `json:"needItems"`
	NeedDeletes int64   `json:"needDeletes"`
	RemoteState string  `json:"remoteState"`
	Sequence    int64   `json:"sequence"`
}

// PendingDevice is one entry of /rest/cluster/pending/devices.
type PendingDevice struct {
	Time    time.Time `json:"time"`
	Name    string    `json:"name"`
	Address string    `json:"address"`
}

// PendingFolderOffer is one device offering a pending folder.
type PendingFolderOffer struct {
	Time             time.Time `json:"time"`
	Label            string    `json:"label"`
	ReceiveEncrypted bool      `json:"receiveEncrypted"`
	RemoteEncrypted  bool      `json:"remoteEncrypted"`
}

// PendingFolder is one entry of /rest/cluster/pending/folders.
type PendingFolder struct {
	OfferedBy map[string]PendingFolderOffer `json:"offeredBy"`
}

// Event is one entry of /rest/events. ID is local to the event subscription
// (one per event mask) and restarts at 1 when Syncthing restarts.
type Event struct {
	ID       int64           `json:"id"`
	GlobalID int64           `json:"globalID"`
	Type     string          `json:"type"`
	Time     time.Time       `json:"time"`
	Data     json.RawMessage `json:"data"`
}

// Event payloads used by the collector.

// StateChangedData is the data of a StateChanged event.
type StateChangedData struct {
	Folder string `json:"folder"`
	From   string `json:"from"`
	To     string `json:"to"`
	Error  string `json:"error"`
}

// FolderSummaryData is the data of a FolderSummary event.
type FolderSummaryData struct {
	Folder  string   `json:"folder"`
	Summary DBStatus `json:"summary"`
}

// FolderCompletionData is the data of a FolderCompletion event.
type FolderCompletionData struct {
	Folder string `json:"folder"`
	Device string `json:"device"`
	Completion
}

// FolderErrorsData is the data of a FolderErrors event.
type FolderErrorsData struct {
	Folder string      `json:"folder"`
	Errors []FileError `json:"errors"`
}

// FolderWatchStateData is the data of a FolderWatchStateChanged event.
type FolderWatchStateData struct {
	Folder string `json:"folder"`
	From   string `json:"from"`
	To     string `json:"to"`
}

// FolderIDData is the data of FolderPaused and FolderResumed events.
type FolderIDData struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// DeviceConnectedData is the data of a DeviceConnected event.
type DeviceConnectedData struct {
	ID            string `json:"id"`
	Addr          string `json:"addr"`
	DeviceName    string `json:"deviceName"`
	ClientName    string `json:"clientName"`
	ClientVersion string `json:"clientVersion"`
	Type          string `json:"type"`
}

// DeviceIDData is the data of DeviceDisconnected, DevicePaused and DeviceResumed events.
type DeviceIDData struct {
	ID     string `json:"id"`
	Device string `json:"device"`
	Error  string `json:"error"`
}

// DiskChangeData is the data of LocalChangeDetected and RemoteChangeDetected events.
type DiskChangeData struct {
	Folder     string `json:"folder"`
	FolderID   string `json:"folderID"`
	Label      string `json:"label"`
	Action     string `json:"action"`
	Type       string `json:"type"`
	Path       string `json:"path"`
	ModifiedBy string `json:"modifiedBy"`
}
