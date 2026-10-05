package analytics_test

import (
	"context"
	"testing"
	"time"

	. "spendbot/internal/analytics"
	"spendbot/internal/kaspi"
	"spendbot/internal/merchant"
	"spendbot/internal/store"
)

// Drill-down must agree with the charts: the operations of a category, a merchant or a
// column add up to exactly what the analytics page shows for it.
func TestOperationsMatchDashboard(t *testing.T) {
	st := testStore(t)
	seed(t, st)
	rows := ledger(t, st)
	p := Period{Key: "x", From: time.Date(2026, 8, 1, 0, 0, 0, 0, almaty), To: time.Date(2026, 10, 1, 0, 0, 0, 0, almaty)}
	for _, exclude := range [][]string{nil, {"Gifts"}} {
		d := Build(rows, p, exclude)
		all := Operations(rows, Filter{Period: p, Exclude: exclude})
		if all.Total != d.Totals.Spend {
			t.Errorf("exclude %v: all spending %d, dashboard %d", exclude, all.Total, d.Totals.Spend)
		}
		for _, c := range d.Categories {
			if got := Operations(rows, Filter{Period: p, Category: c.Label, Exclude: exclude}); got.Total != c.Value || int64(len(got.Ops)) != c.Count {
				t.Errorf("exclude %v: category %s: ops %d (%d), chart %d (%d)", exclude, c.Label, got.Total, len(got.Ops), c.Value, c.Count)
			}
		}
		for _, m := range d.Merchants {
			if got := Operations(rows, Filter{Period: p, Merchant: m.Key, Exclude: exclude}); got.Total != m.Total {
				t.Errorf("merchant %s: ops %d, table %d", m.Name, got.Total, m.Total)
			}
		}
		for _, b := range append(d.Trend, d.Months...) {
			if got := Operations(rows, Filter{Period: Period{From: b.From, To: b.To}, Exclude: exclude}); got.Total != b.Value {
				t.Errorf("column %s: ops %d, chart %d", b.Label, got.Total, b.Value)
			}
		}
	}
}

func TestOperationsFilters(t *testing.T) {
	st := testStore(t)
	seed(t, st)
	rows := ledger(t, st)
	p := Period{From: time.Date(2026, 8, 1, 0, 0, 0, 0, almaty), To: time.Date(2026, 10, 1, 0, 0, 0, 0, almaty)}

	// a split purchase shows only its part in a category, and the whole amount separately
	gifts := Operations(rows, Filter{Period: p, Category: "Gifts"})
	for _, o := range gifts.Ops {
		if o.Merchant == "Sportmaster" && (o.Amount != 20_000*tg || o.Full != 60_000*tg) {
			t.Errorf("split part: %+v", o)
		}
	}
	// transfers: sent and received, not spending
	tr := Operations(rows, Filter{Period: p, Type: TypeTransfers})
	if len(tr.Ops) != 2 || tr.Total != 100_000*tg || tr.In != 300_000*tg {
		t.Errorf("transfers: %+v", tr)
	}
	// search folds case
	if got := Operations(rows, Filter{Period: p, Query: "sport"}); len(got.Ops) != 2 {
		t.Errorf("search: %+v", got.Ops)
	}
	// newest first
	all := Operations(rows, Filter{Period: p, Type: TypeAll})
	for i := 1; i < len(all.Ops); i++ {
		if all.Ops[i].At.After(all.Ops[i-1].At) {
			t.Fatal("not sorted newest first")
		}
	}
}

