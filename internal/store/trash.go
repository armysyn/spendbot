package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Deleting operations. A deleted operation goes to the trash whole — its parts and the keys of
// the statement operations it came from — so it can be restored, a statement imported again
// does not bring it back, and the statement still reconciles with its totals.

// StatusDeleted marks ledger rows of deleted operations (TrashLedger); no live operation has it.
const StatusDeleted = "deleted"

// Trashed is a deleted operation.
type Trashed struct {
	ID         int64 // in the trash
	Tx               // as it was; Tx.ID is the id it had
	Categories []string
	DeletedAt  time.Time
}

// TrashTxs deletes operations into the trash and returns the trash ids, for undo. Ids that do
// not exist are skipped.
func (s *Store) TrashTxs(ctx context.Context, ids []int64, now time.Time) ([]int64, error) {
	var out []int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			t, err := scanTx(tx.QueryRowContext(ctx, "SELECT "+txColumns+" FROM transactions t WHERE t.id = ?", id))
			if errors.Is(err, sql.ErrNoRows) {
				continue
			} else if err != nil {
				return err
			}
			res, err := tx.ExecContext(ctx, `INSERT INTO trash (tx_id, external_key, occurred_at, amount_minor, currency,
				amount_raw, merchant_raw, merchant_norm, card, source, status, note, kind, created_at, deleted_at)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				t.ID, nullString(t.ExternalKey), fmtTime(t.OccurredAt), t.AmountMinor, t.Currency, nullString(t.AmountRaw),
				nullString(t.MerchantRaw), nullString(t.MerchantNorm), nullString(t.Card), t.Source, t.Status,
				nullString(t.Note), nullString(t.Kind), fmtTime(t.CreatedAt), fmtTime(now))
			if err != nil {
				return err
			}
			tid, err := res.LastInsertId()
			if err != nil {
				return err
			}
			for _, q := range []string{
				`INSERT INTO trash_splits (trash_id, category_id, amount_minor, note)
					SELECT ?, category_id, amount_minor, note FROM splits WHERE tx_id = ? ORDER BY id`,
				`INSERT INTO trash_links (op_key, trash_id) SELECT op_key, ? FROM statement_links WHERE tx_id = ?`,
			} {
				if _, err := tx.ExecContext(ctx, q, tid, id); err != nil {
					return err
				}
			}
			// parts, questions and statement links go with it (ON DELETE CASCADE); a trigger
			// tells ClickHouse
			if _, err := tx.ExecContext(ctx, "DELETE FROM transactions WHERE id = ?", id); err != nil {
				return err
			}
			out = append(out, tid)
		}
		return nil
	})
	return out, err
}

// RestoreTrash brings deleted operations back with their parts and statement links, under new
// ids (ClickHouse has already deleted the old ones), and returns how many came back.
func (s *Store) RestoreTrash(ctx context.Context, ids []int64) (int, error) {
	n := 0
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		for _, tid := range ids {
			res, err := tx.ExecContext(ctx, `INSERT INTO transactions (external_key, occurred_at, amount_minor, currency,
				amount_raw, merchant_raw, merchant_norm, card, source, status, note, kind, created_at)
				SELECT external_key, occurred_at, amount_minor, currency, amount_raw, merchant_raw, merchant_norm,
					card, source, status, note, kind, created_at FROM trash WHERE id = ?`, tid)
			if err != nil {
				return err
			}
			if k, _ := res.RowsAffected(); k == 0 {
				continue
			}
			id, err := res.LastInsertId()
			if err != nil {
				return err
			}
			for _, q := range []string{
				`INSERT INTO splits (tx_id, category_id, amount_minor, note)
					SELECT ?, category_id, amount_minor, note FROM trash_splits WHERE trash_id = ? ORDER BY id`,
				`INSERT OR IGNORE INTO statement_links (op_key, tx_id) SELECT op_key, ? FROM trash_links WHERE trash_id = ?`,
			} {
				if _, err := tx.ExecContext(ctx, q, id, tid); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, "DELETE FROM trash WHERE id = ?", tid); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}

// Trash lists deleted operations, the last deleted first.
func (s *Store) Trash(ctx context.Context, loc *time.Location) ([]Trashed, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.id, t.tx_id, COALESCE(t.external_key, ''), t.occurred_at, t.amount_minor,
			t.currency, COALESCE(t.amount_raw, ''), COALESCE(t.merchant_raw, ''), COALESCE(t.merchant_norm, ''),
			COALESCE(t.card, ''), t.source, t.status, COALESCE(t.note, ''), COALESCE(t.kind, ''), t.created_at, t.deleted_at,
			COALESCE((SELECT group_concat(c.name, '|') FROM trash_splits sp JOIN categories c ON c.id = sp.category_id
				WHERE sp.trash_id = t.id), '')
		FROM trash t ORDER BY t.deleted_at DESC, t.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Trashed
	for rows.Next() {
		var r Trashed
		var occurred, created, deleted, cats string
		if err := rows.Scan(&r.ID, &r.Tx.ID, &r.ExternalKey, &occurred, &r.AmountMinor, &r.Currency, &r.AmountRaw,
			&r.MerchantRaw, &r.MerchantNorm, &r.Card, &r.Source, &r.Status, &r.Note, &r.Kind, &created, &deleted, &cats); err != nil {
			return nil, err
		}
		r.OccurredAt, r.CreatedAt, r.DeletedAt = parseTime(occurred).In(loc), parseTime(created).In(loc), parseTime(deleted).In(loc)
		if cats != "" {
			r.Categories = strings.Split(cats, "|")
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TrashLedger returns deleted statement operations as ledger rows with StatusDeleted: the
// bank counted them, so reconciliation with statement totals still does.
func (s *Store) TrashLedger(ctx context.Context, loc *time.Location) ([]LedgerRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.tx_id, t.occurred_at, t.amount_minor, t.currency,
			COALESCE(t.merchant_raw, ''), COALESCE(t.merchant_norm, ''), COALESCE(t.kind, ''), t.source
		FROM trash t WHERE EXISTS (SELECT 1 FROM trash_links l WHERE l.trash_id = t.id) ORDER BY t.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LedgerRow
	for rows.Next() {
		r := LedgerRow{Status: StatusDeleted}
		var at string
		if err := rows.Scan(&r.TxID, &at, &r.Amount, &r.Currency, &r.Merchant, &r.MerchantNorm, &r.Kind, &r.Source); err != nil {
			return nil, err
		}
		r.At = parseTime(at).In(loc)
		out = append(out, r)
	}
	return out, rows.Err()
}

// OpTrashed reports whether a statement operation with this key was deleted.
func (s *Store) OpTrashed(ctx context.Context, key string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM trash_links WHERE op_key = ?", key).Scan(&n)
	return n > 0, err
}

// LinkTrashedOp remembers one more statement key of a deleted operation (the statement was
// downloaded again and the operation got another key).
func (s *Store) LinkTrashedOp(ctx context.Context, key string, trashID int64) error {
	_, err := s.db.ExecContext(ctx, "INSERT OR IGNORE INTO trash_links (op_key, trash_id) VALUES (?, ?)", key, trashID)
	return err
}

// keyTrashed reports whether an operation with this external key was deleted.
func keyTrashed(ctx context.Context, q *sql.Tx, key string) (bool, error) {
	if key == "" {
		return false, nil
	}
	var n int
	err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM trash WHERE external_key = ?", key).Scan(&n)
	return n > 0, err
}

// TrashCount is the number of deleted operations.
func (s *Store) TrashCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM trash").Scan(&n)
	return n, err
}
