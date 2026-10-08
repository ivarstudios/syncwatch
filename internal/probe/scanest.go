package probe

import (
	"context"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/pathx"
)

// scanEstimate is what syncwatch's structure scan would cost a server,
// worked out by walking the shares the way the scan does.
type scanEstimate struct {
	Shares          int           // share top-level listings
	Projects        int           // project folders found in the shares
	Synced          int           // of them inside a Syncthing folder
	Walked          int           // project folders walked before the budget ran out
	ProjectListings int           // listings below the walked project folders
	SyncedListings  int           // of them below synced project folders
	Listings        int           // all listings made by the estimate
	Took            time.Duration // time for all listings, one at a time
	Stopped         bool          // the budget ran out
}

// Hourly is the estimated listings of a regular scan: the shares, and the
// project folders that aren't Syncthing folders.
func (e scanEstimate) Hourly() int {
	return e.Shares + e.scale(e.ProjectListings-e.SyncedListings, e.Projects-e.Synced, e.walkedUnsynced())
}

// Baseline is the estimated listings of a baseline scan: every project folder.
func (e scanEstimate) Baseline() int {
	return e.Shares + e.scale(e.ProjectListings, e.Projects, e.Walked)
}

// PerListing is the average time of one listing.
func (e scanEstimate) PerListing() time.Duration {
	if e.Listings == 0 {
		return 0
	}
	return e.Took / time.Duration(e.Listings)
}

// walkedUnsynced is how many walked project folders weren't synced. Projects
// are walked unsynced first, so it is all of them unless the budget ran out
// before the synced ones.
func (e scanEstimate) walkedUnsynced() int {
	return min(e.Walked, e.Projects-e.Synced)
}

// scale extrapolates listings measured on walked of total project folders.
func (e scanEstimate) scale(listings, total, walked int) int {
	if walked <= 0 || walked >= total {
		return listings
	}
	return listings * total / walked
}

// estimateScan walks the shares like the collector's structure scan:
// every share's top level, then every project folder (matching cs) down to
// cs.Depth levels, skipping ignored directories. Folders not in Syncthing
// are walked first; walking stops once budget listings were made.
func estimateScan(ctx context.Context, browse func(dir string) ([]string, error), shares, folderPaths []string,
	st pathx.Style, cs *config.CompiledStructure, budget int) scanEstimate {
	var e scanEstimate
	start := time.Now()
	list := func(dir string) []string {
		e.Listings++
		entries, err := browse(dir)
		if err != nil {
			return nil
		}
		return entries
	}
	synced := func(p string) bool {
		for _, f := range folderPaths {
			if st.Equal(f, p) || st.Within(p, f) {
				return true
			}
		}
		return false
	}
	var unsynced, inSync []string
	for _, sh := range shares {
		e.Shares++
		for _, p := range list(sh) {
			p = st.Clean(p)
			if name := st.Base(p); cs.Ignored(name) || !cs.IsProject(name) {
				continue
			}
			e.Projects++
			if synced(p) {
				e.Synced++
				inSync = append(inSync, p)
			} else {
				unsynced = append(unsynced, p)
			}
		}
	}
	if !cs.HasProjectPattern() {
		// Without a project pattern the scan doesn't look inside folders.
		e.Projects, e.Synced = 0, 0
		e.Took = time.Since(start)
		return e
	}
	var walk func(dir string, depth int) int
	walk = func(dir string, depth int) int {
		if depth > cs.Depth || ctx.Err() != nil {
			return 0
		}
		n := 1
		for _, c := range list(dir) {
			if !cs.Ignored(st.Base(st.Clean(c))) {
				n += walk(st.Clean(c), depth+1)
			}
		}
		return n
	}
	for i, p := range append(unsynced, inSync...) {
		if e.Listings >= budget || ctx.Err() != nil {
			e.Stopped = true
			break
		}
		n := walk(p, 1)
		e.Walked++
		e.ProjectListings += n
		if i >= len(unsynced) {
			e.SyncedListings += n
		}
	}
	e.Took = time.Since(start)
	return e
}
