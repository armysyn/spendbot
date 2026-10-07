package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"spendbot/internal/kaspi"
)

// Editing operations and categories from the web page.

// UncategorizeTx makes an operation not spending: its parts are removed, a transfer goes
// back to plain information, anything else is marked skipped.
func (s *Store) UncategorizeTx(ctx context.Context, id int64) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var kind string
		err := tx.QueryRowContext(ctx, "SELECT COALESCE(kind, '') FROM transactions WHERE id = ?", id).Scan(&kind)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		status := StatusIgnored
		if kind == kaspi.Transfer {
			status = StatusInfo
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM splits WHERE tx_id = ?", id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM questions WHERE tx_id = ?", id); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE transactions SET status = ? WHERE id = ?", status, id)
		return err
	})
}

// RecategorizeMerchant moves every purchase of a merchant into one category — answered,
// waiting for an answer or skipped — and remembers the merchant with minHits so new
// payments follow. Purchases split between several categories keep their parts.
func (s *Store) RecategorizeMerchant(ctx context.Context, norm string, categoryID int64, minHits int, now time.Time) (int, error) {
	var n int
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT t.id FROM transactions t
			WHERE t.merchant_norm = ? AND COALESCE(t.kind, '') IN ('', ?)
			  AND t.status IN (?, ?, ?, ?, ?) AND t.amount_minor != 0
			  AND (SELECT COUNT(DISTINCT category_id) FROM splits WHERE tx_id = t.id) <= 1`,
			norm, kaspi.Purchase, StatusDone, StatusReview, StatusPending, StatusAsked, StatusIgnored)
		if err != nil {
			return err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range ids {
			for _, q := range []struct {
				sql  string
				args []any
			}{
				{"DELETE FROM splits WHERE tx_id = ?", []any{id}},
				{"DELETE FROM questions WHERE tx_id = ?", []any{id}},
				{"INSERT INTO splits (tx_id, category_id, amount_minor) SELECT id, ?, amount_minor FROM transactions WHERE id = ?", []any{categoryID, id}},
				{"UPDATE transactions SET status = ? WHERE id = ?", []any{StatusDone, id}},
			} {
				if _, err := tx.ExecContext(ctx, q.sql, q.args...); err != nil {
					return err
				}
			}
		}
		n = len(ids)
		if _, err := tx.ExecContext(ctx, `INSERT INTO merchant_rules (merchant_norm, category_id, hits, updated_at)
			VALUES (?, ?, ?, ?) ON CONFLICT(merchant_norm) DO UPDATE SET category_id = excluded.category_id,
			hits = MAX(merchant_rules.hits, excluded.hits), updated_at = excluded.updated_at`,
			norm, categoryID, minHits, fmtTime(now)); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE merchant_questions SET status = 'answered', answered_at = ?
			WHERE merchant_norm = ? AND status = 'open'`, fmtTime(now), norm)
		return err
	})
	return n, err
}

// AllCategories lists categories including archived ones, alphabetically.
func (s *Store) AllCategories(ctx context.Context) ([]Category, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, name, archived, savings, debt FROM categories ORDER BY archived, name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Category
	for rows.Next() {
		var c Category
		if err := rows.Scan(&c.ID, &c.Name, &c.Archived, &c.Savings, &c.Debt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// RestoreCategory brings an archived category back.
func (s *Store) RestoreCategory(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE categories SET archived = 0 WHERE id = ?", id)
	return err
}

// CategoryRules counts remembered merchants per category.
func (s *Store) CategoryRules(ctx context.Context) (map[int64]int, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT category_id, COUNT(*) FROM merchant_rules GROUP BY category_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int{}
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// MergeCategory moves everything from one category into another — operation parts,
// merchant memory, guesses — and deletes the emptied category.
func (s *Store) MergeCategory(ctx context.Context, from, into int64) error {
	if from == into {
		return errors.New("a category cannot be merged into itself")
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, q := range []struct {
			sql  string
			args []any
		}{
			// splits updates have no trigger: touch the transactions so ClickHouse picks them up
			{`UPDATE transactions SET updated_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
				WHERE id IN (SELECT tx_id FROM splits WHERE category_id = ?)`, []any{from}},
			{"UPDATE splits SET category_id = ? WHERE category_id = ?", []any{into, from}},
			{"UPDATE merchant_rules SET category_id = ? WHERE category_id = ?", []any{into, from}},
			{"UPDATE merchant_questions SET guess_category_id = ? WHERE guess_category_id = ?", []any{into, from}},
			{"DELETE FROM categories WHERE id = ?", []any{from}},
		} {
			if _, err := tx.ExecContext(ctx, q.sql, q.args...); err != nil {
				return err
			}
		}
		return nil
	})
}
