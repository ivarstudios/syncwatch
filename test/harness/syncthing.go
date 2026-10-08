// Package harness runs a simulated Syncthing deployment for integration
// tests and demos: real Syncthing processes acting as "servers" (NASes) and
// "PCs", configured through their own REST APIs, plus a fake Discord webhook.
// Only the harness writes to these test instances; syncwatch stays read-only.
package harness

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Instance is one Syncthing process.
type Instance struct {
	Name     string
	Server   bool
	Home     string
	GUIPort  int
	SyncPort int
	APIKey   string
	DeviceID string

	bin      string
	cmd      *exec.Cmd
	done     chan struct{}
	hc       *http.Client
	baseURL  string // set for remote instances (containers)
	syncAddr string
}

// Remote returns an Instance for a Syncthing that is already running, e.g. in
// a container. Start and Stop are not available; manage it externally.
func Remote(name string, server bool, baseURL, apiKey, syncAddr string) *Instance {
	i := newInstance("", "", name, server, 0, 0)
	i.baseURL, i.APIKey, i.syncAddr, i.Home = strings.TrimRight(baseURL, "/"), apiKey, syncAddr, ""
	return i
}

// WaitReady waits until the API answers and records the device ID.
func (i *Instance) WaitReady(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var st struct {
			MyID string `json:"myID"`
		}
		if err := i.Call(ctx, http.MethodGet, "/rest/system/status", nil, &st); err == nil && st.MyID != "" {
			i.DeviceID = st.MyID
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("%s did not answer in time", i.Name)
}

// URL is the GUI/API base URL.
func (i *Instance) URL() string {
	if i.baseURL != "" {
		return i.baseURL
	}
	return fmt.Sprintf("https://127.0.0.1:%d", i.GUIPort)
}

// SyncAddr is the address other devices dial.
func (i *Instance) SyncAddr() string {
	if i.syncAddr != "" {
		return i.syncAddr
	}
	return fmt.Sprintf("tcp://127.0.0.1:%d", i.SyncPort)
}

// ShareDir is the on-disk path of a share root on a server.
func (i *Instance) ShareDir(share string) string { return filepath.Join(i.Home, "share", share) }

// PCDir is where a PC keeps a synced folder.
func (i *Instance) PCDir(label string) string { return filepath.Join(i.Home, "sync", label) }

func newInstance(bin, dir, name string, server bool, guiPort, syncPort int) *Instance {
	return &Instance{
		Name: name, Server: server, Home: filepath.Join(dir, strings.ToLower(name)),
		GUIPort: guiPort, SyncPort: syncPort, APIKey: "key-" + strings.ToLower(name) + "-0123456789",
		bin: bin,
		hc: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // harness only
		}},
	}
}

var replacements = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`<gui enabled="true" tls="false"`), `<gui enabled="true" tls="true"`},
	{regexp.MustCompile(`<globalAnnounceEnabled>true`), `<globalAnnounceEnabled>false`},
	{regexp.MustCompile(`<localAnnounceEnabled>true`), `<localAnnounceEnabled>false`},
	{regexp.MustCompile(`<relaysEnabled>true`), `<relaysEnabled>false`},
	{regexp.MustCompile(`<natEnabled>true`), `<natEnabled>false`},
	{regexp.MustCompile(`<startBrowser>true`), `<startBrowser>false`},
	{regexp.MustCompile(`<urAccepted>0<`), `<urAccepted>-1<`},
	{regexp.MustCompile(`<autoUpgradeIntervalH>\d+`), `<autoUpgradeIntervalH>0`},
	{regexp.MustCompile(`<crashReportingEnabled>true`), `<crashReportingEnabled>false`},
}

// generate creates the instance's keys and config.
func (i *Instance) generate() error {
	if err := os.MkdirAll(i.Home, 0o755); err != nil {
		return err
	}
	out, err := exec.Command(i.bin, "generate", "--home="+i.Home, "--no-port-probing").CombinedOutput()
	if err != nil {
		return fmt.Errorf("syncthing generate %s: %v: %s", i.Name, err, out)
	}
	path := filepath.Join(i.Home, "config.xml")
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	s := string(b)
	for _, r := range replacements {
		s = r.re.ReplaceAllString(s, r.repl)
	}
	s = regexp.MustCompile(`(<gui [^>]*>\s*<address>)[^<]*(</address>)`).ReplaceAllString(s, fmt.Sprintf("${1}127.0.0.1:%d${2}", i.GUIPort))
	s = regexp.MustCompile(`<apikey>[^<]*</apikey>`).ReplaceAllString(s, "<apikey>"+i.APIKey+"</apikey>")
	s = regexp.MustCompile(`<listenAddress>[^<]*</listenAddress>`).ReplaceAllString(s, "<listenAddress>"+i.SyncAddr()+"</listenAddress>")
	return os.WriteFile(path, []byte(s), 0o644)
}

