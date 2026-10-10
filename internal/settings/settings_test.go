package settings

import (
	"bytes"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/store"
)

func memStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.OpenMemory(bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// Load gives nil before the first save, the saved configuration with its
// defaults after it, and an error for a stored value it cannot read. Secrets
// never go into the stored configuration.
func TestLoadAndSave(t *testing.T) {
	st := memStore(t)
	if c, err := Load(st); c != nil || err != nil {
		t.Fatalf("empty store: %v %v", c, err)
	}
	c := config.Default()
	c.Servers = []config.Server{{ID: "akka", Name: "AKKA", URL: "https://10.0.0.1:8384", APIKey: "secret-akka"}}
	c.Notify.DiscordWebhook = "https://discord.com/api/webhooks/1/secret-hook"
	c.API.Token = "swt_secret"
	c.Notify.WeeklyTop = 12
	if err := Save(st, c); err != nil {
		t.Fatal(err)
	}
	raw, _ := st.Setting(keyConfig)
	for _, secret := range []string{"secret-akka", "secret-hook", "swt_secret"} {
		if strings.Contains(raw, secret) {
			t.Fatalf("the stored configuration contains %q: %s", secret, raw)
		}
	}
	got, err := Load(st)
	if err != nil || got == nil {
		t.Fatalf("load: %v %v", got, err)
	}
	if len(got.Servers) != 1 || got.Servers[0].URL != "https://10.0.0.1:8384" || got.Notify.WeeklyTop != 12 || got.Collector.Mode != config.ModeEvents {
		t.Fatalf("loaded %+v", got)
	}
	if err := st.SetSetting(keyConfig, "{not json"); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(st); err == nil || !strings.Contains(err.Error(), "cannot read the stored configuration") {
		t.Fatalf("corrupt configuration: %v", err)
	}
}

// ImportSecrets moves the secrets of a file into the encrypted store, binds
// each API key to the URL it came with, and clears them from the
// configuration. Secrets that the file does not have stay as they are.
func TestImportSecrets(t *testing.T) {
	st := memStore(t)
	if err := st.SetSecret(store.SecretAPIToken, "swt_kept"); err != nil {
		t.Fatal(err)
	}
	c := config.Default()
	c.Servers = []config.Server{
		{ID: "akka", Name: "AKKA", URL: "https://10.0.0.1:8384", APIKey: "key-akka"},
		{ID: "sol", Name: "SOL", URL: "https://10.0.0.2:8384"},
	}
	c.Notify.DiscordWebhook = "https://discord.com/api/webhooks/1/hook"
	n, err := ImportSecrets(st, c)
	if err != nil || n != 2 {
		t.Fatalf("imported %d: %v", n, err)
	}
	if c.Servers[0].APIKey != "" || c.Notify.DiscordWebhook != "" || c.API.Token != "" {
		t.Fatalf("secrets left in the configuration: %+v", c)
	}
	if key, url, _ := st.ServerKey("akka"); key != "key-akka" || url != "https://10.0.0.1:8384" {
		t.Fatalf("API key %q bound to %q", key, url)
	}
	if hook, _ := st.Secret(store.SecretDiscordWebhook); hook != "https://discord.com/api/webhooks/1/hook" {
		t.Fatalf("webhook %q", hook)
	}
	if token, _ := st.Secret(store.SecretAPIToken); token != "swt_kept" {
		t.Fatalf("a secret that the file does not have changed: %q", token)
	}
	if st.HasSecret(store.ServerAPIKeySecret("sol")) {
		t.Fatal("an API key for a server without one in the file")
	}
}

// A key typed for a different URL removes the pinned certificate, as for a
// new server. The same URL keeps it.
func TestSetServerKeyPin(t *testing.T) {
	st := memStore(t)
	if err := SetServerKey(st, "akka", "k1", "https://10.0.0.1:8384"); err != nil {
		t.Fatal(err)
	}
	_ = st.SetPin("akka", "AA:BB")
	if err := SetServerKey(st, "akka", "k2", "https://10.0.0.1:8384/"); err != nil {
		t.Fatal(err)
	}
	if pin, _ := st.Pin("akka"); pin != "AA:BB" {
		t.Fatal("the same URL removed the pinned certificate")
	}
	if err := SetServerKey(st, "akka", "k3", "https://10.0.0.9:8384"); err != nil {
		t.Fatal(err)
	}
	if pin, _ := st.Pin("akka"); pin != "" {
		t.Fatal("a new URL kept the pinned certificate")
	}
	if key, url, _ := st.ServerKey("akka"); key != "k3" || url != "https://10.0.0.9:8384" {
		t.Fatalf("key %q for %q", key, url)
	}
}

// KeyUsable: only a stored, readable key that was typed for the server's
// current URL is sent. NeedKey lists the servers without one.
func TestKeyUsable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "syncwatch.db")
	right := bytes.Repeat([]byte{1}, 32)
	st, err := store.Open(path, right)
	if err != nil {
		t.Fatal(err)
	}
	akka := config.Server{ID: "akka", Name: "AKKA", URL: "https://10.0.0.1:8384"}
	sol := config.Server{ID: "sol", Name: "SOL", URL: "https://10.0.0.2:8384"}
	old := config.Server{ID: "old", Name: "OLD", URL: "https://10.0.0.3:8384"}
	none := config.Server{ID: "none", Name: "NONE", URL: "https://10.0.0.4:8384"}
	_ = SetServerKey(st, "akka", "k-akka", "https://10.0.0.1:8384/") // same endpoint
	_ = SetServerKey(st, "sol", "k-sol", "https://10.0.0.9:8384")    // typed for another URL
	_ = st.SetSecret(store.ServerAPIKeySecret("old"), "legacy")      // stored before keys were bound
	cases := []struct {
		s    config.Server
		want bool
	}{{akka, true}, {sol, false}, {old, false}, {none, false}}
	for _, c := range cases {
		if got := KeyUsable(st, c.s); got != c.want {
			t.Errorf("%s: KeyUsable %v, want %v", c.s.Name, got, c.want)
		}
	}
	cfg := &config.Config{Servers: []config.Server{akka, sol, old, none}}
	if got := NeedKey(st, cfg); !slices.Equal(got, []string{"SOL", "OLD", "NONE"}) {
		t.Fatalf("NeedKey %v", got)
	}
	st.Close()

	// With another secret key (a lost secret.key) the stored key cannot be
	// decrypted: it is not usable, and the server needs its key again.
	st, err = store.Open(path, bytes.Repeat([]byte{2}, 32))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if KeyUsable(st, akka) {
		t.Fatal("an API key that cannot be decrypted is usable")
	}
	if got := NeedKey(st, cfg); !slices.Equal(got, []string{"AKKA", "SOL", "OLD", "NONE"}) {
		t.Fatalf("NeedKey with another secret key %v", got)
	}
}

