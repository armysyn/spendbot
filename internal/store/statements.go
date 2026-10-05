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
}

// SaveStatement remembers a statement. A statement for the same period may be downloaded later
// with more operations on the last day; totals are kept from the version with no fewer operations.
func (s *Store) SaveStatement(ctx context.Context, si StatementInfo) error {
	sum, err := json.Marshal(si.Summary)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO statements (account, period_from, period_to, summary, ops, imported_at)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(account, period_from, period_to) DO UPDATE SET
		summary = excluded.summary, ops = excluded.ops, imported_at = excluded.imported_at
		WHERE excluded.ops >= statements.ops`,
		si.Account, si.From.Format("2006-01-02"), si.To.Format("2006-01-02"), string(sum), si.Ops, fmtTime(si.ImportedAt))
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
