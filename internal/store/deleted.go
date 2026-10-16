package store

import (
	"encoding/json"

	"github.com/ivarstudios/syncwatch/internal/model"
)

// ShareListing is what syncwatch keeps of the last listing of a share, so
// that the first scan after a restart also finds the project folders that
// were deleted while syncwatch did not run.
type ShareListing struct {
	Dirs     int      // the directories in the listing
	Unsynced []string // the paths of the project folders not in Syncthing (rules.NotInSyncthing)
}

// ShareListings returns the last listings of the shares of a server, by
// share path.
func (s *Store) ShareListings(server string) (map[string]ShareListing, error) {
	rows, err := s.db.Query(`SELECT share, dirs, unsynced FROM share_listings WHERE server=?`, server)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ShareListing{}
	for rows.Next() {
		var share, unsynced string
		var l ShareListing
		if err := rows.Scan(&share, &l.Dirs, &unsynced); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(unsynced), &l.Unsynced); err != nil {
			return nil, err
		}
		out[share] = l
	}
	return out, rows.Err()
}

// DeletedDirs returns the deleted project folders of a server, by path.
func (s *Store) DeletedDirs(server string) (map[string]*model.DeletedDir, error) {
	rows, err := s.db.Query(`SELECT path, name, share, share_name, detected_at, all_gone, size FROM deleted_dirs WHERE server=?`, server)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*model.DeletedDir{}
	for rows.Next() {
		d := &model.DeletedDir{}
		var at int64
		var size string
		if err := rows.Scan(&d.Path, &d.Name, &d.Share, &d.ShareName, &at, &d.All, &size); err != nil {
			return nil, err
		}
		d.DetectedAt = fromMS(at)
		if size != "" {
			d.Size = &model.DirSize{}
			if err := json.Unmarshal([]byte(size), d.Size); err != nil {
				return nil, err
			}
		}
		out[d.Path] = d
	}
	return out, rows.Err()
}

// SaveListings records, in one transaction, the new listings of shares
// (by share path), the folders that the listings found deleted, and the
// paths of the deleted folders that the listings have again.
func (s *Store) SaveListings(server string, listings map[string]ShareListing, deleted []*model.DeletedDir, back []string) error {
	if len(listings) == 0 && len(deleted) == 0 && len(back) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	for share, l := range listings {
		unsynced, err := json.Marshal(l.Unsynced)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO share_listings(server, share, dirs, unsynced) VALUES(?,?,?,?)
			ON CONFLICT(server, share) DO UPDATE SET dirs=excluded.dirs, unsynced=excluded.unsynced`,
			server, share, l.Dirs, string(unsynced)); err != nil {
			return err
		}
	}
	for _, d := range deleted {
		size := ""
		if d.Size != nil {
			b, err := json.Marshal(d.Size)
			if err != nil {
				return err
			}
			size = string(b)
		}
		if _, err := tx.Exec(`INSERT INTO deleted_dirs(server, path, name, share, share_name, detected_at, all_gone, size) VALUES(?,?,?,?,?,?,?,?)
			ON CONFLICT(server, path) DO NOTHING`,
			server, d.Path, d.Name, d.Share, d.ShareName, ms(d.DetectedAt), d.All, size); err != nil {
			return err
		}
	}
	for _, p := range back {
		if _, err := tx.Exec(`DELETE FROM deleted_dirs WHERE server=? AND path=?`, server, p); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// RemoveDeletedDir forgets a deleted folder of a server. It reports whether
// the folder was there.
func (s *Store) RemoveDeletedDir(server, path string) (bool, error) {
	res, err := s.db.Exec(`DELETE FROM deleted_dirs WHERE server=? AND path=?`, server, path)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// ForgetListings deletes the share listings and the deleted folders of a
// server that left the configuration: a new server with the same name gets
// the same ID, and must not inherit them.
func (s *Store) ForgetListings(server string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	for _, q := range []string{`DELETE FROM share_listings WHERE server=?`, `DELETE FROM deleted_dirs WHERE server=?`} {
		if _, err := tx.Exec(q, server); err != nil {
			return err
		}
	}
	return tx.Commit()
}