func TestPrevPeriod(t *testing.T) {
	m := Period{Key: "2026-03", From: time.Date(2026, 3, 1, 0, 0, 0, 0, almaty), To: time.Date(2026, 3, 11, 0, 0, 0, 0, almaty)}
	if pr := m.Prev(); pr.From.Format("2006-01-02") != "2026-02-01" || pr.To.Format("2006-01-02") != "2026-02-11" {
		t.Errorf("month in progress compares with the same days: %v – %v", pr.From, pr.To)
	}
	full := Period{Key: "2026-03", From: time.Date(2026, 3, 1, 0, 0, 0, 0, almaty), To: time.Date(2026, 4, 1, 0, 0, 0, 0, almaty)}
	if pr := full.Prev(); pr.To.Format("2006-01-02") != "2026-03-01" {
		t.Errorf("a full month compares with the whole previous one, not past it: %v", pr.To)
	}
	d30 := Period{Key: "30d", From: time.Date(2026, 9, 6, 0, 0, 0, 0, almaty), To: time.Date(2026, 10, 6, 0, 0, 0, 0, almaty)}
	if pr := d30.Prev(); pr.Days() != 30 || !pr.To.Equal(d30.From) {
		t.Errorf("30 days: %v – %v", pr.From, pr.To)
	}
	if c := Compare(150, 100); !c.OK || c.Pct != 50 || c.Diff != 50 {
		t.Errorf("compare: %+v", c)
	}
	if c := Compare(150, 0); c.OK {
		t.Error("no percentage against zero")
	}
}

func TestSortStatsGroups(t *testing.T) {
	st := testStore(t)
	seed(t, st)
	rows := ledger(t, st)
	p := Period{From: time.Date(2026, 8, 1, 0, 0, 0, 0, almaty), To: time.Date(2026, 10, 1, 0, 0, 0, 0, almaty)}

	// the amount range compares amounts without the sign: the 5,000 refund is inside 1,000–10,000
	r := Operations(rows, Filter{Period: p, Min: 1_000 * tg, Max: 10_000 * tg})
	if len(r.Ops) != 4 {
		t.Fatalf("range: %+v", r.Ops)
	}
	SortOps(r.Ops, SortBig)
	if r.Ops[0].Amount != 10_000*tg || r.Ops[len(r.Ops)-1].Amount != 1_800*tg {
		t.Errorf("sorted: %+v", r.Ops)
	}
	s := StatsOf(r.Ops)
	if s.Count != 4 || s.Largest != 10_000*tg || s.Smallest != 1_800*tg || s.Median != 5_000*tg || s.Avg != (10_000+5_000+5_000+1_800)*tg/4 {
		t.Errorf("stats: %+v", s)
	}
	// money in only
	in := Operations(rows, Filter{Period: p, Type: TypeAll, Dir: DirIn})
	for _, o := range in.Ops {
		if o.Amount >= 0 {
			t.Errorf("money in: %+v", o)
		}
	}
	// groups add up to the list, months in time order
	all := Operations(rows, Filter{Period: p})
	for _, g := range Groupings {
		var out int64
		n := 0
		for _, x := range GroupOps(all.Ops, g.Key) {
			out += x.Out - x.In
			n += x.Count
		}
		if out != all.Total || n != len(all.Ops) {
			t.Errorf("group %s: %d in %d ops, list %d in %d", g.Key, out, n, all.Total, len(all.Ops))
		}
	}
	if ms := GroupOps(all.Ops, "month"); len(ms) != 2 || ms[0].Label != "August 2026" {
		t.Errorf("months: %+v", ms)
	}
}

