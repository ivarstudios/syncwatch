package app

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/store"
)

// The store moves the API token of an older version when it opens, also
// after a downgrade and a new update, but it has no logger: the serve
// command writes what happened in the log.
func TestServeLogsTheMoveOfTheOldAPIToken(t *testing.T) {
	dir := t.TempDir()
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	olderVersion := func(v string) {
		t.Helper()
		st, err := OpenStore(dir, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := st.SetSecret(store.SecretAPIToken, v); err != nil {
			t.Fatal(err)
		}
		st.Close()
	}
	start := func() *store.Store {
		t.Helper()
		logs.Reset()
		st, err := openServeStore(dir, "", log)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	olderVersion("swt_old_value_1")
	st := start()
	st.Close()
	if !strings.Contains(logs.String(), `level=INFO msg="the API token of an older version is now the API token \"API token\"" scope=full`) {
		t.Fatalf("the first update:\n%s", logs.String())
	}
	st = start()
	st.Close()
	if strings.Contains(logs.String(), "older version") {
		t.Fatalf("the next start:\n%s", logs.String())
	}

	olderVersion("swt_old_value_2")
	st = start()
	st.Close()
	if !strings.Contains(logs.String(), `level=INFO msg="the API token of an older version replaces the value of the API token \"API token\""`) {
		t.Fatalf("the update after a downgrade:\n%s", logs.String())
	}

	st = start()
	legacy, err := st.APITokenByName(store.LegacyTokenName)
	if err != nil || legacy == nil {
		t.Fatalf("the API token %q: %+v %v", store.LegacyTokenName, legacy, err)
	}
	if err := st.DeleteAPIToken(legacy.ID); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < store.MaxAPITokens; i++ {
		if _, err := st.AddAPIToken(fmt.Sprintf("token %d", i), store.ScopeHA, fmt.Sprintf("swt_value_%d_0123456789", i), time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()
	olderVersion("swt_old_value_3")
	st = start()
	st.Close()
	if !strings.Contains(logs.String(), `level=WARN msg="cannot move the API token of an older version: there are 5 API tokens and none has the name \"API token\". Settings shows the old API token as refused"`) {
		t.Fatalf("the update with 5 other API tokens:\n%s", logs.String())
	}
}