// Start launches the process and waits until the API answers.
func (i *Instance) Start(ctx context.Context) error {
	if i.cmd != nil {
		return nil
	}
	if i.baseURL != "" {
		return fmt.Errorf("%s is a remote instance", i.Name)
	}
	logf, err := os.OpenFile(filepath.Join(i.Home, "syncthing.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	cmd := exec.Command(i.bin, "serve", "--home="+i.Home, "--no-browser", "--no-restart", "--no-upgrade")
	cmd.Env = append(os.Environ(), "STNOUPGRADE=1", "STNODEFAULTFOLDER=1")
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		logf.Close()
		return err
	}
	i.cmd = cmd
	i.done = make(chan struct{})
	go func() {
		_ = cmd.Wait()
		logf.Close()
		close(i.done)
	}()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var st struct {
			MyID string `json:"myID"`
		}
		if err := i.Call(ctx, http.MethodGet, "/rest/system/status", nil, &st); err == nil && st.MyID != "" {
			i.DeviceID = st.MyID
			return nil
		}
		select {
		case <-i.done:
			return fmt.Errorf("%s exited during startup; see %s", i.Name, filepath.Join(i.Home, "syncthing.log"))
		case <-time.After(300 * time.Millisecond):
		}
	}
	return fmt.Errorf("%s did not start in time", i.Name)
}

// Running reports whether the process is up.
func (i *Instance) Running() bool { return i.cmd != nil }

// Stop shuts Syncthing down through its API, killing it if needed.
func (i *Instance) Stop() error {
	if i.cmd == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = i.Call(ctx, http.MethodPost, "/rest/system/shutdown", nil, nil)
	cancel()
	select {
	case <-i.done:
	case <-time.After(15 * time.Second):
		_ = i.cmd.Process.Kill()
		<-i.done
	}
	i.cmd = nil
	// Windows keeps the port briefly; give it a moment.
	if runtime.GOOS == "windows" {
		time.Sleep(500 * time.Millisecond)
	}
	return nil
}

// ReplaceCert stops the instance, deletes its GUI certificate and starts it
// again, so Syncthing generates a new self-signed one.
func (i *Instance) ReplaceCert(ctx context.Context) error {
	if err := i.Stop(); err != nil {
		return err
	}
	for _, f := range []string{"https-cert.pem", "https-key.pem"} {
		if err := os.Remove(filepath.Join(i.Home, f)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return i.Start(ctx)
}

// Call makes an API request to the test instance (any method: the harness
// sets up scenarios; syncwatch itself never writes).
func (i *Instance) Call(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, i.URL()+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("X-API-Key", i.APIKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := i.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s %s: HTTP %d: %s", i.Name, method, path, resp.StatusCode, strings.TrimSpace(string(b)))
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

// AddDevice configures a remote device.
func (i *Instance) AddDevice(ctx context.Context, other *Instance) error {
	return i.Call(ctx, http.MethodPost, "/rest/config/devices", map[string]any{
		"deviceID": other.DeviceID, "name": other.Name, "addresses": []string{other.SyncAddr()},
	}, nil)
}

// SetName sets the instance's own device name.
func (i *Instance) SetName(ctx context.Context) error {
	return i.Call(ctx, http.MethodPatch, "/rest/config/devices/"+url.PathEscape(i.DeviceID), map[string]any{"name": i.Name}, nil)
}

// AddFolder configures a folder shared with the given devices.
func (i *Instance) AddFolder(ctx context.Context, id, label, path string, with []*Instance) error {
	devs := []map[string]string{{"deviceID": i.DeviceID}}
	for _, w := range with {
		if w != i {
			devs = append(devs, map[string]string{"deviceID": w.DeviceID})
		}
	}
	return i.Call(ctx, http.MethodPost, "/rest/config/folders", map[string]any{
		"id": id, "label": label, "path": path, "type": "sendreceive", "devices": devs,
		"rescanIntervalS": 60, "fsWatcherEnabled": true, "fsWatcherDelayS": 1,
	}, nil)
}

// PauseFolder pauses or resumes a folder.
func (i *Instance) PauseFolder(ctx context.Context, id string, paused bool) error {
	return i.Call(ctx, http.MethodPatch, "/rest/config/folders/"+url.PathEscape(id), map[string]any{"paused": paused}, nil)
}

// RemoveFolder removes a folder from the config (files stay).
func (i *Instance) RemoveFolder(ctx context.Context, id string) error {
	return i.Call(ctx, http.MethodDelete, "/rest/config/folders/"+url.PathEscape(id), nil, nil)
}

// Rescan asks Syncthing to rescan a folder now.
func (i *Instance) Rescan(ctx context.Context, id string) error {
	return i.Call(ctx, http.MethodPost, "/rest/db/scan?folder="+url.QueryEscape(id), nil, nil)
}

// Connected reports whether the instance is connected to another device.
func (i *Instance) Connected(ctx context.Context, other *Instance) bool {
	var c struct {
		Connections map[string]struct {
			Connected bool `json:"connected"`
		} `json:"connections"`
	}
	if err := i.Call(ctx, http.MethodGet, "/rest/system/connections", nil, &c); err != nil {
		return false
	}
	return c.Connections[other.DeviceID].Connected
}
