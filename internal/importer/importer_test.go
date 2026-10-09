package importer

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"spendbot/internal/analytics"
	"spendbot/internal/kaspi"
	"spendbot/internal/statement"
	"spendbot/internal/store"
)

var almaty, _ = time.LoadLocation("Asia/Almaty")

func day(d int) time.Time { return time.Date(2026, 3, d, 0, 0, 0, 0, almaty) }

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestImport(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, almaty)

	// A Wallet payment late on the 11th shows up on the 12th in the statement.
	wallet, _, _ := st.InsertTx(ctx, store.Tx{ExternalKey: "w", OccurredAt: time.Date(2026, 3, 11, 23, 50, 0, 0, almaty),
		AmountMinor: 341000, Currency: "KZT", MerchantRaw: "IP TESTOV", MerchantNorm: "testov",
		Source: store.SourceWallet, Status: store.StatusDone, CreatedAt: now})
	// A shop whose category is already known.
	food, _ := st.FindCategory(ctx, "Groceries")
	known, _, _ := st.InsertTx(ctx, store.Tx{ExternalKey: "k", OccurredAt: day(1), AmountMinor: 100, Currency: "KZT",
		MerchantNorm: "small", Source: store.SourceWallet, CreatedAt: now})
	st.CloseTx(ctx, known, []store.Split{{CategoryID: food.ID, AmountMinor: 100}}, true, now)

	s := statement.Statement{Account: "*1234", Summary: map[string]int64{"Покупки": -123000 - 341000 - 47000 - 59000},
		Ops: []statement.Op{
			{Date: day(12), AmountMinor: -123000, Currency: "KZT", Kind: kaspi.Purchase, Details: "ИП ''АЛМАЗ''", Seq: 1},
			{Date: day(12), AmountMinor: 1000000, Currency: "KZT", Kind: kaspi.FromOwn, Details: "С Kaspi Депозита", Seq: 2},
			{Date: day(12), AmountMinor: -341000, Currency: "KZT", Kind: kaspi.Purchase, Details: "ИП ТЕСТОВ", Seq: 3},
			{Date: day(12), AmountMinor: -47000, Currency: "KZT", Kind: kaspi.Purchase, Details: "SMALL", Seq: 4},
			{Date: day(12), AmountMinor: -59000, Currency: "KZT", Kind: kaspi.Purchase, Details: "KIOSK.KZ", Seq: 5},
		}}
	res, err := Import(ctx, st, s, now)
	if err != nil {
		t.Fatal(err)
	}
	if res.Matched != 1 || res.Auto != 1 || res.Review != 2 || res.Info != 1 || res.Verify != nil {
		t.Fatalf("result: %+v", res)
	}
	if tx, _ := st.GetTx(ctx, wallet); tx.Status != store.StatusDone {
		t.Fatalf("wallet tx touched: %+v", tx)
	}

	// importing again adds nothing
	res, _ = Import(ctx, st, s, now)
	if res.Duplicates != 5 || res.Review+res.Auto+res.Info+res.Matched != 0 {
		t.Fatalf("reimport: %+v", res)
	}

	// uncategorized statement purchases show up in the summary, incoming money does not
	totals, _ := st.Totals(ctx, day(1), day(20))
	var open, foodSum int64
	for _, ct := range totals {
		switch ct.Category {
		case "":
			open = ct.Minor
		case "Groceries":
			foodSum = ct.Minor
		}
	}
	if open != 123000+59000 || foodSum != 100+47000 {
		t.Fatalf("totals: %+v", totals)
	}

	// merchants to ask about
	ms, _ := st.ReviewMerchants(ctx)
	if len(ms) != 2 || ms[0].TotalMinor != 123000 {
		t.Fatalf("review merchants: %+v", ms)
	}
}

// The same statement downloaded in the evening: new operations appeared on top of the last
// day, and two identical purchases are still there. Nothing is duplicated, new ones are added.
func TestReimportNewerVersion(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	now := time.Date(2026, 10, 5, 21, 0, 0, 0, almaty)
	op := func(amount int64, details string) statement.Op {
		return statement.Op{Date: day(5), AmountMinor: amount, Currency: "KZT", Kind: kaspi.Purchase, Details: details}
	}
	morning := statement.Statement{Account: "*1234", From: day(1), To: day(5), Summary: map[string]int64{"Покупки": -3000},
		Ops: []statement.Op{op(-1000, "KIOSK.KZ"), op(-1000, "KIOSK.KZ"), op(-1000, "UBER")}}
	evening := statement.Statement{Account: "*1234", From: day(1), To: day(5), Summary: map[string]int64{"Покупки": -8000},
		Ops: []statement.Op{op(-5000, "MAGNUM"), op(-1000, "KIOSK.KZ"), op(-1000, "KIOSK.KZ"), op(-1000, "UBER")}}

	if res, _ := Import(ctx, st, morning, now); res.Review != 3 {
		t.Fatalf("morning: %+v", res)
	}
	res, err := Import(ctx, st, evening, now)
	if err != nil || res.Review != 1 || res.Duplicates != 3 {
		t.Fatalf("evening: %+v %v", res, err)
	}
	// the morning version imported again — header totals stay from the fuller evening one
	Import(ctx, st, morning, now)
	infos, _ := st.Statements(ctx, almaty)
	if len(infos) != 1 || infos[0].Summary["Покупки"] != -8000 {
		t.Fatalf("statement summary: %+v", infos)
	}
	ms, _ := st.ReviewMerchants(ctx)
	var n int
	for _, m := range ms {
		n += m.Count
	}
	if n != 4 {
		t.Fatalf("operations in db: %d, want 4", n)
	}
}

// A refund from a known merchant reduces their category; one from an unknown merchant goes to questions.
func TestRefunds(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, almaty)
	clothes, _ := st.FindCategory(ctx, "Clothing")
	w, _, _ := st.InsertTx(ctx, store.Tx{ExternalKey: "w", OccurredAt: day(1), AmountMinor: 100, Currency: "KZT",
		MerchantNorm: "sportmaster", Source: store.SourceWallet, CreatedAt: now})
	st.CloseTx(ctx, w, []store.Split{{CategoryID: clothes.ID, AmountMinor: 100}}, true, now)

	s := statement.Statement{Account: "*1", Ops: []statement.Op{
		{Date: day(3), AmountMinor: 500000, Currency: "KZT", Kind: kaspi.Purchase, Details: "SPORTMASTER"},
		{Date: day(3), AmountMinor: 20000, Currency: "KZT", Kind: kaspi.Purchase, Details: "NEW SHOP"},
	}}
	res, err := Import(ctx, st, s, now)
	if err != nil || res.Auto != 1 || res.Review != 1 || res.Info != 0 {
		t.Fatalf("result: %+v %v", res, err)
	}
	totals, _ := st.Totals(ctx, day(1), day(10))
	for _, ct := range totals {
		if ct.Category == "Clothing" && ct.Minor != 100-500000 {
			t.Fatalf("refund must reduce the category: %+v", ct)
		}
	}
}

// A transfer to a person marked as spending (rent) gets the category straight from a new statement.
func TestTransferRule(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, almaty)
	rent, _ := st.AddCategory(ctx, "Rent")
	if _, err := st.CategorizeTransfers(ctx, "Adam S.", "adam s", rent, 3, now); err != nil {
		t.Fatal(err)
	}
	s := statement.Statement{Account: "*1", Ops: []statement.Op{
		{Date: day(3), AmountMinor: -26000000, Currency: "KZT", Kind: kaspi.Transfer, Details: "Adam S."},
		{Date: day(3), AmountMinor: 500000, Currency: "KZT", Kind: kaspi.TopUp, Details: "Adam S."},
		{Date: day(3), AmountMinor: -100000, Currency: "KZT", Kind: kaspi.Transfer, Details: "Friend F."},
	}}
	if _, err := Import(ctx, st, s, now); err != nil {
		t.Fatal(err)
	}
	totals, _ := st.Totals(ctx, day(1), day(10))
	if len(totals) != 1 || totals[0].Category != "Rent" || totals[0].Minor != 26000000 {
		t.Fatalf("totals: %+v", totals)
	}
}

// A deleted operation stays deleted when the statement, or a newer version of it, is imported
// again; the statement still reconciles; restored, it is back once.
func TestDeletedStaysDeleted(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	now := time.Date(2026, 10, 5, 21, 0, 0, 0, almaty)
	op := func(amount int64, details string) statement.Op {
		return statement.Op{Date: day(5), AmountMinor: amount, Currency: "KZT", Kind: kaspi.Purchase, Details: details}
	}
	morning := statement.Statement{Account: "*0000", From: day(1), To: day(5), Summary: map[string]int64{"Покупки": -3000},
		Ops: []statement.Op{op(-1000, "KIOSK.KZ"), op(-2000, "UBER")}}
	evening := statement.Statement{Account: "*0000", From: day(1), To: day(5), Summary: map[string]int64{"Покупки": -8000},
		Ops: []statement.Op{op(-5000, "MAGNUM"), op(-1000, "KIOSK.KZ"), op(-2000, "UBER")}}
	Import(ctx, st, morning, now)
	rows, _ := st.Ledger(ctx, almaty)
	var uber int64
	for _, r := range rows {
		if r.Merchant == "UBER" {
			uber = r.TxID
		}
	}
	trashed, err := st.TrashTxs(ctx, []int64{uber}, now)
	if err != nil || len(trashed) != 1 {
		t.Fatalf("trash: %v %v", trashed, err)
	}
	if res, _ := Import(ctx, st, morning, now); res.Deleted != 1 || res.Duplicates != 1 {
		t.Fatalf("again: %+v", res)
	}
	if res, _ := Import(ctx, st, evening, now); res.Deleted != 1 || res.Review != 1 {
		t.Fatalf("newer version: %+v", res)
	}
	live, _ := st.Ledger(ctx, almaty)
	gone, _ := st.TrashLedger(ctx, almaty)
	if len(live) != 2 || len(gone) != 1 || gone[0].Status != store.StatusDeleted {
		t.Fatalf("live %d, deleted %d", len(live), len(gone))
	}
	infos, _ := st.Statements(ctx, almaty)
	if rec := analytics.Reconcile(append(live, gone...), infos[0]); !rec.OK || rec.Deleted != 1 {
		t.Fatalf("reconcile: %+v", rec)
	}
	if n, err := st.RestoreTrash(ctx, trashed); err != nil || n != 1 {
		t.Fatalf("restore: %d %v", n, err)
	}
	if res, _ := Import(ctx, st, evening, now); res.Duplicates != 3 || res.Deleted != 0 {
		t.Fatalf("after restore: %+v", res)
	}
	if live, _ := st.Ledger(ctx, almaty); len(live) != 3 {
		t.Fatalf("live after restore: %d", len(live))
	}
}
