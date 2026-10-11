package web

import (
	"crypto/subtle"
	"slices"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/settings"
)

// An import replaces the complete configuration (W-055). Before it does,
// the card "Export and import" shows a preview: the sections that the file
// changes, the sections that go back to their default values, the servers
// that the import removes with their API keys, the servers that need the
// API key again, the kinds of secrets in the file and whether the file gives
// access to more people. The file waits in memory, bound to the session that
// sent it, until the administrator confirms the preview (step 2 of
// handleImport). The page never shows the file or a secret from it.

const (
	importTTL         = 15 * time.Minute // a preview expires after this time
	maxPendingImports = 20               // with more pending imports, the oldest goes
)

// pendingImport is a file that waits for the confirmation of its preview.
type pendingImport struct {
	id    string
	data  []byte    // the file as it was sent, with its secrets
	rev   string    // configRev of the configuration that the preview compared
	key   string    // confirmKey of the session that sent the file
	at    time.Time // when the file was sent
	inUse bool      // a confirmation of it runs now (claim)
}

// pendingImports holds the files of the previews of one web server.
type pendingImports struct {
	mu    sync.Mutex
	items []*pendingImport // oldest first
}

// importsOf holds the pending imports of each web server (the Server struct
// in server.go has no field for them).
var importsOf sync.Map // *Server → *pendingImports

func (s *Server) pendingImports() *pendingImports {
	p, _ := importsOf.LoadOrStore(s, &pendingImports{})
	return p.(*pendingImports)
}

// add keeps the file of a preview and returns the ID of the pending import.
func (p *pendingImports) add(data []byte, rev, key string, now time.Time) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.expire(now)
	for len(p.items) >= maxPendingImports {
		p.dropAt(0)
	}
	id := randomToken(16)
	p.items = append(p.items, &pendingImport{id: id, data: slices.Clone(data), rev: rev, key: key, at: now})
	return id
}

// claim returns a copy of the pending import id of the session key and marks
// it as in use, thus a second confirmation of the same preview (a double
// click) does not apply the file again. It returns nil when the pending
// import does not exist, expired, belongs to another session or is in use.
// The caller removes it, or releases it to keep it for a corrected
// confirmation.
func (p *pendingImports) claim(id, key string, now time.Time) *pendingImport {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.expire(now)
	for _, it := range p.items {
		if subtle.ConstantTimeCompare([]byte(it.id), []byte(id)) == 1 && it.key == key && !it.inUse {
			it.inUse = true
			cp := *it
			cp.data = slices.Clone(it.data)
			return &cp
		}
	}
	return nil
}

// release ends the claim of the pending import id.
func (p *pendingImports) release(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, it := range p.items {
		if it.id == id {
			it.inUse = false
		}
	}
}

// remove deletes the pending import id.
func (p *pendingImports) remove(id string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, it := range p.items {
		if it.id == id {
			p.dropAt(i)
			return
		}
	}
}

// expire deletes the pending imports that are older than importTTL.
func (p *pendingImports) expire(now time.Time) {
	for i := 0; i < len(p.items); {
		if now.Sub(p.items[i].at) > importTTL {
			p.dropAt(i)
			continue
		}
		i++
	}
}

// dropAt deletes the pending import at index i and clears its bytes, which
// can hold secrets.
func (p *pendingImports) dropAt(i int) {
	clear(p.items[i].data)
	p.items = slices.Delete(p.items, i, i+1)
}

// importPreview is what the card "Export and import" shows before an import
// (the data key ImportPreview). It holds no value of a secret.
type importPreview struct {
	ID           string
	Sections     []string // the sections of the Settings page that the file changes
	Defaults     []string // the sections that are not in the file and go back to their default values, one line each
	Removed      []string // the servers that the import removes, one line each
	KeyAgain     []string // the servers that need the API key again, one line each
	Secrets      []string // the kinds of secrets in the file, never their values
	Weakens      []string // the access changes when the file gives access to more people
	Changes      []string // the lines of the settings notice
	DeleteLabel  string   // the label of the checkbox delete_servers; "" when no server goes
	NeedPassword bool     // the import needs the administrator password
	Error        string   // why step 2 refused the import
}

// importKeys are the top-level keys of a file that an import sets to their
// default values when the file does not have them, with the names of the
// Settings page, in the order of the page. The servers are not in the list:
// each removed server has a line of its own.
var importKeys = []struct{ key, label string }{
	{"devices", "Devices"},
	{"site", "Site where syncwatch runs"},
	{"notifications", "Notifications"},
	{"timezone", "Time zone"},
	{"public_url", "Dashboard URL"},
	{"checks", "Checks"},
	{"thresholds", "Thresholds of the checks"},
	{"structure", "Structure rules"},
	{"shares", "Shares"},
	{"collector", "Collector"},
	{"access", "Access"},
}

