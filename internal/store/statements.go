package store

import (
	"context"
	"encoding/json"
	"time"
)

// StatementInfo is an imported statement with the totals from its header.
type StatementInfo struct {
	Account    string
	From, To   time.Time // inclusive, as printed in the statement
	Summary    map[string]int64
	Ops        int
	ImportedAt time.Time
	// Opening and Closing are the card balances at the ends of the period; HasBalance is false
	// for statements that did not print them or were imported before they were read.
	Opening, Closing int64
	HasBalance       bool
}

// SaveStatement remembers a statement. A statement for the same period may be downloaded later
// with more operations on the last day; totals are kept from the version with no fewer operations.
func (s *Store) SaveStatement(ctx context.Context, si StatementInfo) error {
	sum, err := json.Marshal(si.Summary)
	if err != nil {
		return err
	}
	var open, closing any
	if si.HasBalance {
		open, closing = si.Opening, si.Closing
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO statements (account, period_from, period_to, summary, ops, imported_at, opening_minor, closing_minor)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(account, period_from, period_to) DO UPDATE SET
		summary = excluded.summary, ops = excluded.ops, imported_at = excluded.imported_at,
		opening_minor = COALESCE(excluded.opening_minor, statements.opening_minor),
		closing_minor = COALESCE(excluded.closing_minor, statements.closing_minor)
		WHERE excluded.ops >= statements.ops`,
		si.Account, si.From.Format("2006-01-02"), si.To.Format("2006-01-02"), string(sum), si.Ops, fmtTime(si.ImportedAt), open, closing)
	if err != nil {
		return err
	}
	// balances from a statement imported again fill in ones an earlier import did not read
	if si.HasBalance {
		_, err = s.db.ExecContext(ctx, `UPDATE statements SET opening_minor = COALESCE(opening_minor, ?), closing_minor = COALESCE(closing_minor, ?)
			WHERE account = ? AND period_from = ? AND period_to = ?`, si.Opening, si.Closing,
			si.Account, si.From.Format("2006-01-02"), si.To.Format("2006-01-02"))
	}
	return err
}

// Statements lists imported statements, newest first.
func (s *Store) Statements(ctx context.Context, loc *time.Location) ([]StatementInfo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT account, period_from, period_to, summary, ops, imported_at
		FROM statements ORDER BY period_to DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StatementInfo
	for rows.Next() {
		var si StatementInfo
		var from, to, sum, imported string
		if err := rows.Scan(&si.Account, &from, &to, &sum, &si.Ops, &imported); err != nil {
			return nil, err
		}
		si.From, _ = time.ParseInLocation("2006-01-02", from, loc)
		si.To, _ = time.ParseInLocation("2006-01-02", to, loc)
		si.ImportedAt = parseTime(imported)
		if err := json.Unmarshal([]byte(sum), &si.Summary); err != nil {
			return nil, err
		}
		out = append(out, si)
	}
	return out, rows.Err()
}

// SetKind sets the statement operation kind on a Wallet transaction it matched, so that
// reconciliation by kind counts it even though there is no separate statement row.
func (s *Store) SetKind(ctx context.Context, txID int64, kind string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE transactions SET kind = ? WHERE id = ?", nullString(kind), txID)
	return err
}
