package store

import (
	"bytes"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	key, err := LoadKey(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(dir, "syncwatch.db"), key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSecrets(t *testing.T) {
	dir := t.TempDir()
	key, _ := LoadKey(dir, "")
	key2, _ := LoadKey(dir, "")
	if !bytes.Equal(key, key2) {
		t.Fatal("key not persisted")
	}
	s, err := Open(filepath.Join(dir, "db"), key)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SetSecret("a", "hunter2"); err != nil {
		t.Fatal(err)
	}
	v, err := s.Secret("a")
	if err != nil || v != "hunter2" || !s.HasSecret("a") || s.HasSecret("b") {
		t.Fatalf("got %q %v", v, err)
	}
	// The plaintext must not appear in the database file.
	s.Close()
	for _, f := range []string{"db", "db-wal"} {
		b, _ := os.ReadFile(filepath.Join(dir, f))
		if bytes.Contains(b, []byte("hunter2")) {
			t.Fatalf("plaintext secret in %s", f)
		}
	}
	// A different key can't decrypt.
	other, _ := LoadKey(t.TempDir(), "")
	s2, _ := Open(filepath.Join(dir, "db"), other)
	defer s2.Close()
	if _, err := s2.Secret("a"); err == nil {
		t.Fatal("decrypted with the wrong key")
	}
	// SYNCWATCH_SECRET_KEY takes a random key only; a passphrase hashed once
	// could be guessed from a copy of the database.
	raw := bytes.Repeat([]byte{7}, 32)
	if k, err := LoadKey("", base64.StdEncoding.EncodeToString(raw)); err != nil || !bytes.Equal(k, raw) {
		t.Fatalf("base64 key: %v", err)
	}
	for _, bad := range []string{"a passphrase that is long enough", "short", base64.StdEncoding.EncodeToString(raw[:16])} {
		if _, err := LoadKey("", bad); err == nil || !strings.Contains(err.Error(), "openssl rand -base64 32") {
			t.Fatalf("%q: %v", bad, err)
		}
	}
}

func TestRecordsAndHistory(t *testing.T) {
	s := testStore(t)
	now := time.Now().Truncate(time.Millisecond)
	r := &Record{Finding: model.Finding{ID: "x", Check: "H1", Checks: []string{"H1"}, Severity: model.Error, Category: "health",
		Server: "a", Subject: "A", Message: "down", Urgent: true, Details: []model.Detail{{Key: "k", Value: "v"}}},
		Since: now, OpenedAt: now, LastSeen: now}
	if err := s.SaveRecord(r); err != nil {
		t.Fatal(err)
	}
	if err := s.HistoryOpen(r); err != nil {
		t.Fatal(err)
	}
	open, _ := s.OpenRecords()
	if len(open) != 1 || open[0].Details[0].Value != "v" || !open[0].Urgent || !open[0].OpenedAt.Equal(now) {
		t.Fatalf("%+v", open)
	}
	r.ResolvedAt, r.PingedAt = now.Add(time.Minute), now
	_ = s.SaveRecord(r)
	_ = s.HistoryResolve("x", r.ResolvedAt)
	pend, _ := s.PendingResolved()
	if len(pend) != 1 {
		t.Fatal("pending resolved")
	}
	h, _ := s.HistoryBetween(now.Add(-time.Hour), now.Add(time.Hour))
	if len(h) != 1 || h[0].ResolvedAt.IsZero() {
		t.Fatalf("history %+v", h)
	}
}

func TestConflicts(t *testing.T) {
	s := testStore(t)
	t0 := time.Now()
	_ = s.RecordConflicts("a", "s1", map[string]int64{"f": 5}, t0)
	_ = s.RecordConflicts("a", "s1", map[string]int64{"f": 7}, t0.Add(time.Hour))
	_ = s.RecordConflicts("a", "s2", map[string]int64{"f": 1}, t0.Add(2*time.Hour)) // restart
	got, _ := s.ConflictsSince(t0.Add(-time.Minute))
	if got["a"]["f"] != 3 {
		t.Fatalf("got %v", got)
	}
}

func TestObservationsAndSnoozes(t *testing.T) {
	s := testStore(t)
	t0 := time.Now().Truncate(time.Millisecond)
	_ = s.ApplyObservations(map[string]Observation{"a": {Since: t0}, "b": {Since: t0, Servers: []string{"x", "y"}}}, nil)
	_ = s.ApplyObservations(nil, []string{"a"})
	obs, _ := s.Observations()
	if len(obs) != 1 || !obs["b"].Since.Equal(t0) || len(obs["b"].Servers) != 2 {
		t.Fatal(obs)
	}
	_ = s.SetSnooze(Snooze{FindingID: "f", Kind: SnoozeKind, Until: t0.Add(time.Hour), CreatedAt: t0})
	z, _ := s.Snoozes()
	if !z["f"].Active(t0) || z["f"].Active(t0.Add(2*time.Hour)) {
		t.Fatal("snooze")
	}
}

func TestSessions(t *testing.T) {
	s := testStore(t)
	t0 := time.Now()
	_ = s.CreateSession(Session{TokenHash: "h", Role: "admin", CSRF: "c", CreatedAt: t0, ExpiresAt: t0.Add(time.Hour)})
	z, _ := s.Session("h", t0)
	if z == nil || z.Role != "admin" {
		t.Fatal("session")
	}
	if z, _ := s.Session("h", t0.Add(2*time.Hour)); z != nil {
		t.Fatal("expired session returned")
	}
}

func TestServerKeyBinding(t *testing.T) {
	s := testStore(t)
	if err := s.SetServerKey("akka", "k1", "https://10.0.0.1:8384"); err != nil {
		t.Fatal(err)
	}
	key, url, err := s.ServerKey("akka")
	if err != nil || key != "k1" || url != "https://10.0.0.1:8384" {
		t.Fatalf("got %q %q %v", key, url, err)
	}
	// A key stored before keys were bound comes back without a URL.
	_ = s.SetSecret(ServerAPIKeySecret("sol"), "legacy")
	if key, url, _ := s.ServerKey("sol"); key != "legacy" || url != "" {
		t.Fatalf("legacy key: %q %q", key, url)
	}
	if err := s.SetServerKey("akka", "", ""); err != nil || s.HasSecret(ServerAPIKeySecret("akka")) {
		t.Fatal("empty key must delete the secret")
	}
}
