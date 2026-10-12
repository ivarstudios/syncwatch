package store

import "time"

// FolderActivity is what syncwatch keeps about a folder on a server across
// restarts.
type FolderActivity struct {
	FirstSeen       time.Time // when syncwatch saw the folder on the server for the first time
	LastLocalChange time.Time // the newest local change that syncwatch saw; zero when none
}

// FolderActivity returns the activity of the folders of a server, by folder ID.
func (s *Store) FolderActivity(server string) (map[string]FolderActivity, error) {
	rows, err := s.db.Query(`SELECT folder, first_seen, last_local_change FROM folder_activity WHERE server=?`, server)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]FolderActivity{}
	for rows.Next() {
		var folder string
		var first, last int64
		if err := rows.Scan(&folder, &first, &last); err != nil {
			return nil, err
		}
		out[folder] = FolderActivity{FirstSeen: fromMS(first), LastLocalChange: fromMS(last)}
	}
	return out, rows.Err()
}

// NoteFoldersSeen records at as the first-seen time of the folders that have
// no row yet. The rows of the other folders do not change.
func (s *Store) NoteFoldersSeen(server string, folders []string, at time.Time) error {
	if len(folders) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	for _, f := range folders {
		if _, err := tx.Exec(`INSERT INTO folder_activity(server, folder, first_seen) VALUES(?,?,?) ON CONFLICT(server, folder) DO NOTHING`,
			server, f, ms(at)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetLastLocalChange records a local change of a folder. An older time than
// the stored one changes nothing. A folder without a row gets the first-seen
// time at.
func (s *Store) SetLastLocalChange(server, folder string, at time.Time) error {
	_, err := s.db.Exec(`INSERT INTO folder_activity(server, folder, first_seen, last_local_change) VALUES(?,?,?,?)
		ON CONFLICT(server, folder) DO UPDATE SET last_local_change=max(last_local_change, excluded.last_local_change)`,
		server, folder, ms(at), ms(at))
	return err
}