// "First time" and "only once" look at the whole history, not at the current filters.
func TestRepeatFilter(t *testing.T) {
	st := testStore(t)
	seed(t, st)
	rows := ledger(t, st)
	p := Period{From: time.Date(2026, 8, 1, 0, 0, 0, 0, almaty), To: time.Date(2026, 10, 1, 0, 0, 0, 0, almaty)}
	names := func(f Filter) map[string]int {
		out := map[string]int{}
		for _, o := range Operations(rows, f).Ops {
			out[o.Merchant]++
		}
		return out
	}
	// Magnum twice (as "MAGNUM" and "MAGNUM #12"), cash three times: only the first of each
	first := names(Filter{Period: p, Repeat: RepeatFirst})
	if first["Magnum"] != 1 || first[CashMerchant] != 1 || first["Yandex Go"] != 1 {
		t.Errorf("first: %v", first)
	}
	once := names(Filter{Period: p, Repeat: RepeatOnce})
	// the Sportmaster purchase and its refund go in different directions: each is the only one of its own
	if once["Magnum"] != 0 || once[CashMerchant] != 0 || once["Yandex Go"] != 1 || once["Sportmaster"] != 2 {
		t.Errorf("once: %v", once)
	}
	// the amount range does not change who counts as new: the second Magnum purchase (5,000)
	// is not a first one even when the first (10,000) is filtered out by the amount
	if got := names(Filter{Period: p, Repeat: RepeatFirst, Max: 6_000 * tg}); got["Magnum"] != 0 {
		t.Errorf("first within an amount range: %v", got)
	}
	tr := Operations(rows, Filter{Period: p, Type: TypeTransfers, Dir: DirOut, Repeat: RepeatOnce, Min: 20_000 * tg, Max: 200_000 * tg})
	if len(tr.Ops) != 1 || tr.Ops[0].Merchant != "Aigerim A." {
		t.Errorf("transfers sent once: %+v", tr.Ops)
	}
}

// On the transfers page an amount range checks each transfer, not a person's total, and the
// counts for "only once" still see every transfer.
func TestPeoplePerOperation(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	add := func(key, who string, day int, amount int64, kind string) {
		_, _, err := st.InsertTx(ctx, store.Tx{ExternalKey: key, OccurredAt: time.Date(2026, 9, day, 12, 0, 0, 0, almaty),
			AmountMinor: amount * tg, Currency: "KZT", MerchantRaw: who, MerchantNorm: merchant.Normalize(who), Kind: kind,
			Source: store.SourceImport, Status: store.StatusInfo, CreatedAt: time.Now()})
		if err != nil {
			t.Fatal(err)
		}
	}
	add("1", "Anna A.", 1, 10_000, kaspi.Transfer)
	add("2", "Anna A.", 2, 130_000, kaspi.Transfer)
	add("3", "Anna A.", 3, -150_000, kaspi.TopUp)
	add("4", "Boris B.", 4, 300_000, kaspi.Transfer)
	add("5", "Vera V.", 5, 140_000, kaspi.Transfer)
	rows := ledger(t, st)
	p := Period{From: time.Date(2026, 9, 1, 0, 0, 0, 0, almaty), To: time.Date(2026, 10, 1, 0, 0, 0, 0, almaty)}

	// Boris's 300,000 is out of range although Anna's total (140,000 sent) would not matter either
	res := People(rows, PeopleFilter{Period: p, Min: 120_000 * tg, Max: 160_000 * tg})
	if len(res.People) != 2 || len(res.Ops) != 3 {
		t.Fatalf("people %+v ops %+v", res.People, res.Ops)
	}
	for _, pp := range res.People {
		if pp.Name == "Anna A." && (pp.Sent != 130_000*tg || pp.SentN != 1 || pp.Received != 150_000*tg) {
			t.Errorf("Anna's sums cover only the matching transfers: %+v", pp)
		}
	}
	// one person, sent only
	anna := People(rows, PeopleFilter{Period: p, Name: "Anna A.", Dir: DirOut, Min: 120_000 * tg, Max: 160_000 * tg})
	if len(anna.Ops) != 1 || anna.Ops[0].Amount != 130_000*tg {
		t.Errorf("Anna, sent 120–160k: %+v", anna.Ops)
	}
	// "sent only once, 120–160k": Anna sent twice, so only Vera
	once := People(rows, PeopleFilter{Period: p, Dir: DirOut, Min: 120_000 * tg, Max: 160_000 * tg, TimesMin: 1, TimesMax: 1})
	if len(once.People) != 1 || once.People[0].Name != "Vera V." {
		t.Errorf("once: %+v", once.People)
	}
}
