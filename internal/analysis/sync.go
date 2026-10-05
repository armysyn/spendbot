// Package analysis is background spending analysis: ClickHouse sync, batched merchant
// questions and insights (subscriptions, anomalies, trends, tips).
package analysis

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"spendbot/internal/clickhouse"
	"spendbot/internal/merchant"
	"spendbot/internal/store"
)

// operation is a row of the operations table in ClickHouse.
type operation struct {
	TxID         int64    `json:"tx_id"`
	OccurredAt   int64    `json:"occurred_at"` // unix time: a string would be read in the column's time zone
	Amount       int64    `json:"amount"`      // minor units, spending positive
	Currency     string   `json:"currency"`
	Merchant     string   `json:"merchant"`
	MerchantNorm string   `json:"merchant_norm"`
	Card         string   `json:"card"`
	Source       string   `json:"source"`
	Kind         string   `json:"kind"`
	Status       string   `json:"status"`
	Categories   []string `json:"categories"`
	SplitAmounts []int64  `json:"split_amounts"`
	Version      uint64   `json:"version"`
	Deleted      uint8    `json:"deleted"`
}

// Migrate creates the database and the table. ReplacingMergeTree(version, deleted): an edit
// is a new row with a higher version, a deletion is a row with deleted = 1; queries use FINAL.
func Migrate(ctx context.Context, ch *clickhouse.Client, tz string) error {
	if err := ch.ExecRaw(ctx, "CREATE DATABASE IF NOT EXISTS "+ch.DB()); err != nil {
		return err
	}
	return ch.Exec(ctx, fmt.Sprintf(`CREATE TABLE IF NOT EXISTS operations (
		tx_id Int64,
		occurred_at DateTime('%s'),
		amount Int64,
		currency LowCardinality(String),
		merchant String,
		merchant_norm String,
		card String,
		source LowCardinality(String),
		kind LowCardinality(String),
		status LowCardinality(String),
		categories Array(String),
		split_amounts Array(Int64),
		version UInt64,
		deleted UInt8
	) ENGINE = ReplacingMergeTree(version, deleted) ORDER BY tx_id`, tz))
}

const (
	cursorKey  = "ch_sync_cursor"
	deletedKey = "ch_sync_deleted"
	syncBatch  = 2000
)

// Sync sends to ClickHouse everything that changed in SQLite since the last run.
// It returns the number of rows sent.
func Sync(ctx context.Context, st *store.Store, ch *clickhouse.Client, now time.Time) (int, error) {
	settled := now.Add(-2 * time.Second)
	cur, err := loadCursor(ctx, st)
	if err != nil {
		return 0, err
	}
	sent := 0
	for {
		rows, next, err := st.ChangedTxs(ctx, cur, settled, syncBatch)
		if err != nil {
			return sent, err
		}
		if len(rows) == 0 {
			break
		}
		version := uint64(now.UnixNano())
		ops := make([]operation, len(rows))
		for i, r := range rows {
			ops[i] = toOperation(r, version)
		}
		if err := clickhouse.Insert(ctx, ch, "operations", ops); err != nil {
			return sent, err
		}
		sent += len(ops)
		cur = next
		if err := st.SetKV(ctx, cursorKey, cur.UpdatedAt+"|"+strconv.FormatInt(cur.ID, 10)); err != nil {
			return sent, err
		}
		if len(rows) < syncBatch {
			break
		}
	}

	since, err := st.GetKV(ctx, deletedKey)
	if err != nil {
		return sent, err
	}
	ids, since, err := st.DeletedSince(ctx, since, settled)
	if err != nil || len(ids) == 0 {
		return sent, err
	}
	version := uint64(now.UnixNano())
	dels := make([]operation, len(ids))
	for i, id := range ids {
		dels[i] = operation{TxID: id, Version: version, Deleted: 1,
			Categories: []string{}, SplitAmounts: []int64{}}
	}
	if err := clickhouse.Insert(ctx, ch, "operations", dels); err != nil {
		return sent, err
	}
	return sent + len(dels), st.SetKV(ctx, deletedKey, since)
}

func loadCursor(ctx context.Context, st *store.Store) (store.SyncCursor, error) {
	v, err := st.GetKV(ctx, cursorKey)
	if err != nil || v == "" {
		return store.SyncCursor{}, err
	}
	at, id, _ := strings.Cut(v, "|")
	n, _ := strconv.ParseInt(id, 10, 64)
	return store.SyncCursor{UpdatedAt: at, ID: n}, nil
}

func toOperation(r store.SyncRow, version uint64) operation {
	cats, amounts := r.Categories, r.SplitAmounts
	if cats == nil {
		cats, amounts = []string{}, []int64{}
	}
	m := r.MerchantRaw
	if m != "" {
		m = merchant.Display(m)
	}
	return operation{
		TxID:         r.ID,
		OccurredAt:   r.OccurredAt.Unix(),
		Amount:       r.AmountMinor,
		Currency:     r.Currency,
		Merchant:     m,
		MerchantNorm: r.MerchantNorm,
		Card:         r.Card,
		Source:       r.Source,
		Kind:         r.Kind,
		Status:       r.Status,
		Categories:   cats,
		SplitAmounts: amounts,
		Version:      version,
	}
}
