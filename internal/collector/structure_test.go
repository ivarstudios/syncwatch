package collector

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/stclient"
	"github.com/ivarstudios/syncwatch/internal/store"
)

// browseLog records the directories listed through /rest/system/browse and
// lets a test fail listings of chosen directories.
type browseLog struct {
	mu     sync.Mutex
	listed []string
	fail   map[string]bool     // directories (trailing separator included) answered with 500
	extra  map[string][]string // listings not in the fixture, by directory with trailing separator
}

func (b *browseLog) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rest/system/browse" {
			cur := r.URL.Query().Get("current")
			b.mu.Lock()
			b.listed = append(b.listed, cur)
			fail := b.fail[cur]
			extra, ok := b.extra[cur]
			b.mu.Unlock()
			if fail {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			if ok {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(extra)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (b *browseLog) reset() {
	b.mu.Lock()
	b.listed = nil
	b.mu.Unlock()
}

// count returns how many listings had the given suffix (a directory name).
func (b *browseLog) count(suffix string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, p := range b.listed {
		if strings.HasSuffix(strings.TrimRight(p, `\`), suffix) {
			n++
		}
	}
	return n
}

// startCollector builds a collector for the fixture server behind h (white
// box: no manager, no loop) and runs its first full check, so that
// scan() can be called directly with chosen arguments.
func startCollector(t *testing.T, h http.Handler, yamlTail string) (*Collector, *model.Hub, *config.Holder, func()) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	cfg, err := config.ParseYAML([]byte(rulesYAML + fmt.Sprintf("servers: [{name: AKKA, url: %q}]\n", srv.URL) + yamlTail))
	if err != nil {
		t.Fatal(err)
	}
	holder, _ := config.NewHolder(cfg)
	st, _ := store.OpenMemory(make([]byte, 32))
	hub := model.NewHub(nil)
	sc := cfg.Servers[0]
	hub.SetServers([]*model.Server{{ID: sc.ID, Name: sc.Name, URL: sc.URL}})
	c, err := newCollector(sc, "k", hub, st, holder, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := c.fullCheck(ctx); err != nil {
		t.Fatal(err)
	}
	c.succeeded()
	return c, hub, holder, func() { cancel(); srv.Close(); c.client.Close() }
}

func structureOf(hub *model.Hub) *model.Structure {
	var out *model.Structure
	hub.View("akka", func(s *model.Server) { out = s.Structure.Clone() })
	return out
}

func shareByName(t *testing.T, st *model.Structure, name string) *model.Share {
	t.Helper()
	for _, sh := range st.Shares {
		if sh.Name == name {
			return sh
		}
	}
	t.Fatalf("share %s missing in %+v", name, st.Shares)
	return nil
}

// A share whose listing fails keeps its previous directory list (marked
// stale by the error), so that the directory findings (S4–S7) don't resolve
// and come back around an outage or a slow listing.
func TestScanKeepsDirsWhenListingFails(t *testing.T) {
	fake := newFakeST(t)
	bl := &browseLog{fail: map[string]bool{}}
	c, hub, _, stop := startCollector(t, bl.wrap(fake), "")
	defer stop()
	ctx := context.Background()
	if err := c.scan(ctx, true); err != nil {
		t.Fatal(err)
	}
	first := structureOf(hub)
	src := shareByName(t, first, "AKKA-LIVE-1SOURCE")
	if len(src.Dirs) != 6 || src.Error != "" {
		t.Fatalf("first scan: %d dirs, error %q", len(src.Dirs), src.Error)
	}

	bl.mu.Lock()
	bl.fail[src.Path+`\`] = true
	bl.mu.Unlock()
	time.Sleep(10 * time.Millisecond) // a later ScannedAt
	if err := c.scan(ctx, false); err != nil {
		t.Fatal(err)
	}
	second := structureOf(hub)
	src2 := shareByName(t, second, "AKKA-LIVE-1SOURCE")
	if src2.Error == "" || !strings.Contains(src2.Error, "500") {
		t.Fatalf("listing error not recorded: %q", src2.Error)
	}
	if len(src2.Dirs) != 6 || !src2.ScannedAt.Equal(src.ScannedAt) {
		t.Fatalf("stale share: %d dirs (want 6 kept), scanned %v (want %v)", len(src2.Dirs), src2.ScannedAt, src.ScannedAt)
	}
	// The other share was listed as usual.
	if pf := shareByName(t, second, "AKKA-LIVE-2PROJECTFILES"); pf.Error != "" || len(pf.Dirs) != 2 || !pf.ScannedAt.After(src.ScannedAt) {
		t.Fatalf("healthy share: %+v", pf)
	}

	// Once the listing works again the share is fresh.
	bl.mu.Lock()
	bl.fail = map[string]bool{}
	bl.mu.Unlock()
	if err := c.scan(ctx, false); err != nil {
		t.Fatal(err)
	}
	if src3 := shareByName(t, structureOf(hub), "AKKA-LIVE-1SOURCE"); src3.Error != "" || len(src3.Dirs) != 6 || !src3.ScannedAt.After(src.ScannedAt) {
		t.Fatalf("recovered share: %+v", src3)
	}
}

const alphaRenders = `D:\tmp\swsim\st\akka\share\AKKA-LIVE-2PROJECTFILES\LIVE-ALPHA-2PROJECTFILES\renders`

// Nested entries that a scan doesn't find again (those from events inside
// synced folders, which regular scans don't walk) are kept while the
// directory exists, but only if they still count under the current rules.
func TestScanReappliesRulesToKeptEntries(t *testing.T) {
	fake := newFakeST(t)
	bl := &browseLog{extra: map[string][]string{alphaRenders + `\`: {alphaRenders + `\LIVE-NESTED-2PROJECTFILES`}}}
	c, hub, h, stop := startCollector(t, bl.wrap(fake), "")
	defer stop()
	ctx := context.Background()
	if err := c.scan(ctx, true); err != nil {
		t.Fatal(err)
	}
	base := h.Get()
	event := stclient.DiskChangeData{FolderID: "p-alpha", Type: "dir", Action: "modified", Path: `renders\LIVE-NESTED-2PROJECTFILES`}
	nested := func() int { return len(structureOf(hub).Nested) }

	// Unchanged rules: the entry survives the scan (one parent listing).
	c.diskChange(event, time.Now())
	if err := c.scan(ctx, false); err != nil {
		t.Fatal(err)
	}
	if nested() != 1 {
		t.Fatalf("entry not kept: %+v", structureOf(hub).Nested)
	}

	changes := map[string]func(c *config.Config){
		"ignore_dirs with the middle directory": func(c *config.Config) { c.Structure.IgnoreDirs = append(c.Structure.IgnoreDirs, "renders") },
		"a depth that no longer reaches it":     func(c *config.Config) { c.Structure.NestedScanDepth = 1 },
		"a pattern it doesn't match":            func(c *config.Config) { c.Structure.ProjectPattern = "^LIVE-[A-K]" },
		"its share excluded": func(c *config.Config) {
			c.Shares.Exclude = map[string][]string{"akka": {`D:\tmp\swsim\st\akka\share\AKKA-LIVE-2PROJECTFILES`}}
		},
	}
	for what, change := range changes {
		if err := h.Set(base.Clone()); err != nil {
			t.Fatal(err)
		}
		c.diskChange(event, time.Now())
		if nested() != 1 {
			t.Fatalf("%s: entry not added", what)
		}
		next := base.Clone()
		change(next)
		if err := h.Set(next); err != nil {
			t.Fatal(err)
		}
		if err := c.scan(ctx, false); err != nil {
			t.Fatal(err)
		}
		if nested() != 0 {
			t.Errorf("%s: stale entry kept: %+v", what, structureOf(hub).Nested)
		}
	}
}
