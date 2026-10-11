package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"time"
)

// Settings keys of the heartbeat and of the times when syncwatch did not run.
const (
	keyAliveAt = "app.alive_at" // Unix ms
	keyGaps    = "app.gaps"     // JSON list of Gap
)

// The gap list keeps the gaps of the last 30 days, at most 50.
const (
	gapKeep = 30 * 24 * time.Hour
	maxGaps = 50
)

// Gap is a time when syncwatch did not run.
type Gap struct {
	From     time.Time `json:"from"`
	To       time.Time `json:"to"`
	Reported bool      `json:"reported"` // a notification told about it
}

// AliveAt returns the time of the last heartbeat, or zero when there was none.
func (s *Store) AliveAt() (time.Time, error) {
	v, err := s.Setting(keyAliveAt)
	if err != nil || v == "" {
		return time.Time{}, err
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	return fromMS(n), nil
}

// SetAliveAt records a heartbeat.
func (s *Store) SetAliveAt(t time.Time) error {
	return s.SetSetting(keyAliveAt, strconv.FormatInt(ms(t), 10))
}

// AddGap records a gap. The list keeps the gaps that ended in the 30 days
// before the end of the newest gap, at most 50, oldest first.
func (s *Store) AddGap(g Gap) error {
	return s.updateGaps(func(gaps []Gap) []Gap {
		gaps = append(gaps, g)
		sort.SliceStable(gaps, func(i, j int) bool { return gaps[i].To.Before(gaps[j].To) })
		cut := gaps[len(gaps)-1].To.Add(-gapKeep)
		i := 0
		for i < len(gaps) && gaps[i].To.Before(cut) {
			i++
		}
		gaps = gaps[i:]
		if len(gaps) > maxGaps {
			gaps = gaps[len(gaps)-maxGaps:]
		}
		return gaps
	})
}

// Gaps returns the gaps that ended at or after since, oldest first.
func (s *Store) Gaps(since time.Time) ([]Gap, error) {
	var all, out []Gap
	if _, err := s.JSONSetting(keyGaps, &all); err != nil {
		return nil, err
	}
	for _, g := range all {
		if !g.To.Before(since) {
			out = append(out, g)
		}
	}
	return out, nil
}

// MarkGapsReported marks the gaps that ended at or before upTo as reported.
func (s *Store) MarkGapsReported(upTo time.Time) error {
	return s.updateGaps(func(gaps []Gap) []Gap {
		for i := range gaps {
			if !gaps[i].To.After(upTo) {
				gaps[i].Reported = true
			}
		}
		return gaps
	})
}

// updateGaps changes the gap list in one transaction, so a heartbeat at the
// start and a notification that marks gaps cannot lose a change.
func (s *Store) updateGaps(fn func([]Gap) []Gap) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	var raw string
	if err := tx.QueryRow(`SELECT value FROM settings WHERE key=?`, keyGaps).Scan(&raw); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var gaps []Gap
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &gaps); err != nil {
			return err
		}
	}
	gaps = fn(gaps)
	if gaps == nil {
		gaps = []Gap{}
	}
	b, err := json.Marshal(gaps)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, keyGaps, string(b)); err != nil {
		return err
	}
	return tx.Commit()
}
