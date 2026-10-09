package store

import (
	"context"
	"testing"
)

func TestTrash(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	food, home := mustCat(t, s, "Groceries"), mustCat(t, s, "Home")
	id, _, _ := s.InsertTx(ctx, walletTx("w1", 30000))
	s.CloseTx(ctx, id, []Split{{CategoryID: food, AmountMinor: 10000}, {CategoryID: home, AmountMinor: 20000}}, false, now)
	other, _, _ := s.InsertTx(ctx, walletTx("w2", 5000))

	trashed, err := s.TrashTxs(ctx, []int64{id, 999}, now)
	if err != nil || len(trashed) != 1 {
		t.Fatalf("trash: %v %v", trashed, err)
	}
	if _, err := s.GetTx(ctx, id); err != ErrNotFound {
		t.Fatalf("still there: %v", err)
	}
	// Wallet sending the same payment again does not bring it back
	if got, dup, err := s.InsertTx(ctx, walletTx("w1", 30000)); err != nil || !dup || got != 0 {
		t.Fatalf("resend: %d %v %v", got, dup, err)
	}
	// a merged category follows into the trash
	if err := s.MergeCategory(ctx, food, home); err != nil {
		t.Fatal(err)
	}
	items, _ := s.Trash(ctx, now.Location())
	if len(items) != 1 || items[0].Tx.ID != id || len(items[0].Categories) != 2 || items[0].AmountMinor != 30000 {
		t.Fatalf("trash list: %+v", items)
	}
	if n, _ := s.TrashCount(ctx); n != 1 {
		t.Fatalf("count %d", n)
	}

	if n, err := s.RestoreTrash(ctx, trashed); err != nil || n != 1 {
		t.Fatalf("restore: %d %v", n, err)
	}
	rows, _ := s.Ledger(ctx, now.Location())
	var back LedgerRow
	for _, r := range rows {
		if r.TxID != other {
			back = r
		}
	}
	if len(rows) != 2 || back.TxID == id || back.Status != StatusDone || len(back.Splits) != 2 || back.Splits[0]+back.Splits[1] != 30000 ||
		back.Categories[0] != "Home" {
		t.Fatalf("restored: %+v", back)
	}
	if n, _ := s.TrashCount(ctx); n != 0 {
		t.Fatalf("trash not empty: %d", n)
	}
}
