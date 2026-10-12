package notify

import (
	"strings"
	"testing"

	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/store"
)

// A stale finding is not always about an unreachable server: since r1 a
// held H5 is stale while the device is away, and H7/C2 wait
// while one server of the PC is unreachable. The suffix says only
// that the data is not current.
func TestStaleLineSaysNoCurrentData(t *testing.T) {
	r := &store.Record{ID: "abc", Checks: []string{"H5"}, Severity: model.Warn, Message: "Device PC-1 is stuck at 99.9% of folder X", Stale: true}
	line := findingLine(r, "")
	if !strings.Contains(line, "_(no current data, last known state)_") || strings.Contains(line, "unreachable") {
		t.Errorf("stale line: %q", line)
	}
	r.Stale = false
	if line := findingLine(r, ""); strings.Contains(line, "last known state") {
		t.Errorf("live line: %q", line)
	}
}
