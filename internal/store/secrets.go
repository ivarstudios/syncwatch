package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Secret names.
const (
	SecretDiscordWebhook = "notify.discord_webhook"
	SecretAPIToken       = "api.token"
)

// ServerAPIKeySecret is the secret name of a server's API key.
func ServerAPIKeySecret(serverID string) string { return "server." + serverID + ".api_key" }

// boundKey is how a server's API key is stored: together with the URL it was
// entered for, inside the same encrypted value, so neither can be changed
// without the other.
type boundKey struct {
	Key string `json:"key"`
	URL string `json:"url"`
}

// SetServerKey stores a server's API key bound to the URL it was entered for.
// The collector only sends the key to that URL.
func (s *Store) SetServerKey(serverID, key, url string) error {
	if key == "" {
		return s.DeleteSecret(ServerAPIKeySecret(serverID))
	}
	b, err := json.Marshal(boundKey{Key: key, URL: url})
	if err != nil {
		return err
	}
	return s.SetSecret(ServerAPIKeySecret(serverID), string(b))
}

// ServerKey returns a server's API key and the URL it is bound to. A key
// stored without a URL (before keys were bound) comes back with url "".
func (s *Store) ServerKey(serverID string) (key, url string, err error) {
	v, err := s.Secret(ServerAPIKeySecret(serverID))
	if err != nil || v == "" {
		return "", "", err
	}
	var bk boundKey
	if strings.HasPrefix(v, "{") && json.Unmarshal([]byte(v), &bk) == nil && bk.Key != "" {
		return bk.Key, bk.URL, nil
	}
	return v, "", nil
}

// LoadKey returns the secrets key: from envValue if set (base64 of 32 random
// bytes), otherwise from <dataDir>/secret.key, which is created with a random
// key on first start. Passphrases aren't accepted: hashed without a slow key
// derivation they could be guessed from a copy of the database.
func LoadKey(dataDir, envValue string) ([]byte, error) {
	if envValue = strings.TrimSpace(envValue); envValue != "" {
		b, err := base64.StdEncoding.DecodeString(envValue)
		if err != nil || len(b) != 32 {
			return nil, errors.New("SYNCWATCH_SECRET_KEY must be a base64-encoded 32-byte random key; create one with: openssl rand -base64 32")
		}
		return b, nil
	}
	path := filepath.Join(dataDir, "secret.key")
	if b, err := os.ReadFile(path); err == nil {
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(key) != 32 {
			return nil, fmt.Errorf("%s is not a valid key", path)
		}
		return key, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("writing %s: %w", path, err)
	}
	return key, nil
}

func (s *Store) aead() (cipher.AEAD, error) {
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// SetSecret encrypts and stores a secret. An empty value deletes it.
func (s *Store) SetSecret(name, value string) error {
	if value == "" {
		return s.DeleteSecret(name)
	}
	gcm, err := s.aead()
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	ct := gcm.Seal(nil, nonce, []byte(value), []byte(name))
	_, err = s.db.Exec(`INSERT INTO secrets(name,nonce,ciphertext,updated_at) VALUES(?,?,?,?)
		ON CONFLICT(name) DO UPDATE SET nonce=excluded.nonce, ciphertext=excluded.ciphertext, updated_at=excluded.updated_at`,
		name, nonce, ct, time.Now().UnixMilli())
	return err
}

// Secret decrypts a secret. It returns "" and no error when it isn't set.
func (s *Store) Secret(name string) (string, error) {
	var nonce, ct []byte
	err := s.db.QueryRow(`SELECT nonce, ciphertext FROM secrets WHERE name=?`, name).Scan(&nonce, &ct)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	gcm, err := s.aead()
	if err != nil {
		return "", err
	}
	pt, err := gcm.Open(nil, nonce, ct, []byte(name))
	if err != nil {
		return "", fmt.Errorf("secret %s can't be decrypted (wrong secret key?)", name)
	}
	return string(pt), nil
}

// HasSecret reports whether a secret is set, without decrypting it.
func (s *Store) HasSecret(name string) bool {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM secrets WHERE name=?`, name).Scan(&n)
	return n > 0
}

// DeleteSecret removes a secret.
func (s *Store) DeleteSecret(name string) error {
	_, err := s.db.Exec(`DELETE FROM secrets WHERE name=?`, name)
	return err
}
