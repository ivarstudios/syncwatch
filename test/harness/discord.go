package harness

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
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
	Fail int
}

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

func (f *FakeDiscord) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	fail := f.Fail > 0
	if fail {
		f.Fail--
	}
	f.mu.Unlock()
	if fail {
		http.Error(w, `{"message":"simulated outage"}`, http.StatusInternalServerError)
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
