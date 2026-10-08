package probe

import (
	"context"
	"fmt"
	"testing"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/pathx"
)

// tree builds a share with projects project folders, each fanout wide on
// every level below it, plus a hidden directory and a non-project folder.
func tree(projects, fanout, levels int) map[string][]string {
	t := map[string][]string{}
	share := "/s/X-LIVE-1SOURCE"
	var add func(dir string, level int)
	add = func(dir string, level int) {
		if level > levels {
			return
		}
		for i := 1; i <= fanout; i++ {
			c := fmt.Sprintf("%s/d%d", dir, i)
			t[dir] = append(t[dir], c)
			add(c, level+1)
		}
		t[dir] = append(t[dir], dir+"/.hidden")
	}
	for p := 1; p <= projects; p++ {
		dir := fmt.Sprintf("%s/LIVE-P%03d", share, p)
		t[share] = append(t[share], dir)
		add(dir, 1)
	}
	t[share] = append(t[share], share+"/random stuff", share+"/@Recycle")
	return t
}

func TestEstimateScan(t *testing.T) {
	fs := tree(10, 3, 4)
	calls := 0
	browse := func(dir string) ([]string, error) {
		calls++
		return fs[dir], nil
	}
	cs, err := config.Structure{ProjectPattern: "^LIVE-", IgnoreDirs: config.DefaultIgnoreDirs}.Compile()
	if err != nil {
		t.Fatal(err)
	}
	st := pathx.StyleFor("/")
	// Projects 1–7 are Syncthing folders, 8–10 aren't.
	var folders []string
	for p := 1; p <= 7; p++ {
		folders = append(folders, fmt.Sprintf("/s/X-LIVE-1SOURCE/LIVE-P%03d", p))
	}
	// Per project, depth 3: the folder, 3 children, 9 grandchildren.
	const perProject = 1 + 3 + 9
	e := estimateScan(context.Background(), browse, []string{"/s/X-LIVE-1SOURCE"}, folders, st, cs, 1_000_000)
	if e.Projects != 10 || e.Synced != 7 || e.Stopped || calls != e.Listings {
		t.Fatalf("estimate: %+v (calls %d)", e, calls)
	}
	if got, want := e.Hourly(), 1+3*perProject; got != want {
		t.Errorf("hourly %d, want %d", got, want)
	}
	if got, want := e.Baseline(), 1+10*perProject; got != want {
		t.Errorf("baseline %d, want %d", got, want)
	}

	// With a small budget the walk stops and extrapolates; the folders
	// that aren't synced are walked first.
	calls = 0
	e = estimateScan(context.Background(), browse, []string{"/s/X-LIVE-1SOURCE"}, folders, st, cs, 1+4*perProject)
	if !e.Stopped || e.Walked != 4 || calls > 1+4*perProject {
		t.Fatalf("budget: %+v (calls %d)", e, calls)
	}
	if got, want := e.Hourly(), 1+3*perProject; got != want {
		t.Errorf("hourly with budget %d, want %d", got, want)
	}
	if got, want := e.Baseline(), 1+10*perProject; got != want {
		t.Errorf("baseline with budget %d, want %d", got, want)
	}

	// Without a project pattern the scan only lists the shares.
	none, _ := config.Structure{IgnoreDirs: config.DefaultIgnoreDirs}.Compile()
	e = estimateScan(context.Background(), browse, []string{"/s/X-LIVE-1SOURCE"}, folders, st, none, 1_000_000)
	if e.Hourly() != 1 || e.Baseline() != 1 {
		t.Fatalf("no pattern: hourly %d, baseline %d", e.Hourly(), e.Baseline())
	}
}
