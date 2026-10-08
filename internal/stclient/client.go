// Package stclient is a read-only client for the Syncthing 2.x REST API.
//
// It exposes one typed method per allowlisted GET endpoint and nothing else:
// there is no generic request method, no way to choose the HTTP method, and
// /rest/config (which contains the GUI password hash and API key) is not on the
// list. TLS certificates are checked during the handshake, before the API key
// is sent: certificates that chain to a trusted CA are accepted, self-signed
// ones are trusted on first use and pinned by SHA-256.
package stclient

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type endpoint string

// The complete set of endpoints the client can request. Every request is a GET.
const (
	epHealth         endpoint = "/rest/noauth/health"
	epSystemStatus   endpoint = "/rest/system/status"
	epSystemVersion  endpoint = "/rest/system/version"
	epConnections    endpoint = "/rest/system/connections"
	epDeviceStats    endpoint = "/rest/stats/device"
	epFolderStats    endpoint = "/rest/stats/folder"
	epConfigFolders  endpoint = "/rest/config/folders"
	epConfigDevices  endpoint = "/rest/config/devices"
	epDBStatus       endpoint = "/rest/db/status"
	epFolderErrors   endpoint = "/rest/folder/errors"
	epCompletion     endpoint = "/rest/db/completion"
	epPendingDevices endpoint = "/rest/cluster/pending/devices"
	epPendingFolders endpoint = "/rest/cluster/pending/folders"
	epBrowse         endpoint = "/rest/system/browse"
	epEvents         endpoint = "/rest/events"
	epMetrics        endpoint = "/metrics"
)

var allowlist = map[endpoint]bool{
	epHealth: true, epSystemStatus: true, epSystemVersion: true, epConnections: true,
	epDeviceStats: true, epFolderStats: true, epConfigFolders: true, epConfigDevices: true,
	epDBStatus: true, epFolderErrors: true, epCompletion: true, epPendingDevices: true,
	epPendingFolders: true, epBrowse: true, epEvents: true, epMetrics: true,
}

// Allowlist returns the endpoint paths the client may request, for tests and docs.
func Allowlist() []string {
	out := make([]string, 0, len(allowlist))
	for ep := range allowlist {
		out = append(out, string(ep))
	}
	return out
}

// EventMask is the set of events the collector subscribes to.
var EventMask = []string{
	"StateChanged", "FolderSummary", "FolderCompletion", "FolderErrors",
	"FolderWatchStateChanged", "FolderPaused", "FolderResumed",
	"DeviceConnected", "DeviceDisconnected", "DevicePaused", "DeviceResumed",
	"PendingDevicesChanged", "PendingFoldersChanged", "ConfigSaved",
	"LocalChangeDetected", "RemoteChangeDetected",
}

// maxBody caps any response body. Browse and config listings are the largest.
const maxBody = 64 << 20

// Options configure a Client.
type Options struct {
	Name    string // server name, used in errors
	URL     string // base URL of the Syncthing GUI, e.g. https://192.168.1.10:8384
	APIKey  string
	Pin     string          // pinned SHA-256 certificate fingerprint (hex), or "" for none yet
	OnPin   func(fp string) // called when a self-signed certificate is trusted on first use
	Timeout time.Duration   // per-request timeout (default 30 s); events use their own
	Retries int             // extra attempts for failed requests (default 2)
	RootCAs *x509.CertPool  // nil means the system pool
}

// CertInfo describes the last certificate the server presented.
type CertInfo struct {
	Fingerprint string
	Subject     string
	NotAfter    time.Time
	CASigned    bool
}

// CertChangedError is returned when a pinned self-signed certificate changes.
// The connection is closed during the handshake, so no API key is sent.
type CertChangedError struct {
	Server   string
	Old, New string
	NotAfter time.Time
}

func (e *CertChangedError) Error() string {
	return fmt.Sprintf("%s: TLS certificate changed (pinned %s, got %s)", e.Server, ShortFP(e.Old), ShortFP(e.New))
}

// StatusError is a non-2xx response.
type StatusError struct {
	Endpoint string
	Code     int
	Body     string
}

func (e *StatusError) Error() string {
	if e.Code == http.StatusUnauthorized || e.Code == http.StatusForbidden {
		return fmt.Sprintf("%s: HTTP %d (check the API key)", e.Endpoint, e.Code)
	}
	return fmt.Sprintf("%s: HTTP %d %s", e.Endpoint, e.Code, e.Body)
}

// Client talks to one Syncthing instance.
type Client struct {
	opts Options
	base *url.URL
	host string
	hc   *http.Client

	mu   sync.Mutex
	pin  string
	cert CertInfo
}