// importSection returns the value of a top-level key of importKeys in the
// configuration r (without its secrets).
func importSection(r *config.Config, key string) any {
	switch key {
	case "devices":
		return r.Devices
	case "site":
		return r.Site
	case "notifications":
		return r.Notify
	case "timezone":
		return r.Timezone
	case "public_url":
		return r.PublicURL
	case "checks":
		return r.Checks
	case "thresholds":
		return r.Thresholds
	case "structure":
		return r.Structure
	case "shares":
		return r.Shares
	case "collector":
		return r.Collector
	case "access":
		return r.Access
	}
	return nil
}

// buildImportPreview compares the file data, parsed as c (with its
// secrets), with the configuration old.
func (s *Server) buildImportPreview(data []byte, old, c *config.Config) *importPreview {
	p := &importPreview{Sections: config.ChangedSections(old, c), Changes: s.changesOf(old, c)}

	// The sections that are not in the file and whose value changes: the
	// value of the parsed file is the default value.
	var top map[string]yaml.Node
	_ = yaml.Unmarshal(data, &top)
	ro, rc := old.Redacted(), c.Redacted()
	for _, k := range importKeys {
		if _, ok := top[k.key]; ok {
			continue
		}
		a, errA := yaml.Marshal(importSection(ro, k.key))
		b, errB := yaml.Marshal(importSection(rc, k.key))
		if errA == nil && errB == nil && string(a) == string(b) {
			continue
		}
		var effect string
		switch k.key {
		case "access":
			effect = strings.Join(accessEffects(old, c), " ")
		case "devices":
			effect = "H11 is on again for each device."
		case "site", "timezone", "public_url":
			effect = "The value changes to " + orNone(importSection(rc, k.key).(string)) + "."
		default:
			effect = "syncwatch uses the default values."
		}
		p.Defaults = append(p.Defaults, k.label+": not in the file. "+effect)
	}

	removed := removedServers(old, c)
	for _, name := range removed {
		p.Removed = append(p.Removed, "syncwatch removes the server "+name+" and deletes its API key and pinned certificate.")
	}
	switch len(removed) {
	case 0:
	case 1:
		p.DeleteLabel = "Remove the server " + removed[0] + " (syncwatch deletes its API key)"
	default:
		p.DeleteLabel = "Remove the servers " + andList(removed) + " (syncwatch deletes their API keys)"
	}

	before := map[string]config.Server{}
	for _, sv := range old.Servers {
		before[sv.ID] = sv
	}
	for _, sv := range c.Servers {
		if sv.APIKey != "" {
			continue // the file has the API key
		}
		if o, ok := before[sv.ID]; ok && !config.SameEndpoint(o.URL, sv.URL) {
			p.KeyAgain = append(p.KeyAgain, "The API URL of the server "+sv.Name+" changes. Type the API key again for "+sv.Name+".")
		} else if !settings.KeyUsable(s.st, sv) {
			p.KeyAgain = append(p.KeyAgain, "The server "+sv.Name+" has no API key for its API URL. Type the API key for "+sv.Name+".")
		}
	}

	for _, sv := range c.Servers {
		if sv.APIKey != "" {
			p.Secrets = append(p.Secrets, "the API key of the server "+sv.Name)
		}
	}
	if c.Notify.DiscordWebhook != "" {
		p.Secrets = append(p.Secrets, "the Discord webhook")
	}
	if c.Notify.DiscordNoticesWebhook != "" {
		p.Secrets = append(p.Secrets, "the Discord webhook for settings notices")
	}
	if c.API.Token != "" {
		p.Secrets = append(p.Secrets, "the "+legacyTokenName)
	}

	if weakensAccess(old, c) {
		p.Weakens = accessLines(old, c)
	}
	p.NeedPassword = (len(removed) > 0 || len(p.Weakens) > 0) && s.adminConfigured()
	return p
}

// removedServers returns the names of the servers of old that c does not
// have.
func removedServers(old, c *config.Config) []string {
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

// andList joins names for a sentence: "AKKA", "AKKA and SOL", "AKKA, SOL and
// SIRIUS".
func andList(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// accessLines words the changes of access.mode and access.viewer as the
// settings notice does.
func accessLines(old, c *config.Config) []string {
	var out []string
	if old.Access.Mode != c.Access.Mode {
		out = append(out, describe("access.mode", old.Access.Mode, c.Access.Mode))
	}
	if old.Access.Viewer != c.Access.Viewer {
		out = append(out, describe("access.viewer", old.Access.Viewer, c.Access.Viewer))
	}
	return out
}

// accessEffects says what changes when a file has no access section.
func accessEffects(old, c *config.Config) []string {
	var out []string
	if old.Access.Mode != c.Access.Mode {
		if c.Access.Mode == config.AccessBuiltin {
			out = append(out, "Built-in login changes to on.")
		} else {
			out = append(out, "Built-in login changes to off.")
		}
	}
	if old.Access.Viewer != c.Access.Viewer {
		out = append(out, "Viewer access changes to "+viewerText(c.Access.Viewer)+".")
	}
	if old.Access.SessionTTL != c.Access.SessionTTL {
		out = append(out, "The session length (session_ttl) changes to "+c.Access.SessionTTL.Words()+".")
	}
	return out
}
