package store

import (
	"database/sql"
	"errors"
	"strings"
)

// Admin returns the admin username and password hash.
func (s *Store) Admin() (user, hash string, err error) {
	err = s.DB.QueryRow(`SELECT username, pw_hash FROM admin WHERE id=1`).Scan(&user, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return
}

// SetAdmin stores admin credentials (hash already computed).
func (s *Store) SetAdmin(user, hash string) error {
	_, err := s.DB.Exec(`INSERT INTO admin(id, username, pw_hash, changed_at) VALUES(1,?,?,?)
		ON CONFLICT(id) DO UPDATE SET username=excluded.username, pw_hash=excluded.pw_hash, changed_at=excluded.changed_at`, user, hash, now())
	return err
}

// Session is a logged-in browser.
type Session struct {
	ID        int64
	CSRF      string
	Name      string
	IP        string
	UserAgent string
	Remember  bool
	CreatedAt int64
	LastUsed  int64
	ExpiresAt int64
}

const sessCols = `id, csrf, name, ip, user_agent, remember, created_at, last_used, expires_at`

func scanSession(row interface{ Scan(...any) error }) (Session, error) {
	var se Session
	err := row.Scan(&se.ID, &se.CSRF, &se.Name, &se.IP, &se.UserAgent, &se.Remember, &se.CreatedAt, &se.LastUsed, &se.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return se, ErrNotFound
	}
	return se, err
}

// CreateSession stores a session by token hash.
func (s *Store) CreateSession(tokenHash string, se *Session) error {
	res, err := s.DB.Exec(`INSERT INTO sessions(token_hash, csrf, name, ip, user_agent, remember, created_at, last_used, expires_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		tokenHash, se.CSRF, se.Name, se.IP, se.UserAgent, se.Remember, se.CreatedAt, se.LastUsed, se.ExpiresAt)
	if err != nil {
		return err
	}
	se.ID, _ = res.LastInsertId()
	return nil
}

// SessionByHash finds an unexpired session.
func (s *Store) SessionByHash(tokenHash string, nowUnix int64) (Session, error) {
	return scanSession(s.DB.QueryRow(`SELECT `+sessCols+` FROM sessions WHERE token_hash=? AND expires_at>?`, tokenHash, nowUnix))
}

// TouchSession updates last-used info.
func (s *Store) TouchSession(id int64, ip string, ts int64) {
	_, _ = s.DB.Exec(`UPDATE sessions SET last_used=?, ip=? WHERE id=?`, ts, ip, id)
}

// Sessions lists active sessions.
func (s *Store) Sessions(nowUnix int64) ([]Session, error) {
	rows, err := s.DB.Query(`SELECT `+sessCols+` FROM sessions WHERE expires_at>? ORDER BY last_used DESC`, nowUnix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		se, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, se)
	}
	return out, rows.Err()
}

// RenameSession sets a device's display name.
func (s *Store) RenameSession(id int64, name string) error {
	_, err := s.DB.Exec(`UPDATE sessions SET name=? WHERE id=?`, name, id)
	return err
}

// DeleteSession revokes one session.
func (s *Store) DeleteSession(id int64) error {
	_, err := s.DB.Exec(`DELETE FROM sessions WHERE id=?`, id)
	return err
}

// DeleteAllSessions revokes every session.
func (s *Store) DeleteAllSessions() error {
	_, err := s.DB.Exec(`DELETE FROM sessions`)
	return err
}

// PurgeExpiredSessions removes old rows.
func (s *Store) PurgeExpiredSessions(nowUnix int64) {
	_, _ = s.DB.Exec(`DELETE FROM sessions WHERE expires_at<=?`, nowUnix)
}

// APIToken is a bearer token for integrations such as Home Assistant.
type APIToken struct {
	ID        int64
	Name      string
	Scopes    []string
	CreatedAt int64
	LastUsed  int64
}

// HasScope reports whether t grants scope.
func (t APIToken) HasScope(scope string) bool {
	for _, s := range t.Scopes {
		if s == scope {
			return true
		}
	}
	return false
}

func scanToken(row interface{ Scan(...any) error }) (APIToken, error) {
	var t APIToken
	var scopes string
	err := row.Scan(&t.ID, &t.Name, &scopes, &t.CreatedAt, &t.LastUsed)
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	if scopes != "" {
		t.Scopes = strings.Split(scopes, ",")
	}
	return t, err
}

// CreateToken stores a token by hash.
func (s *Store) CreateToken(name, tokenHash string, scopes []string) error {
	_, err := s.DB.Exec(`INSERT INTO api_tokens(name, token_hash, scopes, created_at) VALUES(?,?,?,?)`, name, tokenHash, strings.Join(scopes, ","), now())
	return err
}

// TokenByHash finds a token and records its use.
func (s *Store) TokenByHash(tokenHash string) (APIToken, error) {
	t, err := scanToken(s.DB.QueryRow(`SELECT id, name, scopes, created_at, last_used FROM api_tokens WHERE token_hash=?`, tokenHash))
	if err == nil {
		_, _ = s.DB.Exec(`UPDATE api_tokens SET last_used=? WHERE id=?`, now(), t.ID)
	}
	return t, err
}

// Tokens lists API tokens.
func (s *Store) Tokens() ([]APIToken, error) {
	rows, err := s.DB.Query(`SELECT id, name, scopes, created_at, last_used FROM api_tokens ORDER BY id`)
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

// DeleteToken revokes a token.
func (s *Store) DeleteToken(id int64) error {
	_, err := s.DB.Exec(`DELETE FROM api_tokens WHERE id=?`, id)
	return err
}

// DeleteAllTokens revokes every token.
func (s *Store) DeleteAllTokens() error {
	_, err := s.DB.Exec(`DELETE FROM api_tokens`)
	return err
}
