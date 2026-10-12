package rules

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/model"
)

// studioExample parses examples/ivar.yaml, the example settings of the
// studio.
func studioExample(t *testing.T) *config.Config {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "examples", "ivar.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.ParseYAML(b)
	if err != nil {
		t.Fatalf("examples/ivar.yaml: %v", err)
	}
	return c
}

// W-056 (b): with the example settings of the studio, the helper folders
// next to the project folders (names that start with "_") get no structure
// finding: no S4 (incorrect name) and no S6 (duplicate name) for _archive
// in 2 shares. A new project folder that is not in Syncthing still gets S7.
// The default ignore_dirs stay without '_*': there the helper folders get
// S4, and _archive also S6.
func TestStudioExampleSkipsHelperFolders(t *testing.T) {
	c := studioExample(t)
	if slices.Contains(config.DefaultIgnoreDirs, "_*") {
		t.Fatal("'_*' is in the default ignore_dirs: it belongs only to the example settings of the studio")
	}
	evaluate := func(c *config.Config) []model.Finding {
		t.Helper()
		cs, err := c.Structure.Compile()
		if err != nil {
			t.Fatal(err)
		}
		akka := server("akka", "AKKA", akkaID)
		fin, src := "/share/CACHEDEV1_DATA/AKKA-LIVE-4FINALS", "/share/CACHEDEV1_DATA/AKKA-LIVE-1SOURCE"
		akka.Folders["f"] = folder("f", "LIVE-ORION2026-4FINALS", fin+"/LIVE-ORION2026-4FINALS")
		akka.Folders["s"] = folder("s", "LIVE-ORION2026-1SOURCE", src+"/LIVE-ORION2026-1SOURCE")
		addShare(akka, cs, fin, "LIVE-ORION2026-4FINALS", "_archive", "_templates")
		addShare(akka, cs, src, "LIVE-ORION2026-1SOURCE", "_archive", "_libraries", "LIVE-NEWPROJECT-1SOURCE")
		return Evaluate(Input{Snapshot: &model.Snapshot{At: now, Servers: []*model.Server{akka}}, Config: c, Structure: cs, Now: now})
	}

	fs := evaluate(c)
	for _, f := range fs {
		if strings.Contains(f.Subject, "/_") {
			t.Errorf("a helper folder has a finding with the example settings: %s %s", f.Check, f.Message)
		}
	}
	mustFind(t, fs, "S7", "AKKA-LIVE-1SOURCE/LIVE-NEWPROJECT-1SOURCE on AKKA")
	for _, name := range []string{"_archive", "_ARCHIVE", "_templates", "_libraries", "@Recycle", ".stfolder"} {
		if cs, _ := c.Structure.Compile(); !cs.Ignored(name) {
			t.Errorf("the example settings do not skip %s", name)
		}
	}

	def := c.Clone()
	def.Structure.IgnoreDirs = append([]string(nil), config.DefaultIgnoreDirs...)
	fs = evaluate(def)
	mustFind(t, fs, "S4", "AKKA-LIVE-4FINALS/_templates on AKKA")
	mustFind(t, fs, "S4", "AKKA-LIVE-1SOURCE/_libraries on AKKA")
	if a := mustFind(t, fs, "S4", "AKKA-LIVE-4FINALS/_archive on AKKA"); !slices.Contains(a.Checks, "S6") {
		t.Errorf("_archive in 2 shares with the default ignore_dirs: %v, want S4 and S6", a.Checks)
	}
}
