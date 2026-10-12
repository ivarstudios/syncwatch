package collector

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/model"
)

// The next baseline scan is due after at most 1 day when the last
// one took less than 10 seconds, else after baseline_interval. A baseline
// interval of 1 day or less does not change.
func TestBaselineEvery(t *testing.T) {
	for _, tc := range []struct {
		interval string
		took     time.Duration
		known    bool
		want     time.Duration
	}{
		{"7d", 800 * time.Millisecond, true, 24 * time.Hour},
		{"7d", 12 * time.Second, true, 7 * 24 * time.Hour},
		{"7d", 10 * time.Second, true, 7 * 24 * time.Hour},
		{"7d", 0, false, 7 * 24 * time.Hour}, // no baseline scan completed yet
		{"2h", 800 * time.Millisecond, true, 2 * time.Hour},
		{"2h", 12 * time.Second, true, 2 * time.Hour},
		{"30d", 9 * time.Second, true, 24 * time.Hour},
	} {
		cfg, err := config.ParseYAML([]byte("collector: {baseline_interval: " + tc.interval + "}\n"))
		if err != nil {
			t.Fatal(err)
		}
		c := &Collector{baseTook: tc.took, baseKnown: tc.known}
		if got := c.baselineEvery(cfg.Collector); got != tc.want {
			t.Errorf("baseline_interval %s, last baseline scan %v (known %v): every %v, want %v", tc.interval, tc.took, tc.known, got, tc.want)
		}
	}
}

// The structure scan that maybeScan starts is a baseline scan by that rule:
// with baseline_interval 7 days, the baseline scan of 25 hours ago makes
// this scan a baseline scan when it was fast, and a regular one when it was
// slow; a fast one of 23 hours ago does not.
func TestMaybeScanUsesDailyBaseline(t *testing.T) {
	for _, tc := range []struct {
		name     string
		interval string
		took     time.Duration
		ago      time.Duration
		baseline bool
	}{
		{"7 days, fast, 25 hours ago", "7d", 800 * time.Millisecond, 25 * time.Hour, true},
		{"7 days, fast, 23 hours ago", "7d", 800 * time.Millisecond, 23 * time.Hour, false},
		{"7 days, slow, 25 hours ago", "7d", 12 * time.Second, 25 * time.Hour, false},
		{"7 days, slow, 7 days ago", "7d", 12 * time.Second, 7*24*time.Hour + time.Minute, true},
		{"2 hours, fast, 3 hours ago", "2h", 800 * time.Millisecond, 3 * time.Hour, true},
		{"2 hours, fast, 1 hour ago", "2h", 800 * time.Millisecond, time.Hour, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, hub, _, stop := startCollector(t, newFakeST(t), "collector: {structure_interval: 1m, baseline_interval: "+tc.interval+"}\n")
			defer stop()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c.scanMu.Lock()
			c.lastScan, c.lastBase = time.Now().Add(-2*time.Minute), time.Now().Add(-tc.ago)
			c.baseTook, c.baseKnown = tc.took, true
			c.scanMu.Unlock()
			c.maybeScan(ctx)
			sv := waitFor(t, hub, "the scan", func(s *model.Server) bool { return s.Structure != nil })
			if sv.Structure.ScanBaseline != tc.baseline {
				t.Fatalf("baseline scan %v, want %v", sv.Structure.ScanBaseline, tc.baseline)
			}
		})
	}
}

// The choice is logged at INFO once for each change, and not at all when
// the baseline interval is 1 day or less.
func TestBaselineChoiceLogged(t *testing.T) {
	for _, tc := range []struct {
		interval  string
		day, week int
	}{
		{"7d", 2, 1},
		{"1d", 0, 0},
	} {
		cfg, _ := config.ParseYAML([]byte("collector: {baseline_interval: " + tc.interval + "}\n"))
		h, _ := config.NewHolder(cfg)
		var logs lockedBuf
		c := &Collector{cfg: h, log: slog.New(slog.NewTextHandler(&logs, nil))}
		for _, took := range []time.Duration{800 * time.Millisecond, 900 * time.Millisecond, 12 * time.Second, 15 * time.Second, time.Second} {
			c.noteBaseline(took)
		}
		day := logs.count(`level=INFO msg="baseline scan every day: the last one took less than 10 seconds"`)
		week := logs.count(`level=INFO msg="baseline scan every 7 days: the last one took 10 seconds or more"`)
		if day != tc.day || week != tc.week {
			t.Errorf("baseline_interval %s: %d daily and %d weekly lines, want %d and %d:\n%s", tc.interval, day, week, tc.day, tc.week, logs.b.String())
		}
		if c.baseTook != time.Second || !c.baseKnown {
			t.Errorf("last baseline scan %v (known %v), want 1s", c.baseTook, c.baseKnown)
		}
	}
}
