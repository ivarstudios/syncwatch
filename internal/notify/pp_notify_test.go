package notify

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/store"
)

// The tests of the pre-release review (prepub) of the notifier.

// Device IDs of 2 PCs with the same name, EDIT-01: one at the office, one at
// site B. Syncthing does not make device names unique.
const (
	officePC = "OFFICE2-AAAAAAA-BBBBBBB-CCCCCCC-DDDDDDD-EEEEEEE-FFFFFFF-GGGGGGG"
	siteBPC  = "SITEB22-AAAAAAA-BBBBBBB-CCCCCCC-DDDDDDD-EEEEEEE-FFFFFFF-GGGGGGG"
)

// Two PCs with the same name are 2 devices: their findings have a line each
// in every message. The views of 1 PC from both servers stay 1 line (text-3).
func TestTwoPCsWithTheSameNameHaveTwoLines(t *testing.T) {
	cfg := twoNAS()
	now := local(2026, 10, 6, 12, 0)
	rec := func(id, check, server, subject, message, device string) *store.Record {
		return &store.Record{Finding: model.Finding{ID: id, Check: check, Checks: []string{check}, Severity: model.Warn, Server: server, Urgent: true,
			Subject: subject, Message: message, Details: []model.Detail{{Key: "Folder ID", Value: "live-x"}, {Key: "Device ID", Value: device}}},
			OpenedAt: now.Add(-25 * time.Hour), PingedAt: now.Add(-24 * time.Hour), ResolvedAt: now}
	}
	h5 := func(id, server, name, device, pct string) *store.Record {
		return rec(id, "H5", server, "LIVE-X on EDIT-01",
			fmt.Sprintf("Device EDIT-01 is stuck at %s%% of folder LIVE-X for more than 24 hours (120 remaining items, seen from %s)", pct, name), device)
	}
	h9 := func(id, server, name, device string) *store.Record {
		return rec(id, "H9", server, "EDIT-01 on "+name, "Unknown device EDIT-01 requests a connection to "+name, device)
	}
	h11 := func(id, server, name, device string) *store.Record {
		return rec(id, "H11", server, name+" ↔ EDIT-01", "Device EDIT-01 is connected to "+name+" through a relay, which is slow", device)
	}
	for _, c := range []struct {
		what string
		rs   []*store.Record
		want int
	}{
		{"H5 of 2 PCs", []*store.Record{h5("a", "akka", "AKKA", officePC, "40"), h5("b", "sol", "SOL", siteBPC, "12")}, 2},
		{"H5 of 1 PC from both servers", []*store.Record{h5("a", "akka", "AKKA", officePC, "40"), h5("b", "sol", "SOL", officePC, "12")}, 1},
		{"H9 of 2 devices", []*store.Record{h9("c", "akka", "AKKA", officePC), h9("d", "sol", "SOL", siteBPC)}, 2},
		{"H9 of 1 device on both servers", []*store.Record{h9("c", "akka", "AKKA", officePC), h9("d", "sol", "SOL", officePC)}, 1},
		{"H11 of 2 PCs", []*store.Record{h11("e", "akka", "AKKA", officePC), h11("f", "sol", "SOL", siteBPC)}, 2},
	} {
		for kind, m := range map[string]Message{
			"urgent":   UrgentMessage(cfg, c.rs),
			"repeat":   RepeatMessage(cfg, c.rs, now),
			"reminder": ReminderMessage(cfg, c.rs, now),
			"resolved": ResolvedMessage(cfg, c.rs, nil),
		} {
			if lines := m.Sections[0].Lines; len(lines) != c.want {
				t.Errorf("%s, %s message: %d lines, want %d: %q", c.what, kind, len(lines), c.want, lines)
			}
		}
		night := OvernightMessage(cfg, c.rs, nil, nil, nil, nil)
		if want := fmt.Sprintf("New and still open (%d)", c.want); night.Sections[0].Title != want {
			t.Errorf("%s, overnight summary: %q, want %q", c.what, night.Sections[0].Title, want)
		}
	}
	// The line of the PC at site B has its own state and link.
	lines := strings.Join(UrgentMessage(cfg, []*store.Record{h5("a", "akka", "AKKA", officePC, "40"), h5("b", "sol", "SOL", siteBPC, "12")}).Sections[0].Lines, "\n")
	if !strings.Contains(lines, "stuck at 12% of folder LIVE-X") || !strings.Contains(lines, "/problems#f-b)") || strings.Contains(lines, "also seen from") {
		t.Errorf("urgent lines:\n%s", lines)
	}
}

// Through the rules and the engine: 2 unknown devices with the same name
// request a connection, one to AKKA and one to SOL (H9 urgent). The urgent
// message has 2 lines (text-3).
func TestTwoUnknownDevicesWithTheSameNameInTheUrgentMessage(t *testing.T) {
	start := local(2026, 10, 6, 10, 0)
	r := newRig(t, start, "AKKA", "SOL")
	setNotify(t, r, func(c *config.Config) {
		urgent, debounce := true, config.Duration(time.Minute)
		c.Checks = map[string]config.Check{"H9": {Urgent: &urgent, Debounce: &debounce}}
	})
	for server, id := range map[string]string{"akka": officePC, "sol": siteBPC} {
		r.hub.Update(server, func(s *model.Server) {
			s.PendingDevices = []model.PendingDevice{{DeviceID: id, Name: "EDIT-01", Address: "10.0.0.9:22000", Time: start}}
		})
	}
	r.at(start)
	r.at(start.Add(time.Minute + holdUrgent))
	msgs := r.sender.take()
	if len(msgs) != 1 || msgs[0].Kind != KindUrgent {
		t.Fatalf("urgent message: %+v", msgs)
	}
	if lines := msgs[0].Sections[0].Lines; len(lines) != 2 || strings.Contains(strings.Join(lines, "\n"), "also on") {
		t.Errorf("%s: %q", msgs[0].Title, lines)
	}
}
