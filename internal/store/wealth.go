package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Holding kinds. Debts are the last four.
var (
	AssetKinds = []string{"cash", "deposit", "investment", "property", "vehicle", "asset"}
	DebtKinds  = []string{"loan", "mortgage", "credit", "debt"}
)

// IsDebtKind reports a kind that is owed, not owned.
func IsDebtKind(k string) bool {
	for _, d := range DebtKinds {
		if d == k {
			return true
		}
	}
	return false
}

// ValidKind reports a known holding kind.
func ValidKind(k string) bool {
	if IsDebtKind(k) {
		return true
	}
	for _, a := range AssetKinds {
		if a == k {
			return true
		}
	}
	return false
}

// Holding is something owned or owed, with its values over time, oldest first.
type Holding struct {
	ID        int64
	Name      string
	Kind      string
	Liquid    bool
	Note      string
	Archived  bool
	CreatedAt time.Time
	Values    []HoldingValue
}

type HoldingValue struct {
	ID     int64
	On     time.Time
	Amount int64
}

// Holdings lists holdings with their values, active first.
func (s *Store) Holdings(ctx context.Context, loc *time.Location) ([]Holding, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, kind, liquid, COALESCE(note, ''), archived, created_at
		FROM holdings ORDER BY archived, id`)
	if err != nil {
		return nil, err
	}
	var out []Holding
	idx := map[int64]int{}
	for rows.Next() {
		var h Holding
		var created string
		if err := rows.Scan(&h.ID, &h.Name, &h.Kind, &h.Liquid, &h.Note, &h.Archived, &created); err != nil {
			rows.Close()
			return nil, err
		}
		h.CreatedAt = parseTime(created)
		idx[h.ID] = len(out)
		out = append(out, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	vrows, err := s.db.QueryContext(ctx, "SELECT id, holding_id, on_date, amount_minor FROM holding_values ORDER BY on_date, id")
	if err != nil {
		return nil, err
	}
	defer vrows.Close()
	for vrows.Next() {
		var v HoldingValue
		var hid int64
		var on string
		if err := vrows.Scan(&v.ID, &hid, &on, &v.Amount); err != nil {
			return nil, err
		}
		v.On, _ = time.ParseInLocation("2006-01-02", on, loc)
		if i, ok := idx[hid]; ok {
			out[i].Values = append(out[i].Values, v)
		}
	}
	return out, vrows.Err()
}

// SaveHolding adds a holding (ID 0) or updates its name, kind, liquidity and note.
func (s *Store) SaveHolding(ctx context.Context, h Holding, now time.Time) (int64, error) {
	if !ValidKind(h.Kind) {
		return 0, errors.New("unknown kind")
	}
	if h.ID == 0 {
		var id int64
		err := s.db.QueryRowContext(ctx, `INSERT INTO holdings (name, kind, liquid, note, created_at) VALUES (?, ?, ?, ?, ?)
			RETURNING id`, h.Name, h.Kind, h.Liquid, nullString(h.Note), fmtTime(now)).Scan(&id)
		return id, err
	}
	res, err := s.db.ExecContext(ctx, "UPDATE holdings SET name = ?, kind = ?, liquid = ?, note = ? WHERE id = ?",
		h.Name, h.Kind, h.Liquid, nullString(h.Note), h.ID)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotFound
	}
	return h.ID, nil
}

// SetHoldingValue records a value on a date, replacing one on the same date.
func (s *Store) SetHoldingValue(ctx context.Context, id int64, on time.Time, amount int64) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO holding_values (holding_id, on_date, amount_minor) VALUES (?, ?, ?)
		ON CONFLICT(holding_id, on_date) DO UPDATE SET amount_minor = excluded.amount_minor`, id, on.Format("2006-01-02"), amount)
	return err
}

func (s *Store) DeleteHoldingValue(ctx context.Context, holdingID, valueID int64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM holding_values WHERE id = ? AND holding_id = ?", valueID, holdingID)
	return err
}

func (s *Store) SetHoldingArchived(ctx context.Context, id int64, archived bool) error {
	_, err := s.db.ExecContext(ctx, "UPDATE holdings SET archived = ? WHERE id = ?", archived, id)
	return err
}

func (s *Store) DeleteHolding(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM holdings WHERE id = ?", id)
	return err
}

// SetDebtCategory marks a category as repaying debts, or not.
func (s *Store) SetDebtCategory(ctx context.Context, id int64, on bool) error {
	_, err := s.db.ExecContext(ctx, "UPDATE categories SET debt = ? WHERE id = ?", on, id)
	return err
}

// DebtCategories lists categories marked as repaying debts.
func (s *Store) DebtCategories(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name FROM categories WHERE debt = 1 ORDER BY name")
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

// CardBalance is the card balance a statement printed on a date.
type CardBalance struct {
	Account string
	On      time.Time
	Amount  int64
}

// CardBalances lists the balances statements printed, oldest first.
func (s *Store) CardBalances(ctx context.Context, loc *time.Location) ([]CardBalance, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT account, period_from, opening_minor, period_to, closing_minor FROM statements
		WHERE opening_minor IS NOT NULL AND closing_minor IS NOT NULL ORDER BY period_from`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	var out []CardBalance
	for rows.Next() {
		var acc, from, to string
		var open, closing sql.NullInt64
		if err := rows.Scan(&acc, &from, &open, &to, &closing); err != nil {
			return nil, err
		}
		for _, p := range []struct {
			on string
			v  int64
		}{{from, open.Int64}, {to, closing.Int64}} {
			if key := acc + p.on; !seen[key] {
				seen[key] = true
				t, _ := time.ParseInLocation("2006-01-02", p.on, loc)
				out = append(out, CardBalance{Account: acc, On: t, Amount: p.v})
			}
		}
	}
	return out, rows.Err()
}
