package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// OpLinked reports whether a statement operation with this key was already imported.
func (s *Store) OpLinked(ctx context.Context, key string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM statement_links WHERE op_key = ?", key).Scan(&n)
	return n > 0, err
}

func (s *Store) LinkOp(ctx context.Context, key string, txID int64) error {
	_, err := s.db.ExecContext(ctx, "INSERT OR IGNORE INTO statement_links (op_key, tx_id) VALUES (?, ?)", key, txID)
	return err
}

// FindUnlinkedTx finds a Wallet or manual transaction with the same amount in [from, to)
// that is not linked to any statement operation yet, so a purchase that came from Wallet
// is not counted twice when the statement is imported.
func (s *Store) FindUnlinkedTx(ctx context.Context, amountMinor int64, currency string, from, to time.Time) (int64, bool, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT t.id FROM transactions t
		WHERE t.source IN ('wallet', 'manual') AND t.amount_minor = ? AND t.currency = ?
		  AND t.occurred_at >= ? AND t.occurred_at < ?
		  AND NOT EXISTS (SELECT 1 FROM statement_links l WHERE l.tx_id = t.id)
		ORDER BY t.occurred_at LIMIT 1`, amountMinor, currency, fmtTime(from), fmtTime(to)).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return id, err == nil, err
}

// Twin is an imported operation found by content: a live one (ID) or a deleted one (TrashID).
type Twin struct {
	ID, TrashID int64
}

// ImportTwins lists already imported operations with the same day, amount, details and kind,
// deleted ones included, in import order. They identify an operation on re-import even when its
// key changed (a newer version of the statement got more operations on the same day).
func (s *Store) ImportTwins(ctx context.Context, from, to time.Time, amountMinor int64, details, kind string) ([]Twin, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, 0, id FROM transactions
		WHERE source = 'import' AND occurred_at >= ? AND occurred_at < ? AND amount_minor = ?
		  AND COALESCE(merchant_raw, '') = ? AND COALESCE(kind, '') = ?
		UNION ALL
		SELECT 0, id, tx_id FROM trash
		WHERE source = 'import' AND occurred_at >= ? AND occurred_at < ? AND amount_minor = ?
		  AND COALESCE(merchant_raw, '') = ? AND COALESCE(kind, '') = ?
		ORDER BY 3`, fmtTime(from), fmtTime(to), amountMinor, details, kind, fmtTime(from), fmtTime(to), amountMinor, details, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Twin
	for rows.Next() {
		var t Twin
		var order int64
		if err := rows.Scan(&t.ID, &t.TrashID, &order); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
