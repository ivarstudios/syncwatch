package model

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
)

// Severity of a finding.
type Severity int

const (
	Info  Severity = 1
	Warn  Severity = 2
	Error Severity = 3
)

func (s Severity) String() string {
	switch s {
	case Error:
		return "ERROR"
	case Warn:
		return "WARN"
	default:
		return "INFO"
	}
}

// Lower is the lowercase name, for CSS classes and filters.
func (s Severity) Lower() string { return strings.ToLower(s.String()) }

// Label is the display name of the severity: ERROR, WARNING or INFO. The
// API keeps the values of String() and Lower() (error, warn, info).
func (s Severity) Label() string {
	switch s {
	case Error:
		return "ERROR"
	case Warn:
		return "WARNING"
	default:
		return "INFO"
	}
}

// ParseSeverity parses ERROR/WARN/INFO (or the display names), case-insensitively.
func ParseSeverity(s string) Severity {
	switch strings.ToUpper(s) {
	case "ERROR":
		return Error
	case "WARN", "WARNING":
		return Warn
	default:
		return Info
	}
}

// Categories of checks.
const (
	CatHealth    = "health"
	CatCross     = "cross"
	CatStructure = "structure"
	CatCleanup   = "cleanup"
)

// Detail is one key/value line in a finding's technical details.
type Detail struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Finding is one problem or suggestion produced by the rules.
type Finding struct {
	ID       string   `json:"id"`
	Check    string   `json:"check"`  // primary check, e.g. "H4"
	Checks   []string `json:"checks"` // all reasons; usually just Check
	Severity Severity `json:"-"`
	Category string   `json:"category"`
	Server   string   `json:"server,omitempty"`  // server the finding comes from, "" for cross-server
	Related  []string `json:"related,omitempty"` // other servers it depends on
	Subject  string   `json:"subject"`
	Message  string   `json:"message"`
	Details  []Detail `json:"details,omitempty"`
	Paths    []string `json:"paths,omitempty"` // up to 5 failing file paths
	Link     string   `json:"link,omitempty"`  // Open in Syncthing
	// ResolvedText is one sentence for the resolved message, for example
	// "Server SOL can be reached again."; "" when the rule sets none.
	ResolvedText string `json:"resolved_text,omitempty"`

	// MinDuration is how long the condition must hold before the finding opens.
	MinDuration time.Duration `json:"-"`
	// Since is when the condition started, if the source data knows it.
	Since time.Time `json:"-"`
	// Urgent is set from the check configuration.
	Urgent bool `json:"urgent"`
	// Held marks a finding whose condition cannot be seen now, from the
	// last known state (H5 for a device that is away). The engine keeps an
	// open record open and stale, opens no new one and stops the timer of
	// its observation. Not stored.
	Held bool `json:"-"`
	// Covers are the IDs of the findings of another check whose condition
	// this finding reports now (S2 lists the S1 of the Syncthing folders
	// inside a share root). The engine closes such an open finding without
	// a resolved message: the condition continues. Not stored.
	Covers []string `json:"-"`
}

// FindingID is a stable hash of a check, server and subject key.
func FindingID(check, server, key string) string {
	sum := sha256.Sum256([]byte(check + "\x00" + server + "\x00" + key))
	return hex.EncodeToString(sum[:8])
}

// DetailValue returns the value of a detail key, or "".
func (f *Finding) DetailValue(key string) string {
	for _, d := range f.Details {
		if d.Key == key {
			return d.Value
		}
	}
	return ""
}
