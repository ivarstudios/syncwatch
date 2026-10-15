package gate

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/stclient"
)

// sizeSetup is a gate in front of a fake Syncthing whose only folder is
// <tmp>/share/LIVE-A, thus <tmp>/share is the share the gate may list. The
// gate keeps its sizes in cache.
func sizeSetup(t *testing.T, cache string) (st *fakeSyncthing, g *Gate, gsURL func(path string, measure bool) (int, stclient.DirSize, string), share string) {
	t.Helper()
	st, g, gs := setup(t)
	share = filepath.Join(t.TempDir(), "share")
	if err := os.MkdirAll(filepath.Join(share, "LIVE-A"), 0o755); err != nil {
		t.Fatal(err)
	}
	st.set(string(filepath.Separator), []string{filepath.Join(share, "LIVE-A")})
	g.sizes = newSizer(cache, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return st, g, func(path string, measure bool) (int, stclient.DirSize, string) {
		t.Helper()
		q := "/syncwatch/size?path=" + url.QueryEscape(path)
		if measure {
			q += "&measure=1"
		}
		code, body := get(t, gs, http.MethodGet, q, token)
		var ds stclient.DirSize
		if code == http.StatusOK {
			if err := json.Unmarshal([]byte(body), &ds); err != nil {
				t.Fatalf("%s: %v: %s", q, err, body)
			}
		}
		return code, ds, body
	}, share
}

// waitMeasured asks for path until the gate has measured it.
func waitMeasured(t *testing.T, ask func(string, bool) (int, stclient.DirSize, string), path string) stclient.DirSize {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, ds, _ := ask(path, false); !ds.MeasuredAt.IsZero() && ds.State == "" {
			return ds
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s was not measured", path)
	return stclient.DirSize{}
}

func write(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The gate answers /syncwatch/size itself, only for a directory inside a
// share that a browse may list, and Syncthing never gets the request.
func TestSizeOnlyInsideAShare(t *testing.T) {
	st, _, ask, share := sizeSetup(t, "")
	other := filepath.Join(filepath.Dir(share), "other")
	if code, ds, body := ask(filepath.Join(share, "LIVE-B"), false); code != http.StatusOK || ds.State != "" || !ds.MeasuredAt.IsZero() {
		t.Errorf("a directory in the share: %d %s", code, body)
	}
	for _, p := range []string{
		share,                          // the share itself
		filepath.Join(other, "LIVE-B"), // another directory
		share + string(filepath.Separator) + ".." + string(filepath.Separator) + "other", // out of the share with ..
		"LIVE-B", // a relative path
	} {
		if code, _, body := ask(p, true); code != http.StatusForbidden || !strings.Contains(body, "directory not in a permitted browse root") {
			t.Errorf("%s: %d %s, want 403", p, code, body)
		}
	}
	_, _, gs := setup(t)
	for _, c := range []struct {
		method, query, tok string
		want               int
	}{
		{http.MethodGet, "?path=%2Fs%2FX", "", http.StatusForbidden},            // no token
		{http.MethodPost, "?path=%2Fs%2FX", token, http.StatusMethodNotAllowed}, // only GET
		{http.MethodGet, "", token, http.StatusBadRequest},                      // no path
		{http.MethodGet, "?path=%2Fs%2FX&measure=yes", token, http.StatusBadRequest},
		{http.MethodGet, "?path=%2Fs%2FX&delete=1", token, http.StatusBadRequest}, // only the parameters of the client
	} {
		if code, body := get(t, gs, c.method, "/syncwatch/size"+c.query, c.tok); code != c.want {
			t.Errorf("%s %s: %d %s, want %d", c.method, c.query, code, body, c.want)
		}
	}
	if n := st.countPath(pathSize); n != 0 {
		t.Errorf("Syncthing got %d size requests", n)
	}
}

// A measurement adds the sizes of the files below the directory, in the
// queue of the gate, and the result survives a restart of the gate.
func TestSizeMeasures(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "sizes.json")
	_, g, ask, share := sizeSetup(t, cache)
	dir := filepath.Join(share, "LIVE-B")
	write(t, filepath.Join(dir, "a.bin"), 1000)
	write(t, filepath.Join(dir, "sub", "b.bin"), 2000)
	if err := os.MkdirAll(filepath.Join(dir, "sub", "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.Run(ctx)

	if _, ds, body := ask(dir, true); ds.State != stclient.SizeQueued && ds.State != stclient.SizeMeasuring && ds.MeasuredAt.IsZero() {
		t.Fatalf("measure=1 did not queue the directory: %s", body)
	}
	ds := waitMeasured(t, ask, dir)
	if ds.Bytes != 3000 || ds.Files != 2 || ds.Dirs != 2 || !ds.Complete || ds.Skipped != 0 || ds.Error != "" {
		t.Fatalf("result %+v", ds)
	}
	// Measured less than a minute ago: not again.
	if _, again, _ := ask(dir, true); again.State != "" || !again.MeasuredAt.Equal(ds.MeasuredAt) {
		t.Errorf("measured again at once: %+v", again)
	}
	if fi, err := os.Stat(cache); err != nil {
		t.Fatal(err)
	} else if fi.Mode().Perm()&0o077 != 0 && filepath.Separator == '/' {
		t.Errorf("the size file has mode %v", fi.Mode().Perm())
	}
	restarted := newSizer(cache, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if got := restarted.state(dir); got.Bytes != 3000 || got.MeasuredAt.IsZero() || got.State != "" {
		t.Errorf("after a restart: %+v", got)
	}
}

// The gate tells why it cannot measure: a share that is not mounted in its
// container, a directory that does not exist, a symbolic link.
func TestSizeErrors(t *testing.T) {
	st, g, ask, share := sizeSetup(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.Run(ctx)

	ask(filepath.Join(share, "LIVE-GONE"), true)
	if ds := waitMeasured(t, ask, filepath.Join(share, "LIVE-GONE")); !strings.Contains(ds.Error, "does not exist") {
		t.Errorf("missing directory: %+v", ds)
	}

	outside := filepath.Join(t.TempDir(), "outside")
	write(t, filepath.Join(outside, "secret.bin"), 5000)
	if err := os.Symlink(outside, filepath.Join(share, "LIVE-LINK")); err == nil {
		ask(filepath.Join(share, "LIVE-LINK", "deeper"), true)
		if ds := waitMeasured(t, ask, filepath.Join(share, "LIVE-LINK", "deeper")); !strings.Contains(ds.Error, "symbolic link") || ds.Bytes != 0 {
			t.Errorf("through a symbolic link: %+v", ds)
		}
		ask(filepath.Join(share, "LIVE-LINK"), true)
		if ds := waitMeasured(t, ask, filepath.Join(share, "LIVE-LINK")); !strings.Contains(ds.Error, "symbolic link") || ds.Bytes != 0 {
			t.Errorf("a symbolic link: %+v", ds)
		}
	} else {
		t.Logf("no symbolic links on this system: %v", err)
	}

	// Syncthing sees a share that the gate container does not have.
	missing := filepath.Join(t.TempDir(), "not-mounted")
	st.set(string(filepath.Separator), []string{filepath.Join(share, "LIVE-A"), filepath.Join(missing, "LIVE-C")})
	g.rmu.Lock()
	g.rootsAt, g.tryAt = time.Time{}, time.Time{} // read the folder list again
	g.rmu.Unlock()
	ask(filepath.Join(missing, "LIVE-D"), true)
	if ds := waitMeasured(t, ask, filepath.Join(missing, "LIVE-D")); !strings.Contains(ds.Error, "mount it read-only in the gate container") {
		t.Errorf("share not mounted: %+v", ds)
	}
}

// A measurement that takes longer than its budget stops, and its result is
// a lower limit.
func TestSizeBudget(t *testing.T) {
	every, budget := sizePauseEvery, sizeBudget
	sizePauseEvery, sizeBudget = 1, 0
	t.Cleanup(func() { sizePauseEvery, sizeBudget = every, budget })
	_, g, ask, share := sizeSetup(t, "")
	dir := filepath.Join(share, "LIVE-BIG")
	for i := range 5 {
		write(t, filepath.Join(dir, "f"+string(rune('a'+i))), 100)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go g.Run(ctx)
	ask(dir, true)
	if ds := waitMeasured(t, ask, dir); ds.Complete || ds.Error != "" {
		t.Errorf("stopped measurement: %+v", ds)
	}
}
