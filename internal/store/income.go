package store

import (
	"context"
	"errors"
	"time"
)

// SalaryPeriod is a stated monthly salary over a span of months; an empty To is the current one.
type SalaryPeriod struct {
	ID          int64
	From, To    string // 'YYYY-MM'
	AmountMinor int64
	Employer    string
	Note        string
}

// Current reports whether the period has no end.
func (p SalaryPeriod) Current() bool { return p.To == "" }

// SalaryPeriods lists stated salaries, the latest first.
func (s *Store) SalaryPeriods(ctx context.Context) ([]SalaryPeriod, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, from_month, COALESCE(to_month, ''), amount_minor,
		COALESCE(employer, ''), COALESCE(note, '') FROM salary_periods ORDER BY from_month DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SalaryPeriod
	for rows.Next() {
		var p SalaryPeriod
		if err := rows.Scan(&p.ID, &p.From, &p.To, &p.AmountMinor, &p.Employer, &p.Note); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SaveSalaryPeriod adds a period (ID 0) or replaces one.
func (s *Store) SaveSalaryPeriod(ctx context.Context, p SalaryPeriod) (int64, error) {
	if p.ID == 0 {
		var id int64
		err := s.db.QueryRowContext(ctx, `INSERT INTO salary_periods (from_month, to_month, amount_minor, employer, note)
			VALUES (?, ?, ?, ?, ?) RETURNING id`, p.From, nullString(p.To), p.AmountMinor, nullString(p.Employer), nullString(p.Note)).Scan(&id)
		return id, err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE salary_periods SET from_month = ?, to_month = ?, amount_minor = ?,
		employer = ?, note = ? WHERE id = ?`, p.From, nullString(p.To), p.AmountMinor, nullString(p.Employer), nullString(p.Note), p.ID)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotFound
	}
	return p.ID, nil
}

func (s *Store) DeleteSalaryPeriod(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM salary_periods WHERE id = ?", id)
	return err
}

// IncomeSources lists top-up senders counted as income.
func (s *Store) IncomeSources(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT name FROM income_sources ORDER BY name")
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

// SetIncomeSource counts a sender's top-ups as income, or stops counting them.
func (s *Store) SetIncomeSource(ctx context.Context, name string, on bool, now time.Time) error {
	if name == "" {
		return errors.New("no sender")
	}
	var err error
	if on {
		_, err = s.db.ExecContext(ctx, "INSERT OR IGNORE INTO income_sources (name, created_at) VALUES (?, ?)", name, fmtTime(now))
	} else {
		_, err = s.db.ExecContext(ctx, "DELETE FROM income_sources WHERE name = ?", name)
	}
	return err
}