// New creates a client. It does not contact the server.
func New(o Options) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(o.URL, "/"))
	if err != nil {
		return nil, fmt.Errorf("server URL: %w", err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("server URL must start with https:// or http://")
	}
	if u.Host == "" {
		return nil, fmt.Errorf("server URL has no host")
	}
	if o.Timeout <= 0 {
		o.Timeout = 30 * time.Second
	}
	if o.Retries < 0 {
		o.Retries = 0
	} else if o.Retries == 0 {
		o.Retries = 2
	}
	c := &Client{opts: o, base: u, host: u.Hostname(), pin: o.Pin}
	tr := &http.Transport{
		Proxy: nil,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSClientConfig: &tls.Config{
			// Verification happens in VerifyConnection, during the handshake.
			InsecureSkipVerify: true, //nolint:gosec // see verify
			VerifyConnection:   c.verify,
			MinVersion:         tls.VersionTLS12,
		},
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     30 * time.Second,
		ForceAttemptHTTP2:   false,
	}
	c.hc = &http.Client{
		Transport: tr,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return c, nil
}

// Name returns the server name the client was created with.
func (c *Client) Name() string { return c.opts.Name }

// Pin returns the currently pinned fingerprint.
func (c *Client) Pin() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.pin
}

// SetPin replaces the pinned fingerprint, e.g. after an admin accepts a new certificate.
func (c *Client) SetPin(fp string) {
	c.mu.Lock()
	c.pin = fp
	c.mu.Unlock()
	c.hc.CloseIdleConnections()
}

// LastCert returns the last certificate the server presented.
func (c *Client) LastCert() CertInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cert
}

// Close releases idle connections.
func (c *Client) Close() { c.hc.CloseIdleConnections() }

// Fingerprint returns the hex SHA-256 of a certificate's DER bytes.
func Fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return hex.EncodeToString(sum[:])
}

// ShortFP formats a fingerprint for display: the first 16 hex characters in pairs.
func ShortFP(fp string) string {
	if len(fp) > 16 {
		fp = fp[:16]
	}
	var b strings.Builder
	for i := 0; i < len(fp); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		end := i + 2
		if end > len(fp) {
			end = len(fp)
		}
		b.WriteString(strings.ToUpper(fp[i:end]))
	}
	return b.String()
}

func (c *Client) verify(cs tls.ConnectionState) error {
	if len(cs.PeerCertificates) == 0 {
		return errors.New("server presented no certificate")
	}
	leaf := cs.PeerCertificates[0]
	fp := Fingerprint(leaf)
	inter := x509.NewCertPool()
	for _, ic := range cs.PeerCertificates[1:] {
		inter.AddCert(ic)
	}
	_, caErr := leaf.Verify(x509.VerifyOptions{
		DNSName:       c.host,
		Roots:         c.opts.RootCAs,
		Intermediates: inter,
	})

	c.mu.Lock()
	defer c.mu.Unlock()
	c.cert = CertInfo{Fingerprint: fp, Subject: leaf.Subject.String(), NotAfter: leaf.NotAfter, CASigned: caErr == nil}
	if caErr == nil {
		return nil
	}
	switch c.pin {
	case "":
		c.pin = fp
		if c.opts.OnPin != nil {
			c.opts.OnPin(fp)
		}
		return nil
	case fp:
		return nil
	default:
		return &CertChangedError{Server: c.opts.Name, Old: c.pin, New: fp, NotAfter: leaf.NotAfter}
	}
}

// IsCertChanged reports whether err is (or wraps) a CertChangedError.
func IsCertChanged(err error) (*CertChangedError, bool) {
	var ce *CertChangedError
	if errors.As(err, &ce) {
		return ce, true
	}
	return nil, false
}

func (c *Client) url(ep endpoint, q url.Values) string {
	u := *c.base
	u.Path = strings.TrimRight(c.base.Path, "/") + string(ep)
	u.RawPath = ""
	if len(q) > 0 {
		u.RawQuery = q.Encode()
	} else {
		u.RawQuery = ""
	}
	return u.String()
}

