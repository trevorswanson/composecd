package store

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// ErrExists is returned when creating a user or token whose name is taken.
var ErrExists = errors.New("already exists")

// User is a local account. PasswordHash is a bcrypt hash.
type User struct {
	Name         string    `json:"name"`
	Role         string    `json:"role"`
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"createdAt"`
}

// Token is an API token. Only a hash of its secret is stored.
type Token struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Role       string     `json:"role"`
	SecretHash string     `json:"-"`
	CreatedBy  string     `json:"createdBy,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	LastUsedAt *time.Time `json:"lastUsedAt,omitempty"`
}

// Session is a browser sign-in. IDHash is a hash of the cookie's value.
type Session struct {
	IDHash     string
	User       string
	Role       string
	Via        string
	CSRF       string
	CreatedAt  time.Time
	LastSeenAt time.Time
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func (s *Store) AddUser(u *User) error {
	u.CreatedAt = time.Now()
	_, err := s.db.Exec(`INSERT INTO users (name, role, password_hash, created_at) VALUES (?, ?, ?, ?)`,
		u.Name, u.Role, u.PasswordHash, formatTime(u.CreatedAt))
	if isUnique(err) {
		return ErrExists
	}
	return err
}

// User returns the user named name, or ErrNotFound.
func (s *Store) User(name string) (*User, error) {
	u := &User{}
	var created string
	err := s.db.QueryRow(`SELECT name, role, password_hash, created_at FROM users WHERE name = ?`, name).
		Scan(&u.Name, &u.Role, &u.PasswordHash, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	u.CreatedAt, err = parseTime(created)
	return u, err
}

func (s *Store) Users() ([]*User, error) {
	rows, err := s.db.Query(`SELECT name, role, created_at FROM users ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*User{}
	for rows.Next() {
		u := &User{}
		var created string
		if err := rows.Scan(&u.Name, &u.Role, &created); err != nil {
			return nil, err
		}
		if u.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetPassword replaces a user's password hash and signs them out everywhere.
func (s *Store) SetPassword(name, hash string) error {
	res, err := s.db.Exec(`UPDATE users SET password_hash = ? WHERE name = ?`, hash, name)
	if err != nil {
		return err
	}
	if err := affected(res); err != nil {
		return err
	}
	return s.DeleteSessions(name, "password")
}

// DeleteUser removes a user and their sessions.
func (s *Store) DeleteUser(name string) error {
	res, err := s.db.Exec(`DELETE FROM users WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if err := affected(res); err != nil {
		return err
	}
	return s.DeleteSessions(name, "password")
}

func (s *Store) AddToken(t *Token) error {
	t.CreatedAt = time.Now()
	_, err := s.db.Exec(`INSERT INTO tokens (id, name, role, secret_hash, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		t.ID, t.Name, t.Role, t.SecretHash, t.CreatedBy, formatTime(t.CreatedAt))
	if isUnique(err) {
		return ErrExists
	}
	return err
}

// Token returns the token with id, or ErrNotFound.
func (s *Store) Token(id string) (*Token, error) {
	ts, err := s.tokens(`WHERE id = ?`, id)
	if err != nil {
		return nil, err
	}
	if len(ts) == 0 {
		return nil, ErrNotFound
	}
	return ts[0], nil
}

func (s *Store) Tokens() ([]*Token, error) { return s.tokens(`ORDER BY name`) }

func (s *Store) tokens(where string, args ...interface{}) ([]*Token, error) {
	// where is a constant from this file; values are bound parameters.
	rows, err := s.db.Query(`SELECT id, name, role, secret_hash, created_by, created_at, last_used_at FROM tokens `+where, args...) //nolint:gosec // see above
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Token{}
	for rows.Next() {
		t := &Token{}
		var created string
		var used sql.NullString
		if err := rows.Scan(&t.ID, &t.Name, &t.Role, &t.SecretHash, &t.CreatedBy, &created, &used); err != nil {
			return nil, err
		}
		if t.CreatedAt, err = parseTime(created); err != nil {
			return nil, err
		}
		if used.Valid {
			u, err := parseTime(used.String)
			if err != nil {
				return nil, err
			}
			t.LastUsedAt = &u
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TouchToken records that a token was used at t.
func (s *Store) TouchToken(id string, t time.Time) error {
	_, err := s.db.Exec(`UPDATE tokens SET last_used_at = ? WHERE id = ?`, formatTime(t), id)
	return err
}

// DeleteToken revokes the token named name.
func (s *Store) DeleteToken(name string) error {
	res, err := s.db.Exec(`DELETE FROM tokens WHERE name = ?`, name)
	if err != nil {
		return err
	}
	return affected(res)
}

func (s *Store) AddSession(se *Session) error {
	_, err := s.db.Exec(`INSERT INTO sessions (id_hash, user, role, via, csrf, created_at, last_seen_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		se.IDHash, se.User, se.Role, se.Via, se.CSRF, formatTime(se.CreatedAt), formatTime(se.LastSeenAt))
	return err
}

// Session returns the session with idHash, or ErrNotFound.
func (s *Store) Session(idHash string) (*Session, error) {
	se := &Session{}
	var created, seen string
	err := s.db.QueryRow(`SELECT id_hash, user, role, via, csrf, created_at, last_seen_at FROM sessions WHERE id_hash = ?`, idHash).
		Scan(&se.IDHash, &se.User, &se.Role, &se.Via, &se.CSRF, &created, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if se.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	se.LastSeenAt, err = parseTime(seen)
	return se, err
}

// TouchSession records activity on a session.
func (s *Store) TouchSession(idHash string, t time.Time) error {
	_, err := s.db.Exec(`UPDATE sessions SET last_seen_at = ? WHERE id_hash = ?`, formatTime(t), idHash)
	return err
}

// DeleteSession signs one session out.
func (s *Store) DeleteSession(idHash string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE id_hash = ?`, idHash)
	return err
}

// DeleteSessions signs user out of every session made via (e.g. "password").
func (s *Store) DeleteSessions(user, via string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE user = ? AND via = ?`, user, via)
	return err
}

// DeleteSessionsBefore removes sessions last seen before t, or created before created.
func (s *Store) DeleteSessionsBefore(t, created time.Time) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE last_seen_at < ? OR created_at < ?`, formatTime(t), formatTime(created))
	return err
}

func affected(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
