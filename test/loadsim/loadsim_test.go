//go:build loadsim

// Package loadsim measures what syncwatch's structure scan costs a Syncthing
// server on a large, IVAR-like share: how many directory listings a
// baseline scan and an hourly scan send, how long they take, and whether
// the dashboard stays responsive meanwhile.
//
//	go test -tags loadsim -v -timeout 60m ./test/loadsim
//
// LOADSIM_PROJECTS (default 300), LOADSIM_SYNCED (270) and LOADSIM_FANOUT (6)
// shape the share: project folders, how many of them are Syncthing folders,
// and subdirectories per level below each project folder.
package loadsim

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/test/harness"
)

const (
	share = "/var/syncthing/share/NAS-LIVE-1SOURCE"
	swURL = "https://127.0.0.1:18781" // the image serves HTTPS by default
	token = "swt_loadsim_token_0123456789"
)

func dc(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", append([]string{"compose", "-f", "docker-compose.yml"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker compose %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func envInt(key string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(key)); err == nil && n > 0 {
		return n
	}
	return def
}

// scanLine matches syncwatch's "structure scan done" log line.
var scanLine = regexp.MustCompile(`structure scan done.*baseline=(true|false).*listings=(\d+) took=(\S+)`)

type scan struct {
	baseline bool
	listings int
	took     time.Duration
}

func scans(t *testing.T) []scan {
	var out []scan
	sc := bufio.NewScanner(strings.NewReader(dc(t, "logs", "--no-color", "syncwatch")))
	for sc.Scan() {
		if m := scanLine.FindStringSubmatch(sc.Text()); m != nil {
			n, _ := strconv.Atoi(m[2])
			d, _ := time.ParseDuration(m[3])
			out = append(out, scan{baseline: m[1] == "true", listings: n, took: d})
		}
	}
	return out
}

