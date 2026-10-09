package analysis

import (
	"context"
	"time"

	"spendbot/internal/analytics"
	"spendbot/internal/clickhouse"
	"spendbot/internal/store"
)

// LedgerFromClickHouse returns the same rows as store.Ledger, from ClickHouse: the analytics
// computation is shared and the source is a setting.
func LedgerFromClickHouse(ctx context.Context, ch *clickhouse.Client, loc *time.Location) ([]store.LedgerRow, error) {
	type row struct {
		TxID         int64    `json:"tx_id"`
		At           int64    `json:"at"`
		Amount       int64    `json:"amount"`
		Currency     string   `json:"currency"`
		Merchant     string   `json:"merchant"`
		MerchantNorm string   `json:"merchant_norm"`
		Kind         string   `json:"kind"`
		Status       string   `json:"status"`
		Source       string   `json:"source"`
		Categories   []string `json:"categories"`
		Splits       []int64  `json:"split_amounts"`
	}
	rows, err := clickhouse.Select[row](ctx, ch, `SELECT tx_id, toUnixTimestamp(occurred_at) AS at, amount, currency,
		merchant, merchant_norm, kind, status, source, categories, split_amounts
		FROM operations FINAL WHERE deleted = 0 ORDER BY tx_id`)
	if err != nil {
		return nil, err
	}
	out := make([]store.LedgerRow, len(rows))
	for i, r := range rows {
		out[i] = store.LedgerRow{TxID: r.TxID, At: time.Unix(r.At, 0).In(loc), Amount: r.Amount, Currency: r.Currency,
			Merchant: r.Merchant, MerchantNorm: r.MerchantNorm, Kind: r.Kind, Status: r.Status, Source: r.Source}
		if len(r.Categories) > 0 {
			out[i].Categories, out[i].Splits = r.Categories, r.Splits
		}
	}
	return out, nil
}

// MarkPassThrough leaves out of spending the cash that only passed through the card, unless
// the person turned that off; every reader of the ledger calls it, so pages and insights
// count the same.
func MarkPassThrough(ctx context.Context, st *store.Store, rows []store.LedgerRow) ([]store.LedgerRow, error) {
	on, err := st.PassThroughCash(ctx)
	if err != nil || !on {
		return rows, err
	}
	names, err := st.IncomeSources(ctx)
	if err != nil {
		return nil, err
	}
	sources := map[string]bool{}
	for _, n := range names {
		sources[n] = true
	}
	return analytics.MarkPassThrough(rows, sources), nil
}
