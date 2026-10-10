package gate

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/stclient"
)

const (
	realKey = "syncthing-real-api-key-0123456789"
	token   = "gate-token-for-syncwatch-0123456789"
)

// fakeSyncthing answers the allowlisted endpoints and records what it got.
type fakeSyncthing struct {
	mu      sync.Mutex
	reqs    []*http.Request
	sep     string   // pathSeparator in /rest/system/status; "/" when empty
	folders []string // folder paths in /rest/config/folders; /s/LIVE-F when nil
	down    bool     // answer 500 to everything
}

func (f *fakeSyncthing) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.reqs = append(f.reqs, r.Clone(context.Background()))
	sep, folders, down := f.sep, f.folders, f.down
	f.mu.Unlock()
	if r.URL.Path != "/rest/noauth/health" && r.Header.Get("X-API-Key") != realKey {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if down {
		http.Error(w, "down", http.StatusInternalServerError)
		return
	}
	if sep == "" {
		sep = "/"
	}
	if folders == nil {
		folders = []string{"/s/LIVE-F"}
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/rest/noauth/health":
		_, _ = w.Write([]byte(`{"status":"OK"}`))
	case "/rest/system/status":
		s, _ := json.Marshal(sep)
		_, _ = w.Write([]byte(`{"myID":"AAAA","startTime":"2026-10-04T10:00:00Z","pathSeparator":` + string(s) + `}`))
	case "/rest/events":
		_, _ = w.Write([]byte(`[{"id":1,"type":"StateChanged","time":"2026-10-04T10:00:00Z","data":{"folder":"f","from":"idle","to":"scanning"}},
			{"id":2,"type":"ConfigSaved","time":"2026-10-04T10:00:01Z","data":{"gui":{"apiKey":"` + realKey + `","password":"$2a$hash"}}}]`))
	case "/rest/config/folders":
		// The first folder carries the devices, one with a secret.
		var out []string
		for i, p := range folders {
			s, _ := json.Marshal(p)
			f := `{"id":"f` + strconv.Itoa(i) + `","label":"LIVE-F","path":` + string(s) + `,"rescanIntervalS":3600`
			if i == 0 {
				f += `,"devices":[{"deviceID":"AAAA","encryptionPassword":""},{"deviceID":"BBBB","encryptionPassword":"untrusted-secret"}]`
			}
			out = append(out, f+"}")
		}
		_, _ = w.Write([]byte("[" + strings.Join(out, ",") + "]"))
	case "/rest/system/browse":
		s, _ := json.Marshal(r.URL.Query().Get("current") + "sub")
		_, _ = w.Write([]byte("[" + string(s) + "]"))
	case "/rest/system/shutdown", "/rest/config":
		t := "must never be reached"
		_, _ = w.Write([]byte(`{"error":"` + t + `"}`))
	default:
		_, _ = w.Write([]byte(`{}`))
	}
}

// set changes the path separator and folder list Syncthing reports.
func (f *fakeSyncthing) set(sep string, folders []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sep, f.folders = sep, folders
}

func (f *fakeSyncthing) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

// countPath counts the requests for one path.
func (f *fakeSyncthing) countPath(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.reqs {
		if r.URL.Path == path {
			n++
		}
	}
	return n
}

func (f *fakeSyncthing) last() *http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reqs[len(f.reqs)-1]
}

