package analysis

import (
	"context"
	"testing"
	"time"

	"spendbot/internal/clickhouse"
	"spendbot/internal/store"
)

func TestSync(t *testing.T) {
	ch, st, ctx := testCH(t), testStore(t), context.Background()
	at := time.Date(2026, 3, 12, 12, 0, 0, 0, almaty)
	a, _, _ := st.InsertTx(ctx, store.Tx{ExternalKey: "a", OccurredAt: at, AmountMinor: 123000, Currency: "KZT",
		MerchantRaw: "IP SAMAT", MerchantNorm: "samat", Source: store.SourceImport, Status: store.StatusReview, CreatedAt: at})
	b, _, _ := st.InsertTx(ctx, store.Tx{ExternalKey: "b", OccurredAt: at, AmountMinor: 1000, Currency: "KZT",
		Source: store.SourceWallet, CreatedAt: at})

	// Sync takes only rows older than two seconds: rows of the current second may still change.
	if n, _ := Sync(ctx, st, ch, time.Now()); n != 0 {
		t.Fatalf("fresh rows must wait, sent %d", n)
	}
	time.Sleep(3 * time.Second)
	later := time.Now
	if n, err := Sync(ctx, st, ch, later()); err != nil || n != 2 {
		t.Fatalf("first sync: %d %v", n, err)
	}
	if n, _ := Sync(ctx, st, ch, later()); n != 0 {
		t.Fatalf("nothing changed, sent %d", n)
	}

	food, _ := st.FindCategory(ctx, "Groceries")
	st.CloseTx(ctx, a, []store.Split{{CategoryID: food.ID, AmountMinor: 123000}}, false, at)
	st.DeleteTx(ctx, b)
	time.Sleep(3 * time.Second)
	if n, err := Sync(ctx, st, ch, later()); err != nil || n != 2 {
		t.Fatalf("second sync: %d %v", n, err)
	}

	type row struct {
		TxID       int64    `json:"tx_id"`
		Day        string   `json:"day"`
		Status     string   `json:"status"`
		Categories []string `json:"categories"`
	}
	rows, err := clickhouse.Select[row](ctx, ch, "SELECT tx_id, toString(toDate(occurred_at)) AS day, status, categories FROM operations FINAL ORDER BY tx_id")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].TxID != a || rows[0].Day != "2026-03-12" || rows[0].Status != "done" || rows[0].Categories[0] != "Groceries" {
		t.Fatalf("clickhouse rows: %+v", rows)
	}
}
