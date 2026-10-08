package store

import (
	"database/sql"
	"errors"
	"time"
)

// TrafficSample is what one reading of a server's traffic counters added
// since the previous one.
type TrafficSample struct {
	In, Out int64         // bytes received and sent since the previous reading
	Over    time.Duration // time since the previous reading; 0 for the first one
}

// RecordTraffic stores a reading of a server's traffic counters (bytes
// received and sent since Syncthing started) and records the increase since
// the previous reading. The counters restart from zero when Syncthing
// restarts (startTime changes, or a counter goes down); the whole value is
// new then. The first reading is only a baseline.
func (s *Store) RecordTraffic(server, startTime string, in, out int64, at time.Time) (TrafficSample, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return TrafficSample{}, err
	}
	defer tx.Rollback() //nolint:errcheck
	var prevIn, prevOut, prevAt int64
	var prevStart string
	err = tx.QueryRow(`SELECT in_total, out_total, start_time, at FROM traffic_counters WHERE server=?`, server).Scan(&prevIn, &prevOut, &prevStart, &prevAt)
	var sample TrafficSample
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// first reading: baseline only
	case err != nil:
		return TrafficSample{}, err
	default:
		sample.Over = at.Sub(fromMS(prevAt))
		if prevStart != startTime || in < prevIn || out < prevOut {
			sample.In, sample.Out = in, out
		} else {
			sample.In, sample.Out = in-prevIn, out-prevOut
		}
	}
	if sample.In > 0 || sample.Out > 0 {
		if _, err := tx.Exec(`INSERT INTO traffic_deltas(server, at, in_bytes, out_bytes) VALUES(?,?,?,?)`, server, ms(at), sample.In, sample.Out); err != nil {
			return TrafficSample{}, err
		}
	}
	if _, err := tx.Exec(`INSERT INTO traffic_counters(server, in_total, out_total, start_time, at) VALUES(?,?,?,?,?)
		ON CONFLICT(server) DO UPDATE SET in_total=excluded.in_total, out_total=excluded.out_total, start_time=excluded.start_time, at=excluded.at`,
		server, in, out, startTime, ms(at)); err != nil {
		return TrafficSample{}, err
	}
	return sample, tx.Commit()
}

// Traffic is bytes received and sent.
type Traffic struct {
	In, Out int64
}

// TrafficSince sums the traffic recorded per server since t.
func (s *Store) TrafficSince(t time.Time) (map[string]Traffic, error) {
	rows, err := s.db.Query(`SELECT server, SUM(in_bytes), SUM(out_bytes) FROM traffic_deltas WHERE at>=? GROUP BY server`, ms(t))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Traffic{}
	for rows.Next() {
		var server string
		var tr Traffic
		if err := rows.Scan(&server, &tr.In, &tr.Out); err != nil {
			return nil, err
		}
		out[server] = tr
	}
	return out, rows.Err()
}

// PruneTraffic deletes traffic recorded before t.
func (s *Store) PruneTraffic(t time.Time) error {
	_, err := s.db.Exec(`DELETE FROM traffic_deltas WHERE at<?`, ms(t))
	return err
}