func setup(t *testing.T) (*fakeSyncthing, *Gate, *httptest.Server) {
	t.Helper()
	st := &fakeSyncthing{}
	up := httptest.NewTLSServer(st)
	t.Cleanup(up.Close)
	g, err := New(Options{Syncthing: up.URL, APIKey: realKey, Token: token, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	gs := httptest.NewTLSServer(g)
	t.Cleanup(gs.Close)
	return st, g, gs
}

func get(t *testing.T, gs *httptest.Server, method, path, tok string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, gs.URL+path, nil)
	if tok != "" {
		req.Header.Set("X-API-Key", tok)
	}
	resp, err := gs.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestGateOnlyPassesAllowlistedGETs(t *testing.T) {
	st, _, gs := setup(t)
	for _, c := range []struct {
		method, path string
		want         int
	}{
		{http.MethodPost, "/rest/system/status", http.StatusMethodNotAllowed},
		{http.MethodPut, "/rest/config/folders", http.StatusMethodNotAllowed},
		{http.MethodDelete, "/rest/config/folders", http.StatusMethodNotAllowed},
		{http.MethodGet, "/rest/config", http.StatusNotFound},
		{http.MethodGet, "/rest/system/shutdown", http.StatusNotFound},
		{http.MethodGet, "/rest/system/config", http.StatusNotFound},
		{http.MethodGet, "/rest/events/../config", http.StatusNotFound},
		{http.MethodGet, "/rest/config/folders/f", http.StatusNotFound},
		{http.MethodGet, "/rest%2Fconfig", http.StatusNotFound},
		{http.MethodGet, "/", http.StatusNotFound},
	} {
		if code, _ := get(t, gs, c.method, c.path, token); code != c.want {
			t.Errorf("%s %s: %d, want %d", c.method, c.path, code, c.want)
		}
	}
	if n := st.count(); n != 0 {
		t.Fatalf("%d refused requests reached Syncthing", n)
	}
}

func TestGateToken(t *testing.T) {
	st, _, gs := setup(t)
	for _, tok := range []string{"", "wrong", realKey} {
		if code, _ := get(t, gs, http.MethodGet, "/rest/system/status", tok); code != http.StatusForbidden {
			t.Errorf("token %q: %d", tok, code)
		}
	}
	if st.count() != 0 {
		t.Fatal("a request without the gate token reached Syncthing")
	}
	code, body := get(t, gs, http.MethodGet, "/rest/system/status", token)
	if code != 200 || !strings.Contains(body, "AAAA") {
		t.Fatalf("with the token: %d %s", code, body)
	}
	if k := st.last().Header.Get("X-API-Key"); k != realKey {
		t.Fatalf("Syncthing got key %q", k)
	}
	// Syncthing's health needs the token too, but Syncthing gets no key for it.
	if code, _ := get(t, gs, http.MethodGet, "/rest/noauth/health", token); code != 200 || st.last().Header.Get("X-API-Key") != "" {
		t.Fatalf("health: %d, key sent %q", code, st.last().Header.Get("X-API-Key"))
	}
	// The gate's own health answer, for the container healthcheck.
	if code, _ := get(t, gs, http.MethodGet, "/healthz", ""); code != 200 {
		t.Fatalf("gate healthz: %d", code)
	}
}

// Without the token every answer is the same 403, so the allowlist, the
// methods and Syncthing's health can't be probed; only /healthz is open.
func TestGateTokenBeforePath(t *testing.T) {
	st, _, gs := setup(t)
	for _, c := range []struct{ method, path, tok string }{
		{http.MethodGet, "/rest/system/status", ""},
		{http.MethodGet, "/rest/config", ""},
		{http.MethodGet, "/nothing", ""},
		{http.MethodPost, "/rest/system/status", ""},
		{http.MethodPost, "/healthz", ""},
		{http.MethodGet, "/rest/noauth/health", ""},
		{http.MethodGet, "/rest/noauth/health", "wrong"},
		{http.MethodGet, "/rest/system/status?x=1", realKey},
	} {
		if code, _ := get(t, gs, c.method, c.path, c.tok); code != http.StatusForbidden {
			t.Errorf("%s %s token %q: %d, want 403", c.method, c.path, c.tok, code)
		}
	}
	if st.count() != 0 {
		t.Fatal("a request without the gate token reached Syncthing")
	}
	if code, body := get(t, gs, http.MethodGet, "/healthz", ""); code != 200 || body != "ok\n" {
		t.Fatalf("healthz: %d %q", code, body)
	}
}

func TestGateRemovesSecrets(t *testing.T) {
	st, _, gs := setup(t)
	mask := strings.Join(stclient.EventMask, ",")
	code, body := get(t, gs, http.MethodGet, "/rest/events?since=0&timeout=1&events="+mask, token)
	if code != 200 || strings.Contains(body, realKey) || strings.Contains(body, "hash") {
		t.Fatalf("events: %d %s", code, body)
	}
	var evs []map[string]any
	if err := json.Unmarshal([]byte(body), &evs); err != nil || len(evs) != 2 || evs[0]["data"] == nil || evs[1]["data"] != nil {
		t.Fatalf("events after cleaning: %v %s", err, body)
	}
	if q := st.last().URL.Query(); q.Get("since") != "0" || q.Get("timeout") != "1" || q.Get("events") != mask || len(q) != 3 {
		t.Fatalf("query not passed on: %q", st.last().URL.RawQuery)
	}
	code, body = get(t, gs, http.MethodGet, "/rest/config/folders", token)
	if code != 200 || strings.Contains(body, "untrusted-secret") || strings.Contains(body, "encryptionPassword") {
		t.Fatalf("folders: %d %s", code, body)
	}
	if !strings.Contains(body, `"rescanIntervalS":3600`) || !strings.Contains(body, `"deviceID":"BBBB"`) {
		t.Fatalf("folders lost other fields: %s", body)
	}
}

// Only the parameters syncwatch's client sends pass, with the values it
// sends; everything else is a 400 that never reaches Syncthing.
func TestGateQueryAllowlist(t *testing.T) {
	st, _, gs := setup(t)
	mask := strings.Join(stclient.EventMask, ",")
	passed := 0
	for _, c := range []struct {
		path string
		want int
	}{
		{"/rest/system/status", 200},
		{"/rest/system/status?", 200},
		{"/rest/system/status?folder=f", 400},
		{"/rest/system/version?x", 400},
		{"/metrics?x=1", 400},
		{"/rest/noauth/health?x=1", 400},
		{"/rest/db/status?folder=f", 200},
		{"/rest/db/status?folder=f&folder=g", 400},
		{"/rest/db/status?folder=f&device=D", 400},
		{"/rest/db/completion?folder=f&device=D", 200},
		{"/rest/db/completion?folder=f&device=D&x=", 400},
		{"/rest/folder/errors?folder=f&page=1&perpage=5", 200},
		{"/rest/folder/errors?folder=f&page=0&perpage=5", 400},
		{"/rest/folder/errors?folder=f&page=1&perpage=x", 400},
		{"/rest/events", 200},
		{"/rest/events?since=0&timeout=1&events=" + mask, 200},
		{"/rest/events?since=7&timeout=600", 200},
		{"/rest/events?timeout=601", 400},
		{"/rest/events?timeout=-1", 400},
		{"/rest/events?timeout=1s", 400},
		{"/rest/events?since=x", 400},
		{"/rest/events?since=-1", 400},
		{"/rest/events?limit=5", 400},
		{"/rest/events?events=StartupComplete", 400},
		{"/rest/events?events=StateChanged,ConfigSaved", 400},
		{"/rest/events?events=" + mask + ",LoginAttempt", 400},
		{"/rest/events?events=" + mask + ",", 400},
		{"/rest/events?events=" + mask + "&events=" + mask, 400},
		{"/rest/system/status?%zz", 400},
		{"/rest/system/status?a;b", 400},
	} {
		code, body := get(t, gs, http.MethodGet, c.path, token)
		if code != c.want {
			t.Errorf("%s: %d %s, want %d", c.path, code, strings.TrimSpace(body), c.want)
		}
		if code == 200 {
			passed++
		} else if !strings.HasPrefix(body, "syncwatch gate: query not allowed: ") {
			t.Errorf("%s: body %q", c.path, body)
		}
	}
	if n := st.count(); n != passed {
		t.Fatalf("%d requests reached Syncthing, %d passed", n, passed)
	}
	// The mask may come in any order; what was checked is what is sent.
	rev := slices.Clone(stclient.EventMask)
	slices.Reverse(rev)
	if code, _ := get(t, gs, http.MethodGet, "/rest/events?events="+strings.Join(rev, ",")+"&since=3", token); code != 200 {
		t.Fatalf("reversed mask: %d", code)
	}
	if q := st.last().URL.Query(); q.Get("events") != strings.Join(rev, ",") || q.Get("since") != "3" || len(q) != 2 {
		t.Fatalf("forwarded query %q", st.last().URL.RawQuery)
	}
}

// browse lists only directories under the parents of Syncthing's folders
// and under the extra roots; the path is cleaned before it is checked and
// the cleaned path is what Syncthing gets.
func TestGateBrowseRoots(t *testing.T) {
	st, g, gs := setup(t)
	st.set("/", []string{"/s/LIVE-F", "/srv/data/x", "/top", "rel", "~/Sync", "/"})
	g.o.BrowseRoots = []string{"/extra", " "}
	type tc struct {
		current, fwd string
		want         int
	}
	run := func(cases []tc) {
		t.Helper()
		for _, c := range cases {
			reached := st.countPath("/rest/system/browse")
			code, body := get(t, gs, http.MethodGet, "/rest/system/browse?current="+url.QueryEscape(c.current), token)
			if code != c.want {
				t.Errorf("current=%q: %d %s, want %d", c.current, code, strings.TrimSpace(body), c.want)
				continue
			}
			if c.want != 200 {
				if st.countPath("/rest/system/browse") != reached {
					t.Errorf("current=%q: refused but reached Syncthing", c.current)
				}
				continue
			}
			if got := st.last().URL.Query().Get("current"); got != c.fwd {
				t.Errorf("current=%q: Syncthing got %q, want %q", c.current, got, c.fwd)
			}
			if !strings.Contains(body, "sub") {
				t.Errorf("current=%q: answer %q", c.current, body)
			}
		}
	}
	run([]tc{
		{"/s/", "/s/", 200},
		{"/s/LIVE-F/", "/s/LIVE-F/", 200},
		{"/s/LIVE-F/deep/er/", "/s/LIVE-F/deep/er/", 200},
		{"/s//LIVE-F//", "/s/LIVE-F/", 200},
		{"/s/./LIVE-F/../LIVE-G/", "/s/LIVE-G/", 200},
		{"/srv/data/", "/srv/data/", 200},
		{"/extra/", "/extra/", 200},
		{"/extra/a/", "/extra/a/", 200},
		{"~/", "~/", 200},
		{"~/x/", "~/x/", 200},
		{"~/../~/x/", "~/x/", 200},
		{`/s/a\b/`, `/s/a\b/`, 200}, // a backslash is an ordinary character here
		{"/", "", 403},
		{"//", "", 403},
		{"/s/../", "", 403},
		{"/s/LIVE-F/../../etc/", "", 403},
		{"/s/LIVE-F/../../../", "", 403},
		{"/srv/", "", 403},
		{"/S/", "", 403},
		{"/top/", "", 403}, // a folder directly under /: its parent is never listed
		{"/extra2/", "", 403},
		{`/s\..\..\`, "", 403}, // ends with a separator only on Windows
		{"rel/", "", 403},
		{"./", "", 403},
		{"../s/", "", 403},
		{"~", "", 400},
		{"/s", "", 400},
		{"/s/LIVE", "", 400},
		{"", "", 400},
	})
	if code, _ := get(t, gs, http.MethodGet, "/rest/system/browse", token); code != 400 {
		t.Errorf("no current: %d", code)
	}
	if code, _ := get(t, gs, http.MethodGet, "/rest/system/browse?current=/s/&filesystem=fake", token); code != 400 {
		t.Errorf("filesystem parameter: %d", code)
	}

	// A Windows Syncthing: both separators, no case, drives and UNC paths.
	st, g, gs = setup(t)
	st.set(`\`, []string{`D:\Shares\LIVE-F`, `\\nas\share\x`, `C:\`, `E:\Sync\`})
	g.o.BrowseRoots = []string{`F:/extra`}
	run([]tc{
		{`D:\Shares\`, `D:\Shares\`, 200},
		{`d:/shares/live-f/`, `d:\shares\live-f\`, 200},
		{`D:\Shares\LIVE-F\sub/`, `D:\Shares\LIVE-F\sub\`, 200},
		{`D:\Shares\..\Shares\.\LIVE-G\`, `D:\Shares\LIVE-G\`, 200},
		{`\\nas\share\`, `\\nas\share\`, 200},
		{`//nas/share/x/y/`, `\\nas\share\x\y\`, 200},
		{`C:\anything\at\all\`, `C:\anything\at\all\`, 200}, // the folder is the drive root
		{`E:\`, `E:\`, 200},
		{`F:\extra\sub\`, `F:\extra\sub\`, 200},
		{`D:\`, "", 403},
		{`D:\Shares\..\`, "", 403},
		{`D:\Shares\LIVE-F\..\..\Private\`, "", 403},
		{`D:\Shares2\`, "", 403},
		{`\\nas\`, "", 403},
		{`\\nas\other\`, "", 403},
		{`\\?\D:\Shares\`, "", 403},
		{`D:Shares\`, "", 403},
		{`\Shares\`, "", 403},
		{`/Shares/`, "", 403},
		{`~\`, "", 403},
		{`D:\Shares`, "", 400},
	})
}

// The folder list is read on the first browse request and again after ten
// minutes, or after one minute when a directory isn't under it.
func TestGateBrowseRootsRefresh(t *testing.T) {
	st, g, gs := setup(t)
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	g.now = func() time.Time { return now }
	browse := func(dir string) int {
		t.Helper()
		code, _ := get(t, gs, http.MethodGet, "/rest/system/browse?current="+url.QueryEscape(dir), token)
		return code
	}
	reads := func() int { return st.countPath("/rest/config/folders") }
	if code := browse("/s/"); code != 200 || reads() != 1 {
		t.Fatalf("first browse: %d, folder list read %d times", code, reads())
	}
	st.set("/", []string{"/s/LIVE-F", "/t/X"})
	if code := browse("/t/"); code != 403 || reads() != 1 {
		t.Fatalf("new folder within a minute: %d, reads %d", code, reads())
	}
	now = now.Add(61 * time.Second)
	if code := browse("/t/"); code != 200 || reads() != 2 {
		t.Fatalf("new folder after a minute: %d, reads %d", code, reads())
	}
	now = now.Add(5 * time.Minute)
	if code := browse("/s/"); code != 200 || reads() != 2 {
		t.Fatalf("within ten minutes: %d, reads %d", code, reads())
	}
	now = now.Add(5 * time.Minute)
	if code := browse("/s/"); code != 200 || reads() != 3 {
		t.Fatalf("after ten minutes: %d, reads %d", code, reads())
	}
	// Misses don't make it read the list more than once a minute.
	for i := 0; i < 3; i++ {
		if code := browse("/nowhere/"); code != 403 {
			t.Fatalf("miss: %d", code)
		}
	}
	if reads() != 3 {
		t.Fatalf("misses read the list %d times", reads()-3)
	}
}

// While Syncthing's folder list can't be read, browse answers 502 (not
// 403, not a listing); the read is retried after a while.
func TestGateBrowseWithoutFolderList(t *testing.T) {
	st, g, gs := setup(t)
	now := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	g.now = func() time.Time { return now }
	st.down = true
	for i := 0; i < 2; i++ {
		code, body := get(t, gs, http.MethodGet, "/rest/system/browse?current=/s/", token)
		if code != http.StatusBadGateway || !strings.Contains(body, "folder list") {
			t.Fatalf("down: %d %s", code, body)
		}
	}
	if n := st.countPath("/rest/system/status"); n != 1 {
		t.Fatalf("read attempts within the retry wait: %d", n)
	}
	st.mu.Lock()
	st.down = false
	st.mu.Unlock()
	now = now.Add(31 * time.Second)
	if code, _ := get(t, gs, http.MethodGet, "/rest/system/browse?current=/s/", token); code != 200 {
		t.Fatalf("after Syncthing came back: %d", code)
	}
	if st.countPath("/rest/system/browse") != 1 {
		t.Fatal("a browse reached Syncthing while its folder list was unknown")
	}
}

// syncwatch's own client works through the gate with the gate token.
func TestClientThroughGate(t *testing.T) {
	_, _, gs := setup(t)
	cl, err := stclient.New(stclient.Options{Name: "NAS", URL: gs.URL, APIKey: token, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if st, err := cl.SystemStatus(ctx); err != nil || st.MyID != "AAAA" {
		t.Fatalf("status: %+v %v", st, err)
	}
	fs, err := cl.ConfigFolders(ctx)
	if err != nil || len(fs) != 1 || len(fs[0].Devices) != 2 {
		t.Fatalf("folders: %+v %v", fs, err)
	}
	evs, err := cl.Events(ctx, 0, time.Second, stclient.EventMask)
	if err != nil || len(evs) != 2 {
		t.Fatalf("events: %v %v", evs, err)
	}
	if cl.LastCert().Fingerprint == "" {
		t.Fatal("the client didn't see the gate's certificate")
	}
}

// Off loopback, Syncthing's certificate is pinned on first use and a
// changed one is refused.
func TestGatePinsSyncthing(t *testing.T) {
	_, g, gs := setup(t)
	var pinned string
	g.loopback = false
	g.o.OnPin = func(fp string) { pinned = fp }
	if code, _ := get(t, gs, http.MethodGet, "/rest/system/status", token); code != 200 || pinned == "" {
		t.Fatalf("first use: %d, pinned %q", code, pinned)
	}
	// httptest servers share one certificate; this one gets its own.
	other := httptest.NewUnstartedServer(&fakeSyncthing{})
	other.TLS = &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}}
	other.StartTLS()
	defer other.Close()
	g.base.Host = strings.TrimPrefix(other.URL, "https://")
	g.hc.CloseIdleConnections()
	code, body := get(t, gs, http.MethodGet, "/rest/system/status", token)
	if code != http.StatusBadGateway || !strings.Contains(body, "certificate changed") {
		t.Fatalf("changed certificate: %d %s", code, body)
	}
}

func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "syncthing"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestNewChecksOptions(t *testing.T) {
	for _, o := range []Options{
		{Syncthing: "ftp://x", APIKey: "k", Token: token},
		{Syncthing: "https://127.0.0.1:8384", Token: token},
		{Syncthing: "https://127.0.0.1:8384", APIKey: "k", Token: "short"},
	} {
		if _, err := New(o); err == nil {
			t.Errorf("accepted %+v", o)
		}
	}
}
