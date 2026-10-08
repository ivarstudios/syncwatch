// Package store keeps syncwatch's own state in SQLite: settings, encrypted
// secrets, finding history, snoozes, pinned certificates, sessions and the
// notification log. It never stores Syncthing metrics history.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // database/sql driver "sqlite"
)

// Store is the SQLite database.
type Store struct {
	db  *sql.DB
	key []byte // AES-256 key for secrets
}

const schemaVersion = 1

var schema = []string{
	`CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS secrets (name TEXT PRIMARY KEY, nonce BLOB NOT NULL, ciphertext BLOB NOT NULL, updated_at INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS pins (server_id TEXT PRIMARY KEY, fingerprint TEXT NOT NULL, pinned_at INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS observations (id TEXT PRIMARY KEY, since INTEGER NOT NULL, servers TEXT NOT NULL DEFAULT '')`,
	`CREATE TABLE IF NOT EXISTS findings (
		id TEXT PRIMARY KEY,
		check_id TEXT NOT NULL,
		severity INTEGER NOT NULL,
		category TEXT NOT NULL,
		server TEXT NOT NULL,
		subject TEXT NOT NULL,
		message TEXT NOT NULL,
		data TEXT NOT NULL,
		urgent INTEGER NOT NULL,
		since INTEGER NOT NULL,
		opened_at INTEGER NOT NULL,
		last_seen INTEGER NOT NULL,
		resolved_at INTEGER NOT NULL DEFAULT 0,
		stale INTEGER NOT NULL DEFAULT 0,
		pinged_at INTEGER NOT NULL DEFAULT 0,
		resolved_notified_at INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS findings_resolved ON findings(resolved_at)`,
	`CREATE TABLE IF NOT EXISTS history (
		seq INTEGER PRIMARY KEY AUTOINCREMENT,
		finding_id TEXT NOT NULL,
		check_id TEXT NOT NULL,
		severity INTEGER NOT NULL,
		category TEXT NOT NULL,
		server TEXT NOT NULL,
		subject TEXT NOT NULL,
		message TEXT NOT NULL,
		urgent INTEGER NOT NULL,
		opened_at INTEGER NOT NULL,
		resolved_at INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE INDEX IF NOT EXISTS history_opened ON history(opened_at)`,
	`CREATE INDEX IF NOT EXISTS history_resolved ON history(resolved_at)`,
	`CREATE INDEX IF NOT EXISTS history_finding ON history(finding_id)`,
	`CREATE TABLE IF NOT EXISTS snoozes (
		finding_id TEXT PRIMARY KEY,
		kind TEXT NOT NULL,
		until INTEGER NOT NULL DEFAULT 0,
		reason TEXT NOT NULL DEFAULT '',
		subject TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		created_by TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE TABLE IF NOT EXISTS notifications (
		seq INTEGER PRIMARY KEY AUTOINCREMENT,
		sent_at INTEGER NOT NULL,
		kind TEXT NOT NULL,
		channel TEXT NOT NULL,
		ok INTEGER NOT NULL,
		error TEXT NOT NULL DEFAULT '',
		summary TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE TABLE IF NOT EXISTS conflict_counters (server TEXT NOT NULL, folder TEXT NOT NULL, value INTEGER NOT NULL, start_time TEXT NOT NULL, PRIMARY KEY(server, folder))`,
	`CREATE TABLE IF NOT EXISTS conflict_deltas (server TEXT NOT NULL, folder TEXT NOT NULL, at INTEGER NOT NULL, delta INTEGER NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS conflict_deltas_at ON conflict_deltas(at)`,
	`CREATE TABLE IF NOT EXISTS sessions (token_hash TEXT PRIMARY KEY, role TEXT NOT NULL, csrf TEXT NOT NULL, created_at INTEGER NOT NULL, expires_at INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS traffic_counters (server TEXT PRIMARY KEY, in_total INTEGER NOT NULL, out_total INTEGER NOT NULL, start_time TEXT NOT NULL, at INTEGER NOT NULL)`,
	`CREATE TABLE IF NOT EXISTS traffic_deltas (server TEXT NOT NULL, at INTEGER NOT NULL, in_bytes INTEGER NOT NULL, out_bytes INTEGER NOT NULL)`,
	`CREATE INDEX IF NOT EXISTS traffic_deltas_at ON traffic_deltas(at)`,
}

// Open opens (creating if needed) the database at path. key is the 32-byte
// secrets key; see LoadKey.
func Open(path string, key []byte) (*Store, error) {
	if len(key) != 32 {
		return nil, errors.New("store: secrets key must be 32 bytes")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(1)")
	dsn := "file:" + filepath.ToSlash(abs) + "?" + q.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, key: key}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: %w", err)
	}
	return s, nil
}

// OpenMemory opens a private in-memory database, for tests.
func OpenMemory(key []byte) (*Store, error) {
	db, err := sql.Open("sqlite", "file::memory:?_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, key: key}
	if err := s.migrate(); err != nil {
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	for _, stmt := range schema {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	var v int
	_ = s.db.QueryRow(`SELECT value FROM settings WHERE key='schema_version'`).Scan(&v)
	if v > schemaVersion {
		return fmt.Errorf("database schema %d is newer than this syncwatch (%d)", v, schemaVersion)
	}
	_, err := s.db.Exec(`INSERT INTO settings(key,value) VALUES('schema_version',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, schemaVersion)
	return err
}

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMS(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v)
}

// Setting returns a setting, or "" if unset.
func (s *Store) Setting(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetSetting writes a setting.
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// DeleteSetting removes a setting.
func (s *Store) DeleteSetting(key string) error {
	_, err := s.db.Exec(`DELETE FROM settings WHERE key=?`, key)
	return err
}

// JSONSetting decodes a JSON setting into out. found is false if unset.
func (s *Store) JSONSetting(key string, out any) (found bool, err error) {
	v, err := s.Setting(key)
	if err != nil || v == "" {
		return false, err
	}
	return true, json.Unmarshal([]byte(v), out)
}

// SetJSONSetting stores a value as JSON.
func (s *Store) SetJSONSetting(key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.SetSetting(key, string(b))
}

// Ping checks the database.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }
