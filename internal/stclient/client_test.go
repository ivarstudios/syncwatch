package stclient

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type recorded struct {
	method, path, query, apiKey string
}

func fakeSyncthing(t *testing.T) (*httptest.Server, *[]recorded, *sync.Mutex) {
	t.Helper()
	var mu sync.Mutex
	var reqs []recorded
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		reqs = append(reqs, recorded{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("X-API-Key")})
		mu.Unlock()
		switch r.URL.Path {
		case "/rest/noauth/health":
			_, _ = w.Write([]byte(`{"status":"OK"}`))
		case "/rest/system/browse":
			_, _ = w.Write([]byte(`["/a/b"]`))
		case "/rest/config/folders", "/rest/config/devices":
			_, _ = w.Write([]byte(`[{"id":"f","devices":[{"deviceID":"D","encryptionPassword":"secret"}]}]`))
		case "/rest/events":
			_, _ = w.Write([]byte(`[{"id":1,"type":"ConfigSaved","data":{"gui":{"apiKey":"leak"}}},{"id":2,"type":"StateChanged","data":{"folder":"f"}}]`))
		case "/metrics":
			_, _ = w.Write([]byte("# HELP x\nsyncthing_model_folder_conflicts_total{folder=\"a\\\"b\"} 3\nsyncthing_model_folder_conflicts_total{folder=\"c\"} 0\n"))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs, &mu
}

// callAll calls every exported request method of the client.
func callAll(t *testing.T, c *Client) {
	t.Helper()
	ctx := context.Background()
	steps := map[string]func() error{
		"Health":           func() error { return c.Health(ctx) },
		"SystemStatus":     func() error { _, err := c.SystemStatus(ctx); return err },
		"SystemVersion":    func() error { _, err := c.SystemVersion(ctx); return err },
		"Connections":      func() error { _, err := c.Connections(ctx); return err },
		"Traffic":          func() error { _, err := c.Traffic(ctx); return err },
		"DeviceStats":      func() error { _, err := c.DeviceStats(ctx); return err },
		"FolderStats":      func() error { _, err := c.FolderStats(ctx); return err },
		"ConfigFolders":    func() error { _, err := c.ConfigFolders(ctx); return err },
		"ConfigDevices":    func() error { _, err := c.ConfigDevices(ctx); return err },
		"DBStatus":         func() error { _, err := c.DBStatus(ctx, "f"); return err },
		"FolderErrors":     func() error { _, err := c.FolderErrors(ctx, "f", 5); return err },
		"Completion":       func() error { _, err := c.Completion(ctx, "f", "D"); return err },
		"RemoteNeed":       func() error { _, err := c.RemoteNeed(ctx, "f", "D", 20); return err },
		"PendingDevices":   func() error { _, err := c.PendingDevices(ctx); return err },
		"PendingFolders":   func() error { _, err := c.PendingFolders(ctx); return err },
		"Browse":           func() error { _, err := c.Browse(ctx, "/a", "/"); return err },
		"Events":           func() error { _, err := c.Events(ctx, 0, time.Second, EventMask); return err },
		"ConflictCounters": func() error { _, err := c.ConflictCounters(ctx); return err },
		"Size":             func() error { _, err := c.Size(ctx, "/a/b", true); return err },
	}
	// Every exported method that talks to the server must be listed above, so a
	// new method can't be added without this test exercising it.
	accessors := map[string]bool{"Name": true, "Pin": true, "SetPin": true, "LastCert": true, "Close": true}
	typ := reflect.TypeOf(c)
	for i := 0; i < typ.NumMethod(); i++ {
		name := typ.Method(i).Name
		if _, ok := steps[name]; !ok && !accessors[name] {
			t.Errorf("exported method %s is not covered by the allowlist test", name)
		}
	}
	for name, fn := range steps {
		if err := fn(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestOnlyAllowlistedGETs(t *testing.T) {
	srv, reqs, mu := fakeSyncthing(t)
	c, err := New(Options{Name: "t", URL: srv.URL, APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	callAll(t, c)
	mu.Lock()
	defer mu.Unlock()
	allowed := map[string]bool{}
	for _, p := range Allowlist() {
		allowed[p] = true
	}
	seen := map[string]bool{}
	for _, r := range *reqs {
		if r.method != http.MethodGet {
			t.Errorf("non-GET request %s %s", r.method, r.path)
		}
		if !allowed[r.path] {
			t.Errorf("request outside allowlist: %s", r.path)
		}
		if r.path == "/rest/config" || strings.HasPrefix(r.path, "/rest/config/gui") || strings.HasPrefix(r.path, "/rest/config/options") {
			t.Errorf("forbidden config endpoint requested: %s", r.path)
		}
		// Also on /rest/noauth/health: a gate may require its token on every
		// /rest path, and Syncthing ignores the key there.
		if r.apiKey != "k" {
			t.Errorf("%s: missing API key", r.path)
		}
		seen[r.path] = true
	}
	var missing []string
	for p := range allowed {
		if !seen[p] {
			missing = append(missing, p)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("allowlisted endpoints never exercised: %v", missing)
	}
}

func TestAllowlistIsExactly(t *testing.T) {
	want := []string{
		"/metrics", "/rest/cluster/pending/devices", "/rest/cluster/pending/folders",
		"/rest/config/devices", "/rest/config/folders", "/rest/db/completion", "/rest/db/remoteneed", "/rest/db/status",
		"/rest/events", "/rest/folder/errors", "/rest/noauth/health", "/rest/stats/device",
		"/rest/stats/folder", "/rest/system/browse", "/rest/system/connections",
		"/rest/system/status", "/rest/system/version", "/syncwatch/size",
	}
	got := Allowlist()
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("allowlist changed:\n got %v\nwant %v", got, want)
	}
}

func TestConfigSavedDataStripped(t *testing.T) {
	srv, _, _ := fakeSyncthing(t)
	c, _ := New(Options{Name: "t", URL: srv.URL, APIKey: "k"})
	evs, err := c.Events(context.Background(), 0, time.Second, EventMask)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range evs {
		if e.Type == "ConfigSaved" && e.Data != nil {
			t.Fatalf("ConfigSaved data not stripped: %s", e.Data)
		}
		if strings.Contains(string(e.Data), "leak") {
			t.Fatal("API key leaked through events")
		}
	}
}

func TestFolderDevicesDropEncryptionPassword(t *testing.T) {
	srv, _, _ := fakeSyncthing(t)
	c, _ := New(Options{Name: "t", URL: srv.URL, APIKey: "k"})
	fs, err := c.ConfigFolders(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(fs)
	if strings.Contains(string(b), "secret") {
		t.Fatal("encryption password retained")
	}
}

func TestTOFUAndCertChange(t *testing.T) {
	srv, reqs, mu := fakeSyncthing(t)
	var pinned string
	var logs pinLog
	c, _ := New(Options{Name: "t", URL: srv.URL, APIKey: "k", Log: slog.New(slog.NewTextHandler(&logs, nil)), OnPin: func(fp string) { pinned = fp }})
	if err := c.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pinned == "" || pinned != c.Pin() {
		t.Fatalf("expected TOFU pin, got %q / %q", pinned, c.Pin())
	}
	if c.LastCert().CASigned {
		t.Fatal("test cert should not be CA-signed")
	}
	if !strings.Contains(logs.String(), "self-signed certificate accepted and pinned on first use") || !strings.Contains(logs.String(), "server=t") {
		t.Fatalf("first use not logged: %s", logs.String())
	}

	// A client with a different pin must fail during the handshake, before any
	// request (and so any API key) reaches the server.
	mu.Lock()
	before := len(*reqs)
	mu.Unlock()
	c2, _ := New(Options{Name: "t", URL: srv.URL, APIKey: "k", Pin: strings.Repeat("ab", 32)})
	_, err := c2.SystemStatus(context.Background())
	ce, ok := IsCertChanged(err)
	if !ok {
		t.Fatalf("expected CertChangedError, got %v", err)
	}
	if ce.New != pinned {
		t.Fatalf("new fingerprint %s, want %s", ce.New, pinned)
	}
	mu.Lock()
	after := len(*reqs)
	mu.Unlock()
	if after != before {
		t.Fatalf("request reached the server despite a changed certificate")
	}

	// Accepting the new certificate restores access.
	c2.SetPin(ce.New)
	if _, err := c2.SystemStatus(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCASignedCertificateAccepted(t *testing.T) {
	srv, _, _ := fakeSyncthing(t)
	pool := srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	// The httptest certificate is valid for 127.0.0.1 and "example.com". A
	// CA-signed certificate is accepted even when another one is pinned, and
	// then takes over the pin.
	var pinned string
	c, _ := New(Options{Name: "t", URL: srv.URL, APIKey: "k", RootCAs: pool, Pin: strings.Repeat("de", 32),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), OnPin: func(fp string) { pinned = fp }})
	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("CA-signed certificate rejected: %v", err)
	}
	if !c.LastCert().CASigned {
		t.Fatal("expected CASigned")
	}
	if want := Fingerprint(srv.Certificate()); c.Pin() != want || pinned != want {
		t.Fatalf("CA-signed certificate must be pinned: pin %q, OnPin %q, want %q", c.Pin(), pinned, want)
	}
}

func TestUnauthorized(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	c, _ := New(Options{Name: "t", URL: srv.URL, APIKey: "bad"})
	_, err := c.SystemStatus(context.Background())
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 403 {
		t.Fatalf("expected 403 StatusError, got %v", err)
	}
}

// A 401 or 403 from Syncthing itself keeps the API key hint. A gate says
// what it refused, so its own text replaces the hint: a wrong gate token
// is not the same fault as a directory outside the browse roots of the
// gate. The text always has "HTTP 401" or "HTTP 403" for H1.
func TestStatusErrorTexts(t *testing.T) {
	long := "syncwatch gate: " + strings.Repeat("x", 300)
	for _, c := range []struct {
		code       int
		body, want string
	}{
		{401, "", "/rest/system/status: HTTP 401 (check the API key)"},
		{403, "Forbidden", "/rest/system/status: HTTP 403 (check the API key)"},
		{403, "syncwatch gate: gate token missing or not correct",
			"/rest/system/status: HTTP 403: syncwatch gate: gate token missing or not correct"},
		{403, "syncwatch gate: directory not in a permitted browse root",
			"/rest/system/status: HTTP 403: syncwatch gate: directory not in a permitted browse root (add the directory to SYNCWATCH_GATE_BROWSE_ROOTS on the gate)"},
		{502, "syncwatch gate: Syncthing refused the API key of the gate. Restart the gate with the current API key",
			"/rest/system/status: HTTP 502: syncwatch gate: Syncthing refused the API key of the gate. Restart the gate with the current API key"},
		{400, "syncwatch gate: query not permitted\nsecond line", "/rest/system/status: HTTP 400: syncwatch gate: query not permitted"},
		{502, long, "/rest/system/status: HTTP 502: " + long[:200]},
		{500, "boom", "/rest/system/status: HTTP 500 boom"},
	} {
		got := (&StatusError{Endpoint: "/rest/system/status", Code: c.code, Body: c.body}).Error()
		if got != c.want {
			t.Errorf("%d %q:\n got %q\nwant %q", c.code, c.body, got, c.want)
		}
		// H1 finds a refused API key or gate token with this expression.
		if refused := regexp.MustCompile(`HTTP 40[13]\b`).MatchString(got); refused != (c.code == 401 || c.code == 403) {
			t.Errorf("%d %q: refused key %v in %q", c.code, c.body, refused, got)
		}
	}
}

func TestParseConflicts(t *testing.T) {
	got := parseConflicts([]byte("syncthing_model_folder_conflicts_total{folder=\"x,y\"} 7\nother{folder=\"z\"} 1\nsyncthing_model_folder_conflicts_total{device=\"d\",folder=\"q\"} 2e0\n"))
	want := map[string]int64{"x,y": 7, "q": 2}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestShortFP(t *testing.T) {
	if got := ShortFP("0123456789abcdef0123"); got != "01:23:45:67:89:AB:CD:EF" {
		t.Fatal(got)
	}
}

var _ = tls.VersionTLS12

// Size: a gate answers with JSON. Syncthing without a gate answers 404 (or
// its web page with 200), and an older gate refuses the endpoint with 404:
// that is ErrNoSize, not an error of the server.
func TestSize(t *testing.T) {
	answer := ""
	code := http.StatusOK
	var query string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.RawQuery
		w.WriteHeader(code)
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(srv.Close)
	c, _ := New(Options{Name: "t", URL: srv.URL, APIKey: "k"})
	ctx := context.Background()

	answer = `{"path":"/s/LIVE-X","state":"queued","bytes":2048,"files":3,"dirs":1,"complete":true,"measured_at":"2026-10-14T01:10:00Z","took_ms":1500}`
	got, err := c.Size(ctx, "/s/LIVE-X", true)
	if err != nil || got.Bytes != 2048 || got.Files != 3 || got.State != SizeQueued || !got.Complete || got.MeasuredAt.IsZero() {
		t.Fatalf("gate answer: %+v, %v", got, err)
	}
	if query != "measure=1&path=%2Fs%2FLIVE-X" {
		t.Errorf("query %q", query)
	}
	if _, err := c.Size(ctx, "/s/LIVE-X", false); err != nil || query != "path=%2Fs%2FLIVE-X" {
		t.Errorf("without measure: query %q, %v", query, err)
	}
	for _, tc := range []struct {
		code   int
		answer string
	}{
		{http.StatusNotFound, "404 page not found"},
		{http.StatusNotFound, "syncwatch gate: endpoint not permitted"},
		{http.StatusOK, "<!DOCTYPE html><html>"},
	} {
		code, answer = tc.code, tc.answer
		if _, err := c.Size(ctx, "/s/LIVE-X", false); !errors.Is(err, ErrNoSize) {
			t.Errorf("HTTP %d %q: %v, want ErrNoSize", tc.code, tc.answer, err)
		}
	}
	code, answer = http.StatusForbidden, "syncwatch gate: directory not in a permitted browse root"
	if _, err := c.Size(ctx, "/elsewhere/X", false); err == nil || errors.Is(err, ErrNoSize) {
		t.Errorf("a refused directory is an error of its own: %v", err)
	}
}
