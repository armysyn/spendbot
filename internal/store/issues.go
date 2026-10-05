package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Issue kinds, statuses and priorities.
const (
	IssueBug  = "bug"
	IssueIdea = "idea"

	IssueOpen   = "open"
	IssueClosed = "closed"

	PriorityLow    = "low"
	PriorityNormal = "normal"
	PriorityHigh   = "high"
)

// Issue is a problem or a wish about spendbot, kept locally.
type Issue struct {
	ID           int64
	Kind         string
	Title        string
	Body         string
	Status       string
	Priority     string
	Page         string
	GitHub       int64 // the GitHub issue number once copied there; 0 — not yet
	CreatedAt    time.Time
	UpdatedAt    time.Time
	ClosedAt     *time.Time
	CommentCount int
}

type IssueComment struct {
	ID        int64
	Body      string
	CreatedAt time.Time
}

// IssueFilter selects issues; empty fields match everything.
type IssueFilter struct {
	Status   string
	Kind     string
	Priority string
	Query    string // words in the title or the text, case-insensitive
}

const issueColumns = `i.id, i.kind, i.title, i.body, i.status, i.priority, COALESCE(i.page, ''), COALESCE(i.github_number, 0),
	i.created_at, i.updated_at, i.closed_at, (SELECT COUNT(*) FROM issue_comments c WHERE c.issue_id = i.id)`

func scanIssue(r scanner) (Issue, error) {
	var is Issue
	var created, updated string
	var closed sql.NullString
	err := r.Scan(&is.ID, &is.Kind, &is.Title, &is.Body, &is.Status, &is.Priority, &is.Page, &is.GitHub,
		&created, &updated, &closed, &is.CommentCount)
	is.CreatedAt, is.UpdatedAt, is.ClosedAt = parseTime(created), parseTime(updated), nullTime(closed)
	return is, err
}

// Issues lists issues, high priority and newest first. The text search runs in Go:
// SQLite lower() does not know Cyrillic.
func (s *Store) Issues(ctx context.Context, f IssueFilter) ([]Issue, error) {
	q := "SELECT " + issueColumns + " FROM issues i WHERE 1 = 1"
	var args []any
	for col, v := range map[string]string{"i.status": f.Status, "i.kind": f.Kind, "i.priority": f.Priority} {
		if v != "" {
			q += " AND " + col + " = ?"
			args = append(args, v)
		}
	}
	q += " ORDER BY CASE i.priority WHEN 'high' THEN 0 WHEN 'normal' THEN 1 ELSE 2 END, i.id DESC"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	terms := strings.Fields(strings.ToLower(f.Query))
	var out []Issue
	for rows.Next() {
		is, err := scanIssue(rows)
		if err != nil {
			return nil, err
		}
		hay := strings.ToLower(is.Title + " " + is.Body)
		match := true
		for _, t := range terms {
			match = match && strings.Contains(hay, t)
		}
		if match {
			out = append(out, is)
		}
	}
	return out, rows.Err()
}

// IssueCounts counts issues by status.
func (s *Store) IssueCounts(ctx context.Context) (open, closed int, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(status = 'open'), 0), COALESCE(SUM(status = 'closed'), 0) FROM issues`).
		Scan(&open, &closed)
	return open, closed, err
}

func (s *Store) Issue(ctx context.Context, id int64) (Issue, error) {
	is, err := scanIssue(s.db.QueryRowContext(ctx, "SELECT "+issueColumns+" FROM issues i WHERE i.id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return is, ErrNotFound
	}
	return is, err
}

// SaveIssue creates an issue (ID 0) or updates its kind, title, text, priority and page.
func (s *Store) SaveIssue(ctx context.Context, is Issue, now time.Time) (int64, error) {
	if is.ID == 0 {
		var id int64
		err := s.db.QueryRowContext(ctx, `INSERT INTO issues (kind, title, body, priority, page, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING id`, is.Kind, is.Title, is.Body, is.Priority, nullString(is.Page),
			fmtTime(now), fmtTime(now)).Scan(&id)
		return id, err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE issues SET kind = ?, title = ?, body = ?, priority = ?, page = ?, updated_at = ?
		WHERE id = ?`, is.Kind, is.Title, is.Body, is.Priority, nullString(is.Page), fmtTime(now), is.ID)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotFound
	}
	return is.ID, nil
}

// SetIssueStatus closes or reopens an issue.
func (s *Store) SetIssueStatus(ctx context.Context, id int64, status string, now time.Time) error {
	var closed any
	if status == IssueClosed {
		closed = fmtTime(now)
	}
	_, err := s.db.ExecContext(ctx, "UPDATE issues SET status = ?, closed_at = ?, updated_at = ? WHERE id = ?",
		status, closed, fmtTime(now), id)
	return err
}

func (s *Store) DeleteIssue(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM issues WHERE id = ?", id)
	return err
}

// AddIssueComment adds a comment and touches the issue.
func (s *Store) AddIssueComment(ctx context.Context, id int64, body string, now time.Time) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "INSERT INTO issue_comments (issue_id, body, created_at) VALUES (?, ?, ?)",
			id, body, fmtTime(now)); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "UPDATE issues SET updated_at = ? WHERE id = ?", fmtTime(now), id)
		return err
	})
}

func (s *Store) IssueComments(ctx context.Context, id int64) ([]IssueComment, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, body, created_at FROM issue_comments WHERE issue_id = ? ORDER BY id", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IssueComment
	for rows.Next() {
		var c IssueComment
		var at string
		if err := rows.Scan(&c.ID, &c.Body, &at); err != nil {
			return nil, err
		}
		c.CreatedAt = parseTime(at)
		out = append(out, c)
	}
	return out, rows.Err()
}