// fetch performs one GET to an allowlisted endpoint and returns the body.
func (c *Client) fetch(ctx context.Context, ep endpoint, q url.Values, timeout time.Duration, retries int) ([]byte, error) {
	if !allowlist[ep] {
		// Unreachable: endpoint is unexported and every constant is allowlisted.
		panic("stclient: endpoint not allowlisted: " + string(ep))
	}
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		body, err := c.fetchOnce(ctx, ep, q, timeout)
		if err == nil {
			return body, nil
		}
		lastErr = err
		if _, ok := IsCertChanged(err); ok {
			return nil, err
		}
		var se *StatusError
		if errors.As(err, &se) && se.Code < 500 {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

func (c *Client) fetchOnce(ctx context.Context, ep endpoint, q url.Values, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url(ep, q), nil)
	if err != nil {
		return nil, err
	}
	if ep != epHealth {
		req.Header.Set("X-API-Key", c.opts.APIKey)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, cleanErr(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("%s: reading response: %w", ep, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := strings.TrimSpace(string(body))
		if len(msg) > 200 {
			msg = msg[:200]
		}
		return nil, &StatusError{Endpoint: string(ep), Code: resp.StatusCode, Body: msg}
	}
	return body, nil
}

// cleanErr unwraps url.Error so messages don't repeat the URL (which may be long),
// but keeps the CertChangedError reachable through errors.As.
func cleanErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		if ce, ok := IsCertChanged(ue.Err); ok {
			return ce
		}
		return fmt.Errorf("%s: %w", strings.TrimPrefix(ue.Op, "Get"), ue.Err)
	}
	return err
}

func (c *Client) getJSON(ctx context.Context, ep endpoint, q url.Values, out any) error {
	body, err := c.fetch(ctx, ep, q, c.opts.Timeout, c.opts.Retries)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s: decoding response: %w", ep, err)
	}
	return nil
}

// Health calls /rest/noauth/health. It needs no API key.
func (c *Client) Health(ctx context.Context) error {
	var out struct {
		Status string `json:"status"`
	}
	if err := c.getJSON(ctx, epHealth, nil, &out); err != nil {
		return err
	}
	if out.Status != "OK" {
		return fmt.Errorf("health status %q", out.Status)
	}
	return nil
}

// SystemStatus calls /rest/system/status.
func (c *Client) SystemStatus(ctx context.Context) (SystemStatus, error) {
	var out SystemStatus
	err := c.getJSON(ctx, epSystemStatus, nil, &out)
	return out, err
}

// SystemVersion calls /rest/system/version.
func (c *Client) SystemVersion(ctx context.Context) (SystemVersion, error) {
	var out SystemVersion
	err := c.getJSON(ctx, epSystemVersion, nil, &out)
	return out, err
}

// Connections calls /rest/system/connections.
func (c *Client) Connections(ctx context.Context) (map[string]Connection, error) {
	var out Connections
	err := c.getJSON(ctx, epConnections, nil, &out)
	return out.Connections, err
}

// Traffic calls /rest/system/connections for the bytes received and sent
// since Syncthing started.
func (c *Client) Traffic(ctx context.Context) (Traffic, error) {
	var out Connections
	err := c.getJSON(ctx, epConnections, nil, &out)
	return out.Total, err
}

// DeviceStats calls /rest/stats/device.
func (c *Client) DeviceStats(ctx context.Context) (map[string]DeviceStat, error) {
	out := map[string]DeviceStat{}
	err := c.getJSON(ctx, epDeviceStats, nil, &out)
	return out, err
}

// FolderStats calls /rest/stats/folder.
func (c *Client) FolderStats(ctx context.Context) (map[string]FolderStat, error) {
	out := map[string]FolderStat{}
	err := c.getJSON(ctx, epFolderStats, nil, &out)
	return out, err
}

// ConfigFolders calls /rest/config/folders.
func (c *Client) ConfigFolders(ctx context.Context) ([]FolderConfig, error) {
	var out []FolderConfig
	err := c.getJSON(ctx, epConfigFolders, nil, &out)
	return out, err
}

// ConfigDevices calls /rest/config/devices.
func (c *Client) ConfigDevices(ctx context.Context) ([]DeviceConfig, error) {
	var out []DeviceConfig
	err := c.getJSON(ctx, epConfigDevices, nil, &out)
	return out, err
}

// DBStatus calls /rest/db/status for one folder. Syncthing documents it as
// expensive, so the collector calls it only during a full re-check.
func (c *Client) DBStatus(ctx context.Context, folder string) (DBStatus, error) {
	var out DBStatus
	err := c.getJSON(ctx, epDBStatus, url.Values{"folder": {folder}}, &out)
	return out, err
}

// FolderErrors calls /rest/folder/errors for one folder, returning at most perPage errors.
func (c *Client) FolderErrors(ctx context.Context, folder string, perPage int) (FolderErrors, error) {
	var out FolderErrors
	q := url.Values{"folder": {folder}}
	if perPage > 0 {
		q.Set("page", "1")
		q.Set("perpage", strconv.Itoa(perPage))
	}
	err := c.getJSON(ctx, epFolderErrors, q, &out)
	return out, err
}

