package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

var now = time.Date(2026, 10, 5, 9, 32, 0, 0, time.UTC)

func walletTx(key string, amount int64) Tx {
	return Tx{ExternalKey: key, OccurredAt: now, AmountMinor: amount, Currency: "KZT", AmountRaw: "raw",
		MerchantRaw: "MAGNUM", MerchantNorm: "magnum", Card: "Kaspi Gold", Source: SourceWallet, CreatedAt: now}
}

func mustCat(t *testing.T, s *Store, name string) int64 {
	t.Helper()
	c, err := s.FindCategory(context.Background(), name)
	if err != nil {
		t.Fatalf("category %s: %v", name, err)
	}
	return c.ID
}

func TestMigrateIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	for range 2 {
		s, err := Open(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		cats, err := s.Categories(context.Background())
		if err != nil || len(cats) == 0 {
			t.Fatalf("categories: %v %d", err, len(cats))
		}
		s.Close()
	}
}

func TestInsertDedup(t *testing.T) {
	s, ctx := openTest(t), context.Background()
	id1, dup, err := s.InsertTx(ctx, walletTx("k1", 450000))
	if err != nil || dup {
		t.Fatalf("first insert: %v %v", err, dup)
	}
	id2, dup, err := s.InsertTx(ctx, walletTx("k1", 450000))
	if err != nil || !dup || id2 != id1 {
		t.Fatalf("second insert: id=%d dup=%v err=%v", id2, dup, err)
	}
	// manual entries without a key are not deduplicated
	m := walletTx("", 1000)
	a, _, _ := s.InsertTx(ctx, m)
	b, _, _ := s.InsertTx(ctx, m)
	if a == b {
		t.Fatal("manual txs must not dedup")
	}
}

// Invariant: the splits of a done transaction add up to amount_minor.
func TestCloseTxInvariant(t *testing.T) {
	s, ctx := openTest(t), context.Background()
	id, _, _ := s.InsertTx(ctx, walletTx("k", 450000))
	food, gifts := mustCat(t, s, "Groceries"), mustCat(t, s, "gifts")

	_, err := s.CloseTx(ctx, id, []Split{{CategoryID: food, AmountMinor: 100}}, true, now)
	if !errors.Is(err, ErrSplitSum) {
		t.Fatalf("want ErrSplitSum, got %v", err)
	}
	if tx, _ := s.GetTx(ctx, id); tx.Status != StatusPending {
		t.Fatalf("failed close changed status to %s", tx.Status)
	}

	c, err := s.CloseTx(ctx, id, []Split{{CategoryID: food, AmountMinor: 250000}, {CategoryID: gifts, AmountMinor: 200000}}, true, now)
	if err != nil {
		t.Fatal(err)
	}
	if c.Learned {
		t.Fatal("split across categories must not teach the merchant rule")
	}
	tx, _ := s.GetTx(ctx, id)
	splits, _ := s.Splits(ctx, id)
	var sum int64
	for _, sp := range splits {
		sum += sp.AmountMinor
	}
	if tx.Status != StatusDone || sum != tx.AmountMinor {
		t.Fatalf("status=%s sum=%d amount=%d", tx.Status, sum, tx.AmountMinor)
	}
}

func TestUnparsedAmountTakesSplitSum(t *testing.T) {
	s, ctx := openTest(t), context.Background()
	id, _, _ := s.InsertTx(ctx, walletTx("k", 0))
	if _, err := s.CloseTx(ctx, id, []Split{{CategoryID: mustCat(t, s, "Taxi"), AmountMinor: 120000}}, true, now); err != nil {
		t.Fatal(err)
	}
	if tx, _ := s.GetTx(ctx, id); tx.AmountMinor != 120000 {
		t.Fatalf("amount = %d", tx.AmountMinor)
	}
}