// DropRemovedServers deletes the API key and the pinned certificate of each
// server that is not in the new configuration, so a later server with the
// same ID cannot get them.
func TestDropRemovedServers(t *testing.T) {
	st := memStore(t)
	for _, id := range []string{"akka", "sol"} {
		_ = SetServerKey(st, id, "k-"+id, "https://"+id+".invalid:8384")
		_ = st.SetPin(id, "FP-"+id)
	}
	old := &config.Config{Servers: []config.Server{{ID: "akka"}, {ID: "sol"}}}
	c := &config.Config{Servers: []config.Server{{ID: "akka"}}}
	if err := DropRemovedServers(st, nil, c); err != nil {
		t.Fatal(err)
	}
	if !st.HasSecret(store.ServerAPIKeySecret("sol")) {
		t.Fatal("no old configuration: nothing must be deleted")
	}
	if err := DropRemovedServers(st, old, c); err != nil {
		t.Fatal(err)
	}
	if st.HasSecret(store.ServerAPIKeySecret("sol")) {
		t.Fatal("the API key of the removed server is kept")
	}
	if pin, _ := st.Pin("sol"); pin != "" {
		t.Fatal("the pinned certificate of the removed server is kept")
	}
	if !st.HasSecret(store.ServerAPIKeySecret("akka")) {
		t.Fatal("the API key of a kept server is deleted")
	}
	if pin, _ := st.Pin("akka"); pin != "FP-akka" {
		t.Fatal("the pinned certificate of a kept server is removed")
	}
}
