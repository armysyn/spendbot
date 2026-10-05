package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// EnqueueQuestion queues a question about a transaction. Calling it again changes nothing.
func (s *Store) EnqueueQuestion(ctx context.Context, txID int64) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO questions (tx_id) VALUES (?) ON CONFLICT DO NOTHING", txID)
	return err
}

// Unasked returns transactions whose question has not been sent yet.
func (s *Store) Unasked(ctx context.Context) ([]Tx, error) {
	return s.queryTxs(ctx, "SELECT "+txColumns+` FROM transactions t JOIN questions q ON q.tx_id = t.id
		WHERE q.asked_at IS NULL AND t.status = 'pending' ORDER BY t.occurred_at, t.id`)
}

// MarkAsked records the message that asked the question and moves transactions to asked.
// If the question was asked before, the first time is kept: the reminder counts from it.
func (s *Store) MarkAsked(ctx context.Context, msgID int, at time.Time, txIDs ...int64) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, id := range txIDs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO questions (tx_id, tg_message_id, asked_at) VALUES (?, ?, ?)
				ON CONFLICT(tx_id) DO UPDATE SET tg_message_id = excluded.tg_message_id,
				asked_at = COALESCE(questions.asked_at, excluded.asked_at)`, id, msgID, fmtTime(at)); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE transactions SET status = ? WHERE id = ? AND status = ?",
				StatusAsked, id, StatusPending); err != nil {
				return err
			}
		}
		return nil
	})
}

// TxByMessage finds the open transaction asked about in message msgID.
// For a batch message covering several transactions it returns ErrNotFound.
func (s *Store) TxByMessage(ctx context.Context, msgID int) (int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT q.tx_id FROM questions q JOIN transactions t ON t.id = q.tx_id
		WHERE q.tg_message_id = ? AND t.status IN ('pending', 'asked') LIMIT 2`, msgID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(ids) != 1 {
		return 0, ErrNotFound
	}
	return ids[0], nil
}

// CountAskedSince counts questions asked since the given time (for batching).
func (s *Store) CountAskedSince(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM questions WHERE asked_at >= ?", fmtTime(since)).Scan(&n)
	return n, err
}

// DueReminders returns open questions asked before the given time with no reminder yet.
func (s *Store) DueReminders(ctx context.Context, before time.Time) ([]Tx, error) {
	return s.queryTxs(ctx, "SELECT "+txColumns+` FROM transactions t JOIN questions q ON q.tx_id = t.id
		WHERE t.status = 'asked' AND q.reminded_at IS NULL AND q.asked_at < ?
		ORDER BY t.occurred_at, t.id`, fmtTime(before))
}

func (s *Store) MarkReminded(ctx context.Context, at time.Time, txIDs ...int64) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, id := range txIDs {
			if _, err := tx.ExecContext(ctx, "UPDATE questions SET reminded_at = ? WHERE tx_id = ?",
				fmtTime(at), id); err != nil {
				return err
			}
		}
		return nil
	})
}

// QuestionMessage returns the id of the message asking about a transaction, 0 if none.
func (s *Store) QuestionMessage(ctx context.Context, txID int64) (int, error) {
	var id sql.NullInt64
	err := s.db.QueryRowContext(ctx, "SELECT tg_message_id FROM questions WHERE tx_id = ?", txID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return int(id.Int64), err
}

func (s *Store) DeleteQuestion(ctx context.Context, txID int64) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM questions WHERE tx_id = ?", txID)
	return err
}