func TestStructureScanLoad(t *testing.T) {
	projects, synced, fanout := envInt("LOADSIM_PROJECTS", 300), envInt("LOADSIM_SYNCED", 270), envInt("LOADSIM_FANOUT", 6)
	if synced > projects {
		synced = projects
	}
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Minute)
	defer cancel()
	dc(t, "down", "-v", "--remove-orphans")
	dc(t, "up", "-d", "load-nas")
	defer func() {
		if t.Failed() {
			t.Log(dc(t, "logs", "--no-color", "--tail", "80", "syncwatch"))
		}
		if os.Getenv("KEEP_LOADSIM") == "" {
			dc(t, "down", "-v", "--remove-orphans")
		}
	}()
	nas := harness.Remote("NAS", true, "https://127.0.0.1:18711", "key-load-0123456789", "")
	if err := nas.WaitReady(ctx, 3*time.Minute); err != nil {
		t.Fatal(err)
	}

	// The share: project folders with fanout subdirectories on each of
	// three levels (the scan's default depth) and one more level below.
	start := time.Now()
	script := fmt.Sprintf(`set -e
S=%s; F=%d; P=%d
mkdir -p "$S"
i=1
while [ $i -le $P ]; do
  p=$(printf '%%s/LIVE-P%%03d-1SOURCE' "$S" $i)
  args=""
  a=1; while [ $a -le $F ]; do b=1; while [ $b -le $F ]; do c=1; while [ $c -le $F ]; do
    args="$args $p/d$a/d$b/d$c/deeper"
  c=$((c+1)); done; b=$((b+1)); done; a=$((a+1)); done
  mkdir -p $args
  i=$((i+1))
done`, share, fanout, projects)
	dc(t, "exec", "-T", "-u", "1000:1000", "load-nas", "sh", "-c", script)
	perProject := 1 + fanout + fanout*fanout + 2*fanout*fanout*fanout
	t.Logf("share: %d project folders × %d directories = %d directories, made in %v", projects, perProject, projects*perProject, time.Since(start).Round(time.Second))

	start = time.Now()
	for i := 1; i <= synced; i++ {
		label := fmt.Sprintf("LIVE-P%03d-1SOURCE", i)
		if err := nas.Call(ctx, http.MethodPost, "/rest/config/folders", map[string]any{
			"id": fmt.Sprintf("p-%03d", i), "label": label, "path": share + "/" + label, "type": "sendreceive",
			"devices": []map[string]string{{"deviceID": nas.DeviceID}}, "rescanIntervalS": 3600, "fsWatcherEnabled": false,
		}, nil); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("%d Syncthing folders added in %v", synced, time.Since(start).Round(time.Second))

	// syncwatch starts now: its first structure scan is a baseline (it
	// descends into every project folder), the next one a minute later only
	// into the project folders that aren't Syncthing folders.
	dc(t, "up", "-d", "syncwatch")
	sw, _ := harness.StartSyncwatchRemote(swURL, "loadsim-admin-password", token)
	var slowest time.Duration
	deadline := time.Now().Add(45 * time.Minute)
	var got []scan
	for time.Now().Before(deadline) {
		t0 := time.Now()
		if _, err := sw.Status(); err == nil {
			if d := time.Since(t0); d > slowest {
				slowest = d
			}
		}
		got = scans(t)
		if len(got) >= 2 {
			break
		}
		time.Sleep(5 * time.Second)
	}
	if len(got) < 2 {
		t.Fatalf("saw %d structure scans in 45 minutes", len(got))
	}

	// Listings: one for the share, 1+F+F² per project folder descended
	// (depth 3: the project folder and two levels below it are listed).
	descend := 1 + fanout + fanout*fanout
	wantBase := 1 + projects*descend
	wantHourly := 1 + (projects-synced)*descend
	base, hourly := got[0], got[1]
	t.Logf("baseline scan: %d listings in %v (%.1f ms each with %d at a time)", base.listings, base.took, float64(base.took.Milliseconds())/float64(base.listings), 3)
	t.Logf("hourly scan:   %d listings in %v", hourly.listings, hourly.took)
	t.Logf("slowest /api/v1/status answer while scanning: %v", slowest.Round(time.Millisecond))
	if !base.baseline || hourly.baseline {
		t.Errorf("scan kinds: %+v %+v", base, hourly)
	}
	if base.listings != wantBase || hourly.listings != wantHourly {
		t.Errorf("listings: baseline %d (want %d), hourly %d (want %d)", base.listings, wantBase, hourly.listings, wantHourly)
	}
	if slowest > 5*time.Second {
		t.Errorf("the dashboard API took %v to answer during a scan", slowest)
	}
	st, err := sw.Status()
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("health: %s", st.Health)
	if n := len(st.Findings); n == 0 {
		t.Error("no findings at all; expected S7 for the project folders that aren't Syncthing folders")
	}

	// The probe estimates the same numbers without running syncwatch.
	dir := t.TempDir()
	bin := dir + string(os.PathSeparator) + "syncwatch"
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/syncwatch").CombinedOutput(); err != nil {
		t.Fatalf("building syncwatch: %v\n%s", err, out)
	}
	report := dir + string(os.PathSeparator) + "probe.md"
	probe := exec.Command(bin, "probe", "--name", "NAS", "--url", "https://127.0.0.1:18711", "--out", report,
		"--event-wait", "5s", "--event-timeout", "5s", "--project-pattern", "^LIVE-")
	probe.Env = append(os.Environ(), "SYNCWATCH_PROBE_API_KEY=key-load-0123456789")
	if out, err := probe.CombinedOutput(); err != nil {
		t.Fatalf("probe: %v\n%s", err, out)
	}
	b, _ := os.ReadFile(report)
	est := regexp.MustCompile(`(Regular|Baseline) scan .*about \*\*(\d+) listings\*\*`).FindAllStringSubmatch(string(b), -1)
	if len(est) != 2 {
		t.Fatalf("no estimate in the probe report:\n%s", b)
	}
	for _, m := range est {
		n, _ := strconv.Atoi(m[2])
		want := wantHourly
		if m[1] == "Baseline" {
			want = wantBase
		}
		t.Logf("probe estimate, %s scan: %d listings (syncwatch made %d)", strings.ToLower(m[1]), n, want)
		if n != want {
			t.Errorf("probe estimate for the %s scan: %d, syncwatch made %d", strings.ToLower(m[1]), n, want)
		}
	}
}
