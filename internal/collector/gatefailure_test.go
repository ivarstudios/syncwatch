package collector

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/stclient"
)

// A gate answers 502 when Syncthing is unreachable or refused the gate's
// API key, and 429 when this client has too many parallel requests. Such an
// answer on a per-folder request is the server's problem, not the folder's
// (integration of r1-1 and r1-2). Syncthing's own 5xx stays with the folder.
func TestGateFailureIsServerLevel(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"transport error", errors.New("dial tcp: connection refused"), true},
		{"key refused by Syncthing", &stclient.StatusError{Endpoint: "/rest/db/status", Code: 403, Body: "CSRF Error"}, true},
		{"wrong gate token", &stclient.StatusError{Endpoint: "/rest/db/status", Code: 403, Body: "syncwatch gate: gate token missing or not correct"}, true},
		{"gate: key of the gate refused", &stclient.StatusError{Endpoint: "/rest/db/status", Code: 502, Body: "syncwatch gate: Syncthing refused the API key of the gate. Restart the gate with the current API key"}, true},
		{"gate: Syncthing unreachable", &stclient.StatusError{Endpoint: "/rest/db/completion", Code: 502, Body: "syncwatch gate: Syncthing unreachable: dial tcp 127.0.0.1:8384: connection refused\n"}, true},
		{"gate: too many requests", &stclient.StatusError{Endpoint: "/rest/db/status", Code: 429, Body: "syncwatch gate: too many parallel requests"}, true},
		{"Syncthing 500", &stclient.StatusError{Endpoint: "/rest/db/status", Code: 500, Body: "database is broken"}, false},
		{"Syncthing 502 without gate", &stclient.StatusError{Endpoint: "/rest/db/status", Code: 502, Body: "Bad Gateway"}, false},
		{"Syncthing 429 without gate", &stclient.StatusError{Endpoint: "/rest/db/status", Code: 429, Body: "slow down"}, false},
		{"gate: query refused", &stclient.StatusError{Endpoint: "/rest/db/status", Code: 400, Body: "syncwatch gate: query not permitted: x"}, false},
		{"paused folder", &stclient.StatusError{Endpoint: "/rest/db/completion", Code: 404, Body: "folder is paused"}, false},
	}
	for _, tc := range cases {
		if got := serverLevel(tc.err); got != tc.want {
			t.Errorf("%s: serverLevel = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A full check through a gate whose Syncthing stops: the gate answers 502
// on one folder's db/status. The server goes down with the gate's text; no
// folder gets the error, so no H4 opens on it.
func TestGate502OnFolderRequestMarksServerDown(t *testing.T) {
	fake := newFakeST(t)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rest/db/status" && r.URL.Query().Get("folder") == "p-beta" {
			http.Error(w, "syncwatch gate: Syncthing unreachable: connection refused", http.StatusBadGateway)
			return
		}
		fake.ServeHTTP(w, r)
	}))
	defer srv.Close()
	hub, _, _, stop := startManager(t, srv, "collector: {event_timeout: 5s, down_after_failures: 1}", nil, nil)
	defer stop()
	sv := waitFor(t, hub, "down", func(s *model.Server) bool { return s.Status == model.StatusDown })
	if !strings.Contains(sv.LastError, "HTTP 502: syncwatch gate: Syncthing unreachable") {
		t.Fatalf("LastError %q", sv.LastError)
	}
	for id, f := range sv.Folders {
		if f.Error != "" {
			t.Fatalf("folder %s has the error %q; a gate failure is not a folder problem", id, f.Error)
		}
	}
}
