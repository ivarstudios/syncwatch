package web

// Tests of the pre-release review (stage pp, area auth): the last use of an
// API token during a replace (tokens-2), the login limit for many addresses
// (sessions-2), the length of an administrator session (sessions-3) and the
// login of an administrator in a browser with a viewer session (sessions-4).

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/store"
)

// tokens-2: a request reads the API token with the old value. Then the
// administrator selects Replace, and then the request records its use. The
// use must not go to the new value: no program sent it, and the card is
// what the administrator reads to see if Home Assistant has the new value.
func TestAPITokenUseDuringReplace(t *testing.T) {
	e := adminEnv(t, twoServers+"timezone: UTC\n")
	old := e.addToken("Home Assistant", store.ScopeHA)
	id := e.tokenID("Home Assistant")
	at := time.Date(2026, 10, 12, 9, 6, 0, 0, time.UTC)
	const value = "swt_new_value_of_the_replace"
	// The first time that a request with an API token asks for the time is
	// the record of its use (touchAPIToken), after the read of the row.
	var replaced atomic.Bool
	e.web.now = func() time.Time {
		if replaced.CompareAndSwap(false, true) {
			if err := e.st.ReplaceAPIToken(id, value, at); err != nil {
				t.Error(err)
			}
		}
		return at
	}
	if code, body := call(t, http.DefaultClient, e.srv.URL+"/api/v1/ha", old); code != http.StatusOK || !replaced.Load() {
		t.Fatalf("the request with the old value: %d %s, replace during the request %v", code, body, replaced.Load())
	}
	if tok, err := e.st.APITokenByValue(value); err != nil || tok == nil || !tok.LastUsed.IsZero() {
		t.Fatalf("the new value after the request with the old value: %+v %v, want no last use", tok, err)
	}
	_, page := e.get("/settings")
	if api := section(t, page, "api"); !strings.Contains(api, `<dt>Last used</dt><dd>never</dd>`) {
		t.Errorf("the card after the replace: %s", api)
	}
	if code, _ := call(t, http.DefaultClient, e.srv.URL+"/api/v1/ha", old); code != http.StatusUnauthorized {
		t.Errorf("the old value after the replace: %d", code)
	}
	// The new value records its own use.
	if code, _ := call(t, http.DefaultClient, e.srv.URL+"/api/v1/ha", value); code != http.StatusOK {
		t.Fatalf("the new value: %d", code)
	}
	if tok, _ := e.st.APITokenByValue(value); tok == nil || !tok.LastUsed.Equal(at) {
		t.Errorf("the new value after its own request: %+v", tok)
	}
}