// Completion calls /rest/db/completion for one folder and remote device.
func (c *Client) Completion(ctx context.Context, folder, device string) (Completion, error) {
	var out Completion
	err := c.getJSON(ctx, epCompletion, url.Values{"folder": {folder}, "device": {device}}, &out)
	return out, err
}

// PendingDevices calls /rest/cluster/pending/devices.
func (c *Client) PendingDevices(ctx context.Context) (map[string]PendingDevice, error) {
	out := map[string]PendingDevice{}
	err := c.getJSON(ctx, epPendingDevices, nil, &out)
	return out, err
}

// PendingFolders calls /rest/cluster/pending/folders.
func (c *Client) PendingFolders(ctx context.Context) (map[string]PendingFolder, error) {
	out := map[string]PendingFolder{}
	err := c.getJSON(ctx, epPendingFolders, nil, &out)
	return out, err
}

// Browse lists the subdirectories of dir on the server. Syncthing 2.x matches
// the last path segment by prefix (not as a glob), so dir is sent as-is with a
// trailing separator to list its children. sep is the server's path separator.
func (c *Client) Browse(ctx context.Context, dir, sep string) ([]string, error) {
	if sep == "" {
		sep = "/"
	}
	if !strings.HasSuffix(dir, sep) {
		dir += sep
	}
	var out []string
	err := c.getJSON(ctx, epBrowse, url.Values{"current": {dir}}, &out)
	return out, err
}

// Events long-polls /rest/events. timeout is the server-side long-poll timeout;
// the HTTP request gets 15 s more. ConfigSaved events are returned without
// data, because their payload contains the GUI API key and password hash.
func (c *Client) Events(ctx context.Context, since int64, timeout time.Duration, mask []string) ([]Event, error) {
	q := url.Values{
		"since":   {strconv.FormatInt(since, 10)},
		"timeout": {strconv.Itoa(int(timeout / time.Second))},
	}
	if len(mask) > 0 {
		q.Set("events", strings.Join(mask, ","))
	}
	body, err := c.fetch(ctx, epEvents, q, timeout+15*time.Second, 0)
	if err != nil {
		return nil, err
	}
	var evs []Event
	if err := json.Unmarshal(body, &evs); err != nil {
		return nil, fmt.Errorf("%s: decoding response: %w", epEvents, err)
	}
	for i := range evs {
		if evs[i].Type == "ConfigSaved" {
			evs[i].Data = nil
		}
	}
	return evs, nil
}

// ConflictCounters reads /metrics and returns syncthing_model_folder_conflicts_total per folder.
func (c *Client) ConflictCounters(ctx context.Context) (map[string]int64, error) {
	body, err := c.fetch(ctx, epMetrics, nil, c.opts.Timeout, c.opts.Retries)
	if err != nil {
		return nil, err
	}
	return parseConflicts(body), nil
}

const conflictMetric = "syncthing_model_folder_conflicts_total"

func parseConflicts(body []byte) map[string]int64 {
	out := map[string]int64{}
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, conflictMetric+"{") {
			continue
		}
		rest := line[len(conflictMetric)+1:]
		end := strings.LastIndex(rest, "}")
		if end < 0 {
			continue
		}
		folder, ok := labelValue(rest[:end], "folder")
		if !ok {
			continue
		}
		val := strings.Fields(rest[end+1:])
		if len(val) == 0 {
			continue
		}
		f, err := strconv.ParseFloat(val[0], 64)
		if err != nil {
			continue
		}
		out[folder] = int64(f)
	}
	return out
}

// labelValue extracts one label from a Prometheus label set such as folder="a\"b",x="y".
func labelValue(labels, name string) (string, bool) {
	i := 0
	for i < len(labels) {
		for i < len(labels) && (labels[i] == ',' || labels[i] == ' ') {
			i++
		}
		eq := strings.IndexByte(labels[i:], '=')
		if eq < 0 {
			return "", false
		}
		key := strings.TrimSpace(labels[i : i+eq])
		i += eq + 1
		if i >= len(labels) || labels[i] != '"' {
			return "", false
		}
		i++
		var val strings.Builder
		for i < len(labels) && labels[i] != '"' {
			if labels[i] == '\\' && i+1 < len(labels) {
				i++
				switch labels[i] {
				case 'n':
					val.WriteByte('\n')
				default:
					val.WriteByte(labels[i])
				}
			} else {
				val.WriteByte(labels[i])
			}
			i++
		}
		i++ // closing quote
		if key == name {
			return val.String(), true
		}
	}
	return "", false
}
