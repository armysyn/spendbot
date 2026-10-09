package store

import (
	"context"
	"database/sql"
	"time"
)

// LedgerRow is an operation for analytics: one row per transaction with its parts.
// ClickHouse returns the same shape, so one computation serves both sources.
type LedgerRow struct {
	TxID         int64
	At           time.Time // in the report time zone
	Amount       int64     // spending is positive, income negative
	Currency     string
	Merchant     string // as in the statement or Wallet
	MerchantNorm string
	Kind         string
	Status       string
	Source       string
	Categories   []string // part categories in order; empty — uncategorized
	Splits       []int64  // part amounts
	// PassThrough is the part of the amount, positive, that only passed through the card as
	// cash (analytics.MarkPassThrough): a whole withdrawal, or what of a top-up paid for one.
	PassThrough int64
}

// Ledger returns all operations from SQLite. Personal finance means tens of thousands of
// rows: computing in memory is faster and simpler than maintaining a second SQL dialect.
func (s *Store) Ledger(ctx context.Context, loc *time.Location) ([]LedgerRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.id, t.occurred_at, t.amount_minor, t.currency,
			COALESCE(t.merchant_raw, ''), COALESCE(t.merchant_norm, ''), COALESCE(t.kind, ''), t.status, t.source,
			c.name, sp.amount_minor
		FROM transactions t
		LEFT JOIN splits sp ON sp.tx_id = t.id
		LEFT JOIN categories c ON c.id = sp.category_id
		ORDER BY t.id, sp.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LedgerRow
	for rows.Next() {
		var r LedgerRow
		var at string
		var cat sql.NullString
		var split sql.NullInt64
		if err := rows.Scan(&r.TxID, &at, &r.Amount, &r.Currency, &r.Merchant, &r.MerchantNorm, &r.Kind,
			&r.Status, &r.Source, &cat, &split); err != nil {
			return nil, err
		}
		if n := len(out); n > 0 && out[n-1].TxID == r.TxID {
			out[n-1].Categories = append(out[n-1].Categories, cat.String)
			out[n-1].Splits = append(out[n-1].Splits, split.Int64)
			continue
		}
		r.At = parseTime(at).In(loc)
		if cat.Valid {
			r.Categories, r.Splits = []string{cat.String}, []int64{split.Int64}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
