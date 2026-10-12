package store

import "time"

// AddConflicts records n new conflicts in a folder of a server, next to the
// increases of the conflict counters that RecordConflicts records:
// ConflictsSince counts both. The collector uses it for the conflict copies
// that other devices made and that reached the server (H10).
func (s *Store) AddConflicts(server, folder string, n int64, at time.Time) error {
	if n <= 0 {
		return nil
	}
	_, err := s.db.Exec(`INSERT INTO conflict_deltas(server, folder, at, delta) VALUES(?,?,?,?)`, server, folder, ms(at), n)
	return err
}
