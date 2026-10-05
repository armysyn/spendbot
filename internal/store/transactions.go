package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	StatusPending = "pending" // waiting for classification or a question
	StatusAsked   = "asked"   // question sent
	StatusDone    = "done"    // categorized
	StatusIgnored = "ignored" // skipped by the user
	StatusReview  = "review"  // statement purchase waiting for an answer in a question batch
	StatusInfo    = "info"    // not spending (incoming money, transfers): kept for analysis

	SourceWallet = "wallet"
	SourceManual = "manual"
	SourceImport = "import"
)

type Tx struct {
	ID           int64
	ExternalKey  string // empty for manual entries
	OccurredAt   time.Time
	AmountMinor  int64 // 0 — the amount could not be parsed, see AmountRaw
	Currency     string
	AmountRaw    string
	MerchantRaw  string
	MerchantNorm string
	Card         string
	Source       string
	Status       string
	Note         string
	Kind         string // operation kind from the statement
	CreatedAt    time.Time
}

type Split struct {
	CategoryID  int64
	AmountMinor int64
	Note        string
}

// txColumns expects the transactions table to be aliased as t.
const txColumns = `t.id, COALESCE(t.external_key, ''), t.occurred_at, t.amount_minor, t.currency,
	COALESCE(t.amount_raw, ''), COALESCE(t.merchant_raw, ''), COALESCE(t.merchant_norm, ''), COALESCE(t.card, ''),
	t.source, t.status, COALESCE(t.note, ''), COALESCE(t.kind, ''), t.created_at`

type scanner interface{ Scan(...any) error }

