package app

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A project folder outside Syncthing that goes from its share: the scan
// finds it deleted (a yellow box, S8), and a restart of syncwatch keeps it
// before the first contact: S8 stays open, without a "resolved" message.
// ✕ then removes it.
func TestDeletedFolderOverRestart(t *testing.T) {
	fake := newFixtureST(t)
	srv := httptest.NewTLSServer(fake)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	boot := filepath.Join(dir, "syncwatch.yaml")
	yaml := fmt.Sprintf("servers: [{name: AKKA, url: %q, api_key: k}]\n", srv.URL) +
		"structure: {project_pattern: '^(ME-)?LIVE-[A-Za-z0-9]'}\ncollector: {event_timeout: 5s}\naccess: {mode: proxy}\n"
	if err := os.WriteFile(boot, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs lockedBuffer
	o := Options{DataDir: filepath.Join(dir, "data"), ConfigFile: boot, Log: slog.New(slog.NewTextHandler(&logs, nil))}
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar, Timeout: 30 * time.Second}
	get := func(base, path string) string {
		t.Helper()
		resp, err := client.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	waitFor := func(base, path, want string) string {
		t.Helper()
		var page string
		for deadline := time.Now().Add(30 * time.Second); ; {
			if page = get(base, path); strings.Contains(page, want) {
				return page
			}
			if time.Now().After(deadline) {
				t.Fatalf("no %q on %s:\n%s\nlog:\n%s", want, path, page, logs.String())
			}
			time.Sleep(200 * time.Millisecond)
		}
	}
	const (
		share = `D:\tmp\swsim\st\akka\share\AKKA-LIVE-1SOURCE`
		s7    = `Directory LIVE-NOTSYNCED-1SOURCE in share AKKA-LIVE-1SOURCE on AKKA:`
		s8    = `Project folder LIVE-NOTSYNCED-1SOURCE in share AKKA-LIVE-1SOURCE on AKKA is deleted.`
		box   = `<span aria-hidden="true">⚠</span> deleted</span>`
	)

	addr, stop := runUntilListening(t, o)
	base := "http://" + addr
	page := waitFor(base, "/problems", s7)
	fake.remove(share, "LIVE-NOTSYNCED-1SOURCE")
	post := func(path string, form url.Values) string {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, base+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", base)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST %s: HTTP %d\n%s", path, resp.StatusCode, b)
		}
		return string(b)
	}
	post("/structure/scan", url.Values{"csrf": {csrfField.FindStringSubmatch(page)[1]}, "server": {"akka"}, "back": {"/problems"}})
	waitFor(base, "/problems", s8)
	waitFor(base, "/grid", box)
	stop()

	// The restart: without a contact, the grid and the findings have the
	// deleted folder.
	srv.Close()
	logs.mu.Lock()
	logs.buf.Reset()
	logs.mu.Unlock()
	addr, stop = runUntilListening(t, o)
	defer stop()
	base = "http://" + addr
	// The server is down now: only the store can give them.
	waitFor(base, "/grid", box)
	waitFor(base, "/problems", s8)
	time.Sleep(time.Second) // a few evaluations
	if strings.Contains(logs.String(), `msg="finding resolved" check=S8`) {
		t.Fatalf("S8 resolved after the restart:\n%s", logs.String())
	}

	// ✕ removes it, and S8 resolves.
	grid := get(base, "/grid")
	post("/grid/forget", url.Values{"csrf": {csrfField.FindStringSubmatch(grid)[1]}, "server": {"akka"}, "path": {share + `\LIVE-NOTSYNCED-1SOURCE`}, "back": {"/grid"}})
	for deadline := time.Now().Add(10 * time.Second); !strings.Contains(logs.String(), `msg="finding resolved" check=S8`); {
		if time.Now().After(deadline) {
			t.Fatalf("S8 still open after ✕\nlog:\n%s", logs.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if strings.Contains(get(base, "/grid"), box) {
		t.Error("the yellow box is still there after ✕")
	}
}
