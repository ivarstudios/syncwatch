package store

import (
	"database/sql"
	"errors"
	"time"
)

// Pin returns the pinned certificate fingerprint of a server, or "".
func (s *Store) Pin(serverID string) (string, error) {
	var fp string
	err := s.db.QueryRow(`SELECT fingerprint FROM pins WHERE server_id=?`, serverID).Scan(&fp)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return fp, err
}

// SetPin pins a server's certificate fingerprint. An empty fingerprint removes the pin.
func (s *Store) SetPin(serverID, fp string) error {
	if fp == "" {
		_, err := s.db.Exec(`DELETE FROM pins WHERE server_id=?`, serverID)
		return err
	}
	_, err := s.db.Exec(`INSERT INTO pins(server_id, fingerprint, pinned_at) VALUES(?,?,?)
		ON CONFLICT(server_id) DO UPDATE SET fingerprint=excluded.fingerprint, pinned_at=excluded.pinned_at`,
		serverID, fp, time.Now().UnixMilli())
	return err
}

// Snooze kinds.
const (
	SnoozeKind = "snooze"
	IgnoreKind = "ignore"
)

// Snooze hides a finding until a time, or for good when ignored.
type Snooze struct {
	FindingID string
	Kind      string
	Until     time.Time // zero for ignore
	Reason    string
	Subject   string
	CreatedAt time.Time
	CreatedBy string
}

// Active reports whether the snooze applies at t.
func (z Snooze) Active(t time.Time) bool {
	return z.Kind == IgnoreKind || z.Until.After(t)
}

// SetSnooze snoozes or ignores a finding.
func (s *Store) SetSnooze(z Snooze) error {
	_, err := s.db.Exec(`INSERT INTO snoozes(finding_id, kind, until, reason, subject, created_at, created_by) VALUES(?,?,?,?,?,?,?)
		ON CONFLICT(finding_id) DO UPDATE SET kind=excluded.kind, until=excluded.until, reason=excluded.reason,
		subject=excluded.subject, created_at=excluded.created_at, created_by=excluded.created_by`,
		z.FindingID, z.Kind, ms(z.Until), z.Reason, z.Subject, ms(z.CreatedAt), z.CreatedBy)
	return err
}

// ClearSnooze removes a snooze or ignore.
func (s *Store) ClearSnooze(id string) error {
	_, err := s.db.Exec(`DELETE FROM snoozes WHERE finding_id=?`, id)
	return err
}

