package store

import (
	"context"
	"time"
)

// CategoryTotal is the sum for a category in one currency over a period.
type CategoryTotal struct {
	Category string // empty — uncategorized spending
	Currency string
	Minor    int64
	Count    int
}

// Totals sums spending by category over [from, to). Uncategorized (pending/asked/review)
// spending comes as rows with an empty category; skipped (ignored) and non-spending (info)
// operations are left out. SQL does the math, not a model.
func (s *Store) Totals(ctx context.Context, from, to time.Time) ([]CategoryTotal, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT c.name, t.currency, SUM(s.amount_minor), COUNT(DISTINCT t.id)
		FROM transactions t JOIN splits s ON s.tx_id = t.id JOIN categories c ON c.id = s.category_id
		WHERE t.status = 'done' AND t.occurred_at >= ? AND t.occurred_at < ?
		GROUP BY c.name, t.currency
		UNION ALL
		SELECT '', t.currency, SUM(t.amount_minor), COUNT(*)
		FROM transactions t
		WHERE t.status IN ('pending', 'asked', 'review') AND t.occurred_at >= ? AND t.occurred_at < ?
		GROUP BY t.currency
		ORDER BY 3 DESC`, fmtTime(from), fmtTime(to), fmtTime(from), fmtTime(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CategoryTotal
	for rows.Next() {
		var ct CategoryTotal
		if err := rows.Scan(&ct.Category, &ct.Currency, &ct.Minor, &ct.Count); err != nil {
			return nil, err
		}
		out = append(out, ct)
	}
	return out, rows.Err()
}

// ExportRow is a CSV row: one spending part (an uncategorized transaction has one row without a category).
type ExportRow struct {
	Tx          Tx
	Category    string
	AmountMinor int64
	SplitNote   string
}

// ExportRows returns all spending over [from, to) except skipped, one row per part.
func (s *Store) ExportRows(ctx context.Context, from, to time.Time) ([]ExportRow, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+txColumns+`,
			COALESCE(c.name, ''), COALESCE(s.amount_minor, t.amount_minor), COALESCE(s.note, '')
		FROM transactions t
		LEFT JOIN splits s ON s.tx_id = t.id LEFT JOIN categories c ON c.id = s.category_id
		WHERE t.status NOT IN ('ignored', 'info') AND t.occurred_at >= ? AND t.occurred_at < ?
		ORDER BY t.occurred_at, t.id, s.id`, fmtTime(from), fmtTime(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExportRow
	for rows.Next() {
		var r ExportRow
		var occurred, created string
		t := &r.Tx
		if err := rows.Scan(&t.ID, &t.ExternalKey, &occurred, &t.AmountMinor, &t.Currency, &t.AmountRaw,
			&t.MerchantRaw, &t.MerchantNorm, &t.Card, &t.Source, &t.Status, &t.Note, &t.Kind, &created,
			&r.Category, &r.AmountMinor, &r.SplitNote); err != nil {
			return nil, err
		}
		t.OccurredAt, t.CreatedAt = parseTime(occurred), parseTime(created)
		out = append(out, r)
	}
	return out, rows.Err()
}
