package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

type Category struct {
	ID       int64
	Name     string
	Archived bool
	Savings  bool // savings: can be excluded from spending on the analytics page
}

type Rule struct {
	MerchantNorm string
	CategoryID   int64
	Hits         int
}

// Categories lists active categories alphabetically.
func (s *Store) Categories(ctx context.Context) ([]Category, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, name, archived, savings FROM categories WHERE archived = 0 ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Category
	for rows.Next() {
		var c Category
		if err := rows.Scan(&c.ID, &c.Name, &c.Archived, &c.Savings); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) Category(ctx context.Context, id int64) (Category, error) {
	var c Category
	err := s.db.QueryRowContext(ctx, "SELECT id, name, archived, savings FROM categories WHERE id = ?", id).
		Scan(&c.ID, &c.Name, &c.Archived, &c.Savings)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// FindCategory finds an active category case-insensitively. SQLite lower() does not
// know Cyrillic, so the comparison is done in Go.
func (s *Store) FindCategory(ctx context.Context, name string) (Category, error) {
	cats, err := s.Categories(ctx)
	if err != nil {
		return Category{}, err
	}
	for _, c := range cats {
		if strings.EqualFold(c.Name, strings.TrimSpace(name)) {
			return c, nil
		}
	}
	return Category{}, ErrNotFound
}

// AddCategory creates a category or restores an archived one with the same name.
func (s *Store) AddCategory(ctx context.Context, name string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `INSERT INTO categories (name) VALUES (?)
		ON CONFLICT(name) DO UPDATE SET archived = 0 RETURNING id`, name).Scan(&id)
	return id, err
}

func (s *Store) RenameCategory(ctx context.Context, id int64, name string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE categories SET name = ? WHERE id = ?", name, id)
	return err
}

// ArchiveCategory hides a category from buttons and forgets merchant rules pointing to it.
// Old transactions keep the category.
func (s *Store) ArchiveCategory(ctx context.Context, id int64) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE categories SET archived = 1 WHERE id = ?", id); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM merchant_rules WHERE category_id = ?", id)
		return err
	})
}

// TopCategories returns the n most used active categories since the given time.
func (s *Store) TopCategories(ctx context.Context, n int, since time.Time) ([]Category, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.id, c.name, c.archived, c.savings FROM splits s
		JOIN transactions t ON t.id = s.tx_id JOIN categories c ON c.id = s.category_id
		WHERE c.archived = 0 AND t.occurred_at >= ?
		GROUP BY c.id ORDER BY COUNT(*) DESC, c.name LIMIT ?`, fmtTime(since), n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Category
	for rows.Next() {
		var c Category
		if err := rows.Scan(&c.ID, &c.Name, &c.Archived, &c.Savings); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Rule returns the memory rule for a normalized merchant name.
func (s *Store) Rule(ctx context.Context, norm string) (Rule, bool, error) {
	return ruleTx(ctx, s.db, norm)
}

type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func ruleTx(ctx context.Context, q rowQuerier, norm string) (Rule, bool, error) {
	r := Rule{MerchantNorm: norm}
	err := q.QueryRowContext(ctx, `SELECT r.category_id, r.hits FROM merchant_rules r
		JOIN categories c ON c.id = r.category_id WHERE r.merchant_norm = ? AND c.archived = 0`, norm).
		Scan(&r.CategoryID, &r.Hits)
	if errors.Is(err, sql.ErrNoRows) {
		return r, false, nil
	}
	return r, err == nil, err
}

// SetSavings marks exactly the categories in ids as savings.
func (s *Store) SetSavings(ctx context.Context, ids []int64) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE categories SET savings = 0"); err != nil {
			return err
		}
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, "UPDATE categories SET savings = 1 WHERE id = ?", id); err != nil {
				return err
			}
		}
		return nil
	})
}

// SavingsNames lists savings categories, archived ones included: old transactions stay in them.
func (s *Store) SavingsNames(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name FROM categories WHERE savings = 1 ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
