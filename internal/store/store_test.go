package store

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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

// Two first starts at once (serve plus a maintenance command, say) must end
// up with the same key: the second writer may not overwrite the first's.
func TestKeyFileCreatedOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.key")
	first := bytes.Repeat([]byte{1}, 32)
	if err := createKeyFile(path, first); err != nil {
		t.Fatal(err)
	}
	if err := createKeyFile(path, bytes.Repeat([]byte{2}, 32)); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("existing key file overwritten: %v", err)
	}
	if key, err := LoadKey(dir, ""); err != nil || !bytes.Equal(key, first) {
		t.Fatalf("got %x %v", key, err)
	}
	if fi, _ := os.Stat(path); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v", fi.Mode().Perm())
	}

	dir = t.TempDir()
	var wg sync.WaitGroup
	keys := make([][]byte, 8)
	for i := range keys {
		wg.Add(1)
		go func() {
			defer wg.Done()
			k, err := LoadKey(dir, "")
			if err != nil {
				t.Error(err)
			}
			keys[i] = k
		}()
	}
	wg.Wait()
	stored, _ := LoadKey(dir, "")
	for i, k := range keys {
		if !bytes.Equal(k, stored) {
			t.Fatalf("start %d got a key that isn't the stored one", i)
		}
	}
}

// The database holds password hashes, session tokens and the encrypted
// secrets; it and its WAL and SHM files are for the owner only.
func TestDatabaseFilesOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file modes mean nothing on Windows")
	}
	dir := t.TempDir()
	dirMode, _ := os.Stat(dir)
	path := filepath.Join(dir, "syncwatch.db")
	s, err := Open(path, make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, f := range []string{path, path + "-wal", path + "-shm"} {
		fi, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("%s has mode %v", filepath.Base(f), fi.Mode().Perm())
		}
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != dirMode.Mode().Perm() {
		t.Errorf("the data directory's mode was changed to %v", fi.Mode().Perm())
	}
}

// A missing or replaced secret key is found at start: every stored secret is
// unreadable with the new key, and none with the right one.
func TestUnreadableSecrets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "syncwatch.db")
	right := bytes.Repeat([]byte{1}, 32)
	s, err := Open(path, right)
	if err != nil {
		t.Fatal(err)
	}
	if bad, total, err := s.UnreadableSecrets(); err != nil || bad != nil || total != 0 {
		t.Fatalf("empty store: %v %d %v", bad, total, err)
	}
	_ = s.SetSecret(SecretDiscordWebhook, "https://discord.com/api/webhooks/1/x")
	_ = s.SetServerKey("akka", "k1", "https://10.0.0.1:8384")
	if bad, total, err := s.UnreadableSecrets(); err != nil || bad != nil || total != 2 {
		t.Fatalf("right key: %v %d %v", bad, total, err)
	}
	s.Close()
	s, err = Open(path, bytes.Repeat([]byte{2}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	bad, total, err := s.UnreadableSecrets()
	if err != nil || total != 2 || strings.Join(bad, ",") != SecretDiscordWebhook+","+ServerAPIKeySecret("akka") {
		t.Fatalf("other key: %v %d %v", bad, total, err)
	}
	// A secret typed again with the new key is readable, the others are not.
	_ = s.SetSecret(SecretDiscordWebhook, "https://discord.com/api/webhooks/1/y")
	if bad, total, _ := s.UnreadableSecrets(); total != 2 || len(bad) != 1 || bad[0] != ServerAPIKeySecret("akka") {
		t.Fatalf("after a new webhook: %v %d", bad, total)
	}
}

func TestSettingsWithPrefix(t *testing.T) {
	s := testStore(t)
	for k, v := range map[string]string{"cli.notice.1": "a", "cli.notice.2": "b", "cli.noticeX": "c", "cli_notice.3": "d", "other": "e"} {
		if err := s.SetSetting(k, v); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.SettingsWithPrefix("cli.notice.")
	if err != nil || len(got) != 2 || got["cli.notice.1"] != "a" || got["cli.notice.2"] != "b" {
		t.Fatalf("got %v %v", got, err)
	}
	if got, err := s.SettingsWithPrefix("none."); err != nil || len(got) != 0 {
		t.Fatalf("no match: %v %v", got, err)
	}
}

func TestConfigRevision(t *testing.T) {
	s := testStore(t)
	if rev, err := s.ConfigRevision(); err != nil || rev != 0 {
		t.Fatalf("initial revision %d %v", rev, err)
	}
	for want := int64(1); want <= 3; want++ {
		if rev, err := s.BumpConfigRevision(); err != nil || rev != want {
			t.Fatalf("bump: got %d %v, want %d", rev, err, want)
		}
	}
	if rev, err := s.ConfigRevision(); err != nil || rev != 3 {
		t.Fatalf("revision %d %v", rev, err)
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

// A second process can see secret.key between the moment its creator opened
// it (O_EXCL) and the moment the key was written: an existing, still empty
// file. That is not a corrupt key but a key on its way, so LoadKey waits
// for it instead of failing the start.
func TestKeyFileReadWhileBeingWritten(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.key")
	if err := os.WriteFile(path, nil, 0o600); err != nil { // created, not yet written
		t.Fatal(err)
	}
	want := bytes.Repeat([]byte{7}, 32)
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(want)+"\n"), 0o600)
	}()
	key, err := LoadKey(dir, "")
	if err != nil || !bytes.Equal(key, want) {
		t.Fatalf("key still being written by another process: got %x, %v", key, err)
	}
	// A file that stays invalid is still an error, after the wait.
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "secret.key"), []byte("not base64!\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKey(dir, ""); err == nil || !strings.Contains(err.Error(), "not a valid secret key") {
		t.Fatalf("corrupt key file accepted: %v", err)
	}
}
