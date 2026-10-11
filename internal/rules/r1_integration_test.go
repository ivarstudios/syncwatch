package rules

import (
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/stclient"
)

// Contract C2 end to end: the texts that r1-1's stclient makes for each
// refusal go into Server.LastError (collector.shortErr), and r1-3's H1
// tells a refused key or gate token from a gate whose own key Syncthing
// refused (502: the gate must be restarted; H1 keeps "unreachable" and the
// gate's instruction).
func TestH1WithTheClientTexts(t *testing.T) {
	c := ivarConfig(t)
	sol := server("sol", "SOL", solID)
	sol.Status, sol.DownSince = model.StatusDown, now.Add(-10*time.Minute)
	for _, tc := range []struct {
		name    string
		err     *stclient.StatusError
		refused bool
	}{
		{"key refused by Syncthing", &stclient.StatusError{Endpoint: "/rest/system/status", Code: 403, Body: "CSRF Error"}, true},
		{"key refused, 401", &stclient.StatusError{Endpoint: "/rest/system/status", Code: 401, Body: "Not Authorized"}, true},
		{"wrong gate token", &stclient.StatusError{Endpoint: "/rest/system/status", Code: 403, Body: "syncwatch gate: gate token missing or not correct\n"}, true},
		{"gate key refused by Syncthing", &stclient.StatusError{Endpoint: "/rest/system/status", Code: 502, Body: "syncwatch gate: Syncthing refused the API key of the gate. Restart the gate with the current API key"}, false},
		{"gate cannot reach Syncthing", &stclient.StatusError{Endpoint: "/rest/system/status", Code: 502, Body: "syncwatch gate: Syncthing unreachable: connection refused"}, false},
	} {
		sol.LastError = tc.err.Error()
		h1 := mustFind(t, run(t, c, sol), "H1", "SOL")
		wantPrefix := "Server SOL is unreachable: "
		if tc.refused {
			wantPrefix = "Server SOL refuses the API key or the gate token: "
		}
		if h1.Message != wantPrefix+sol.LastError {
			t.Errorf("%s: message %q, want the prefix %q and the client text %q", tc.name, h1.Message, wantPrefix, sol.LastError)
		}
		if got := h1.DetailValue("What to do") != ""; got != tc.refused {
			t.Errorf("%s: 'What to do' row %v, want %v", tc.name, got, tc.refused)
		}
	}
}
