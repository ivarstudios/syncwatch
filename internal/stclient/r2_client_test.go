package stclient

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// The answer of /rest/db/remoteneed of Syncthing 2.1.5 and 2.1.6 (the test
// kit), shortened to 1 directory and 1 file.
const remoteNeedAnswer = `{
 "files": [
  {"blocksHash": null, "deleted": false, "ignored": false, "invalid": false, "localFlags": 0,
   "modified": "2026-10-11T18:35:46.170772576+02:00", "modifiedBy": "WVM2UE6", "mustRescan": false,
   "name": "r2fix3", "noPermissions": false, "numBlocks": 0, "permissions": "0755",
   "platform": {"Unix": null, "Windows": null, "Linux": null, "Darwin": null, "FreeBSD": null, "NetBSD": null},
   "previousBlocksHash": null, "sequence": 25, "size": 0, "type": "FILE_INFO_TYPE_DIRECTORY", "version": ["WVM2UE6:1791736546"]},
  {"blocksHash": "ob8pHdnXg2dWHlROgwtRoNPwWjS8PI5tc221+67Y5hc=", "deleted": false, "ignored": false, "invalid": false,
   "localFlags": 0, "modified": "2026-10-11T18:35:46.167603517+02:00", "modifiedBy": "WVM2UE6", "mustRescan": false,
   "name": "r2fix3/shot-a.mov", "noPermissions": false, "numBlocks": 3, "permissions": "0644",
   "platform": {"Unix": null, "Windows": null, "Linux": null, "Darwin": null, "FreeBSD": null, "NetBSD": null},
   "previousBlocksHash": null, "sequence": 26, "size": 300000, "type": "FILE_INFO_TYPE_FILE", "version": ["WVM2UE6:1791736546"]},
  {"name": "old/notes.txt", "size": 12, "deleted": true}
 ],
 "page": 1,
 "perpage": 20
}`

// RemoteNeed asks for the first page with the folder, the device and the
// page size, and reads the names, sizes and deletions of the answer.
func TestRemoteNeed(t *testing.T) {
	var got url.Values
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/db/remoteneed" {
			http.NotFound(w, r)
			return
		}
		got = r.URL.Query()
		_, _ = w.Write([]byte(remoteNeedAnswer))
	}))
	defer srv.Close()
	c, _ := New(Options{Name: "t", URL: srv.URL, APIKey: "k"})
	const dev = "4PN542F-6INLZTY-ENGWNSD-7FV7WWJ-HIATIPR-2QPM2ZO-TFR5TRA-WF5QTQN"
	rn, err := c.RemoteNeed(context.Background(), "live-fjord-4finals", dev, 20)
	if err != nil {
		t.Fatal(err)
	}
	want := url.Values{"folder": {"live-fjord-4finals"}, "device": {dev}, "page": {"1"}, "perpage": {"20"}}
	if got.Encode() != want.Encode() {
		t.Fatalf("query %q, want %q", got.Encode(), want.Encode())
	}
	if rn.Page != 1 || rn.PerPage != 20 || len(rn.Files) != 3 {
		t.Fatalf("answer %+v", rn)
	}
	if f := rn.Files[1]; f.Name != "r2fix3/shot-a.mov" || f.Size != 300000 || f.Deleted {
		t.Fatalf("file %+v", f)
	}
	if f := rn.Files[2]; f.Name != "old/notes.txt" || !f.Deleted {
		t.Fatalf("deleted file %+v", f)
	}
}

// An older gate refuses the new endpoint with 404 "endpoint not permitted",
// the gate of the published version 78ef2d8 with 404 "endpoint not
// allowed". Syncthing's own 404 (an unknown folder) and other gate
// refusals are not that answer.
func TestGateRefusedEndpoint(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{&StatusError{Endpoint: "/rest/db/remoteneed", Code: 404, Body: "syncwatch gate: endpoint not permitted"}, true},
		{&StatusError{Endpoint: "/rest/db/remoteneed", Code: 404, Body: "syncwatch gate: endpoint not allowed"}, true},
		{&StatusError{Endpoint: "/rest/db/remoteneed", Code: 400, Body: "syncwatch gate: endpoint not allowed"}, false},
		{&StatusError{Endpoint: "/rest/db/remoteneed", Code: 404, Body: "no such folder"}, false},
		{&StatusError{Endpoint: "/rest/db/remoteneed", Code: 400, Body: "syncwatch gate: query not permitted: the parameter perpage must be between 1 and 100"}, false},
		{&StatusError{Endpoint: "/rest/db/remoteneed", Code: 403, Body: "syncwatch gate: gate token missing or not correct"}, false},
		{context.DeadlineExceeded, false},
		{nil, false},
	} {
		if got := GateRefusedEndpoint(c.err); got != c.want {
			t.Errorf("%v: %v, want %v", c.err, got, c.want)
		}
	}
}

// /rest/system/status of Syncthing 2.1.6 on AKKA in the test kit with its
// relay up: a relay listener and a TCP listener.
const systemStatusAnswer = `{
 "alloc": 6597904,
 "connectionServiceStatus": {
  "relay://10.123.1.60:22067/?id=JYSLS6U-5C7LIXN-KXWTAPB-PMJBIYG-OGMFVCC-GQTJJRG-IKYZ2ZX-2QBLQAC": {
   "error": null,
   "lanAddresses": ["relay://10.123.1.60:22067/?id=JYSLS6U-5C7LIXN-KXWTAPB-PMJBIYG-OGMFVCC-GQTJJRG-IKYZ2ZX-2QBLQAC"],
   "wanAddresses": ["relay://10.123.1.60:22067/?id=JYSLS6U-5C7LIXN-KXWTAPB-PMJBIYG-OGMFVCC-GQTJJRG-IKYZ2ZX-2QBLQAC"]
  },
  "tcp://0.0.0.0:22000": {
   "error": "listen tcp 0.0.0.0:22000: bind: address already in use",
   "lanAddresses": ["tcp://0.0.0.0:22000", "tcp://10.123.1.10:22000"],
   "wanAddresses": ["tcp://0.0.0.0:0", "tcp://0.0.0.0:22000"]
  }
 },
 "myID": "WVM2UE6-6OP22Q4-KDQZARD-EJWADG5-DGSQXFG-52ADGFO-E5FPJSX-EMSNYQS",
 "pathSeparator": "/",
 "startTime": "2026-10-11T18:33:30+02:00",
 "uptime": 25
}`

// SystemStatus reads the listeners: their error (null when none) and their
// addresses.
func TestSystemStatusListeners(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(systemStatusAnswer))
	}))
	defer srv.Close()
	c, _ := New(Options{Name: "t", URL: srv.URL, APIKey: "k"})
	st, err := c.SystemStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.MyID == "" || len(st.ConnectionServiceStatus) != 2 {
		t.Fatalf("status %+v", st)
	}
	relay := st.ConnectionServiceStatus["relay://10.123.1.60:22067/?id=JYSLS6U-5C7LIXN-KXWTAPB-PMJBIYG-OGMFVCC-GQTJJRG-IKYZ2ZX-2QBLQAC"]
	if relay.Error != nil || len(relay.WANAddresses) != 1 || len(relay.LANAddresses) != 1 {
		t.Fatalf("relay listener %+v", relay)
	}
	tcp := st.ConnectionServiceStatus["tcp://0.0.0.0:22000"]
	if tcp.Error == nil || *tcp.Error != "listen tcp 0.0.0.0:22000: bind: address already in use" || len(tcp.WANAddresses) != 2 {
		t.Fatalf("tcp listener %+v", tcp)
	}
}
