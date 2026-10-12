package store

import (
	"bytes"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// Open tells what it did with the single API token of an older version, so
// the serve command can write it in the log: added at the first update,
// replaced after a downgrade and a new update, nothing for a value that an
// API token has already, refused when 5 other API tokens exist, and nothing
// at the next starts.
func TestOpenReportsTheMoveOfTheOldAPIToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "syncwatch.db")
	key := bytes.Repeat([]byte{5}, 32)
	open := func() *Store {
		t.Helper()
		s, err := Open(path, key)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	olderVersion := func(s *Store, v string) {
		t.Helper()
		if err := s.SetSecret(SecretAPIToken, v); err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
	want := func(step string, s *Store, w OldTokenMove) {
		t.Helper()
		if got := s.OldAPITokenMove(); got != w {
			t.Fatalf("%s: OldAPITokenMove() = %d, want %d", step, got, w)
		}
	}

	s := open()
	want("a new database", s, OldTokenNone)
	olderVersion(s, "swt_old_value_1")
	s = open()
	want("the first update", s, OldTokenAdded)
	s.Close()
	s = open()
	want("the next start", s, OldTokenNone)
	olderVersion(s, "swt_old_value_2") // a downgrade makes a new value
	s = open()
	want("the update after the downgrade", s, OldTokenReplaced)
	olderVersion(s, "swt_old_value_2")
	s = open()
	want("a value that an API token has already", s, OldTokenNone)

	legacy, err := s.APITokenByName(LegacyTokenName)
	if err != nil || legacy == nil {
		t.Fatalf("the API token %q: %+v %v", LegacyTokenName, legacy, err)
	}
	if err := s.DeleteAPIToken(legacy.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxAPITokens; i++ {
		if _, err := s.AddAPIToken(fmt.Sprintf("token %d", i), ScopeHA, fmt.Sprintf("swt_value_%d_0123456789", i), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	olderVersion(s, "swt_old_value_3")
	s = open()
	want("5 API tokens and none is \"API token\"", s, OldTokenRefused)
	if !s.HasSecret(SecretAPIToken) {
		t.Fatal("the refused value is not kept as a secret")
	}
	s.Close()
	s = open()
	want("the start after the refusal", s, OldTokenNone)
	s.Close()
}
