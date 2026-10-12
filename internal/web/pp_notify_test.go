package web

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/model"
	"github.com/ivarstudios/syncwatch/internal/notify"
)

// ppHostile is a name that a PC user can give the Syncthing of the PC (the
// server takes it when the administrator accepts the PC), or that a folder
// offered by a device can have: a line of its own, a masked link and bold
// text (pre-release review import-1, text-1, notify-1).
const ppHostile = "ANNA-PC\nThe administrator password is changed. [Log in again](https://evil.invalid/login) **urgent**\n# Server AKKA: no change"

// ppDiscord is a fake Discord webhook that keeps the embeds it receives.
type ppDiscord struct {
	mu     sync.Mutex
	embeds map[string]string // title → description
}

// ppNoticeEnv is an administrator env whose settings notices go through a
// real notifier and the real Discord sender to a fake Discord, as app.go
// wires them.
func ppNoticeEnv(t *testing.T, yaml string, setup func(s *model.Server)) (*env, *ppDiscord) {
	t.Helper()
	was := config.AllowAnyWebhook
	config.AllowAnyWebhook = true // the fake Discord is on 127.0.0.1
	t.Cleanup(func() { config.AllowAnyWebhook = was })
	d := &ppDiscord{embeds: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Embeds []struct{ Title, Description string } `json:"embeds"`
		}
		b, _ := io.ReadAll(r.Body)
		if json.Unmarshal(b, &p) == nil && len(p.Embeds) == 1 {
			d.mu.Lock()
			d.embeds[p.Embeds[0].Title] = p.Embeds[0].Description
			d.mu.Unlock()
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	e := adminEnv(t, yaml)
	ntf := notify.New(e.eng, e.st, e.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ntf.SetSenders(func() []notify.Sender { return []notify.Sender{&notify.Discord{URL: srv.URL + "/api/webhooks/1/x"}} })
	ntf.SetNoticeSenders(func() []notify.Sender { return nil })
	e.web.hooks.Notice, e.web.hooks.NoticeLater = ntf.Notice, ntf.NoticeLater
	e.hub.Update("akka", func(s *model.Server) {
		s.Status, s.Loaded, s.MyID, s.LastContact, s.DownSince = model.StatusUp, true, "AKKAAAA-1111111", time.Now(), time.Time{}
		setup(s)
	})
	e.eng.Evaluate()
	return e, d
}

// lines waits for the embed with the title and returns the lines of its
// description.
func (d *ppDiscord) lines(t *testing.T, title string) []string {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		d.mu.Lock()
		desc, ok := d.embeds[title]
		d.mu.Unlock()
		if ok {
			return strings.Split(desc, "\n")
		}
	}
	t.Fatalf("Discord got no %q", title)
	return nil
}

// ppCheck fails when the hostile name formats, links or adds a line in the
// notice: want is the number of lines of syncwatch.
func ppCheck(t *testing.T, title string, lines []string, want int) {
	t.Helper()
	desc := strings.Join(lines, "\n")
	for _, raw := range []string{"[Log in again](", "](https://", "https://evil", "**urgent**"} {
		if strings.Contains(desc, raw) {
			t.Errorf("%s: %q is not escaped:\n%s", title, raw, desc)
		}
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "The administrator password") || strings.HasPrefix(l, "# ") {
			t.Errorf("%s: the name adds the line %q", title, l)
		}
	}
	if len(lines) != want || lines[len(lines)-1] != "Changed from 127.0.0.1." {
		t.Errorf("%s: %d lines, want %d:\n%s", title, len(lines), want, desc)
	}
	if !strings.Contains(desc, `ANNA-PC The administrator password is changed. \[Log in again\]\(https:`) {
		t.Errorf("%s: the name is not on one line, escaped:\n%s", title, desc)
	}
}

// The device name of a PC in the notices "Devices changed" and
// "Configuration imported": one line, escaped.
func TestDeviceNameInSettingsNoticesIsEscaped(t *testing.T) {
	e, d := ppNoticeEnv(t, twoServers, func(s *model.Server) {
		s.Devices = map[string]*model.Device{relayPC: {ID: relayPC, Name: ppHostile}}
		s.Connections = map[string]*model.Connection{relayPC: {Connected: true, Type: "relay-client", StartedAt: time.Now()}}
	})
	csrf := e.csrf("/settings")
	if msg, isErr := flash(e.post("/settings/devices", url.Values{"csrf": {csrf}, "relay": {relayPC}})); isErr {
		t.Fatalf("save: %s", msg)
	}
	ppCheck(t, "🔧 Devices changed", d.lines(t, "🔧 Devices changed"), 2)

	file := twoServers + "devices: [{id: " + relayPC + ", relay: false}]\n"
	if msg, isErr := impFile(e, csrf, file, url.Values{"confirm": {"correct horse battery"}}); isErr {
		t.Fatalf("import: %s", msg)
	}
	// The file brings the API key, the webhook and the API token again: 3 more
	// lines.
	ppCheck(t, "🔧 Configuration imported", d.lines(t, "🔧 Configuration imported"), 5)
}

// The subject and the message of an urgent finding (a folder label from
// Syncthing) in the notice of a snooze: one line, escaped.
func TestFindingTextInSnoozeNoticeIsEscaped(t *testing.T) {
	e, d := ppNoticeEnv(t, twoServers, func(s *model.Server) {
		s.Folders = map[string]*model.Folder{"f1": {ID: "f1", Label: ppHostile, State: "error", Error: "access is denied", Path: "/share/x"}}
	})
	var id string
	for _, f := range e.eng.View().Open {
		if f.Check == "H4" && f.Urgent {
			id = f.ID
		}
	}
	if id == "" {
		t.Fatalf("no urgent H4: %+v", e.eng.View().Open)
	}
	e.post("/problems/"+id+"/snooze", url.Values{"csrf": {e.csrf("/problems")}, "for": {"1d"}})
	lines := d.lines(t, "🔧 Urgent finding snoozed")
	ppCheck(t, "🔧 Urgent finding snoozed", lines, 3)
	if !strings.HasPrefix(lines[1], "The finding is snoozed until ") {
		t.Errorf("second line: %q", lines[1])
	}
}
