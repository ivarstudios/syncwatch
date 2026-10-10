package harness

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DiscordMessage is one webhook payload received by the fake.
type DiscordMessage struct {
	At      time.Time
	Content string `json:"content"`
	Embeds  []struct {
		Title       string `json:"title"`
		Description string `json:"description"`
		Color       int    `json:"color"`
		URL         string `json:"url"`
	} `json:"embeds"`
	AllowedMentions struct {
		Parse []string `json:"parse"`
		Roles []string `json:"roles"`
	} `json:"allowed_mentions"`
	Problems []string `json:"-"` // limit violations found by the fake
}

// Text is the content and embed text together, for matching.
func (m DiscordMessage) Text() string {
	var b strings.Builder
	b.WriteString(m.Content)
	for _, e := range m.Embeds {
		b.WriteString("\n" + e.Title + "\n" + e.Description)
	}
	return b.String()
}

// Mentions reports whether the message pings a role.
func (m DiscordMessage) Mentions(role string) bool {
	return strings.Contains(m.Content, "<@&"+role+">")
}

// FakeDiscord records webhook posts and checks Discord's size limits.
type FakeDiscord struct {
	mu   sync.Mutex
	msgs []DiscordMessage
	srv  *http.Server
	ln   net.Listener
	// Fail makes the next N posts return HTTP 500.
	Fail    int
	mode    fakeMode // from SetMode; the zero value answers 204
	refused int      // posts answered with an error
}

// fakeMode is an error answer that replaces the 204, as in the kit's fake
// Discord: HTTP 500, 400 (a field error), 404 (Unknown Webhook) or 429
// (rate limited, with retry_after). left is the number of posts it still
// applies to; 0 means every post.
type fakeMode struct {
	spec       string
	status     int
	retryAfter float64
	left       int
}

var modeSpec = regexp.MustCompile(`^(ok|fail500|fail400|fail404|ratelimit)(?::([0-9.]+))?(?::([0-9]+))?$`)

// StartFakeDiscord listens on addr (e.g. "127.0.0.1:0").
func StartFakeDiscord(addr string) (*FakeDiscord, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	f := &FakeDiscord{ln: ln}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/webhooks/{id}/{token}", f.handle)
	mux.HandleFunc("GET /messages", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(f.Messages())
	})
	mux.HandleFunc("GET /mode", f.handleMode)
	mux.HandleFunc("POST /mode", f.handleMode)
	f.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = f.srv.Serve(ln) }()
	return f, nil
}

// URL is the webhook URL to configure.
func (f *FakeDiscord) URL() string {
	return fmt.Sprintf("http://%s/api/webhooks/123/fake-token", f.ln.Addr().String())
}

// Close stops the server.
func (f *FakeDiscord) Close() { _ = f.srv.Close() }

// SetMode selects the answer to the next posts, with the names of the kit's
// fake Discord: "ok" (the default: 204), "fail500[:N]", "fail400[:N]" (a
// field error), "fail404[:N]" (Unknown Webhook) or
// "ratelimit[:RETRY_AFTER[:N]]" (429, retry_after in seconds, default 2).
// With N, the mode applies to the next N posts, then the fake answers 204
// again. Fail has priority over the mode.
func (f *FakeDiscord) SetMode(spec string) error {
	spec = strings.TrimSpace(spec)
	m := modeSpec.FindStringSubmatch(spec)
	if m == nil {
		return fmt.Errorf("unknown mode %q: use ok, fail500[:N], fail400[:N], fail404[:N] or ratelimit[:RETRY_AFTER[:N]]", spec)
	}
	mode := fakeMode{spec: spec}
	switch m[1] {
	case "ok":
		if m[2] != "" || m[3] != "" {
			return fmt.Errorf("unknown mode %q: ok has no arguments", spec)
		}
		mode = fakeMode{}
	case "ratelimit":
		mode.status, mode.retryAfter = http.StatusTooManyRequests, 2
		if m[2] != "" {
			ra, err := strconv.ParseFloat(m[2], 64)
			if err != nil || ra < 0 {
				return fmt.Errorf("unknown mode %q: RETRY_AFTER must be a number of seconds", spec)
			}
			mode.retryAfter = ra
		}
		if m[3] != "" {
			if mode.left, _ = strconv.Atoi(m[3]); mode.left < 1 {
				return fmt.Errorf("unknown mode %q: N must be 1 or more", spec)
			}
		}
	default:
		mode.status, _ = strconv.Atoi(strings.TrimPrefix(m[1], "fail"))
		if m[3] != "" {
			return fmt.Errorf("unknown mode %q: %s takes only N", spec, m[1])
		}
		if m[2] != "" {
			n, err := strconv.Atoi(m[2])
			if err != nil || n < 1 {
				return fmt.Errorf("unknown mode %q: N must be a whole number, 1 or more", spec)
			}
			mode.left = n
		}
	}
	f.mu.Lock()
	f.mode = mode
	f.mu.Unlock()
	return nil
}

