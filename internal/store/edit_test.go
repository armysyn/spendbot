package store

import (
	"context"
	"testing"
	"time"

	"spendbot/internal/kaspi"
)

func TestRecategorizeMerchant(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	food, home, gifts := mustCat(t, s, "Groceries"), mustCat(t, s, "Home"), mustCat(t, s, "Gifts")
	add := func(key string, amount int64, status string) int64 {
		tx := walletTx(key, amount)
		tx.Source, tx.Kind, tx.Status = SourceImport, kaspi.Purchase, status
		id, _, err := s.InsertTx(ctx, tx)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	done := add("a", 1000, StatusPending)
	s.CloseTx(ctx, done, []Split{{CategoryID: food, AmountMinor: 1000}}, false, now)
	review := add("b", 2000, StatusReview)
	split := add("c", 3000, StatusPending)
	s.CloseTx(ctx, split, []Split{{CategoryID: food, AmountMinor: 1000}, {CategoryID: gifts, AmountMinor: 2000}}, false, now)
	s.AddMerchantQuestion(ctx, "magnum", "MAGNUM", 0, now)

	n, err := s.RecategorizeMerchant(ctx, "magnum", home, 3, now)
	if err != nil || n != 2 {
		t.Fatalf("moved %d, %v", n, err)
	}
	for _, id := range []int64{done, review} {
		sp, _ := s.Splits(ctx, id)
		tx, _ := s.GetTx(ctx, id)
		if len(sp) != 1 || sp[0].CategoryID != home || tx.Status != StatusDone {
			t.Errorf("tx %d: %+v %s", id, sp, tx.Status)
		}
	}
	if sp, _ := s.Splits(ctx, split); len(sp) != 2 {
		t.Error("a split purchase keeps its parts")
	}
	if r, ok, _ := s.Rule(ctx, "magnum"); !ok || r.CategoryID != home || r.Hits < 3 {
		t.Errorf("rule %+v", r)
	}
	if ms, _ := s.ReviewMerchants(ctx); len(ms) != 0 {
		t.Errorf("still waiting: %+v", ms)
	}
}

func TestUncategorizeTx(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	food := mustCat(t, s, "Groceries")
	for _, c := range []struct{ kind, want string }{{kaspi.Purchase, StatusIgnored}, {kaspi.Transfer, StatusInfo}} {
		tx := walletTx(c.kind, 500)
		tx.Kind = c.kind
		id, _, _ := s.InsertTx(ctx, tx)
		s.CloseTx(ctx, id, []Split{{CategoryID: food, AmountMinor: 500}}, false, now)
		if err := s.UncategorizeTx(ctx, id); err != nil {
			t.Fatal(err)
		}
		got, _ := s.GetTx(ctx, id)
		if sp, _ := s.Splits(ctx, id); len(sp) != 0 || got.Status != c.want {
			t.Errorf("%s: %s %+v", c.kind, got.Status, sp)
		}
	}
}

func TestMergeCategory(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	food := mustCat(t, s, "Groceries")
	shop, _ := s.AddCategory(ctx, "Supermarket")
	s.SetSavings(ctx, []int64{shop})
	id, _, _ := s.InsertTx(ctx, walletTx("a", 700))
	s.CloseTx(ctx, id, []Split{{CategoryID: shop, AmountMinor: 700}}, true, now)
	s.AddMerchantQuestion(ctx, "other", "OTHER", shop, now)

	if err := s.MergeCategory(ctx, shop, food); err != nil {
		t.Fatal(err)
	}
	if sp, _ := s.Splits(ctx, id); sp[0].CategoryID != food {
		t.Errorf("split %+v", sp)
	}
	if r, _, _ := s.Rule(ctx, "magnum"); r.CategoryID != food {
		t.Errorf("rule %+v", r)
	}
	if _, err := s.Category(ctx, shop); err != ErrNotFound {
		t.Errorf("merged category must be gone: %v", err)
	}
	if err := s.MergeCategory(ctx, food, food); err == nil {
		t.Error("merge into itself")
	}
	all, _ := s.AllCategories(ctx)
	for _, c := range all {
		if c.Name == "Groceries" && c.Savings {
			t.Error("the target keeps its own savings mark")
		}
	}
}

func TestResetPassword(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	s.SetKV(ctx, KVWebPassword, "pbkdf2-sha256$1$a$b")
	s.CreateSession(ctx, "x", "", now, now.Add(time.Hour))
	if err := s.ResetPassword(ctx); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.GetKV(ctx, KVWebPassword); v != "" {
		t.Error("password kept")
	}
	if ok, _ := s.SessionValid(ctx, "x", now); ok {
		t.Error("session kept")
	}
}
