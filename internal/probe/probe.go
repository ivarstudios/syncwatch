// Package probe implements "syncwatch probe", the tool that checks
// how a real Syncthing server's API behaves and writes the results to
// docs/api-findings.md. With --record it saves the (secret-free, typed)
// responses as test fixtures.
package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
		rep.add("**The connection failed:** %v", err)
		return stop(o, rep, log, fmt.Errorf("the connection failed: %w", err))
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
		rep.add("- Plain HTTP: the probe sends the API key unencrypted.")
	}
	rep.add("- First request: %s", time.Since(start).Round(time.Millisecond))
	rep.add("")

	version, _ := cl.SystemVersion(ctx)
	rec("system-status", status)
	rec("system-version", version)
	rep.add("### Server")
	rep.add("")
	rep.add("- Version: %s (OS %s, architecture %s, container: %v)", version.Version, version.OS, version.Arch, version.Container)
	rep.add("- Device ID: `%s`, path separator `%s`, started %s", status.MyID, status.PathSeparator, status.StartTime.Format(time.RFC3339))
	rep.add("")

	// 9. Counts and 5. full re-check timing
	t0 := time.Now()
	requests := 0
	folders, err := cl.ConfigFolders(ctx)
	requests++
	if err != nil {
		rep.add("**The request `config/folders` failed:** %v", err)
		return stop(o, rep, log, fmt.Errorf("the request config/folders failed: %w", err))
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
	rep.add("### Counts (9) and full check (5)")
	rep.add("")
	rep.add("- Folders: %d · devices: %d · folder-device pairs: %d · devices connected now: %d", len(folders), len(devices), pairs, countConnected(conns))
	rep.add("- Full check: %d requests in %s (3 requests at a time)", requests, full.Round(time.Millisecond))
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
	rep.add("- remoteState from `db/completion`: %v", remote)
	rep.add("- Decision: monitor the CPU and RAM of the NAS while the full check runs. If the load is high, use the polled mode.")
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
	rep.add("- `lastFile.at` is set for %d of %d folders. If the value is zero, Syncthing did not record a change. C1 skips those folders.", withLast, len(fstats))
	rep.add("")

	// 6. metrics
	rep.add("### Conflict counters (6)")
	rep.add("")
	if cc, err := cl.ConflictCounters(ctx); err != nil {
		rep.add("- The request `/metrics` failed: %v", err)
	} else {
		total := int64(0)
		for _, v := range cc {
			total += v
		}
		rec("conflicts", cc)
		rep.add("- `/metrics` is reachable with the API key. `syncthing_model_folder_conflicts_total` is present for %d folders (total %d after the last Syncthing start).", len(cc), total)
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
			rep.add("- `%s`: cannot browse: %v", sh, err)
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
		rep.add("- `%s`: %d directories in %s · hidden or system: %v · with spaces, non-ASCII or brackets: %v", sh, len(entries), dur.Round(time.Millisecond), hidden, special)
		// Check that names with special characters can be browsed into.
		for _, name := range special {
			p := st.Join(sh, name)
			if _, err := cl.Browse(ctx, p, sep); err != nil {
				rep.add("  - cannot browse `%s`: %v", name, err)
			} else {
				rep.add("  - the browse of `%s` works without escape characters", name)
			}
			break
		}
	}
	rec("browse", browsed)
	rep.add("- Slowest listing: %s. Compare the counts with `ls` on the NAS. The counts include hidden directories.", slowest.Round(time.Millisecond))
	rep.add("")

	// What the structure scan would cost, walked like the scan does.
	rep.add("### Structure scan estimate")
	rep.add("")
	cs, err := config.Structure{ProjectPattern: o.ProjectPattern, IgnoreDirs: config.DefaultIgnoreDirs, NestedScanDepth: o.ScanDepth}.Compile()
	if err != nil {
		rep.add("- The --project-pattern value is not valid: %v", err)
	} else {
		var paths []string
		for _, f := range folders {
			paths = append(paths, f.Path)
		}
		est := estimateScan(ctx, func(dir string) ([]string, error) { return cl.Browse(ctx, dir, sep) }, shares, paths, st, cs, o.ScanBudget)
		switch {
		case !cs.HasProjectPattern():
			rep.add("- There is no `--project-pattern`. Without it, syncwatch lists only the top level of the %d shares in each scan. Use the `project_pattern` of your structure rules to estimate the baseline scan.", est.Shares)
		default:
			rep.add("- %d project folders match `%s` in %d shares. %d of them are inside Syncthing folders.", est.Projects, o.ProjectPattern, est.Shares, est.Synced)
			if est.Stopped {
				rep.add("- The budget of %d listings ended after %d project folders. The numbers below are estimates.", o.ScanBudget, est.Walked)
			}
			per := est.PerListing()
			rep.add("- Structure scan (every `structure_interval`, default 1 hour, only project folders not in Syncthing): approximately **%d listings**, %s with 3 requests at a time.", est.Hourly(), (per * time.Duration(est.Hourly()) / 3).Round(time.Second))
			rep.add("- Baseline scan (every `baseline_interval`, default 7 days, every project folder): approximately **%d listings**, %s with 3 requests at a time.", est.Baseline(), (per * time.Duration(est.Baseline()) / 3).Round(time.Second))
			rep.add("- Measured: %d listings in %s, %s each (depth %d). Monitor the CPU of the NAS during the probe run. If the load was high, decrease `nested_scan_depth` or increase the intervals.", est.Listings, est.Took.Round(time.Millisecond), per.Round(100*time.Microsecond), cs.Depth)
		}
	}
	rep.add("")

	// 3, 4. events
	rep.add("### Events (3, 4)")
	rep.add("")
	evs, err := cl.Events(ctx, 0, time.Second, stclient.EventMask)
	if err != nil {
		rep.add("- The subscription with the event mask failed: %v", err)
		return stop(o, rep, log, fmt.Errorf("the subscription with the event mask failed: %w", err))
	}
	last := int64(0)
	for _, e := range evs {
		if e.ID > last {
			last = e.ID
		}
	}
	rep.add("- Syncthing accepted the event mask (%d names). The subscription starts at event ID %d.", len(stclient.EventMask), last)
	ts := time.Now()
	_, err = cl.Events(ctx, last, o.EventTimeout, []string{"StartupComplete"})
	if err != nil {
		rep.add("- A %s long-poll **failed** after %s (decrease `event_timeout`): %v", o.EventTimeout, time.Since(ts).Round(time.Second), err)
	} else {
		rep.add("- A %s long-poll returned normally after %s (the network path keeps the connection open)", o.EventTimeout, time.Since(ts).Round(time.Second))
	}
	log.Info("probe reads the event stream", "for", o.EventWait)
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
			rep.add("- The event long-poll failed: %v", err)
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
	rep.add("- Events in %s: %v", o.EventWait, counts)
	if len(compSamples) > 0 {
		has := strings.Contains(compSamples[0], "remoteState")
		rep.add("- FolderCompletion includes `remoteState`: %v", has)
		rep.add("  - sample: `%s`", clip(compSamples[0], 300))
	} else {
		rep.add("- The probe saw no FolderCompletion event. The full check calls `db/completion` for each pair in both cases.")
	}
	for _, s := range diskSamples {
		rep.add("  - disk event: `%s`", clip(s, 300))
	}
	rep.add("- Event IDs are separate for each subscription (one subscription for each event mask). They restart at 1 when Syncthing restarts. The collector checks again when the IDs decrease or `startTime` changes.")
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

// stop writes the report as far as the probe got and returns why the probe
// stopped early. A probe that could not test the server exits with status 1.
func stop(o Options, rep *report, log *slog.Logger, err error) error {
	return errors.Join(err, write(o, rep, log))
}

// stdout gets the report with --out - and when --out cannot be written.
var stdout io.Writer = os.Stdout

func write(o Options, rep *report, log *slog.Logger) error {
	text := strings.Join(rep.lines, "\n") + "\n"
	if o.Out == "" || o.Out == "-" {
		fmt.Fprint(stdout, text)
		return nil
	}
	err := os.MkdirAll(filepath.Dir(o.Out), 0o755)
	if err == nil {
		existing, _ := os.ReadFile(o.Out)
		if len(existing) == 0 {
			existing = []byte("# Syncthing API probe report\n\n`syncwatch probe` writes this file. Each section is one probe run for one server.\n\n## Probe runs\n\n")
		}
		err = os.WriteFile(o.Out, append(existing, []byte(text+"\n")...), 0o644)
	}
	if err != nil {
		// The run took minutes: its results are not lost.
		fmt.Fprint(stdout, text)
		return fmt.Errorf("cannot write the probe report to %s (syncwatch printed it to standard output): %w", o.Out, err)
	}
	log.Info("probe report written", "file", o.Out)
	return nil
}
