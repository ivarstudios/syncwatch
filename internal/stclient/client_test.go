package stclient

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
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
		"PendingDevices":   func() error { _, err := c.PendingDevices(ctx); return err },
		"PendingFolders":   func() error { _, err := c.PendingFolders(ctx); return err },
		"Browse":           func() error { _, err := c.Browse(ctx, "/a", "/"); return err },
		"Events":           func() error { _, err := c.Events(ctx, 0, time.Second, EventMask); return err },
		"ConflictCounters": func() error { _, err := c.ConflictCounters(ctx); return err },
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
		if r.path == "/rest/noauth/health" && r.apiKey != "" {
			t.Errorf("API key sent to noauth endpoint")
		}
		if r.path != "/rest/noauth/health" && r.apiKey != "k" {
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
		"/rest/config/devices", "/rest/config/folders", "/rest/db/completion", "/rest/db/status",
		"/rest/events", "/rest/folder/errors", "/rest/noauth/health", "/rest/stats/device",
		"/rest/stats/folder", "/rest/system/browse", "/rest/system/connections",
		"/rest/system/status", "/rest/system/version",
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
	c, _ := New(Options{Name: "t", URL: srv.URL, APIKey: "k", OnPin: func(fp string) { pinned = fp }})
	if err := c.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pinned == "" || pinned != c.Pin() {
		t.Fatalf("expected TOFU pin, got %q / %q", pinned, c.Pin())
	}
	if c.LastCert().CASigned {
		t.Fatal("test cert should not be CA-signed")
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
	// The httptest certificate is valid for 127.0.0.1 and "example.com".
	c, _ := New(Options{Name: "t", URL: srv.URL, APIKey: "k", RootCAs: pool, Pin: "deadbeef"})
	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("CA-signed certificate rejected: %v", err)
	}
	if !c.LastCert().CASigned {
		t.Fatal("expected CASigned")
	}
	if c.Pin() != "deadbeef" {
		t.Fatal("CA-signed certificate must not change the pin")
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