func scanTx(r scanner) (Tx, error) {
	var t Tx
	var occurred, created string
	err := r.Scan(&t.ID, &t.ExternalKey, &occurred, &t.AmountMinor, &t.Currency, &t.AmountRaw,
		&t.MerchantRaw, &t.MerchantNorm, &t.Card, &t.Source, &t.Status, &t.Note, &t.Kind, &created)
	t.OccurredAt, t.CreatedAt = parseTime(occurred), parseTime(created)
	return t, err
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// InsertTx saves a transaction. If one with the same ExternalKey exists, it returns its id and dup=true.
func (s *Store) InsertTx(ctx context.Context, t Tx) (id int64, dup bool, err error) {
	if t.Status == "" {
		t.Status = StatusPending
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO transactions
			(external_key, occurred_at, amount_minor, currency, amount_raw, merchant_raw, merchant_norm,
			 card, source, status, note, kind, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(external_key) DO NOTHING`,
			nullString(t.ExternalKey), fmtTime(t.OccurredAt), t.AmountMinor, t.Currency, nullString(t.AmountRaw),
			nullString(t.MerchantRaw), nullString(t.MerchantNorm), nullString(t.Card), t.Source, t.Status,
			nullString(t.Note), nullString(t.Kind), fmtTime(t.CreatedAt))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			id, err = res.LastInsertId()
			return err
		}
		dup = true
		return tx.QueryRowContext(ctx, "SELECT id FROM transactions WHERE external_key = ?", t.ExternalKey).Scan(&id)
	})
	return id, dup, err
}

func (s *Store) GetTx(ctx context.Context, id int64) (Tx, error) {
	t, err := scanTx(s.db.QueryRowContext(ctx, "SELECT "+txColumns+" FROM transactions t WHERE t.id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}

func (s *Store) DeleteTx(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM transactions WHERE id = ?", id)
	return err
}

func (s *Store) SetStatus(ctx context.Context, id int64, status string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE transactions SET status = ? WHERE id = ?", status, id)
	return err
}

func (s *Store) queryTxs(ctx context.Context, query string, args ...any) ([]Tx, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tx
	for rows.Next() {
		t, err := scanTx(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// OpenTxs returns uncategorized transactions (pending and asked), oldest first.
func (s *Store) OpenTxs(ctx context.Context) ([]Tx, error) {
	return s.queryTxs(ctx, "SELECT "+txColumns+` FROM transactions t
		WHERE t.status IN ('pending', 'asked') ORDER BY t.occurred_at, t.id`)
}

// Unrouted returns pending transactions without a question yet: they need to go through
// classification (after a restart in the middle of processing, for example).
func (s *Store) Unrouted(ctx context.Context) ([]Tx, error) {
	return s.queryTxs(ctx, "SELECT "+txColumns+` FROM transactions t
		WHERE t.status = 'pending' AND NOT EXISTS (SELECT 1 FROM questions q WHERE q.tx_id = t.id)
		ORDER BY t.id`)
}

// LastTxAt returns the time of the latest transaction from the given source.
func (s *Store) LastTxAt(ctx context.Context, source string) (time.Time, bool, error) {
	var v sql.NullString
	err := s.db.QueryRowContext(ctx, "SELECT MAX(created_at) FROM transactions WHERE source = ?", source).Scan(&v)
	if err != nil || !v.Valid {
		return time.Time{}, false, err
	}
	return parseTime(v.String), true, nil
}

func (s *Store) Splits(ctx context.Context, txID int64) ([]Split, error) {
	return querySplits(ctx, s.db, txID)
}

// Closed describes what closing a transaction changed, so it can be undone.
type Closed struct {
	TxID       int64
	PrevStatus string
	PrevAmount int64
	PrevSplits []Split
	Learned    bool  // the merchant rule was changed
	PrevRule   *Rule // the rule before the change; nil — there was none
}

var ErrSplitSum = errors.New("parts do not add up to the transaction amount")

// CloseTx splits a transaction into categories and marks it done. Invariant: the splits
// add up to amount_minor. If the amount was not parsed (0), it is taken from the splits.
// learn=true means a person confirmed the answer, so the merchant rule is updated when the
// whole transaction went into one category.
func (s *Store) CloseTx(ctx context.Context, txID int64, splits []Split, learn bool, now time.Time) (Closed, error) {
	c := Closed{TxID: txID}
	if len(splits) == 0 {
		return c, errors.New("no categories given")
	}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		t, err := scanTx(tx.QueryRowContext(ctx, "SELECT "+txColumns+" FROM transactions t WHERE t.id = ?", txID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		c.PrevStatus, c.PrevAmount = t.Status, t.AmountMinor

		var sum int64
		for _, sp := range splits {
			sum += sp.AmountMinor
		}
		amount := t.AmountMinor
		if amount == 0 {
			amount = sum
		}
		if sum != amount || sum == 0 {
			return fmt.Errorf("%w: %d instead of %d", ErrSplitSum, sum, amount)
		}

		if c.PrevSplits, err = querySplits(ctx, tx, txID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM splits WHERE tx_id = ?", txID); err != nil {
			return err
		}
		for _, sp := range splits {
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO splits (tx_id, category_id, amount_minor, note) VALUES (?, ?, ?, ?)",
				txID, sp.CategoryID, sp.AmountMinor, nullString(sp.Note)); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, "UPDATE transactions SET status = ?, amount_minor = ? WHERE id = ?",
			StatusDone, amount, txID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM questions WHERE tx_id = ?", txID); err != nil {
			return err
		}

		if !learn || t.MerchantNorm == "" || !singleCategory(splits) {
			return nil
		}
		prev, ok, err := ruleTx(ctx, tx, t.MerchantNorm)
		if err != nil {
			return err
		}
		if ok {
			c.PrevRule = &prev
		}
		hits := 1
		if ok && prev.CategoryID == splits[0].CategoryID {
			hits = prev.Hits + 1
		}
		c.Learned = true
		_, err = tx.ExecContext(ctx, `INSERT INTO merchant_rules (merchant_norm, category_id, hits, updated_at)
			VALUES (?, ?, ?, ?) ON CONFLICT(merchant_norm) DO UPDATE SET
			category_id = excluded.category_id, hits = excluded.hits, updated_at = excluded.updated_at`,
			t.MerchantNorm, splits[0].CategoryID, hits, fmtTime(now))
		return err
	})
	return c, err
}

// UndoClose restores the transaction and the merchant rule to their state before CloseTx.
func (s *Store) UndoClose(ctx context.Context, c Closed, now time.Time) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		var norm string
		if err := tx.QueryRowContext(ctx, "SELECT COALESCE(merchant_norm, '') FROM transactions WHERE id = ?",
			c.TxID).Scan(&norm); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM splits WHERE tx_id = ?", c.TxID); err != nil {
			return err
		}
		for _, sp := range c.PrevSplits {
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO splits (tx_id, category_id, amount_minor, note) VALUES (?, ?, ?, ?)",
				c.TxID, sp.CategoryID, sp.AmountMinor, nullString(sp.Note)); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, "UPDATE transactions SET status = ?, amount_minor = ? WHERE id = ?",
			c.PrevStatus, c.PrevAmount, c.TxID); err != nil {
			return err
		}
		if !c.Learned {
			return nil
		}
		if c.PrevRule == nil {
			_, err := tx.ExecContext(ctx, "DELETE FROM merchant_rules WHERE merchant_norm = ?", norm)
			return err
		}
		_, err := tx.ExecContext(ctx,
			"UPDATE merchant_rules SET category_id = ?, hits = ?, updated_at = ? WHERE merchant_norm = ?",
			c.PrevRule.CategoryID, c.PrevRule.Hits, fmtTime(now), norm)
		return err
	})
}

func singleCategory(splits []Split) bool {
	for _, sp := range splits[1:] {
		if sp.CategoryID != splits[0].CategoryID {
			return false
		}
	}
	return true
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func querySplits(ctx context.Context, q querier, txID int64) ([]Split, error) {
	rows, err := q.QueryContext(ctx,
		"SELECT category_id, amount_minor, COALESCE(note, '') FROM splits WHERE tx_id = ? ORDER BY id", txID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Split
	for rows.Next() {
		var sp Split
		if err := rows.Scan(&sp.CategoryID, &sp.AmountMinor, &sp.Note); err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}
