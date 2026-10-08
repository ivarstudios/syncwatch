package app

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/stclient"
)

// lockedBuffer is a log sink the gate's goroutine writes to while the test
// reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *lockedBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

// runGate starts RunGate and returns its address and token.
func runGate(t *testing.T, o GateOptions) (addr, tok string, stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	addrc, tokc, errc := make(chan string, 1), make(chan string, 1), make(chan error, 1)
	o.Listen, o.OnListen, o.TokenReady = "127.0.0.1:0", func(a string) { addrc <- a }, func(tk string) { tokc <- tk }
	go func() { errc <- RunGate(ctx, o) }()
	select {
	case err := <-errc:
		cancel()
		t.Fatalf("gate: %v", err)
	case tok = <-tokc:
	}
	addr = <-addrc
	return addr, tok, func() { cancel(); <-errc }
}

func TestRunGate(t *testing.T) {
	const key = "real-syncthing-key-0123456789"
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != key {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`{"myID":"AAAA","pathSeparator":"/"}`))
	}))
	defer up.Close()
	dir := t.TempDir()
	cfgXML := filepath.Join(dir, "config.xml")
	_ = os.WriteFile(cfgXML, []byte(`<configuration version="37"><gui enabled="true" tls="true"><address>0.0.0.0:8384</address><apikey>`+key+`</apikey></gui></configuration>`), 0o600)
	var logs lockedBuffer
	o := GateOptions{DataDir: filepath.Join(dir, "gate"), Syncthing: up.URL, ConfigXML: cfgXML, Log: slog.New(slog.NewTextHandler(&logs, nil))}

	addr, tok, stop := runGate(t, o)
	if !strings.HasPrefix(tok, "swg_") || !strings.Contains(logs.String(), tok) {
		t.Fatalf("token %q not created and shown once", tok)
	}
	cl, err := stclient.New(stclient.Options{Name: "NAS", URL: "https://" + addr, APIKey: tok, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if st, err := cl.SystemStatus(context.Background()); err != nil || st.MyID != "AAAA" {
		t.Fatalf("through the gate: %+v %v", st, err)
	}
	if !cl.LastCert().CASigned && cl.LastCert().Fingerprint == "" {
		t.Fatal("the gate didn't serve HTTPS")
	}
	// The plain-HTTP port refuses: the gate only speaks HTTPS.
	if resp, err := http.Get("http://" + addr + "/healthz"); err == nil {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(b), "ok") {
			t.Fatal("the gate answered over plain HTTP")
		}
	}
	stop()

	// A restart keeps the token and doesn't show it again.
	logs.Reset()
	_, tok2, stop2 := runGate(t, o)
	defer stop2()
	if tok2 != tok || strings.Contains(logs.String(), tok) {
		t.Fatalf("restart: token %q (was %q), shown again: %v", tok2, tok, strings.Contains(logs.String(), tok))
	}
	if fi, _ := os.Stat(filepath.Join(o.DataDir, "gate-token")); os.PathSeparator == '/' && fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode %v", fi.Mode().Perm())
	}
}

func TestGateNeedsAKey(t *testing.T) {
	if _, err := gateSyncthingKey(GateOptions{}); err == nil {
		t.Fatal("no key accepted")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "config.xml")
	_ = os.WriteFile(p, []byte(`<configuration><gui></gui></configuration>`), 0o600)
	if _, err := gateSyncthingKey(GateOptions{ConfigXML: p}); err == nil {
		t.Fatal("config.xml without a key accepted")
	}
	kf := filepath.Join(dir, "key")
	_ = os.WriteFile(kf, []byte("from-file\n"), 0o600)
	if k, err := gateSyncthingKey(GateOptions{KeyFile: kf, Key: "from-env"}); err != nil || k != "from-file" {
		t.Fatalf("key file: %q %v", k, err)
	}
}
