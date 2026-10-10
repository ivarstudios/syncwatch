package web

import (
	"net/url"
	"reflect"
	"testing"

	"github.com/ivarstudios/syncwatch/internal/config"
)

// The Structure form used to turn an omitted ignore_dirs into an empty list,
// which dropped the default ignores while the same YAML through import kept
// them. An omitted key means the defaults; an explicit [] means none.
func TestStructureFormKeepsDefaultIgnoreDirs(t *testing.T) {
	e := adminEnv(t, twoServers)
	csrf := e.csrf("/settings")
	if msg, isErr := flash(e.post("/settings/structure", url.Values{"csrf": {csrf}, "yaml": {"structure: {project_pattern: '^LIVE-'}"}})); isErr {
		t.Fatal(msg)
	}
	if got := e.cfg.Get().Structure.IgnoreDirs; !reflect.DeepEqual(got, config.DefaultIgnoreDirs) {
		t.Fatalf("ignore_dirs after a form without the key: %q, want the defaults", got)
	}
	if msg, isErr := flash(e.post("/settings/structure", url.Values{"csrf": {csrf}, "yaml": {"structure: {project_pattern: '^LIVE-', ignore_dirs: []}"}})); isErr {
		t.Fatal(msg)
	}
	if got := e.cfg.Get().Structure.IgnoreDirs; len(got) != 0 {
		t.Fatalf("ignore_dirs after an explicit []: %q, want none", got)
	}
}
