package collector

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/pathx"
)

// With the example settings of the studio (examples/ivar.yaml),
// the scans do not search a helper folder (a name that starts with "_") for
// nested project folders, also inside a project folder. A different
// subdirectory still counts.
func TestStudioExampleNestedScanSkipsHelperFolders(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "examples", "ivar.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.ParseYAML(b)
	if err != nil {
		t.Fatalf("examples/ivar.yaml: %v", err)
	}
	cs, err := c.Structure.Compile()
	if err != nil {
		t.Fatal(err)
	}
	st := pathx.StyleFor("/")
	share := "/share/CACHEDEV1_DATA/AKKA-LIVE-2PROJECTFILES"
	top := share + "/LIVE-ORION2026-2PROJECTFILES"
	tree := map[string][]string{
		top:                 {top + "/_old", top + "/renders"},
		top + "/_old":       {top + "/_old/LIVE-OLD-2PROJECTFILES"},
		top + "/renders":    {top + "/renders/LIVE-COPY-2PROJECTFILES"},
		share + "/_archive": {share + "/_archive/LIVE-OLD-2PROJECTFILES"},
	}
	var listed []string
	browse := func(dir string) ([]string, error) {
		listed = append(listed, dir)
		return tree[dir], nil
	}
	var found []string
	(&Collector{}).descend(context.Background(), browse, st, cs, &model.Share{Path: share}, &model.Dir{Path: top}, top, 1, cs.Depth, nil, time.Now(),
		func(n *model.NestedDir) { found = append(found, n.Path) })
	if len(found) != 1 || found[0] != top+"/renders/LIVE-COPY-2PROJECTFILES" {
		t.Fatalf("nested project folders %v, want only the one below renders", found)
	}
	if slices.Contains(listed, top+"/_old") {
		t.Fatalf("the scan listed the helper folder _old: %v", listed)
	}
	// The events and the entries kept between scans use the same rule.
	for _, p := range []string{top + "/_old/LIVE-OLD-2PROJECTFILES", share + "/_archive/LIVE-OLD-2PROJECTFILES"} {
		if _, ok := nestedIn(st, cs, share, p); ok {
			t.Errorf("%s counts as a nested project folder", p)
		}
	}
	if _, ok := nestedIn(st, cs, share, top+"/renders/LIVE-COPY-2PROJECTFILES"); !ok {
		t.Error("the project folder below renders does not count")
	}
}
