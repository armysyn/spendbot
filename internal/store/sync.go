package store

import (
	"context"
	"time"
)

// SyncCursor is the sync position: (updated_at, id) of the last transaction sent.
type SyncCursor struct {
	UpdatedAt string
	ID        int64
}

// SyncRow is a transaction with its parts for analytics.
type SyncRow struct {
	Tx
	Categories   []string
	SplitAmounts []int64
}

// ChangedTxs returns transactions changed after cursor but before settled. The settled bound
// (a couple of seconds in the past) is needed because updated_at has one-second precision:
// rows of the current second may still change and the cursor would skip them.
func (s *Store) ChangedTxs(ctx context.Context, cur SyncCursor, settled time.Time, limit int) ([]SyncRow, SyncCursor, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+txColumns+`, t.updated_at FROM transactions t
		WHERE (t.updated_at > ? OR (t.updated_at = ? AND t.id > ?)) AND t.updated_at < ?
		ORDER BY t.updated_at, t.id LIMIT ?`, cur.UpdatedAt, cur.UpdatedAt, cur.ID, fmtTime(settled), limit)
	if err != nil {
		return nil, cur, err
	}
	var out []SyncRow
	for rows.Next() {
		var r SyncRow
		var occurred, created, updated string
		t := &r.Tx
		if err := rows.Scan(&t.ID, &t.ExternalKey, &occurred, &t.AmountMinor, &t.Currency, &t.AmountRaw,
			&t.MerchantRaw, &t.MerchantNorm, &t.Card, &t.Source, &t.Status, &t.Note, &t.Kind, &created, &updated); err != nil {
			rows.Close()
			return nil, cur, err
		}
		t.OccurredAt, t.CreatedAt = parseTime(occurred), parseTime(created)
		out = append(out, r)
		cur = SyncCursor{UpdatedAt: updated, ID: t.ID}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, cur, err
	}
	for i := range out {
		srows, err := s.db.QueryContext(ctx, `SELECT c.name, s.amount_minor FROM splits s
			JOIN categories c ON c.id = s.category_id WHERE s.tx_id = ? ORDER BY s.id`, out[i].ID)
		if err != nil {
			return nil, cur, err
		}
		for srows.Next() {
			var name string
			var amount int64
			if err := srows.Scan(&name, &amount); err != nil {
				srows.Close()
				return nil, cur, err
			}
			out[i].Categories = append(out[i].Categories, name)
			out[i].SplitAmounts = append(out[i].SplitAmounts, amount)
		}
		srows.Close()
	}
	return out, cur, nil
}

// DeletedSince returns transactions deleted after since (by deleted_at, before settled).
func (s *Store) DeletedSince(ctx context.Context, since string, settled time.Time) ([]int64, string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT tx_id, deleted_at FROM deleted_txs
		WHERE deleted_at > ? AND deleted_at < ? ORDER BY deleted_at`, since, fmtTime(settled))
	if err != nil {
		return nil, since, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id, &since); err != nil {
			return nil, since, err
		}
		ids = append(ids, id)
	}
	return ids, since, rows.Err()
}
