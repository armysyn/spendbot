package store

import (
	"context"
	"database/sql"
	"time"

	"spendbot/internal/kaspi"
)

// Counterparty is someone money was sent to or received from. Kaspi statements show only
// a first name and the initial of the surname ("Aigerim A."), so namesakes with the same
// initial are merged into one row.
type Counterparty struct {
	Name        string
	Category    string // transfers to them count as spending in this category; empty — not spending
	Sent        int64  // sent (transfers), tiyn
	SentN       int
	Received    int64 // received (top-ups), tiyn
	ReceivedN   int
	First, Last time.Time
}

// Counterparties lists all transfer recipients and top-up senders by turnover.
func (s *Store) Counterparties(ctx context.Context) ([]Counterparty, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT g.name, g.sent, g.sent_n, g.received, g.received_n, g.first, g.last,
			COALESCE(c.name, '')
		FROM (SELECT merchant_raw AS name,
				SUM(CASE WHEN kind = ?1 THEN amount_minor ELSE 0 END) AS sent,
				SUM(CASE WHEN kind = ?1 THEN 1 ELSE 0 END) AS sent_n,
				-SUM(CASE WHEN kind = ?2 THEN amount_minor ELSE 0 END) AS received,
				SUM(CASE WHEN kind = ?2 THEN 1 ELSE 0 END) AS received_n,
				MIN(occurred_at) AS first, MAX(occurred_at) AS last,
				MAX(merchant_norm) AS norm, SUM(ABS(amount_minor)) AS turnover
			FROM transactions
			WHERE kind IN (?1, ?2) AND currency = 'KZT' AND COALESCE(merchant_raw, '') != ''
			GROUP BY merchant_raw) g
		LEFT JOIN merchant_rules r ON r.merchant_norm = g.norm
		LEFT JOIN categories c ON c.id = r.category_id
		ORDER BY g.turnover DESC`, kaspi.Transfer, kaspi.TopUp)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Counterparty
	for rows.Next() {
		var c Counterparty
		var first, last string
		if err := rows.Scan(&c.Name, &c.Sent, &c.SentN, &c.Received, &c.ReceivedN, &first, &last, &c.Category); err != nil {
			return nil, err
		}
		c.First, c.Last = parseTime(first), parseTime(last)
		out = append(out, c)
	}
	return out, rows.Err()
}

// CounterpartyOps lists all transfers to a person and top-ups from them, newest first.
func (s *Store) CounterpartyOps(ctx context.Context, name string) ([]Tx, error) {
	return s.queryTxs(ctx, "SELECT "+txColumns+` FROM transactions t
		WHERE t.kind IN (?, ?) AND t.merchant_raw = ? ORDER BY t.occurred_at DESC, t.id DESC`,
		kaspi.Transfer, kaspi.TopUp, name)
}

// CategorizeTransfers makes transfers to a person spending in a category (rent, say):
// all their transfers get the category, and a rule by name marks transfers from future
// statements. Top-ups from them are left alone.
func (s *Store) CategorizeTransfers(ctx context.Context, name, norm string, categoryID int64, minHits int, now time.Time) (int, error) {
	var n int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM splits WHERE tx_id IN
			(SELECT id FROM transactions WHERE kind = ? AND merchant_raw = ?)`, kaspi.Transfer, name); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO splits (tx_id, category_id, amount_minor)
			SELECT id, ?, amount_minor FROM transactions WHERE kind = ? AND merchant_raw = ?`,
			categoryID, kaspi.Transfer, name); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, "UPDATE transactions SET status = 'done' WHERE kind = ? AND merchant_raw = ?",
			kaspi.Transfer, name)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		_, err = tx.ExecContext(ctx, `INSERT INTO merchant_rules (merchant_norm, category_id, hits, updated_at)
			VALUES (?, ?, ?, ?) ON CONFLICT(merchant_norm) DO UPDATE SET category_id = excluded.category_id,
			hits = excluded.hits, updated_at = excluded.updated_at`, norm, categoryID, minHits, fmtTime(now))
		return err
	})
	return int(n), err
}

// UncategorizeTransfers turns transfers to a person back into non-spending and forgets the rule.
func (s *Store) UncategorizeTransfers(ctx context.Context, name, norm string) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM splits WHERE tx_id IN
			(SELECT id FROM transactions WHERE kind = ? AND merchant_raw = ?)`, kaspi.Transfer, name); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE transactions SET status = 'info' WHERE kind = ? AND merchant_raw = ?",
			kaspi.Transfer, name); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM merchant_rules WHERE merchant_norm = ?", norm)
		return err
	})
}
