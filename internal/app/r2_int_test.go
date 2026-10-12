package app

import (
	"slices"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/store"
)

// The CLI import names the API token as the dashboard import does, by the
// rule of settings.ImportSecrets: "replaced" only when an API token named
// "API token" exists (without regard to case). A secret api.token of an older
// version does not count.
func TestCLIImportTokenLineFollowsTheImportRule(t *testing.T) {
	st, err := store.OpenMemory(make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	c := config.Default()
	c.API.Token = "swt_imported_value"
	line := func() string {
		t.Helper()
		for _, l := range importChanges(st, config.Default(), c) {
			if slices.Contains([]string{`The API token "API token" is added.`, `The API token "API token" is replaced.`}, l) {
				return l
			}
		}
		t.Fatal("no line for the API token")
		return ""
	}
	if err := st.SetSecret(store.SecretAPIToken, "swt_old_version"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddAPIToken("Home Assistant", store.ScopeHA, "swt_ha_value", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := line(); got != `The API token "API token" is added.` {
		t.Fatalf("old secret and another API token: %q", got)
	}
	if _, err := st.AddAPIToken("api TOKEN", store.ScopeFull, "swt_full_value", time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := line(); got != `The API token "API token" is replaced.` {
		t.Fatalf("an API token with the name in another case: %q", got)
	}
}