// Snoozes returns every snooze, including expired ones.
func (s *Store) Snoozes() (map[string]Snooze, error) {
	rows, err := s.db.Query(`SELECT finding_id, kind, until, reason, subject, created_at, created_by FROM snoozes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Snooze{}
	for rows.Next() {
		var z Snooze
		var until, created int64
		if err := rows.Scan(&z.FindingID, &z.Kind, &until, &z.Reason, &z.Subject, &created, &z.CreatedBy); err != nil {
			return nil, err
		}
		z.Until, z.CreatedAt = fromMS(until), fromMS(created)
		out[z.FindingID] = z
	}
	return out, rows.Err()
}

// PruneSnoozes deletes snoozes that expired before t.
func (s *Store) PruneSnoozes(t time.Time) error {
	_, err := s.db.Exec(`DELETE FROM snoozes WHERE kind=? AND until<?`, SnoozeKind, ms(t))
	return err
}

// LogNotification records a sent (or failed) notification.
func (s *Store) LogNotification(at time.Time, kind, channel string, ok bool, errText, summary string) error {
	_, err := s.db.Exec(`INSERT INTO notifications(sent_at, kind, channel, ok, error, summary) VALUES(?,?,?,?,?,?)`,
		ms(at), kind, channel, b2i(ok), errText, summary)
	if err == nil {
		_, err = s.db.Exec(`DELETE FROM notifications WHERE seq <= (SELECT MAX(seq) FROM notifications) - 1000`)
	}
	return err
}

// NotificationEntry is one line of the notification log.
type NotificationEntry struct {
	SentAt  time.Time
	Kind    string
	Channel string
	OK      bool
	Error   string
	Summary string
}

// Notifications returns the most recent notification log entries.
func (s *Store) Notifications(limit int) ([]NotificationEntry, error) {
	rows, err := s.db.Query(`SELECT sent_at, kind, channel, ok, error, summary FROM notifications ORDER BY seq DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NotificationEntry
	for rows.Next() {
		var n NotificationEntry
		var at int64
		var ok int
		if err := rows.Scan(&at, &n.Kind, &n.Channel, &ok, &n.Error, &n.Summary); err != nil {
			return nil, err
		}
		n.SentAt, n.OK = fromMS(at), ok == 1
		out = append(out, n)
	}
	return out, rows.Err()
}

// RecordConflicts stores conflict counters read from a server and records the
// increase since the last reading. Counters restart from zero when Syncthing
// restarts (startTime changes), in which case the whole value is new.
func (s *Store) RecordConflicts(server, startTime string, counters map[string]int64, at time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	for folder, val := range counters {
		var prev int64
		var prevStart string
		err := tx.QueryRow(`SELECT value, start_time FROM conflict_counters WHERE server=? AND folder=?`, server, folder).Scan(&prev, &prevStart)
		var delta int64
		switch {
		case errors.Is(err, sql.ErrNoRows):
			delta = 0 // first reading: baseline only
		case err != nil:
			return err
		case prevStart != startTime || val < prev:
			delta = val
		default:
			delta = val - prev
		}
		if delta > 0 {
			if _, err := tx.Exec(`INSERT INTO conflict_deltas(server, folder, at, delta) VALUES(?,?,?,?)`, server, folder, ms(at), delta); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`INSERT INTO conflict_counters(server, folder, value, start_time) VALUES(?,?,?,?)
			ON CONFLICT(server, folder) DO UPDATE SET value=excluded.value, start_time=excluded.start_time`, server, folder, val, startTime); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ConflictsSince sums conflict increases per server and folder since t.
func (s *Store) ConflictsSince(t time.Time) (map[string]map[string]int64, error) {
	rows, err := s.db.Query(`SELECT server, folder, SUM(delta) FROM conflict_deltas WHERE at>=? GROUP BY server, folder`, ms(t))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]int64{}
	for rows.Next() {
		var server, folder string
		var n int64
		if err := rows.Scan(&server, &folder, &n); err != nil {
			return nil, err
		}
		if out[server] == nil {
			out[server] = map[string]int64{}
		}
		out[server][folder] = n
	}
	return out, rows.Err()
}

// PruneConflicts deletes conflict increases recorded before t.
func (s *Store) PruneConflicts(t time.Time) error {
	_, err := s.db.Exec(`DELETE FROM conflict_deltas WHERE at<?`, ms(t))
	return err
}

// Session is a logged-in browser session.
type Session struct {
	TokenHash string
	Role      string
	CSRF      string
	CreatedAt time.Time
	ExpiresAt time.Time
}

// CreateSession stores a session.
func (s *Store) CreateSession(z Session) error {
	_, err := s.db.Exec(`INSERT INTO sessions(token_hash, role, csrf, created_at, expires_at) VALUES(?,?,?,?,?)`,
		z.TokenHash, z.Role, z.CSRF, ms(z.CreatedAt), ms(z.ExpiresAt))
	return err
}

// Session loads a session that hasn't expired at t, or nil.
func (s *Store) Session(tokenHash string, t time.Time) (*Session, error) {
	var z Session
	var created, expires int64
	err := s.db.QueryRow(`SELECT token_hash, role, csrf, created_at, expires_at FROM sessions WHERE token_hash=? AND expires_at>?`, tokenHash, ms(t)).
		Scan(&z.TokenHash, &z.Role, &z.CSRF, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	z.CreatedAt, z.ExpiresAt = fromMS(created), fromMS(expires)
	return &z, nil
}

// DeleteSession logs a session out.
func (s *Store) DeleteSession(tokenHash string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token_hash=?`, tokenHash)
	return err
}

// DeleteSessions logs out every session with a role ("" for all).
func (s *Store) DeleteSessions(role string) error {
	if role == "" {
		_, err := s.db.Exec(`DELETE FROM sessions`)
		return err
	}
	_, err := s.db.Exec(`DELETE FROM sessions WHERE role=?`, role)
	return err
}

// PruneSessions deletes expired sessions.
func (s *Store) PruneSessions(t time.Time) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE expires_at<=?`, ms(t))
	return err
}