func TestMerchantLearningAndUndo(t *testing.T) {
	s, ctx := openTest(t), context.Background()
	food, cafe := mustCat(t, s, "Groceries"), mustCat(t, s, "Cafes & restaurants")
	closeAs := func(key string, cat int64) Closed {
		id, _, _ := s.InsertTx(ctx, walletTx(key, 1000))
		c, err := s.CloseTx(ctx, id, []Split{{CategoryID: cat, AmountMinor: 1000}}, true, now)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	hits := func() (int64, int) {
		r, ok, err := s.Rule(ctx, "magnum")
		if err != nil || !ok {
			return 0, 0
		}
		return r.CategoryID, r.Hits
	}

	closeAs("a", food)
	closeAs("b", food)
	last := closeAs("c", food)
	if cat, h := hits(); cat != food || h != 3 {
		t.Fatalf("rule = %d/%d", cat, h)
	}
	if err := s.UndoClose(ctx, last, now); err != nil {
		t.Fatal(err)
	}
	if _, h := hits(); h != 2 {
		t.Fatalf("after undo hits = %d", h)
	}
	if tx, _ := s.GetTx(ctx, last.TxID); tx.Status != StatusPending {
		t.Fatalf("undo status = %s", tx.Status)
	}

	// a different category resets the counter
	closeAs("d", cafe)
	if cat, h := hits(); cat != cafe || h != 1 {
		t.Fatalf("rule after change = %d/%d", cat, h)
	}

	// an archived category disables the rule
	if err := s.ArchiveCategory(ctx, cafe); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Rule(ctx, "magnum"); ok {
		t.Fatal("rule must be gone after archiving")
	}
}

func TestQuestionsFlow(t *testing.T) {
	s, ctx := openTest(t), context.Background()
	a, _, _ := s.InsertTx(ctx, walletTx("a", 1000))
	b, _, _ := s.InsertTx(ctx, walletTx("b", 2000))

	if u, _ := s.Unrouted(ctx); len(u) != 2 {
		t.Fatalf("unrouted = %d", len(u))
	}
	s.EnqueueQuestion(ctx, a)
	s.EnqueueQuestion(ctx, b)
	if u, _ := s.Unrouted(ctx); len(u) != 0 {
		t.Fatalf("unrouted after enqueue = %d", len(u))
	}
	if u, _ := s.Unasked(ctx); len(u) != 2 {
		t.Fatalf("unasked = %d", len(u))
	}

	// a batch: one message for two transactions, so it cannot identify one
	if err := s.MarkAsked(ctx, 77, now, a, b); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TxByMessage(ctx, 77); !errors.Is(err, ErrNotFound) {
		t.Fatalf("batch message must be ambiguous, got %v", err)
	}
	// a separate question about a does not move the first-asked time
	s.MarkAsked(ctx, 78, now.Add(time.Hour), a)
	if id, err := s.TxByMessage(ctx, 78); err != nil || id != a {
		t.Fatalf("TxByMessage = %d %v", id, err)
	}
	if n, _ := s.CountAskedSince(ctx, now.Add(-time.Minute)); n != 2 {
		t.Fatalf("asked since = %d", n)
	}

	due, _ := s.DueReminders(ctx, now.Add(3*time.Hour))
	if len(due) != 2 {
		t.Fatalf("due = %d", len(due))
	}
	s.MarkReminded(ctx, now, a, b)
	if due, _ := s.DueReminders(ctx, now.Add(3*time.Hour)); len(due) != 0 {
		t.Fatalf("due after remind = %d", len(due))
	}

	// closing deletes the question
	s.CloseTx(ctx, a, []Split{{CategoryID: mustCat(t, s, "Other"), AmountMinor: 1000}}, false, now)
	if m, _ := s.QuestionMessage(ctx, a); m != 0 {
		t.Fatal("question must be deleted on close")
	}
}

func TestTotalsAndExport(t *testing.T) {
	s, ctx := openTest(t), context.Background()
	food, gifts := mustCat(t, s, "Groceries"), mustCat(t, s, "Gifts")
	a, _, _ := s.InsertTx(ctx, walletTx("a", 450000))
	s.CloseTx(ctx, a, []Split{{CategoryID: food, AmountMinor: 250000}, {CategoryID: gifts, AmountMinor: 200000, Note: "for mom"}}, true, now)
	s.InsertTx(ctx, walletTx("b", 1000)) // uncategorized
	c, _, _ := s.InsertTx(ctx, walletTx("c", 5000))
	s.SetStatus(ctx, c, StatusIgnored)
	old := walletTx("d", 9999)
	old.OccurredAt = now.AddDate(0, -2, 0)
	s.InsertTx(ctx, old)

	totals, err := s.Totals(ctx, now.AddDate(0, 0, -7), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int64{"Groceries": 250000, "Gifts": 200000, "": 1000}
	if len(totals) != len(want) {
		t.Fatalf("totals = %+v", totals)
	}
	for _, ct := range totals {
		if want[ct.Category] != ct.Minor {
			t.Errorf("%q = %d, want %d", ct.Category, ct.Minor, want[ct.Category])
		}
	}

	rows, err := s.ExportRows(ctx, now.AddDate(0, 0, -7), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[1].SplitNote != "for mom" || rows[2].Category != "" {
		t.Fatalf("export rows = %+v", rows)
	}
}

func TestBackup(t *testing.T) {
	s, ctx := openTest(t), context.Background()
	s.InsertTx(ctx, walletTx("a", 1000))
	path := filepath.Join(t.TempDir(), "backup.db")
	if err := s.Backup(ctx, path); err != nil {
		t.Fatal(err)
	}
	b, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if txs, _ := b.OpenTxs(ctx); len(txs) != 1 {
		t.Fatalf("backup has %d txs", len(txs))
	}
}
