package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// MerchantStat is a merchant to ask about: a summary of their uncategorized purchases.
type MerchantStat struct {
	Norm        string
	Display     string // name as printed in the statement
	Count       int
	TotalMinor  int64
	Currency    string
	First, Last time.Time
}

// ReviewMerchants lists merchants with purchases in review that have no open question yet.
func (s *Store) ReviewMerchants(ctx context.Context) ([]MerchantStat, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.merchant_norm, MAX(t.merchant_raw), COUNT(*), SUM(t.amount_minor),
			MAX(t.currency), MIN(t.occurred_at), MAX(t.occurred_at)
		FROM transactions t
		WHERE t.status = 'review' AND COALESCE(t.merchant_norm, '') != ''
		  AND NOT EXISTS (SELECT 1 FROM merchant_questions q WHERE q.merchant_norm = t.merchant_norm AND q.status = 'open')
		GROUP BY t.merchant_norm ORDER BY SUM(t.amount_minor) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MerchantStat
	for rows.Next() {
		var m MerchantStat
		var first, last string
		if err := rows.Scan(&m.Norm, &m.Display, &m.Count, &m.TotalMinor, &m.Currency, &first, &last); err != nil {
			return nil, err
		}
		m.First, m.Last = parseTime(first), parseTime(last)
		out = append(out, m)
	}
	return out, rows.Err()
}

// AddMerchantQuestion opens a question about a merchant (or reopens an old one).
func (s *Store) AddMerchantQuestion(ctx context.Context, norm, display string, guess int64, now time.Time) error {
	var g any
	if guess != 0 {
		g = guess
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO merchant_questions (merchant_norm, display, guess_category_id, created_at)
		VALUES (?, ?, ?, ?) ON CONFLICT(merchant_norm) DO UPDATE SET status = 'open', batch_id = NULL,
		display = excluded.display, guess_category_id = excluded.guess_category_id, answer = NULL, answered_at = NULL`,
		norm, display, g, fmtTime(now))
	return err
}

// UnbatchedCount reports how many open questions are not in a batch yet and when the oldest was opened.
func (s *Store) UnbatchedCount(ctx context.Context) (int, time.Time, error) {
	var n int
	var oldest sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), MIN(created_at) FROM merchant_questions
		WHERE status = 'open' AND batch_id IS NULL`).Scan(&n, &oldest)
	return n, parseTime(oldest.String), err
}

// CreateBatch puts up to limit open questions into a batch, most expensive merchants first.
// It returns 0 when there is nothing to batch.
func (s *Store) CreateBatch(ctx context.Context, limit int, now time.Time) (int64, error) {
	var id int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "INSERT INTO question_batches (created_at) VALUES (?)", fmtTime(now))
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		res, err = tx.ExecContext(ctx, `UPDATE merchant_questions SET batch_id = ? WHERE merchant_norm IN (
			SELECT q.merchant_norm FROM merchant_questions q
			LEFT JOIN transactions t ON t.merchant_norm = q.merchant_norm AND t.status = 'review'
			WHERE q.status = 'open' AND q.batch_id IS NULL
			GROUP BY q.merchant_norm ORDER BY COALESCE(SUM(t.amount_minor), 0) DESC LIMIT ?)`, id, limit)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			id = 0
			return errEmptyBatch
		}
		return nil
	})
	if errors.Is(err, errEmptyBatch) {
		return 0, nil
	}
	return id, err
}

var errEmptyBatch = errors.New("empty batch")

type Batch struct {
	ID         int64
	CreatedAt  time.Time
	NotifiedAt *time.Time
	AnsweredAt *time.Time
	Items      int
	TotalMinor int64
}

