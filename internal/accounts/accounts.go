// Package accounts keeps who can sign in to spendbot: accounts with their own database each,
// their password hashes, whose statements they hold, and signed-in browsers. It lives in its
// own small database next to the data, so one person's money never sits in another's file.
package accounts

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("no such account")

// Account is one person's spendbot: a name, a database, a password.
type Account struct {
	ID        int64
	Name      string
	DBPath    string
	Hash      string // the password hash; empty — no password
	Holder    string // the name printed on this account's statements, once seen
	CreatedAt time.Time
}

// Primary reports the first account: it owns the Telegram bot, Apple Wallet and ClickHouse.
func (a Account) Primary() bool { return a.ID == 1 }

// Session is a signed-in browser of an account.
type Session struct {
	ID        string // the SHA-256 of the cookie token
	AccountID int64
	CreatedAt time.Time
	SeenAt    time.Time
	ExpiresAt time.Time
	Agent     string
}

type Registry struct {
	db  *sql.DB
	dir string // where new accounts' databases go
}

const schema = `
CREATE TABLE IF NOT EXISTS accounts (
  id         INTEGER PRIMARY KEY,
  name       TEXT NOT NULL,
  db_path    TEXT NOT NULL UNIQUE,
  password   TEXT,
  holder     TEXT,
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
  id         TEXT PRIMARY KEY,
  account_id INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
  created_at TEXT NOT NULL,
  seen_at    TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  agent      TEXT
);
CREATE INDEX IF NOT EXISTS sessions_account ON sessions(account_id);`

// Open opens the registry at path; new accounts' databases go to the "accounts" folder next to it.
func Open(ctx context.Context, path string) (*Registry, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"+
		"&_pragma=foreign_keys(1)&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("accounts: %w", err)
	}
	// password hashes and sessions: only the owner of the computer account reads the file
	if err := os.Chmod(path, 0o600); err != nil {
		db.Close()
		return nil, err
	}
	return &Registry{db: db, dir: filepath.Join(filepath.Dir(path), "accounts")}, nil
}

func (r *Registry) Close() error { return r.db.Close() }

const layout = time.RFC3339

func ts(t time.Time) string { return t.UTC().Format(layout) }

func parse(s string) time.Time {
	t, _ := time.Parse(layout, s)
	return t
}

func null(s string) any {
	if s == "" {
		return nil
	}
	return s
}

const columns = "id, name, db_path, COALESCE(password, ''), COALESCE(holder, ''), created_at"

func scan(row interface{ Scan(...any) error }) (Account, error) {
	var a Account
	var created string
	err := row.Scan(&a.ID, &a.Name, &a.DBPath, &a.Hash, &a.Holder, &created)
	a.CreatedAt = parse(created)
	return a, err
}

