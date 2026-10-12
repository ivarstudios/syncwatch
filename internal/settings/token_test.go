package settings

import (
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/store"
)

// fileWithToken is an imported configuration with a webhook and an API
// token (api.token).
func fileWithToken(t *testing.T, token string) *config.Config {
	t.Helper()
	c, err := config.ParseYAML([]byte("notifications: {discord_webhook: \"https://discord.com/api/webhooks/1/hook\"}\napi: {token: " + token + "}\n"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// The API token of a file (api.token) is the value of the API token
// named "API token". An import adds it with the scope full, or gives it the
// new value and keeps its scope. The old value stops working.
func TestImportAPIToken(t *testing.T) {
	st := memStore(t)
	n, err := ImportSecrets(st, fileWithToken(t, "swt_first_value"))
	if err != nil || n != 2 {
		t.Fatalf("imported %d: %v", n, err)
	}
	if st.HasSecret(store.SecretAPIToken) {
		t.Fatal("the import wrote the secret api.token of an older version")
	}
	list, _ := st.APITokens()
	if len(list) != 1 || list[0].Name != store.LegacyTokenName || list[0].Scope != store.ScopeFull {
		t.Fatalf("API tokens after the first import: %+v", list)
	}
	if tok, _ := st.APITokenByValue("swt_first_value"); tok == nil || tok.ID != list[0].ID {
		t.Fatal("the imported value does not work")
	}

	// The scope stays: an administrator gave the API token the scope ha.
	ha := list[0]
	if err := st.DeleteAPIToken(ha.ID); err != nil {
		t.Fatal(err)
	}
	if ha, err = st.AddAPIToken("api TOKEN", store.ScopeHA, "swt_first_value", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddAPIToken("Scripts", store.ScopeFull, "swt_scripts", time.Now()); err != nil {
		t.Fatal(err)
	}
	if n, err := ImportSecrets(st, fileWithToken(t, "swt_second")); err != nil || n != 2 {
		t.Fatalf("second import %d: %v", n, err)
	}
	if tok, _ := st.APITokenByValue("swt_first_value"); tok != nil {
		t.Fatal("the old value still works after the import")
	}
	tok, _ := st.APITokenByValue("swt_second")
	if tok == nil || tok.ID != ha.ID || tok.Scope != store.ScopeHA || tok.Name != "api TOKEN" {
		t.Fatalf("the imported value: %+v, want the API token %+v with its scope", tok, ha)
	}
	if list, _ := st.APITokens(); len(list) != 2 {
		t.Fatalf("the import added an API token: %+v", list)
	}
	if other, _ := st.APITokenByValue("swt_scripts"); other == nil {
		t.Fatal("the import changed another API token")
	}

	// The same value again changes nothing: the time stays.
	before, _ := st.APITokenByValue("swt_second")
	if n, err := ImportSecrets(st, fileWithToken(t, "swt_second")); err != nil || n != 2 {
		t.Fatalf("import of the same value %d: %v", n, err)
	}
	if after, _ := st.APITokenByValue("swt_second"); after == nil || !after.CreatedAt.Equal(before.CreatedAt) {
		t.Fatalf("the same value: %+v, before %+v", after, before)
	}

	// The value of another API token: refused before a secret is stored.
	c := fileWithToken(t, "swt_scripts")
	if n, err := ImportSecrets(st, c); err == nil || n != 0 || !strings.Contains(err.Error(), `the value of the API token "Scripts"`) {
		t.Fatalf("the value of another API token: %d %v", n, err)
	}
}

// With 5 API tokens and none named "API token", an import with an API
// token is refused with a message that names the limit, and nothing of the
// file is stored. With an API token named "API token" among the 5, the import
// replaces its value.
func TestImportAPITokenLimit(t *testing.T) {
	st := memStore(t)
	for _, name := range []string{"Home Assistant", "Lobby", "Scripts", "Backup", "Laptop"} {
		if _, err := st.AddAPIToken(name, store.ScopeHA, "swt_"+name, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	c := fileWithToken(t, "swt_new_value")
	n, err := ImportSecrets(st, c)
	if err == nil || n != 0 || err.Error() != "There are 5 API tokens. syncwatch cannot add the API token of the file (api.token). Revoke an API token first." {
		t.Fatalf("a sixth API token: %d %v", n, err)
	}
	if st.HasSecret(store.SecretDiscordWebhook) {
		t.Fatal("a refused import stored the webhook")
	}
	if tok, _ := st.APITokenByValue("swt_new_value"); tok != nil {
		t.Fatal("a refused import stored the API token")
	}

	lobby, _ := st.APITokenByName("Lobby")
	if err := st.DeleteAPIToken(lobby.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AddAPIToken(store.LegacyTokenName, store.ScopeFull, "swt_old", time.Now()); err != nil {
		t.Fatal(err)
	}
	if n, err := ImportSecrets(st, fileWithToken(t, "swt_new_value")); err != nil || n != 2 {
		t.Fatalf("5 API tokens with the API token named %q: %d %v", store.LegacyTokenName, n, err)
	}
	if tok, _ := st.APITokenByValue("swt_new_value"); tok == nil || tok.Name != store.LegacyTokenName {
		t.Fatalf("the value did not go to the API token %q: %+v", store.LegacyTokenName, tok)
	}
}