// Mode is the current mode, "ok" when the fake answers 204.
func (f *FakeDiscord) Mode() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mode.status == 0 {
		return "ok"
	}
	if f.mode.left > 0 {
		return fmt.Sprintf("%s (%d left)", f.mode.spec, f.mode.left)
	}
	return f.mode.spec
}

// Refused is the number of posts that got an error answer (Fail or a mode).
func (f *FakeDiscord) Refused() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refused
}

// handleMode answers GET /mode and sets the mode for POST /mode
// {"mode": "fail404:3"}.
func (f *FakeDiscord) handleMode(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var req struct {
			Mode string `json:"mode"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); err != nil {
			http.Error(w, `the body must be JSON, for example {"mode": "fail404:3"}`, http.StatusBadRequest)
			return
		}
		if err := f.SetMode(req.Mode); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"mode": f.Mode()})
}

// takeMode is the mode for one post. It counts a limited mode down. The
// caller holds f.mu.
func (f *FakeDiscord) takeMode() fakeMode {
	m := f.mode
	if f.mode.left > 0 {
		f.mode.left--
		if f.mode.left == 0 {
			f.mode = fakeMode{}
		}
	}
	return m
}

// answer writes the error answer of a mode with the bodies and headers of
// Discord.
func (m fakeMode) answer(w http.ResponseWriter) {
	var body string
	switch m.status {
	case http.StatusBadRequest:
		body = `{"message": "Invalid Form Body", "code": 50035, "errors": {"embeds": {"0": {"description": {"_errors": [{"code": "BASE_TYPE_MAX_LENGTH", "message": "Must be 4096 or fewer in length."}]}}}}}`
	case http.StatusNotFound:
		body = `{"message": "Unknown Webhook", "code": 10015}`
	case http.StatusTooManyRequests:
		body = fmt.Sprintf(`{"message": "You are being rate limited.", "retry_after": %s, "global": false}`, strconv.FormatFloat(m.retryAfter, 'f', -1, 64))
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(m.retryAfter))))
		w.Header().Set("X-RateLimit-Limit", "5")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset-After", strconv.FormatFloat(m.retryAfter, 'f', 3, 64))
		w.Header().Set("X-RateLimit-Scope", "shared")
	default:
		body = `{"message": "500: Internal Server Error", "code": 0}`
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(m.status)
	_, _ = io.WriteString(w, body)
}

func (f *FakeDiscord) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	var mode fakeMode
	fail := f.Fail > 0
	if fail {
		f.Fail--
	} else {
		mode = f.takeMode()
	}
	if fail || mode.status != 0 {
		f.refused++
	}
	f.mu.Unlock()
	if fail {
		http.Error(w, `{"message":"simulated outage"}`, http.StatusInternalServerError)
		return
	}
	if mode.status != 0 {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
		mode.answer(w)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var m DiscordMessage
	if err := json.Unmarshal(body, &m); err != nil {
		http.Error(w, `{"message":"bad json"}`, http.StatusBadRequest)
		return
	}
	m.At = time.Now()
	if len(m.Content) > 2000 {
		m.Problems = append(m.Problems, "content over 2000 characters")
	}
	if len(m.Embeds) > 10 {
		m.Problems = append(m.Problems, "more than 10 embeds")
	}
	total := 0
	for _, e := range m.Embeds {
		if len([]rune(e.Description)) > 4096 {
			m.Problems = append(m.Problems, "embed description over 4096 characters")
		}
		total += len([]rune(e.Title)) + len([]rune(e.Description))
	}
	if total > 6000 {
		m.Problems = append(m.Problems, "embeds over 6000 characters")
	}
	f.mu.Lock()
	f.msgs = append(f.msgs, m)
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// Messages returns every message received so far.
func (f *FakeDiscord) Messages() []DiscordMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]DiscordMessage(nil), f.msgs...)
}

// WaitFor waits for a message (received at or after since) matching pred.
func (f *FakeDiscord) WaitFor(since time.Time, timeout time.Duration, pred func(DiscordMessage) bool) (DiscordMessage, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, m := range f.Messages() {
			if !m.At.Before(since) && pred(m) {
				return m, true
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	return DiscordMessage{}, false
}
