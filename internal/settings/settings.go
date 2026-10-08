// Package settings loads and saves the configuration in the store, keeping
// secrets out of the configuration blob.
package settings

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ivarstudios/syncwatch/internal/config"
	"github.com/ivarstudios/syncwatch/internal/store"
)

const keyConfig = "config"

// Load returns the stored configuration, or nil if none is stored yet.
func Load(st *store.Store) (*config.Config, error) {
	raw, err := st.Setting(keyConfig)
	if err != nil || raw == "" {
		return nil, err
	}
	var c config.Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return nil, fmt.Errorf("stored configuration is corrupt: %w", err)
	}
	c.ApplyDefaults()
	return &c, nil
}

// Save stores a configuration. Secret fields are not part of the JSON.
func Save(st *store.Store, c *config.Config) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return st.SetSetting(keyConfig, string(b))
}

// ImportSecrets moves any secrets present in an imported configuration into
// the encrypted secret store and clears them from c. API keys are bound to
// their server's URL. Secrets that are absent from the import are left as
// they are, but a kept API key is only used while the server's URL matches
// the one it was entered for (see NeedKey).
func ImportSecrets(st *store.Store, c *config.Config) (int, error) {
	n := 0
	set := func(name, v string) error {
		if v == "" {
			return nil
		}
		n++
		return st.SetSecret(name, v)
	}
	for i := range c.Servers {
		sv := &c.Servers[i]
		if sv.APIKey != "" {
			n++
			if err := SetServerKey(st, sv.ID, sv.APIKey, sv.URL); err != nil {
				return n, err
			}
		}
		sv.APIKey = ""
	}
	if err := set(store.SecretDiscordWebhook, c.Notify.DiscordWebhook); err != nil {
		return n, err
	}
	if err := set(store.SecretAPIToken, c.API.Token); err != nil {
		return n, err
	}
	c.Notify.DiscordWebhook, c.API.Token = "", ""
	return n, nil
}

// SetServerKey stores a server's API key bound to url. A key entered for a
// different URL than before also forgets the pinned certificate, as for a
// newly added server.
func SetServerKey(st *store.Store, serverID, key, url string) error {
	_, prev, err := st.ServerKey(serverID)
	if err != nil {
		prev = ""
	}
	if err := st.SetServerKey(serverID, key, url); err != nil {
		return err
	}
	if !config.SameEndpoint(prev, url) {
		return st.SetPin(serverID, "")
	}
	return nil
}

// KeyUsable reports whether a server has an API key that was entered for its
// current URL.
func KeyUsable(st *store.Store, s config.Server) bool {
	key, bound, err := st.ServerKey(s.ID)
	return err == nil && key != "" && config.SameEndpoint(bound, s.URL)
}

// NeedKey returns the names of servers in c whose stored API key was entered
// for a different URL (or none is stored), so the key must be entered again.
func NeedKey(st *store.Store, c *config.Config) []string {
	var out []string
	for _, s := range c.Servers {
		if !KeyUsable(st, s) {
			out = append(out, s.Name)
		}
	}
	return out
}

// DropRemovedServers deletes the API keys and pinned certificates of servers
// that are in old but not in c, so a later server with the same ID can't
// inherit them.
func DropRemovedServers(st *store.Store, old, c *config.Config) error {
	if old == nil {
		return nil
	}
	keep := map[string]bool{}
	for _, s := range c.Servers {
		keep[s.ID] = true
	}
	var errs []error
	for _, s := range old.Servers {
		if !keep[s.ID] {
			errs = append(errs, st.DeleteSecret(store.ServerAPIKeySecret(s.ID)), st.SetPin(s.ID, ""))
		}
	}
	return errors.Join(errs...)
}
