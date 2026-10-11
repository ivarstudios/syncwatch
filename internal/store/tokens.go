package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Scopes of an API token.
const (
	ScopeHA   = "ha"   // only /api/v1/ha
	ScopeFull = "full" // all of /api/v1
)

// Limits of the API tokens.
const (
	MaxAPITokens = 5  // API tokens at the same time
	MaxTokenName = 40 // characters in the name of an API token
)

// LegacyTokenName is the name of the API token that the single API token of
// an older version becomes (see migrateAPIToken).
const LegacyTokenName = "API token"

// Errors of the API token functions, with the texts for the dashboard.
var (
	ErrTooManyTokens = fmt.Errorf("There are %d API tokens. Revoke an API token first.", MaxAPITokens)
	ErrTokenName     = fmt.Errorf("The name of an API token must have 1 to %d characters and no control characters. Each API token needs a different name.", MaxTokenName)
	ErrTokenScope    = errors.New("The scope of an API token must be ha (only /api/v1/ha) or full (all of /api/v1).")
	ErrNoToken       = errors.New("The API token does not exist. Reload the page.")
)

// APIToken is a token of the JSON API. syncwatch keeps only the SHA-256 hash
// of its value.
type APIToken struct {
	ID        string // 8 random lower-case hex characters, for the forms
	Name      string
	Scope     string // ScopeHA or ScopeFull
	CreatedAt time.Time
	LastUsed  time.Time // zero when not used
}

// HashAPIToken returns the hex SHA-256 hash of a token value.
func HashAPIToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// tokenNameOK reports whether a name has 1 to MaxTokenName characters and no
// control or format characters.
func tokenNameOK(name string) bool {
	n := utf8.RuneCountInString(name)
	return utf8.ValidString(name) && n >= 1 && n <= MaxTokenName &&
		strings.IndexFunc(name, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) < 0
}

// newTokenID returns an ID that no API token has.
func newTokenID(q interface {
	QueryRow(string, ...any) *sql.Row
}) (string, error) {
	b := make([]byte, 4)
	for i := 0; i < 10; i++ {
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		id := hex.EncodeToString(b)
		var taken bool
		if err := q.QueryRow(`SELECT EXISTS(SELECT 1 FROM api_tokens WHERE id=?)`, id).Scan(&taken); err != nil {
			return "", err
		}
		if !taken {
			return id, nil
		}
	}
	return "", errors.New("store: cannot make an ID for the API token")
}

const tokenCols = `id, name, scope, created_at, last_used`

func scanToken(sc interface{ Scan(...any) error }) (APIToken, error) {
	var t APIToken
	var created, used int64
	err := sc.Scan(&t.ID, &t.Name, &t.Scope, &created, &used)
	t.CreatedAt, t.LastUsed = fromMS(created), fromMS(used)
	return t, err
}

// APITokens returns the API tokens, oldest first.
func (s *Store) APITokens() ([]APIToken, error) {
	rows, err := s.db.Query(`SELECT ` + tokenCols + ` FROM api_tokens ORDER BY created_at, rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIToken
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// AddAPIToken stores a new API token with the value token. The name is
// trimmed; it must be unique without regard to case.
func (s *Store) AddAPIToken(name, scope, token string, at time.Time) (APIToken, error) {
	name = strings.TrimSpace(name)
	switch {
	case !tokenNameOK(name):
		return APIToken{}, ErrTokenName
	case scope != ScopeHA && scope != ScopeFull:
		return APIToken{}, ErrTokenScope
	case token == "":
		return APIToken{}, errors.New("store: an API token needs a value")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return APIToken{}, err
	}
	defer tx.Rollback() //nolint:errcheck
	rows, err := tx.Query(`SELECT name FROM api_tokens`)
	if err != nil {
		return APIToken{}, err
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return APIToken{}, err
		}
		names = append(names, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return APIToken{}, err
	}
	if len(names) >= MaxAPITokens {
		return APIToken{}, ErrTooManyTokens
	}
	for _, n := range names {
		if strings.EqualFold(n, name) {
			return APIToken{}, ErrTokenName
		}
	}
	id, err := newTokenID(tx)
	if err != nil {
		return APIToken{}, err
	}
	if _, err := tx.Exec(`INSERT INTO api_tokens(id, name, scope, hash, created_at, last_used) VALUES(?,?,?,?,?,0)`,
		id, name, scope, HashAPIToken(token), ms(at)); err != nil {
		return APIToken{}, err
	}
	if err := tx.Commit(); err != nil {
		return APIToken{}, err
	}
	return APIToken{ID: id, Name: name, Scope: scope, CreatedAt: fromMS(ms(at))}, nil
}

// ReplaceAPIToken gives an API token the new value token, made at at. The
// name and the scope stay; the old value stops working, and the token was
// not used yet.
func (s *Store) ReplaceAPIToken(id, token string, at time.Time) error {
	if token == "" {
		return errors.New("store: an API token needs a value")
	}
	return oneRow(s.db.Exec(`UPDATE api_tokens SET hash=?, created_at=?, last_used=0 WHERE id=?`, HashAPIToken(token), ms(at), id))
}

// DeleteAPIToken revokes an API token.
func (s *Store) DeleteAPIToken(id string) error {
	return oneRow(s.db.Exec(`DELETE FROM api_tokens WHERE id=?`, id))
}

// APITokenByValue returns the API token with the value token, or nil and no
// error when no API token has this value.
func (s *Store) APITokenByValue(token string) (*APIToken, error) {
	if token == "" {
		return nil, nil
	}
	t, err := scanToken(s.db.QueryRow(`SELECT `+tokenCols+` FROM api_tokens WHERE hash=?`, HashAPIToken(token)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// TouchAPIToken records that an API token was used at at.
func (s *Store) TouchAPIToken(id string, at time.Time) error {
	return oneRow(s.db.Exec(`UPDATE api_tokens SET last_used=? WHERE id=?`, ms(at), id))
}

// oneRow gives ErrNoToken for a statement that changed no row.
func oneRow(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrNoToken
	}
	return nil
}
