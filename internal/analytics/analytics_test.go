package analytics_test

import (
	"context"
	"fmt"
	"path/filepath"
	. "spendbot/internal/analytics"
	"testing"
	"time"

	"spendbot/internal/kaspi"
	"spendbot/internal/merchant"
	"spendbot/internal/store"
)

var almaty, _ = time.LoadLocation("Asia/Almaty")

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func ledger(t *testing.T, st *store.Store) []store.LedgerRow {
	t.Helper()
	rows, err := st.Ledger(context.Background(), almaty)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

const tg = 100 // tiyn per tenge

func day(m time.Month, d int) time.Time { return time.Date(2026, m, d, 12, 0, 0, 0, almaty) }

// seed creates a data set whose numbers are computed by hand in TestDashboard.
func seed(t *testing.T, st *store.Store) {
	ctx := context.Background()
	cat := func(n string) int64 {
		c, err := st.FindCategory(ctx, n)
		if err != nil {
			t.Fatal(err)
		}
		return c.ID
	}
	k := 0
	add := func(at time.Time, raw string, amount int64, source, kind, status string, splits ...store.Split) int64 {
		k++
		id, _, err := st.InsertTx(ctx, store.Tx{ExternalKey: fmt.Sprint(k), OccurredAt: at, AmountMinor: amount * tg,
			Currency: "KZT", MerchantRaw: raw, MerchantNorm: merchant.Normalize(raw), Source: source, Kind: kind,
			Status: status, CreatedAt: at})
		if err != nil {
			t.Fatal(err)
		}
		if len(splits) > 0 {
			if _, err := st.CloseTx(ctx, id, splits, false, at); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	const imp, wal = store.SourceImport, store.SourceWallet
	add(day(8, 3), "MAGNUM", 10_000, wal, kaspi.Purchase, store.StatusPending, store.Split{CategoryID: cat("Groceries"), AmountMinor: 10_000 * tg}) // from Wallet, matched the statement
	add(day(8, 3), "MAGNUM #12", 5_000, imp, kaspi.Purchase, store.StatusReview)                                                                    // the same merchant
	add(day(8, 8), "SPORTMASTER", 60_000, imp, kaspi.Purchase, store.StatusPending,
		store.Split{CategoryID: cat("Clothing"), AmountMinor: 40_000 * tg}, store.Split{CategoryID: cat("Gifts"), AmountMinor: 20_000 * tg})
	add(day(8, 8), "SPORTMASTER", -5_000, imp, kaspi.Purchase, store.StatusInfo) // refund
	add(day(8, 15), "ATM X", 50_000, imp, kaspi.Withdrawal, store.StatusReview)
	add(day(8, 20), "ATM Y", 20_000, imp, kaspi.Withdrawal, store.StatusReview)
	add(day(9, 1), "YANDEX GO", 1_800, wal, "", store.StatusPending, store.Split{CategoryID: cat("Taxi"), AmountMinor: 1_800 * tg})
	add(day(9, 1), "TEST", 999_999, wal, "", store.StatusIgnored)                // skipped — not spending
	add(day(9, 2), "Aigerim A.", 100_000, imp, kaspi.Transfer, store.StatusInfo) // a transfer — not spending
	add(day(9, 2), "From Kaspi Deposit", -300_000, imp, kaspi.TopUp, store.StatusInfo)
	add(day(9, 10), "SHOP", 500, imp, kaspi.Purchase, store.StatusReview)
	// a withdrawal the person gave a category goes to that category, not to "Cash"
	add(day(9, 12), "ATM Z", 30_000, imp, kaspi.Withdrawal, store.StatusPending, store.Split{CategoryID: cat("Gifts"), AmountMinor: 30_000 * tg})
}

func TestDashboard(t *testing.T) {
	st := testStore(t)
	seed(t, st)
	p := Period{Key: "x", From: time.Date(2026, 8, 1, 0, 0, 0, 0, almaty), To: time.Date(2026, 10, 1, 0, 0, 0, 0, almaty)}
	d := Build(ledger(t, st), p, nil)
	if len(d.Problems) > 0 {
		t.Fatalf("invariants: %v", d.Problems)
	}
	eq := func(name string, got, want int64) {
		t.Helper()
		if got != want {
			t.Errorf("%s = %d, want %d", name, got, want)
		}
	}
	tot := d.Totals
	eq("spend", tot.Spend, 172_300*tg)
	eq("purchases", tot.Purchases, 72_300*tg)
	eq("purchase count", tot.PurchaseCount, 5)
	eq("avg check", tot.AvgCheck(), 15_460*tg)
	eq("refunds", tot.Refunds, 5_000*tg)
	eq("cash", tot.Cash, 100_000*tg)
	eq("cash count", tot.CashCount, 3)
	eq("uncategorized", tot.Uncategorized, 500*tg) // 5000 − refund 5000 + 500
	eq("transfers out", tot.TransfersOut, 100_000*tg)
	eq("income", tot.Income, 300_000*tg)
	eq("per day", d.PerDay, 172_300*tg/61)

	// months
	if len(d.Months) != 2 {
		t.Fatalf("months: %+v", d.Months)
	}
	eq("august", d.Months[0].Value, 140_000*tg)
	eq("september", d.Months[1].Value, 32_300*tg)

	// weekdays, descending; the average divides by the number of such days in the calendar
	// (1 Aug 2026 is a Saturday: 9 Saturdays, Mondays and Tuesdays, 8 Thursdays)
	wantDays := []struct {
		name  string
		total int64
		occur int64
	}{{"Saturday", 135_000, 9}, {"Thursday", 20_500, 8}, {"Monday", 15_000, 9}, {"Tuesday", 1_800, 9}}
	for i, w := range wantDays {
		got := d.Weekdays[i]
		if got.Label != w.name || got.Value != w.total*tg || got.Avg != w.total*tg/w.occur {
			t.Errorf("weekday %d: %+v, want %s %d", i, got, w.name, w.total)
		}
	}
	if len(d.Weekdays) != 7 || d.Weekdays[6].Value != 0 {
		t.Errorf("all 7 days expected: %+v", d.Weekdays)
	}

	// merchants: names normalized, cash as one row, the refund subtracted
	wantM := []struct {
		name  string
		total int64
		n     int64
	}{{CashMerchant, 100_000, 3}, {"Sportmaster", 55_000, 1}, {"Magnum", 15_000, 2}, {"Yandex Go", 1_800, 1}, {"Shop", 500, 1}}
	if len(d.Merchants) != len(wantM) {
		t.Fatalf("merchants: %+v", d.Merchants)
	}
	for i, w := range wantM {
		got := d.Merchants[i]
		if got.Name != w.name || got.Total != w.total*tg || got.Count != w.n {
			t.Errorf("merchant %d: %+v, want %+v", i, got, w)
		}
	}

	// categories: a split transaction counts by parts
	wantC := map[string]int64{CashCategory: 70_000, "Clothing": 40_000, "Gifts": 50_000, "Groceries": 10_000, "Taxi": 1_800, Uncategorized: 500}
	if len(d.Categories) != len(wantC) {
		t.Fatalf("categories: %+v", d.Categories)
	}
	for _, c := range d.Categories {
		eq("category "+c.Label, c.Value, wantC[c.Label]*tg)
	}

	// top days
	if d.TopDays[0].Date.Format("2006-01-02") != "2026-08-08" || d.TopDays[0].Total != 55_000*tg ||
		d.TopDays[0].TopMerchant != "Sportmaster" || d.TopDays[0].Count != 2 {
		t.Errorf("top day: %+v", d.TopDays[0])
	}

	// checks: 500 | 1,800 | 5,000, 10,000 | — | 60,000
	for i, want := range []int64{1, 1, 2, 0, 1} {
		eq("bucket "+d.Checks[i].Label, d.Checks[i].Value, want)
	}

	// biggest purchases — no refunds or cash
	if d.Biggest[0].Merchant != "Sportmaster" || d.Biggest[0].Category != "Clothing" || d.Biggest[len(d.Biggest)-1].Amount <= 0 {
		t.Errorf("biggest: %+v", d.Biggest)
	}
}

// Savings are excluded the same way in every block: a part is subtracted, a whole
// transaction disappears together with its counts.
func TestExcludeSavings(t *testing.T) {
	st := testStore(t)
	seed(t, st)
	rows := ledger(t, st)
	p := Period{From: time.Date(2026, 8, 1, 0, 0, 0, 0, almaty), To: time.Date(2026, 10, 1, 0, 0, 0, 0, almaty)}

	// Sportmaster 60,000 = Clothing 40,000 + Gifts 20,000: only the part is excluded;
	// the 30,000 withdrawal filed under Gifts is excluded entirely
	d := Build(rows, p, []string{"Gifts"})
	if len(d.Problems) > 0 {
		t.Fatalf("invariants: %v", d.Problems)
	}
	if d.Totals.Spend != 122_300*tg || d.Totals.PurchaseCount != 5 {
		t.Errorf("spend %d count %d", d.Totals.Spend, d.Totals.PurchaseCount)
	}
	for _, m := range d.Merchants {
		if m.Name == "Sportmaster" && m.Total != 35_000*tg {
			t.Errorf("sportmaster %d", m.Total)
		}
	}
	for _, c := range d.Categories {
		if c.Label == "Gifts" {
			t.Error("excluded category still shown")
		}
	}
	if d.Weekdays[0].Label != "Saturday" || d.Weekdays[0].Value != 85_000*tg {
		t.Errorf("saturday %+v", d.Weekdays[0])
	}

	// Magnum 10,000 is entirely Groceries: it disappears, Magnum #12 for 5,000 remains
	d = Build(rows, p, []string{"Groceries", "Quote ' in a name"})
	if len(d.Problems) > 0 {
		t.Fatalf("invariants: %v", d.Problems)
	}
	if d.Totals.Spend != 162_300*tg || d.Totals.PurchaseCount != 4 {
		t.Errorf("spend %d count %d", d.Totals.Spend, d.Totals.PurchaseCount)
	}
	for _, m := range d.Merchants {
		if m.Name == "Magnum" && (m.Total != 5_000*tg || m.Count != 1) {
			t.Errorf("magnum %+v", m)
		}
	}
}

// A transfer with a category (rent) is spending: in the total, categories and top merchants,
// but not in purchases, checks or transfers to people.
func TestCategorizedTransfer(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	rent, _ := st.AddCategory(ctx, "Rent")
	at := day(9, 29)
	mk := func(key string, amount int64, kind, raw string) int64 {
		id, _, _ := st.InsertTx(ctx, store.Tx{ExternalKey: key, OccurredAt: at, AmountMinor: amount * tg, Currency: "KZT",
			MerchantRaw: raw, MerchantNorm: merchant.Normalize(raw), Source: store.SourceImport, Kind: kind, Status: store.StatusInfo, CreatedAt: at})
		return id
	}
	mk("r", 260_000, kaspi.Transfer, "Adam S.")
	mk("f", 5_000, kaspi.Transfer, "Friend F.")
	mk("p", 1_000, kaspi.Purchase, "SHOP")
	st.SetStatus(ctx, 3, store.StatusReview)
	if n, err := st.CategorizeTransfers(ctx, "Adam S.", "adam s", rent, 3, at); err != nil || n != 1 {
		t.Fatalf("categorize: %d %v", n, err)
	}
	d := Build(ledger(t, st), Period{From: day(9, 1), To: day(10, 1)}, nil)
	if len(d.Problems) > 0 {
		t.Fatalf("%v", d.Problems)
	}
	tot := d.Totals
	if tot.Spend != 261_000*tg || tot.TransferSpend != 260_000*tg || tot.Purchases != 1_000*tg || tot.PurchaseCount != 1 || tot.TransfersOut != 5_000*tg {
		t.Fatalf("totals: %+v", tot)
	}
	if d.Merchants[0].Name != "Adam S." || len(d.Biggest) != 1 {
		t.Fatalf("merchants %+v biggest %+v", d.Merchants, d.Biggest)
	}
	found := false
	for _, c := range d.Categories {
		found = found || (c.Label == "Rent" && c.Value == 260_000*tg)
	}
	if !found {
		t.Fatalf("categories: %+v", d.Categories)
	}

	// undo — not spending again
	if err := st.UncategorizeTransfers(ctx, "Adam S.", "adam s"); err != nil {
		t.Fatal(err)
	}
	d = Build(ledger(t, st), Period{From: day(9, 1), To: day(10, 1)}, nil)
	if d.Totals.Spend != 1_000*tg || d.Totals.TransfersOut != 265_000*tg {
		t.Fatalf("after undo: %+v", d.Totals)
	}
}

func TestReconcile(t *testing.T) {
	st := testStore(t)
	seed(t, st)
	rows := ledger(t, st)
	si := store.StatementInfo{From: time.Date(2026, 8, 1, 0, 0, 0, 0, almaty), To: time.Date(2026, 9, 30, 0, 0, 0, 0, almaty),
		Summary: map[string]int64{"Покупки": -70_500 * tg, "Снятия": -100_000 * tg, "Переводы": -100_000 * tg, "Пополнения": 300_000 * tg}}
	rec := Reconcile(rows, si)
	if !rec.OK || len(rec.Lines) != 4 {
		t.Fatalf("reconcile: %+v", rec)
	}
	si.Summary["Покупки"] -= 1
	if rec := Reconcile(rows, si); rec.OK {
		t.Fatal("a one-tiyn difference must be reported")
	}
}

func TestPeriods(t *testing.T) {
	first, last := time.Date(2025, 10, 5, 0, 0, 0, 0, almaty), time.Date(2026, 10, 4, 0, 0, 0, 0, almaty)
	ps := Periods(first, last)
	byKey := map[string]Period{}
	for _, p := range ps {
		byKey[p.Key] = p
	}
	if p := byKey["12m"]; p.From != first || p.To != last.AddDate(0, 0, 1) {
		t.Errorf("12m: %s – %s", p.From, p.To)
	}
	if p := byKey["30d"]; p.Days() != 30 {
		t.Errorf("30d: %d days", p.Days())
	}
	if p := byKey["2026-10"]; p.Days() != 4 {
		t.Errorf("current month must end at last day: %d days", p.Days())
	}
	if p := byKey["2025-10"]; p.From != first {
		t.Errorf("first month must start at first day: %s", p.From)
	}
	if _, ok := byKey["2025-09"]; ok {
		t.Error("no month before data")
	}
	if len(ps) != 4+13 {
		t.Errorf("periods: %d", len(ps))
	}
}
