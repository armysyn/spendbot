package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// KVWebPassword is where the page password hash is kept.
const KVWebPassword = "web_password"

// ResetPassword removes the page password and signs every browser out: the way back in when
// the password is forgotten, run on the computer itself.
func (s *Store) ResetPassword(ctx context.Context) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "DELETE FROM kv WHERE key = ?", KVWebPassword); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM sessions")
		return err
	})
}

// Session is a signed-in browser.
type Session struct {
	ID        string // the token hash
	CreatedAt time.Time
	SeenAt    time.Time
	ExpiresAt time.Time
	Agent     string
}

func (s *Store) CreateSession(ctx context.Context, id, agent string, now, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO sessions (id, created_at, seen_at, expires_at, agent) VALUES (?, ?, ?, ?, ?)",
		id, fmtTime(now), fmtTime(now), fmtTime(expires), nullString(agent))
	return err
}

// SessionValid reports whether a session exists and has not expired.
func (s *Store) SessionValid(ctx context.Context, id string, now time.Time) (bool, error) {
	var expires string
	err := s.db.QueryRowContext(ctx, "SELECT expires_at FROM sessions WHERE id = ?", id).Scan(&expires)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return parseTime(expires).After(now), nil
}

// TouchSession moves the expiry forward: a session used regularly stays signed in.
func (s *Store) TouchSession(ctx context.Context, id string, now, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, "UPDATE sessions SET seen_at = ?, expires_at = ? WHERE id = ?", fmtTime(now), fmtTime(expires), id)
	return err
}

func (s *Store) DeleteSession(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE id = ?", id)
	return err
}

// DeleteSessions signs every browser out, except keep (the current one) when given.
func (s *Store) DeleteSessions(ctx context.Context, keep string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE id != ?", keep)
	return err
}

// Sessions lists live sessions, the latest used first; expired ones are removed.
func (s *Store) Sessions(ctx context.Context, now time.Time) ([]Session, error) {
	if _, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at <= ?", fmtTime(now)); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT id, created_at, seen_at, expires_at, COALESCE(agent, '') FROM sessions ORDER BY seen_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var x Session
		var c, sn, e string
		if err := rows.Scan(&x.ID, &c, &sn, &e, &x.Agent); err != nil {
			return nil, err
		}
		x.CreatedAt, x.SeenAt, x.ExpiresAt = parseTime(c), parseTime(sn), parseTime(e)
		out = append(out, x)
	}
	return out, rows.Err()
}

// DeleteKV removes a housekeeping value.
func (s *Store) DeleteKV(ctx context.Context, key string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM kv WHERE key = ?", key)
	return err
}
