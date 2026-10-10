package probe

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
	"testing"
	"time"
)

// fakeServer answers the probe like a server whose requests fail from the
// endpoint fail on (empty: the first request).
func fakeServer(t *testing.T, fail string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case fail == "" || r.URL.Path == fail:
			http.Error(w, "Forbidden", http.StatusForbidden)
		case r.URL.Path == "/rest/system/status":
			_, _ = io.WriteString(w, `{"myID":"AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA-AAAAAAA","pathSeparator":"/","startTime":"2026-10-11T10:00:00Z"}`)
		case r.URL.Path == "/rest/config/folders" || r.URL.Path == "/rest/config/devices" || r.URL.Path == "/rest/events":
			_, _ = io.WriteString(w, `[]`)
		default:
			_, _ = io.WriteString(w, `{}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func probeOptions(url, out string) Options {
	return Options{Name: "SOL", URL: url, APIKey: "key", Out: out, EventTimeout: time.Second, EventWait: time.Second, ScanDepth: 3, ScanBudget: 10}
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// A probe that cannot test the server writes what it has and returns an
// error, thus "syncwatch probe" exits with status 1.
func TestProbeStopsWithAnError(t *testing.T) {
	for _, c := range []struct{ fail, err, report string }{
		{"", "the connection failed: /rest/system/status: HTTP 403", "**The connection failed:**"},
		{"/rest/config/folders", "the request config/folders failed: /rest/config/folders: HTTP 403", "**The request `config/folders` failed:**"},
		{"/rest/events", "the subscription with the event mask failed", "- The subscription with the event mask failed:"},
	} {
		out := filepath.Join(t.TempDir(), "docs", "api-findings.md")
		err := Run(context.Background(), probeOptions(fakeServer(t, c.fail).URL, out), quiet)
		if err == nil || !strings.Contains(err.Error(), c.err) {
			t.Errorf("fail %q: got %v, want %q", c.fail, err, c.err)
		}
		b, _ := os.ReadFile(out)
		if !strings.Contains(string(b), c.report) || !strings.Contains(string(b), "## SOL") {
			t.Errorf("fail %q: report without the failure:\n%s", c.fail, b)
		}
	}
}

// A report that cannot be written to --out (a docs directory that the user
// of the container cannot write) goes to standard output, then the probe
// returns the error.
func TestProbeReportKeptWhenOutCannotBeWritten(t *testing.T) {
	var got bytes.Buffer
	stdout = &got
	defer func() { stdout = os.Stdout }()
	blocker := filepath.Join(t.TempDir(), "docs")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(blocker, "api-findings.md") // the parent is a file
	err := Run(context.Background(), probeOptions(fakeServer(t, "/rest/events").URL, out), quiet)
	if err == nil || !strings.Contains(err.Error(), "cannot write the probe report to "+out) {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(got.String(), "## SOL") || !strings.Contains(got.String(), "### Counts (9) and full check (5)") {
		t.Fatalf("the report is not on standard output:\n%s", got.String())
	}

	// A complete run whose report cannot be written also fails.
	got.Reset()
	o := probeOptions(fakeServer(t, "/none").URL, out)
	if err := Run(context.Background(), o, quiet); err == nil || !strings.Contains(got.String(), "### Events (3, 4)") {
		t.Fatalf("complete run: %v\n%s", err, got.String())
	}
	// With a writable --out a complete run succeeds.
	got.Reset()
	o.Out = filepath.Join(t.TempDir(), "report.md")
	if err := Run(context.Background(), o, quiet); err != nil || got.Len() != 0 {
		t.Fatalf("complete run: %v\n%s", err, got.String())
	}
}
