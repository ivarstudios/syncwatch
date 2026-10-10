package harness

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// post sends one webhook message to the fake and returns the answer.
func post(t *testing.T, f *FakeDiscord) (int, http.Header, string) {
	t.Helper()
	resp, err := http.Post(f.URL(), "application/json", strings.NewReader(`{"content":"test"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b)
}

// TestFakeDiscordModes checks the error answers of the fake: the default,
// limited and unlimited modes, the Discord bodies and Fail's priority.
func TestFakeDiscordModes(t *testing.T) {
	f, err := StartFakeDiscord("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if code, _, _ := post(t, f); code != http.StatusNoContent || f.Mode() != "ok" {
		t.Fatalf("default: HTTP %d, mode %q, want 204 and ok", code, f.Mode())
	}

	if err := f.SetMode("fail404:2"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		code, h, body := post(t, f)
		if code != http.StatusNotFound || !strings.Contains(body, `"Unknown Webhook"`) || !strings.Contains(body, "10015") || h.Get("Content-Type") != "application/json" {
			t.Fatalf("fail404 post %d: HTTP %d %q (%s)", i+1, code, body, h.Get("Content-Type"))
		}
	}
	if code, _, _ := post(t, f); code != http.StatusNoContent || f.Mode() != "ok" {
		t.Fatalf("after fail404:2: HTTP %d, mode %q, want 204 and ok", code, f.Mode())
	}

	if err := f.SetMode("fail400"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		code, _, body := post(t, f)
		var e struct {
			Message string          `json:"message"`
			Code    int             `json:"code"`
			Errors  json.RawMessage `json:"errors"`
		}
		if code != http.StatusBadRequest || json.Unmarshal([]byte(body), &e) != nil || e.Message != "Invalid Form Body" || e.Code != 50035 || !strings.Contains(string(e.Errors), "_errors") {
			t.Fatalf("fail400 post %d: HTTP %d %q", i+1, code, body)
		}
	}

	if err := f.SetMode("ratelimit:0.5:1"); err != nil {
		t.Fatal(err)
	}
	code, h, body := post(t, f)
	var rl struct {
		RetryAfter float64 `json:"retry_after"`
	}
	if code != http.StatusTooManyRequests || json.Unmarshal([]byte(body), &rl) != nil || rl.RetryAfter != 0.5 || h.Get("Retry-After") != "1" {
		t.Fatalf("ratelimit: HTTP %d %q, Retry-After %q", code, body, h.Get("Retry-After"))
	}
	if code, _, _ := post(t, f); code != http.StatusNoContent {
		t.Fatalf("after ratelimit:0.5:1: HTTP %d, want 204", code)
	}

	// Fail has priority and does not use up a limited mode.
	if err := f.SetMode("fail500:1"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.Fail = 1
	f.mu.Unlock()
	if code, _, body := post(t, f); code != http.StatusInternalServerError || !strings.Contains(body, "simulated outage") {
		t.Fatalf("Fail: HTTP %d %q", code, body)
	}
	if code, _, body := post(t, f); code != http.StatusInternalServerError || !strings.Contains(body, "Internal Server Error") {
		t.Fatalf("fail500:1: HTTP %d %q", code, body)
	}
	if code, _, _ := post(t, f); code != http.StatusNoContent {
		t.Fatalf("after fail500:1: HTTP %d, want 204", code)
	}

	if got := len(f.Messages()); got != 4 {
		t.Errorf("recorded %d messages, want the 4 that got 204", got)
	}
	if got := f.Refused(); got != 8 {
		t.Errorf("Refused() = %d, want 8", got)
	}
}

// TestFakeDiscordModeEndpoint sets the mode through POST /mode, as the kit's
// envctl discord-mode does, and refuses unknown modes.
func TestFakeDiscordModeEndpoint(t *testing.T) {
	f, err := StartFakeDiscord("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	base := strings.TrimSuffix(f.URL(), "/api/webhooks/123/fake-token")

	resp, err := http.Post(base+"/mode", "application/json", strings.NewReader(`{"mode": "fail404:3"}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(b), "fail404:3 (3 left)") {
		t.Fatalf("POST /mode: HTTP %d %q", resp.StatusCode, b)
	}
	if code, _, _ := post(t, f); code != http.StatusNotFound {
		t.Fatalf("after POST /mode: HTTP %d, want 404", code)
	}

	resp, err = http.Get(base + "/mode")
	if err != nil {
		t.Fatal(err)
	}
	b, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), "fail404:3 (2 left)") {
		t.Fatalf("GET /mode: %q", b)
	}

	for _, bad := range []string{"fail418", "fail404:0", "fail404:x", "fail404:1:2", "ratelimit:1:0", "ok:1", "slow:5", ""} {
		if err := f.SetMode(bad); err == nil {
			t.Errorf("SetMode(%q) accepted", bad)
		}
	}
	resp, err = http.Post(base+"/mode", "application/json", strings.NewReader(`{"mode": "nonsense"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST /mode nonsense: HTTP %d, want 400", resp.StatusCode)
	}
	if !strings.HasPrefix(f.Mode(), "fail404:3") {
		t.Errorf("a refused mode changed the mode to %q", f.Mode())
	}
}