// Batches lists open (answered=false) or answered batches, newest first.
func (s *Store) Batches(ctx context.Context, answered bool, limit int) ([]Batch, error) {
	cond := "b.answered_at IS NULL"
	if answered {
		cond = "b.answered_at IS NOT NULL"
	}
	rows, err := s.db.QueryContext(ctx, `SELECT b.id, b.created_at, b.notified_at, b.answered_at,
			(SELECT COUNT(*) FROM merchant_questions q WHERE q.batch_id = b.id),
			(SELECT COALESCE(SUM(t.amount_minor), 0) FROM merchant_questions q
			 JOIN transactions t ON t.merchant_norm = q.merchant_norm AND t.status IN ('review', 'done', 'ignored')
			   AND t.source = 'import'
			 WHERE q.batch_id = b.id)
		FROM question_batches b WHERE `+cond+` ORDER BY b.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Batch
	for rows.Next() {
		var b Batch
		var created string
		var notified, answeredAt sql.NullString
		if err := rows.Scan(&b.ID, &created, &notified, &answeredAt, &b.Items, &b.TotalMinor); err != nil {
			return nil, err
		}
		b.CreatedAt, b.NotifiedAt, b.AnsweredAt = parseTime(created), nullTime(notified), nullTime(answeredAt)
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) MarkBatchNotified(ctx context.Context, id int64, at time.Time) error {
	_, err := s.db.ExecContext(ctx, "UPDATE question_batches SET notified_at = ? WHERE id = ?", fmtTime(at), id)
	return err
}

// BatchItem is one batch row: a question about a merchant with a summary of their purchases.
type BatchItem struct {
	MerchantStat
	GuessCategoryID int64
	Status          string
	Answer          string
	Samples         []Tx // latest purchases, to show what this is about
}

func (s *Store) BatchItems(ctx context.Context, batchID int64) ([]BatchItem, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT q.merchant_norm, q.display, COALESCE(q.guess_category_id, 0), q.status,
			COALESCE(q.answer, ''), COUNT(t.id), COALESCE(SUM(t.amount_minor), 0), COALESCE(MAX(t.currency), 'KZT'),
			COALESCE(MIN(t.occurred_at), ''), COALESCE(MAX(t.occurred_at), '')
		FROM merchant_questions q
		LEFT JOIN transactions t ON t.merchant_norm = q.merchant_norm AND t.source = 'import'
		WHERE q.batch_id = ?
		GROUP BY q.merchant_norm ORDER BY SUM(t.amount_minor) DESC`, batchID)
	if err != nil {
		return nil, err
	}
	var out []BatchItem
	for rows.Next() {
		var it BatchItem
		var first, last string
		if err := rows.Scan(&it.Norm, &it.Display, &it.GuessCategoryID, &it.Status, &it.Answer, &it.Count,
			&it.TotalMinor, &it.Currency, &first, &last); err != nil {
			rows.Close()
			return nil, err
		}
		it.First, it.Last = parseTime(first), parseTime(last)
		out = append(out, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Samples, err = s.queryTxs(ctx, "SELECT "+txColumns+` FROM transactions t
			WHERE t.merchant_norm = ? AND t.source = 'import' ORDER BY t.occurred_at DESC LIMIT 3`, out[i].Norm); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Answer is the answer to one batch question. An empty answer (CategoryID == 0 and !Ignore)
// returns the question to the pool: it goes into the next batch.
type Answer struct {
	CategoryID int64
	Ignore     bool   // not spending or skip: the merchant's purchases become ignored
	Note       string // free text: what this is
}

// AnswerBatch applies a whole batch in one database transaction: it remembers the merchant
// (hits = minHits, so their Wallet payments are categorized automatically from now on) and
// categorizes all of their statement purchases waiting for an answer.
func (s *Store) AnswerBatch(ctx context.Context, batchID int64, answers map[string]Answer, minHits int, now time.Time) (applied int, err error) {
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, "SELECT merchant_norm FROM merchant_questions WHERE batch_id = ? AND status = 'open'", batchID)
		if err != nil {
			return err
		}
		var norms []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				rows.Close()
				return err
			}
			norms = append(norms, n)
		}
		rows.Close()

		for _, norm := range norms {
			a := answers[norm]
			switch {
			case a.Ignore:
				if _, err := tx.ExecContext(ctx, "UPDATE transactions SET status = 'ignored' WHERE merchant_norm = ? AND status = 'review'", norm); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `UPDATE merchant_questions SET status = 'ignored', answer = ?, answered_at = ?
					WHERE merchant_norm = ?`, nullString(a.Note), fmtTime(now), norm); err != nil {
					return err
				}
			case a.CategoryID != 0:
				if _, err := tx.ExecContext(ctx, `INSERT INTO splits (tx_id, category_id, amount_minor, note)
					SELECT id, ?, amount_minor, ? FROM transactions WHERE merchant_norm = ? AND status = 'review'`,
					a.CategoryID, nullString(a.Note), norm); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, "UPDATE transactions SET status = 'done' WHERE merchant_norm = ? AND status = 'review'", norm); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO merchant_rules (merchant_norm, category_id, hits, updated_at)
					VALUES (?, ?, ?, ?) ON CONFLICT(merchant_norm) DO UPDATE SET category_id = excluded.category_id,
					hits = MAX(merchant_rules.hits, excluded.hits), updated_at = excluded.updated_at`,
					norm, a.CategoryID, minHits, fmtTime(now)); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `UPDATE merchant_questions SET status = 'answered', answer = ?, answered_at = ?
					WHERE merchant_norm = ?`, nullString(a.Note), fmtTime(now), norm); err != nil {
					return err
				}
			default:
				if _, err := tx.ExecContext(ctx, "UPDATE merchant_questions SET batch_id = NULL WHERE merchant_norm = ?", norm); err != nil {
					return err
				}
				continue
			}
			applied++
		}
		_, err = tx.ExecContext(ctx, "UPDATE question_batches SET answered_at = ? WHERE id = ?", fmtTime(now), batchID)
		return err
	})
	return applied, err
}

type Insight struct {
	ID         int64
	Key        string
	Kind       string
	Title      string
	Body       string
	CreatedAt  time.Time
	NotifiedAt *time.Time
}

// AddInsight stores an insight unless one with the same key already exists.
func (s *Store) AddInsight(ctx context.Context, in Insight, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO insights (key, kind, title, body, created_at)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT(key) DO NOTHING`, in.Key, in.Kind, in.Title, in.Body, fmtTime(now))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// Insights lists insights that are not dismissed, newest first; unnotified=true returns only unsent ones.
func (s *Store) Insights(ctx context.Context, unnotified bool, limit int) ([]Insight, error) {
	q := "SELECT id, key, kind, title, body, created_at, notified_at FROM insights WHERE dismissed = 0"
	if unnotified {
		q += " AND notified_at IS NULL"
	}
	rows, err := s.db.QueryContext(ctx, q+" ORDER BY id DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Insight
	for rows.Next() {
		var in Insight
		var created string
		var notified sql.NullString
		if err := rows.Scan(&in.ID, &in.Key, &in.Kind, &in.Title, &in.Body, &created, &notified); err != nil {
			return nil, err
		}
		in.CreatedAt, in.NotifiedAt = parseTime(created), nullTime(notified)
		out = append(out, in)
	}
	return out, rows.Err()
}

func (s *Store) MarkInsightsNotified(ctx context.Context, at time.Time, ids ...int64) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, "UPDATE insights SET notified_at = ? WHERE id = ?", fmtTime(at), id); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) DismissInsight(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE insights SET dismissed = 1 WHERE id = ?", id)
	return err
}

// CloseReviewMerchant categorizes all of a merchant's purchases waiting for an answer —
// for when a rule for them appeared after the import.
func (s *Store) CloseReviewMerchant(ctx context.Context, norm string, categoryID int64) (int, error) {
	var n int64
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO splits (tx_id, category_id, amount_minor)
			SELECT id, ?, amount_minor FROM transactions WHERE merchant_norm = ? AND status = 'review'`,
			categoryID, norm); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, "UPDATE transactions SET status = 'done' WHERE merchant_norm = ? AND status = 'review'", norm)
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	return int(n), err
}
