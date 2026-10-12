package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// fixtureST is a fake Syncthing for Run: it answers with the recorded
// fixtures of test/fixtures/sim-akka, and a test can change the listings of
// the shares.
type fixtureST struct {
	t      *testing.T
	dir    string
	mu     sync.Mutex
	browse map[string][]string // the listing of each directory
}

func newFixtureST(t *testing.T) *fixtureST {
	f := &fixtureST{t: t, dir: filepath.Join("..", "..", "test", "fixtures", "sim-akka")}
	if err := json.Unmarshal(f.fixture("browse"), &f.browse); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixtureST) fixture(name string) []byte {
	b, err := os.ReadFile(filepath.Join(f.dir, name+".json"))
	if err != nil {
		f.t.Error(err)
	}
	return b
}

// remove takes a directory out of the listing of its share: the data
// steward corrects a mistake.
func (f *fixtureST) remove(share, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var keep []string
	for _, p := range f.browse[share] {
		if p != share+`\`+name {
			keep = append(keep, p)
		}
	}
	f.browse[share] = keep
}

func (f *fixtureST) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/rest/system/status", "/rest/system/version", "/rest/config/folders", "/rest/config/devices", "/rest/stats/device", "/rest/stats/folder":
		_, _ = w.Write(f.fixture(strings.NewReplacer("/rest/", "", "/", "-").Replace(r.URL.Path)))
	case "/rest/system/connections":
		fmt.Fprintf(w, `{"connections": %s, "total": {}}`, f.fixture("connections"))
	case "/rest/cluster/pending/devices", "/rest/cluster/pending/folders":
		_, _ = w.Write([]byte(`{}`))
	case "/rest/db/status":
		var all map[string]json.RawMessage
		_ = json.Unmarshal(f.fixture("db-status"), &all)
		_, _ = w.Write(all[q.Get("folder")])
	case "/rest/db/completion":
		var all []map[string]any
		_ = json.Unmarshal(f.fixture("completion"), &all)
		for _, c := range all {
			if c["Folder"] == q.Get("folder") && c["Device"] == q.Get("device") {
				delete(c, "Folder")
				delete(c, "Device")
				_ = json.NewEncoder(w).Encode(c)
				return
			}
		}
		_, _ = w.Write([]byte(`{"completion":100,"remoteState":"unknown"}`))
	case "/rest/db/remoteneed":
		_, _ = w.Write([]byte(`{"files":[],"page":1,"perpage":20}`))
	case "/rest/folder/errors":
		_, _ = w.Write([]byte(`{"errors":[]}`))
	case "/metrics":
		var c map[string]int
		_ = json.Unmarshal(f.fixture("conflicts"), &c)
		for k, v := range c {
			fmt.Fprintf(w, "syncthing_model_folder_conflicts_total{folder=%q} %d\n", k, v)
		}
	case "/rest/system/browse":
		f.mu.Lock()
		out := f.browse[strings.TrimRight(q.Get("current"), `\`)]
		f.mu.Unlock()
		if out == nil {
			out = []string{}
		}
		_ = json.NewEncoder(w).Encode(out)
	case "/rest/events":
		// No events: the long-poll ends after 1 second.
		select {
		case <-time.After(time.Second):
		case <-r.Context().Done():
			return
		}
		_, _ = w.Write([]byte(`[]`))
	default:
		http.NotFound(w, r)
	}
}

var csrfField = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

// "Scan now" from the dashboard reaches the collector of the server
// (Hooks.ScanNow: mgr.ScanNow). Run polls a fake Syncthing, the data steward
// corrects the mistake of an S4, and an administrator selects Scan now: the
// S4 resolves at once, and the INFO log line names the login, password or
// proxy. Without the hook, each press answers "syncwatch is not connected
// to server AKKA now" (review of W-056, finding 5).
func TestScanNowReachesTheCollector(t *testing.T) {
	for _, login := range []string{"password", "proxy"} {
		t.Run(login, func(t *testing.T) {
			fake := newFixtureST(t)
			srv := httptest.NewTLSServer(fake)
			defer srv.Close()
			dir := t.TempDir()
			yaml := fmt.Sprintf("servers: [{name: AKKA, url: %q, api_key: k}]\n", srv.URL) +
				"structure: {project_pattern: '^(ME-)?LIVE-[A-Za-z0-9]'}\ncollector: {event_timeout: 5s}\n"
			if login == "proxy" {
				yaml += "access: {mode: proxy}\n"
			}
			boot := filepath.Join(dir, "syncwatch.yaml")
			if err := os.WriteFile(boot, []byte(yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			var logs lockedBuffer
			addr := make(chan string, 1)
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() {
				done <- Run(ctx, Options{DataDir: filepath.Join(dir, "data"), Listen: "127.0.0.1:0", ConfigFile: boot, AdminPassword: "scan now password",
					Log: slog.New(slog.NewTextHandler(&logs, nil)), OnListen: func(a string) { addr <- a }})
			}()
			defer func() {
				cancel()
				select {
				case <-done:
				case <-time.After(20 * time.Second):
					t.Error("Run did not return")
				}
			}()
			var base string
			select {
			case a := <-addr:
				base = "http://" + a
			case err := <-done:
				t.Fatalf("Run returned: %v", err)
			case <-time.After(30 * time.Second):
				t.Fatal("no listener")
			}

			jar, _ := cookiejar.New(nil)
			client := &http.Client{Jar: jar, Timeout: 30 * time.Second}
			do := func(method, path string, form url.Values) string {
				t.Helper()
				req, _ := http.NewRequest(method, base+path, strings.NewReader(form.Encode()))
				if method == http.MethodPost {
					req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
					req.Header.Set("Origin", base)
				}
				resp, err := client.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer resp.Body.Close()
				b, _ := io.ReadAll(resp.Body)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("%s %s: HTTP %d\n%s", method, path, resp.StatusCode, b)
				}
				return string(b)
			}
			csrf := func(page string) string {
				t.Helper()
				m := csrfField.FindStringSubmatch(page)
				if m == nil {
					t.Fatalf("no CSRF token on the page:\n%s", page)
				}
				return m[1]
			}
			if login == "password" {
				do(http.MethodPost, "/login", url.Values{"csrf": {csrf(do(http.MethodGet, "/login", nil))}, "password": {"scan now password"}})
			}

			// The timed scan at the start finds the mistake.
			const s4 = `<span class="msg">Directory random stuff in share AKKA-LIVE-1SOURCE on AKKA:`
			var page string
			for deadline := time.Now().Add(30 * time.Second); ; {
				if page = do(http.MethodGet, "/problems", nil); strings.Contains(page, s4) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("no S4 for the directory random stuff:\n%s\nlog:\n%s", page, logs.String())
				}
				time.Sleep(200 * time.Millisecond)
			}

			fake.remove(`D:\tmp\swsim\st\akka\share\AKKA-LIVE-1SOURCE`, "random stuff")
			page = do(http.MethodPost, "/structure/scan", url.Values{"csrf": {csrf(page)}, "server": {"akka"}, "back": {"/problems"}})
			if want := "The structure scan of server AKKA is complete. syncwatch resolved 1 finding."; !strings.Contains(page, want) || strings.Contains(page, s4) {
				t.Fatalf("the page after Scan now must have %q, and no S4:\n%s\nlog:\n%s", want, page, logs.String())
			}
			line := `level=INFO msg="administrator started a structure scan" server=AKKA role=admin login=` + login + ` ip=127.0.0.1`
			if !strings.Contains(logs.String(), line) {
				t.Fatalf("no %q in the log:\n%s", line, logs.String())
			}
		})
	}
}
