package store

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// API tokens: add, list, look up by value, touch, replace and revoke. The
// store keeps the hash of a value, never the value.
func TestAPITokens(t *testing.T) {
	s := testStore(t)
	t0 := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	ha, err := s.AddAPIToken(" Home Assistant ", ScopeHA, "swt_ha", t0)
	if err != nil || ha.Name != "Home Assistant" || ha.Scope != ScopeHA || !ha.CreatedAt.Equal(t0) || !ha.LastUsed.IsZero() {
		t.Fatalf("add: %+v %v", ha, err)
	}
	if len(ha.ID) != 8 || strings.Trim(ha.ID, "0123456789abcdef") != "" {
		t.Fatalf("ID %q: want 8 lower-case hex characters", ha.ID)
	}
	full, err := s.AddAPIToken("Scripts", ScopeFull, "swt_full", t0.Add(time.Minute))
	if err != nil || full.ID == ha.ID {
		t.Fatalf("second token: %+v %v", full, err)
	}
	list, err := s.APITokens()
	if err != nil || len(list) != 2 || list[0] != ha || list[1] != full {
		t.Fatalf("list: %+v %v", list, err)
	}
	var stored string
	if err := s.db.QueryRow(`SELECT name || scope || hash FROM api_tokens WHERE id=?`, ha.ID).Scan(&stored); err != nil || strings.Contains(stored, "swt_ha") || !strings.Contains(stored, HashAPIToken("swt_ha")) {
		t.Fatalf("stored row %q %v: want the hash, not the value", stored, err)
	}
	if got, err := s.APITokenByValue("swt_ha"); err != nil || got == nil || *got != ha {
		t.Fatalf("lookup: %+v %v", got, err)
	}
	for _, v := range []string{"swt_other", "", "SWT_HA", HashAPIToken("swt_ha")} {
		if got, err := s.APITokenByValue(v); err != nil || got != nil {
			t.Fatalf("lookup %q: %+v %v", v, got, err)
		}
	}

	if err := s.TouchAPIToken(ha.ID, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.APITokenByValue("swt_ha"); !got.LastUsed.Equal(t0.Add(time.Hour)) {
		t.Fatalf("last used %v", got.LastUsed)
	}
	if err := s.TouchAPIToken("nope", t0); !errors.Is(err, ErrNoToken) {
		t.Fatalf("touch of an unknown ID: %v", err)
	}
	// The lookup by name compares without regard to case, as the rule
	// for a unique name.
	for _, name := range []string{"Home Assistant", "home assistant", " HOME ASSISTANT "} {
		if got, err := s.APITokenByName(name); err != nil || got == nil || got.ID != ha.ID {
			t.Fatalf("lookup by the name %q: %+v %v", name, got, err)
		}
	}
	if got, err := s.APITokenByName("Home"); err != nil || got != nil {
		t.Fatalf("lookup by a name that no API token has: %+v %v", got, err)
	}

	// A new value: the old one stops working, the name and the scope stay.
	if err := s.ReplaceAPIToken(ha.ID, "swt_ha2", t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.APITokenByValue("swt_ha"); got != nil {
		t.Fatal("the old value still works")
	}
	got, _ := s.APITokenByValue("swt_ha2")
	if got == nil || got.ID != ha.ID || got.Name != "Home Assistant" || got.Scope != ScopeHA || !got.LastUsed.IsZero() || !got.CreatedAt.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("replaced: %+v", got)
	}
	if err := s.ReplaceAPIToken("nope", "swt_x", t0); !errors.Is(err, ErrNoToken) {
		t.Fatalf("replace of an unknown ID: %v", err)
	}

	if err := s.DeleteAPIToken(full.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.APITokenByValue("swt_full"); got != nil {
		t.Fatal("a revoked API token still works")
	}
	if err := s.DeleteAPIToken(full.ID); !errors.Is(err, ErrNoToken) {
		t.Fatalf("second revoke: %v", err)
	}
	if list, _ := s.APITokens(); len(list) != 1 || list[0].ID != ha.ID {
		t.Fatalf("after the revoke: %+v", list)
	}
}

// A name has 1 to 40 characters, no control characters, and no other API
// token has it, without regard to case. The scope is ha or full. There are
// at most 5 API tokens.
func TestAPITokenRules(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	for _, name := range []string{"", "   ", strings.Repeat("x", MaxTokenName+1), "bell\x07", "zero​width", "bad\xffutf8"} {
		if _, err := s.AddAPIToken(name, ScopeHA, "swt_"+name, now); !errors.Is(err, ErrTokenName) {
			t.Errorf("name %q: %v", name, err)
		}
	}
	for _, scope := range []string{"", "admin", "HA", "Full"} {
		if _, err := s.AddAPIToken("x", scope, "swt_scope", now); !errors.Is(err, ErrTokenScope) {
			t.Errorf("scope %q: %v", scope, err)
		}
	}
	if _, err := s.AddAPIToken("Home Assistant", ScopeHA, "swt_1", now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddAPIToken("HOME ASSISTANT", ScopeFull, "swt_2", now); !errors.Is(err, ErrTokenName) {
		t.Fatalf("the same name in another case: %v", err)
	}
	if _, err := s.AddAPIToken(strings.Repeat("å", MaxTokenName), ScopeFull, "swt_2", now); err != nil {
		t.Fatalf("a name of 40 characters: %v", err)
	}
	for i := 3; i <= MaxAPITokens; i++ {
		if _, err := s.AddAPIToken("token "+string(rune('0'+i)), ScopeFull, "swt_"+string(rune('0'+i)), now); err != nil {
			t.Fatal(err)
		}
	}
	_, err := s.AddAPIToken("one more", ScopeHA, "swt_6", now)
	if !errors.Is(err, ErrTooManyTokens) || err.Error() != "There are 5 API tokens. Revoke an API token first." {
		t.Fatalf("a sixth API token: %v", err)
	}
	list, _ := s.APITokens()
	if err := s.DeleteAPIToken(list[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddAPIToken("one more", ScopeHA, "swt_6", now); err != nil {
		t.Fatalf("after a revoke: %v", err)
	}
	if _, err := s.AddAPIToken("no value", ScopeHA, "", now); err == nil {
		t.Fatal("an API token without a value")
	}
}

// The single API token of the build before api_tokens is the secret
// api.token. The first open with this build moves it into api_tokens: the
// token "API token" with the scope full, created when the secret was saved.
// The secret is gone, and a second open changes nothing.
func TestOldAPITokenMigrated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "syncwatch.db")
	key := bytes.Repeat([]byte{3}, 32)
	s, err := Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetSecret(SecretAPIToken, "swt_old"); err != nil {
		t.Fatal(err)
	}
	var saved int64
	if err := s.db.QueryRow(`SELECT updated_at FROM secrets WHERE name=?`, SecretAPIToken).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	// The database of the build before: the same schema without the new tables.
	for _, table := range []string{"api_tokens", "folder_activity"} {
		if _, err := s.db.Exec(`DROP TABLE ` + table); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()

	s, err = Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.APITokens()
	if err != nil || len(list) != 1 {
		t.Fatalf("after the migration: %+v %v", list, err)
	}
	tok := list[0]
	if tok.Name != "API token" || tok.Scope != ScopeFull || !tok.CreatedAt.Equal(fromMS(saved)) || !tok.LastUsed.IsZero() {
		t.Fatalf("migrated token %+v, want API token, full, created %v", tok, fromMS(saved))
	}
	if got, err := s.APITokenByValue("swt_old"); err != nil || got == nil || got.ID != tok.ID {
		t.Fatalf("the old value: %+v %v", got, err)
	}
	if s.HasSecret(SecretAPIToken) {
		t.Fatal("the secret api.token is still stored")
	}
	if v, _ := s.Setting("schema_version"); v != "1" {
		t.Fatalf("schema_version %q: an older syncwatch must still open the database", v)
	}
	if _, err := s.FolderActivity("akka"); err != nil {
		t.Fatalf("folder_activity: %v", err)
	}
	s.Close()

	s, err = Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := s.APITokens(); len(again) != 1 || again[0] != tok {
		t.Fatalf("a second open changed the API tokens: %+v", again)
	}
	// While api_tokens has a row, a secret api.token stays a secret.
	if err := s.SetSecret(SecretAPIToken, "swt_new"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path, key)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if again, _ := s.APITokens(); len(again) != 1 || !s.HasSecret(SecretAPIToken) {
		t.Fatalf("api.token with a row in api_tokens: %+v, secret %v", again, s.HasSecret(SecretAPIToken))
	}
}

// An old API token that the secret key cannot decrypt is not migrated: it
// stays a secret, and Settings shows it. With the right secret key back, the
// next open migrates it. A stored value that is not an encrypted secret at
// all does not stop the open.
func TestUndecryptableOldAPITokenStays(t *testing.T) {
	path := filepath.Join(t.TempDir(), "syncwatch.db")
	keyA, keyB := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	s, err := Open(path, keyA)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.SetSecret(SecretAPIToken, "swt_old")
	s.Close()

	s, err = Open(path, keyB)
	if err != nil {
		t.Fatal(err)
	}
	if list, _ := s.APITokens(); len(list) != 0 || !s.HasSecret(SecretAPIToken) {
		t.Fatalf("with another secret key: %+v, secret %v", list, s.HasSecret(SecretAPIToken))
	}
	if bad, _, _ := s.UnreadableSecrets(); len(bad) != 1 || bad[0] != SecretAPIToken {
		t.Fatalf("unreadable %v", bad)
	}
	s.Close()

	s, err = Open(path, keyA)
	if err != nil {
		t.Fatal(err)
	}
	if list, _ := s.APITokens(); len(list) != 1 || s.HasSecret(SecretAPIToken) {
		t.Fatalf("with the right secret key again: %+v, secret %v", list, s.HasSecret(SecretAPIToken))
	}
	if err := s.DeleteAPIToken(func() string { l, _ := s.APITokens(); return l[0].ID }()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO secrets(name, nonce, ciphertext, updated_at) VALUES(?, x'010203', x'0405', 1)`, SecretAPIToken); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path, keyA)
	if err != nil {
		t.Fatalf("open with a damaged secret: %v", err)
	}
	defer s.Close()
	if _, err := s.Secret(SecretAPIToken); err == nil || !s.HasSecret(SecretAPIToken) {
		t.Fatalf("a damaged secret: %v, kept %v", err, s.HasSecret(SecretAPIToken))
	}
}
