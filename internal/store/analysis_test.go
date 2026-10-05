package store

import (
	"context"
	"testing"
	"time"
)

func TestBatchFlow(t *testing.T) {
	s, ctx := openTest(t), context.Background()
	add := func(key, norm string, amount int64) int64 {
		id, _, err := s.InsertTx(ctx, Tx{ExternalKey: key, OccurredAt: now, AmountMinor: amount, Currency: "KZT",
			MerchantRaw: norm, MerchantNorm: norm, Source: SourceImport, Status: StatusReview, CreatedAt: now})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	a1, a2 := add("1", "asanov", 300000), add("2", "asanov", 200000)
	b1 := add("3", "boom", 100000)
	c1 := add("4", "cosmo", 50000)
	food := mustCat(t, s, "Groceries")

	ms, _ := s.ReviewMerchants(ctx)
	if len(ms) != 3 || ms[0].Norm != "asanov" || ms[0].Count != 2 || ms[0].TotalMinor != 500000 {
		t.Fatalf("merchants %+v", ms)
	}
	for _, m := range ms {
		s.AddMerchantQuestion(ctx, m.Norm, m.Display, food, now)
	}
	if ms, _ := s.ReviewMerchants(ctx); len(ms) != 0 {
		t.Fatal("open questions must hide merchants")
	}
	if n, _, _ := s.UnbatchedCount(ctx); n != 3 {
		t.Fatalf("unbatched %d", n)
	}
	id, err := s.CreateBatch(ctx, 2, now)
	if err != nil || id == 0 {
		t.Fatal(err)
	}
	items, _ := s.BatchItems(ctx, id)
	if len(items) != 2 || items[0].Norm != "asanov" || items[1].Norm != "boom" || len(items[0].Samples) != 2 {
		t.Fatalf("items %+v", items)
	}

	// asanov → groceries, boom → not spending; no empty answers
	n, err := s.AnswerBatch(ctx, id, map[string]Answer{
		"asanov": {CategoryID: food, Note: "corner shop"},
		"boom":   {Ignore: true},
	}, 3, now)
	if err != nil || n != 2 {
		t.Fatalf("answer: %d %v", n, err)
	}
	for _, id := range []int64{a1, a2} {
		if tx, _ := s.GetTx(ctx, id); tx.Status != StatusDone {
			t.Fatalf("tx %d %s", id, tx.Status)
		}
	}
	if tx, _ := s.GetTx(ctx, b1); tx.Status != StatusIgnored {
		t.Fatalf("ignored: %s", tx.Status)
	}
	if r, ok, _ := s.Rule(ctx, "asanov"); !ok || r.Hits != 3 {
		t.Fatalf("rule %+v", r)
	}
	if open, _ := s.Batches(ctx, false, 10); len(open) != 0 {
		t.Fatalf("open batches %+v", open)
	}

	// a second batch without answers returns the question to the pool
	id2, _ := s.CreateBatch(ctx, 10, now)
	s.AnswerBatch(ctx, id2, map[string]Answer{}, 3, now)
	if n, _, _ := s.UnbatchedCount(ctx); n != 1 {
		t.Fatalf("unanswered must return to pool, got %d", n)
	}
	if tx, _ := s.GetTx(ctx, c1); tx.Status != StatusReview {
		t.Fatalf("c1 %s", tx.Status)
	}
	if id3, _ := s.CreateBatch(ctx, 10, now); id3 == 0 {
		t.Fatal("returned question must be batchable again")
	}
	if id4, _ := s.CreateBatch(ctx, 10, now); id4 != 0 {
		t.Fatal("empty batch must not be created")
	}
}

func TestUpdatedAtTriggers(t *testing.T) {
	s, ctx := openTest(t), context.Background()
	id, _, _ := s.InsertTx(ctx, walletTx("a", 1000))
	var before string
	s.db.QueryRowContext(ctx, "SELECT updated_at FROM transactions WHERE id = ?", id).Scan(&before)
	if before == "" {
		t.Fatal("updated_at not set on insert")
	}
	s.db.ExecContext(ctx, "UPDATE transactions SET updated_at = '2000-01-01T00:00:00Z' WHERE id = ?", id)
	s.CloseTx(ctx, id, []Split{{CategoryID: mustCat(t, s, "Other"), AmountMinor: 1000}}, false, time.Now())
	var after string
	s.db.QueryRowContext(ctx, "SELECT updated_at FROM transactions WHERE id = ?", id).Scan(&after)
	if after <= "2000-01-01T00:00:00Z" {
		t.Fatalf("updated_at not touched: %s", after)
	}
	s.DeleteTx(ctx, id)
	var n int
	s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM deleted_txs WHERE tx_id = ?", id).Scan(&n)
	if n != 1 {
		t.Fatal("delete not recorded")
	}
}