// List returns every account, the first one first.
func (r *Registry) List(ctx context.Context) ([]Account, error) {
	rows, err := r.db.QueryContext(ctx, "SELECT "+columns+" FROM accounts ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		a, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r *Registry) Get(ctx context.Context, id int64) (Account, error) {
	a, err := scan(r.db.QueryRowContext(ctx, "SELECT "+columns+" FROM accounts WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

// Bootstrap makes the existing database the first account when there is none yet, with the
// password hash it kept until now.
func (r *Registry) Bootstrap(ctx context.Context, dbPath, name, hash string, now time.Time) (bool, error) {
	var n int
	if err := r.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM accounts").Scan(&n); err != nil || n > 0 {
		return false, err
	}
	_, err := r.db.ExecContext(ctx, "INSERT INTO accounts (id, name, db_path, password, created_at) VALUES (1, ?, ?, ?, ?)",
		name, dbPath, null(hash), ts(now))
	return err == nil, err
}

// Create adds an account with a database file of its own and returns it.
func (r *Registry) Create(ctx context.Context, name, hash string, now time.Time) (Account, error) {
	if err := os.MkdirAll(r.dir, 0o700); err != nil {
		return Account{}, err
	}
	var id int64
	err := r.db.QueryRowContext(ctx, "INSERT INTO accounts (name, db_path, password, created_at) VALUES (?, ?, ?, ?) RETURNING id",
		name, "pending:"+strconv.FormatInt(now.UnixNano(), 10), null(hash), ts(now)).Scan(&id)
	if err != nil {
		return Account{}, err
	}
	path := filepath.Join(r.dir, "account-"+strconv.FormatInt(id, 10)+".db")
	if _, err := r.db.ExecContext(ctx, "UPDATE accounts SET db_path = ? WHERE id = ?", path, id); err != nil {
		return Account{}, err
	}
	return r.Get(ctx, id)
}

func (r *Registry) SetPassword(ctx context.Context, id int64, hash string) error {
	return r.update(ctx, "UPDATE accounts SET password = ? WHERE id = ?", null(hash), id)
}

func (r *Registry) SetName(ctx context.Context, id int64, name string) error {
	return r.update(ctx, "UPDATE accounts SET name = ? WHERE id = ?", name, id)
}

func (r *Registry) SetHolder(ctx context.Context, id int64, holder string) error {
	return r.update(ctx, "UPDATE accounts SET holder = ? WHERE id = ?", null(holder), id)
}

func (r *Registry) update(ctx context.Context, q string, args ...any) error {
	res, err := r.db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete removes an account and its sessions; its database file is moved aside, not erased.
func (r *Registry) Delete(ctx context.Context, id int64, now time.Time) (string, error) {
	a, err := r.Get(ctx, id)
	if err != nil {
		return "", err
	}
	if a.Primary() {
		return "", errors.New("the first account cannot be deleted")
	}
	if _, err := r.db.ExecContext(ctx, "DELETE FROM accounts WHERE id = ?", id); err != nil {
		return "", err
	}
	kept := strings.TrimSuffix(a.DBPath, ".db") + "-deleted-" + now.Format("20060102-150405") + ".db"
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Rename(a.DBPath+suffix, kept+suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	return kept, nil
}

// ---- sessions ----

func (r *Registry) CreateSession(ctx context.Context, id string, accountID int64, agent string, now, expires time.Time) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO sessions (id, account_id, created_at, seen_at, expires_at, agent)
		VALUES (?, ?, ?, ?, ?, ?)`, id, accountID, ts(now), ts(now), ts(expires), null(agent))
	return err
}

// SessionAccount returns the account of a live session.
func (r *Registry) SessionAccount(ctx context.Context, id string, now time.Time) (int64, bool, error) {
	var acc int64
	var expires string
	err := r.db.QueryRowContext(ctx, "SELECT account_id, expires_at FROM sessions WHERE id = ?", id).Scan(&acc, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return acc, parse(expires).After(now), nil
}

// TouchSession starts the session's lifetime again: one in use stays alive.
func (r *Registry) TouchSession(ctx context.Context, id string, now, expires time.Time) error {
	_, err := r.db.ExecContext(ctx, "UPDATE sessions SET seen_at = ?, expires_at = ? WHERE id = ?", ts(now), ts(expires), id)
	return err
}

func (r *Registry) DeleteSession(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, "DELETE FROM sessions WHERE id = ?", id)
	return err
}

// DeleteSessions signs an account's browsers out, except keep.
func (r *Registry) DeleteSessions(ctx context.Context, accountID int64, keep string) error {
	_, err := r.db.ExecContext(ctx, "DELETE FROM sessions WHERE account_id = ? AND id != ?", accountID, keep)
	return err
}

// Sessions lists an account's live sessions, the latest used first; expired ones are removed.
func (r *Registry) Sessions(ctx context.Context, accountID int64, now time.Time) ([]Session, error) {
	if _, err := r.db.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at <= ?", ts(now)); err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id, account_id, created_at, seen_at, expires_at, COALESCE(agent, '')
		FROM sessions WHERE account_id = ? ORDER BY seen_at DESC`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var s Session
		var c, sn, e string
		if err := rows.Scan(&s.ID, &s.AccountID, &c, &sn, &e, &s.Agent); err != nil {
			return nil, err
		}
		s.CreatedAt, s.SeenAt, s.ExpiresAt = parse(c), parse(sn), parse(e)
		out = append(out, s)
	}
	return out, rows.Err()
}

// SameHolder compares two names from statements: case, spaces and "ё" do not matter.
func SameHolder(a, b string) bool {
	norm := func(s string) string {
		return strings.Join(strings.Fields(strings.ReplaceAll(strings.ToLower(s), "ё", "е")), " ")
	}
	return norm(a) == norm(b)
}
