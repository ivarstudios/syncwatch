package store

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"github.com/ivarstudios/syncwatch/internal/model"
)

// Record is a finding with its lifecycle.
type Record struct {
	model.Finding
	Since              time.Time // when the condition started (for display)
	OpenedAt           time.Time // when the monitor opened it
	LastSeen           time.Time
	ResolvedAt         time.Time // zero while open
	Stale              bool      // frozen because a server it depends on is unreachable
	PingedAt           time.Time // urgent message sent
	ResolvedNotifiedAt time.Time // "resolved" message sent
}

// Open reports whether the finding is open.
func (r *Record) Open() bool { return r.ResolvedAt.IsZero() }

type recordData struct {
	Checks  []string       `json:"checks,omitempty"`
	Related []string       `json:"related,omitempty"`
	Details []model.Detail `json:"details,omitempty"`
	Paths   []string       `json:"paths,omitempty"`
	Link    string         `json:"link,omitempty"`
}

const findingCols = `id, check_id, severity, category, server, subject, message, data, urgent, since, opened_at, last_seen, resolved_at, stale, pinged_at, resolved_notified_at`

func scanRecord(sc interface{ Scan(...any) error }) (*Record, error) {
	var r Record
	var data string
	var sev, urgent, stale int
	var since, opened, last, resolved, pinged, notified int64
	if err := sc.Scan(&r.ID, &r.Check, &sev, &r.Category, &r.Server, &r.Subject, &r.Message, &data,
		&urgent, &since, &opened, &last, &resolved, &stale, &pinged, &notified); err != nil {
		return nil, err
	}
	var d recordData
	_ = json.Unmarshal([]byte(data), &d)
	r.Severity = model.Severity(sev)
	r.Checks, r.Related, r.Details, r.Paths, r.Link = d.Checks, d.Related, d.Details, d.Paths, d.Link
	if len(r.Checks) == 0 {
		r.Checks = []string{r.Check}
	}
	r.Urgent, r.Stale = urgent == 1, stale == 1
	r.Since, r.OpenedAt, r.LastSeen = fromMS(since), fromMS(opened), fromMS(last)
	r.ResolvedAt, r.PingedAt, r.ResolvedNotifiedAt = fromMS(resolved), fromMS(pinged), fromMS(notified)
	return &r, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// SaveRecord inserts or replaces a finding record.
func (s *Store) SaveRecord(r *Record) error {
	data, _ := json.Marshal(recordData{Checks: r.Checks, Related: r.Related, Details: r.Details, Paths: r.Paths, Link: r.Link})
	_, err := s.db.Exec(`INSERT INTO findings(`+findingCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET check_id=excluded.check_id, severity=excluded.severity, category=excluded.category,
		server=excluded.server, subject=excluded.subject, message=excluded.message, data=excluded.data, urgent=excluded.urgent,
		since=excluded.since, opened_at=excluded.opened_at, last_seen=excluded.last_seen, resolved_at=excluded.resolved_at,
		stale=excluded.stale, pinged_at=excluded.pinged_at, resolved_notified_at=excluded.resolved_notified_at`,
		r.ID, r.Check, int(r.Severity), r.Category, r.Server, r.Subject, r.Message, string(data), b2i(r.Urgent),
		ms(r.Since), ms(r.OpenedAt), ms(r.LastSeen), ms(r.ResolvedAt), b2i(r.Stale), ms(r.PingedAt), ms(r.ResolvedNotifiedAt))
	return err
}

// Record loads one finding, or nil.
func (s *Store) Record(id string) (*Record, error) {
	r, err := scanRecord(s.db.QueryRow(`SELECT `+findingCols+` FROM findings WHERE id=?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return r, err
}

// OpenRecords returns every open finding.
func (s *Store) OpenRecords() ([]*Record, error) {
	return s.queryRecords(`SELECT ` + findingCols + ` FROM findings WHERE resolved_at=0`)
}

// PendingResolved returns resolved findings that were pinged but whose
// "resolved" message hasn't been sent yet.
func (s *Store) PendingResolved() ([]*Record, error) {
	return s.queryRecords(`SELECT ` + findingCols + ` FROM findings WHERE resolved_at<>0 AND pinged_at<>0 AND resolved_notified_at=0`)
}

// ResolvedSince returns findings resolved at or after t.
func (s *Store) ResolvedSince(t time.Time) ([]*Record, error) {
	return s.queryRecords(`SELECT `+findingCols+` FROM findings WHERE resolved_at>=? ORDER BY resolved_at DESC`, ms(t))
}

func (s *Store) queryRecords(q string, args ...any) ([]*Record, error) {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Record
	for rows.Next() {
		r, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PruneResolved deletes resolved findings older than t that need no notification.
func (s *Store) PruneResolved(t time.Time) error {
	_, err := s.db.Exec(`DELETE FROM findings WHERE resolved_at<>0 AND resolved_at<? AND (pinged_at=0 OR resolved_notified_at<>0)`, ms(t))
	return err
}

// HistoryEntry is one occurrence of a finding.
type HistoryEntry struct {
	FindingID  string
	Check      string
	Severity   model.Severity
	Category   string
	Server     string
	Subject    string
	Message    string
	Urgent     bool
	OpenedAt   time.Time
	ResolvedAt time.Time
}

// HistoryOpen records that a finding opened.
func (s *Store) HistoryOpen(r *Record) error {
	_, err := s.db.Exec(`INSERT INTO history(finding_id, check_id, severity, category, server, subject, message, urgent, opened_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		r.ID, r.Check, int(r.Severity), r.Category, r.Server, r.Subject, r.Message, b2i(r.Urgent), ms(r.OpenedAt))
	return err
}

// HistoryResolve closes the open occurrence of a finding.
func (s *Store) HistoryResolve(id string, at time.Time) error {
	_, err := s.db.Exec(`UPDATE history SET resolved_at=? WHERE finding_id=? AND resolved_at=0`, ms(at), id)
	return err
}

// HistoryBetween returns occurrences that opened or resolved in [from, to).
func (s *Store) HistoryBetween(from, to time.Time) ([]HistoryEntry, error) {
	rows, err := s.db.Query(`SELECT finding_id, check_id, severity, category, server, subject, message, urgent, opened_at, resolved_at
		FROM history WHERE (opened_at>=? AND opened_at<?) OR (resolved_at>=? AND resolved_at<?) ORDER BY opened_at`,
		ms(from), ms(to), ms(from), ms(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HistoryEntry
	for rows.Next() {
		var h HistoryEntry
		var sev, urgent int
		var opened, resolved int64
		if err := rows.Scan(&h.FindingID, &h.Check, &sev, &h.Category, &h.Server, &h.Subject, &h.Message, &urgent, &opened, &resolved); err != nil {
			return nil, err
		}
		h.Severity, h.Urgent = model.Severity(sev), urgent == 1
		h.OpenedAt, h.ResolvedAt = fromMS(opened), fromMS(resolved)
		out = append(out, h)
	}
	return out, rows.Err()
}

// PruneHistory deletes occurrences resolved before t.
func (s *Store) PruneHistory(t time.Time) error {
	_, err := s.db.Exec(`DELETE FROM history WHERE resolved_at<>0 AND resolved_at<?`, ms(t))
	return err
}

// Observation is a tracked condition: when it was first seen and which
// servers it depends on.
type Observation struct {
	Since   time.Time
	Servers []string
}

// Observations returns every tracked condition.
func (s *Store) Observations() (map[string]Observation, error) {
	rows, err := s.db.Query(`SELECT id, since, servers FROM observations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Observation{}
	for rows.Next() {
		var id, servers string
		var since int64
		if err := rows.Scan(&id, &since, &servers); err != nil {
			return nil, err
		}
		o := Observation{Since: fromMS(since)}
		if servers != "" {
			o.Servers = strings.Split(servers, ",")
		}
		out[id] = o
	}
	return out, rows.Err()
}

// ApplyObservations adds and removes tracked conditions in one transaction.
func (s *Store) ApplyObservations(add map[string]Observation, remove []string) error {
	if len(add) == 0 && len(remove) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	for id, o := range add {
		if _, err := tx.Exec(`INSERT INTO observations(id, since, servers) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET since=excluded.since, servers=excluded.servers`,
			id, ms(o.Since), strings.Join(o.Servers, ",")); err != nil {
			return err
		}
	}
	for _, id := range remove {
		if _, err := tx.Exec(`DELETE FROM observations WHERE id=?`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
