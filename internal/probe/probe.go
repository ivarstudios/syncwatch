// Package probe implements "syncwatch probe", the tool that checks
// how a real Syncthing server's API behaves and writes the results to
// docs/api-findings.md. With --record it saves the (secret-free, typed)
// responses as test fixtures.
package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/pathx"
	"github.com/ivarstudios/syncwatch/internal/stclient"
)

// Options for a probe run.
type Options struct {
	Name, URL, APIKey string
	Pin               string
	Shares            []string
	Out               string
	Record            string
	EventTimeout      time.Duration
	EventWait         time.Duration
	// The structure scan estimate: the rules' project_pattern, the
	// scan depth and the most listings the estimate may make.
	ProjectPattern string
	ScanDepth      int
	ScanBudget     int
}

type report struct {
	lines []string
}

func (r *report) add(format string, a ...any) { r.lines = append(r.lines, fmt.Sprintf(format, a...)) }

// Run probes one server.
func Run(ctx context.Context, o Options, log *slog.Logger) error {
	var pinned string
	cl, err := stclient.New(stclient.Options{Name: o.Name, URL: o.URL, APIKey: o.APIKey, Pin: o.Pin, Timeout: 2 * time.Minute, OnPin: func(fp string) { pinned = fp }})
	if err != nil {
		return err
	}
	rep := &report{}
	rec := func(name string, v any) {
		if o.Record == "" {
			return
		}
		dir := filepath.Join(o.Record, config.Slug(o.Name))
		_ = os.MkdirAll(dir, 0o755)
		b, _ := json.MarshalIndent(v, "", "  ")
		_ = os.WriteFile(filepath.Join(dir, name+".json"), b, 0o644)
	}

	rep.add("## %s — %s", o.Name, time.Now().Format("2006-01-02 15:04 MST"))
	rep.add("")
	rep.add("URL: `%s`", o.URL)
	rep.add("")

	// 8. TLS
	start := time.Now()
	status, err := cl.SystemStatus(ctx)
	if err != nil {
		rep.add("**Could not connect:** %v", err)
		return write(o, rep, log)
	}
	cert := cl.LastCert()
	rep.add("### TLS (8)")
	rep.add("")
	if cert.Fingerprint != "" {
		kind := "self-signed, trusted on first use"
		if cert.CASigned {
			kind = "chains to a trusted CA"
		}
		rep.add("- Certificate: %s, SHA-256 `%s`, expires %s", kind, stclient.ShortFP(cert.Fingerprint), cert.NotAfter.Format("2006-01-02"))
		if pinned != "" {
			rep.add("- Pinned on first use: `%s`", pinned)
		}
	} else {
		rep.add("- Plain HTTP: the API key travels unencrypted.")
	}
	rep.add("- First request took %s", time.Since(start).Round(time.Millisecond))
	rep.add("")

	version, _ := cl.SystemVersion(ctx)
	rec("system-status", status)
	rec("system-version", version)
	rep.add("### Server")
	rep.add("")
	rep.add("- Version: %s (%s/%s, container: %v)", version.Version, version.OS, version.Arch, version.Container)
	rep.add("- Device ID: `%s`, path separator `%s`, started %s", status.MyID, status.PathSeparator, status.StartTime.Format(time.RFC3339))
	rep.add("")

	// 9. Counts and 5. full re-check timing
	t0 := time.Now()
	requests := 0
	folders, err := cl.ConfigFolders(ctx)
	requests++
	if err != nil {
		rep.add("**config/folders failed:** %v", err)
		return write(o, rep, log)
	}
	devices, _ := cl.ConfigDevices(ctx)
	conns, _ := cl.Connections(ctx)
	dstats, _ := cl.DeviceStats(ctx)
	fstats, _ := cl.FolderStats(ctx)
	pdev, _ := cl.PendingDevices(ctx)
	pfold, _ := cl.PendingFolders(ctx)
	requests += 6
	rec("config-folders", folders)
	rec("config-devices", devices)
	rec("connections", conns)
	rec("stats-device", dstats)
	rec("stats-folder", fstats)
	rec("pending-devices", pdev)
	rec("pending-folders", pfold)

	type comp struct {
		Folder, Device string
		stclient.Completion
	}
	var mu sync.Mutex
	dbs := map[string]stclient.DBStatus{}
	var comps []comp
	pairs := 0
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	var dbTimes []time.Duration
	for _, f := range folders {
		f := f
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ts := time.Now()
			ds, err := cl.DBStatus(ctx, f.ID)
			mu.Lock()
			requests++
			dbTimes = append(dbTimes, time.Since(ts))
			if err == nil {
				dbs[f.ID] = ds
			}
			mu.Unlock()
		}()
		for _, d := range f.Devices {
			if d.DeviceID == status.MyID {
				continue
			}
			pairs++
			dev := d.DeviceID
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				c, err := cl.Completion(ctx, f.ID, dev)
				mu.Lock()
				requests++
				if err == nil {
					comps = append(comps, comp{f.ID, dev, c})
				}
				mu.Unlock()
			}()
		}
	}
	wg.Wait()
	full := time.Since(t0)
	rec("db-status", dbs)
	rec("completion", comps)
	sort.Slice(dbTimes, func(i, j int) bool { return dbTimes[i] > dbTimes[j] })
	rep.add("### Counts (9) and full re-check (5)")
	rep.add("")
	rep.add("- %d folders, %d devices, %d (folder, device) pairs, %d connected now", len(folders), len(devices), pairs, countConnected(conns))
	rep.add("- Full re-check: %d requests in %s (3 at a time)", requests, full.Round(time.Millisecond))
	if len(dbTimes) > 0 {
		rep.add("- Slowest `db/status`: %s", dbTimes[0].Round(time.Millisecond))
	}
	states := map[string]int{}
	for _, d := range dbs {
		states[d.State]++
	}
	rep.add("- Folder states: %v", states)
	remote := map[string]int{}
	for _, c := range comps {
		remote[c.RemoteState]++
	}
	rep.add("- remoteState from db/completion: %v", remote)
	rep.add("- Decision gate: watch the NAS CPU and RAM while this runs; if it is noticeable, use the polled mode.")
	rep.add("")

	// 7. lastFile.at
	withLast := 0
	for _, s := range fstats {
		if s.LastFile.At.Year() > 1970 {
			withLast++
		}
	}
	rep.add("### Folder stats (7)")
	rep.add("")
	rep.add("- `lastFile.at` set for %d of %d folders (zero means no change recorded yet; C1 skips those)", withLast, len(fstats))
	rep.add("")

	// 6. metrics
	rep.add("### Conflict counters (6)")
	rep.add("")
	if cc, err := cl.ConflictCounters(ctx); err != nil {
		rep.add("- `/metrics` failed: %v", err)
	} else {
		total := int64(0)
		for _, v := range cc {
			total += v
		}
		rec("conflicts", cc)
		rep.add("- `/metrics` reachable with the API key; `syncthing_model_folder_conflicts_total` present for %d folders (total %d since start)", len(cc), total)
	}
	rep.add("")

	// 1, 2. browse
	shares := o.Shares
	sep := status.PathSeparator
	st := pathx.StyleFor(sep)
	if len(shares) == 0 {
		seen := map[string]bool{}
		for _, f := range folders {
			if p := st.Parent(f.Path); p != "" && !seen[p] {
				seen[p] = true
				shares = append(shares, p)
			}
		}
		sort.Strings(shares)
	}
	rep.add("### Browse (1, 2)")
	rep.add("")
	browsed := map[string][]string{}
	var slowest time.Duration
	for _, sh := range shares {
		ts := time.Now()
		entries, err := cl.Browse(ctx, sh, sep)
		dur := time.Since(ts)
		if dur > slowest {
			slowest = dur
		}
		if err != nil {
			rep.add("- `%s`: error %v", sh, err)
			continue
		}
		browsed[sh] = entries
		var hidden, special []string
		for _, e := range entries {
			name := st.Base(e)
			if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "@") || strings.HasPrefix(name, "#") {
				hidden = append(hidden, name)
			}
			if hasSpecial(name) {
				special = append(special, name)
			}
		}
		rep.add("- `%s`: %d directories in %s; hidden/system: %v; with spaces, non-ASCII or brackets: %v", sh, len(entries), dur.Round(time.Millisecond), hidden, special)
		// Check that names with special characters can be browsed into.
		for _, name := range special {
			p := st.Join(sh, name)
			if _, err := cl.Browse(ctx, p, sep); err != nil {
				rep.add("  - browsing into `%s` failed: %v", name, err)
			} else {
				rep.add("  - browsing into `%s` works without escaping", name)
			}
			break
		}
	}
	rec("browse", browsed)
	rep.add("- Slowest listing: %s. Compare the counts with `ls` on the NAS (hidden directories are included).", slowest.Round(time.Millisecond))
	rep.add("")

	// What the structure scan would cost, walked like the scan does.
	rep.add("### Structure scan estimate")
	rep.add("")
	cs, err := config.Structure{ProjectPattern: o.ProjectPattern, IgnoreDirs: config.DefaultIgnoreDirs, NestedScanDepth: o.ScanDepth}.Compile()
	if err != nil {
		rep.add("- Invalid --project-pattern: %v", err)
	} else {
		var paths []string
		for _, f := range folders {
			paths = append(paths, f.Path)
		}
		est := estimateScan(ctx, func(dir string) ([]string, error) { return cl.Browse(ctx, dir, sep) }, shares, paths, st, cs, o.ScanBudget)
		switch {
		case !cs.HasProjectPattern():
			rep.add("- No `--project-pattern` given: without one, syncwatch lists only the %d share top levels per scan. Pass the `project_pattern` from your rules to estimate the deep scan.", est.Shares)
		default:
			rep.add("- %d project folders matching `%s` in %d shares, %d of them inside Syncthing folders.", est.Projects, o.ProjectPattern, est.Shares, est.Synced)
			if est.Stopped {
				rep.add("- The budget of %d listings ran out after %d project folders; the numbers below are extrapolated.", o.ScanBudget, est.Walked)
			}
			per := est.PerListing()
			rep.add("- Regular scan (every `structure_interval`, default 1h; project folders not in Syncthing): about **%d listings**, %s at 3 at a time.", est.Hourly(), (per * time.Duration(est.Hourly()) / 3).Round(time.Second))
			rep.add("- Baseline scan (every `baseline_interval`, default 7d; every project folder): about **%d listings**, %s at 3 at a time.", est.Baseline(), (per * time.Duration(est.Baseline()) / 3).Round(time.Second))
			rep.add("- Measured: %d listings in %s, %s each (depth %d). Watch the NAS CPU while this ran; if it was noticeable, lower `nested_scan_depth` or raise the intervals.", est.Listings, est.Took.Round(time.Millisecond), per.Round(100*time.Microsecond), cs.Depth)
		}
	}
	rep.add("")

	// 3, 4. events
	rep.add("### Events (3, 4)")
	rep.add("")
	evs, err := cl.Events(ctx, 0, time.Second, stclient.EventMask)
	if err != nil {
		rep.add("- Subscribing with the event mask failed: %v", err)
		return write(o, rep, log)
	}
	last := int64(0)
	for _, e := range evs {
		if e.ID > last {
			last = e.ID
		}
	}
	rep.add("- Event mask accepted (%d names); subscription at event ID %d", len(stclient.EventMask), last)
	ts := time.Now()
	_, err = cl.Events(ctx, last, o.EventTimeout, []string{"StartupComplete"})
	if err != nil {
		rep.add("- A %s long-poll **failed** after %s: %v — lower `event_timeout`", o.EventTimeout, time.Since(ts).Round(time.Second), err)
	} else {
		rep.add("- A %s long-poll returned normally after %s (survives the network path)", o.EventTimeout, time.Since(ts).Round(time.Second))
	}
	log.Info("listening for events", "for", o.EventWait)
	deadline := time.Now().Add(o.EventWait)
	counts := map[string]int{}
	var diskSamples, compSamples []string
	var allEvents []stclient.Event
	for time.Now().Before(deadline) {
		wait := time.Until(deadline)
		if wait > o.EventTimeout {
			wait = o.EventTimeout
		}
		if wait < time.Second {
			break
		}
		evs, err := cl.Events(ctx, last, wait, stclient.EventMask)
		if err != nil {
			rep.add("- Event long-poll error: %v", err)
			break
		}
		for _, e := range evs {
			if e.ID <= last {
				continue
			}
			last = e.ID
			counts[e.Type]++
			allEvents = append(allEvents, e)
			switch e.Type {
			case "LocalChangeDetected", "RemoteChangeDetected":
				if len(diskSamples) < 5 {
					diskSamples = append(diskSamples, string(e.Data))
				}
			case "FolderCompletion":
				if len(compSamples) < 3 {
					compSamples = append(compSamples, string(e.Data))
				}
			}
		}
	}
	rec("events", allEvents)
	rep.add("- Events seen in %s: %v", o.EventWait, counts)
	if len(compSamples) > 0 {
		has := strings.Contains(compSamples[0], "remoteState")
		rep.add("- FolderCompletion carries `remoteState`: %v", has)
		rep.add("  - sample: `%s`", clip(compSamples[0], 300))
	} else {
		rep.add("- No FolderCompletion event seen; the full re-check calls `db/completion` per pair either way.")
	}
	for _, s := range diskSamples {
		rep.add("  - disk event: `%s`", clip(s, 300))
	}
	rep.add("- Event IDs are per subscription (one per event mask) and restart at 1 when Syncthing restarts; the collector re-checks when they go backwards or `startTime` changes.")
	rep.add("")
	return write(o, rep, log)
}

func countConnected(c map[string]stclient.Connection) int {
	n := 0
	for _, x := range c {
		if x.Connected {
			n++
		}
	}
	return n
}

func hasSpecial(s string) bool {
	for _, r := range s {
		if r > unicode.MaxASCII || r == ' ' || r == '[' || r == ']' || r == '*' || r == '?' {
			return true
		}
	}
	return false
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func write(o Options, rep *report, log *slog.Logger) error {
	text := strings.Join(rep.lines, "\n") + "\n"
	if o.Out == "" || o.Out == "-" {
		fmt.Print(text)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(o.Out), 0o755); err != nil {
		return err
	}
	existing, _ := os.ReadFile(o.Out)
	if len(existing) == 0 {
		existing = []byte("# Syncthing API findings\n\nWritten by `syncwatch probe`. Each section is one run against one server.\n\n## Probe runs\n\n")
	}
	if err := os.WriteFile(o.Out, append(existing, []byte(text+"\n")...), 0o644); err != nil {
		return err
	}
	log.Info("probe report written", "file", o.Out)
	return nil
}
