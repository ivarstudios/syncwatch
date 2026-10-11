package web

// Tests of the integrator of wave 3 (r2): contracts between the assignments,
// end to end through the dashboard, the store and the JSON API.

import (
	"html"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/store"
)

// An api.token in an imported file (the 2 steps of fixer 1) becomes the
// value of the API token "API token" (fixer 5's ImportSecrets): the API
// accepts the new value with the full scope and refuses the old one, the
// import card does not call api.token an encrypted secret, and the notice
// names the API token.
func TestImportedAPITokenIsTheAPITokenNamedAPIToken(t *testing.T) {
	e := adminEnv(t, twoServers)
	_, page := e.get("/settings")
	card := html.UnescapeString(section(t, page, "import"))
	if !strings.Contains(card, `The value of <code>api.token</code> becomes the value of the API token "API token".`) ||
		strings.Contains(card, "<code>discord_notices_webhook</code>, <code>api.token</code>)") {
		t.Fatalf("the import card must not list api.token as an encrypted secret:\n%s", card)
	}
	csrf := e.csrf("/settings")
	if msg, isErr := impFile(e, csrf, "servers: [{name: AKKA, url: 'https://a:1'}, {name: SOL, url: 'https://b:1'}]\napi: {token: swt_imported_value}\n", nil); isErr {
		t.Fatalf("import: %s", msg)
	}
	if n := e.noticesText(); !strings.Contains(n, `The API token "API token" is replaced.`) || strings.Contains(n, "swt_imported") {
		t.Fatalf("import notice: %q", n)
	}
	for path, want := range map[string]int{"/api/v1/status": http.StatusOK, "/api/v1/ha": http.StatusOK} {
		if resp, _ := e.apiGet(path, "Bearer swt_imported_value", ""); resp.StatusCode != want {
			t.Errorf("%s with the imported value: %d, want %d", path, resp.StatusCode, want)
		}
		if resp, _ := e.apiGet(path, "Bearer swt_test_token", ""); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s with the old value: %d, want 401", path, resp.StatusCode)
		}
	}
	tokens, err := e.st.APITokens()
	if err != nil || len(tokens) != 1 || tokens[0].Name != "API token" || tokens[0].Scope != "full" {
		t.Fatalf("API tokens after the import: %+v %v", tokens, err)
	}
}

// The line of the import notice for api.token follows the rule of the import
// itself (settings.ImportSecrets): "replaced" only when an API token named
// "API token" exists. A secret api.token of an older version that the store
// did not move (a downgrade made it after the migration) does not count: the
// import adds the API token "API token" next to it. The CLI uses the same
// rule (internal/app).
func TestImportTokenLineFollowsTheImportRule(t *testing.T) {
	e := adminEnv(t, `servers: [{name: AKKA, url: "https://a:1"}]`)
	if _, err := e.st.AddAPIToken("Home Assistant", "ha", "swt_ha_value", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetSecret(store.SecretAPIToken, "swt_old_version"); err != nil {
		t.Fatal(err)
	}
	csrf := e.csrf("/settings")
	if msg, isErr := impFile(e, csrf, "servers: [{name: AKKA, url: 'https://a:1'}]\napi: {token: swt_imported_value}\n", nil); isErr {
		t.Fatalf("import: %s", msg)
	}
	if n := e.noticesText(); !strings.Contains(n, `The API token "API token" is added.`) {
		t.Fatalf("import notice: %q", n)
	}
	tokens, _ := e.st.APITokens()
	var names []string
	for _, tok := range tokens {
		names = append(names, tok.Name+"/"+tok.Scope)
	}
	if strings.Join(names, ",") != "Home Assistant/ha,API token/full" {
		t.Fatalf("API tokens after the import: %v", names)
	}
}
