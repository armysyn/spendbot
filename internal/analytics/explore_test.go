package analytics_test

import (
	"context"
	"fmt"
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

// The categories page agrees with the analytics page: same totals and counts per category.
func TestCategoriesMatchDashboard(t *testing.T) {
	st := testStore(t)
	seed(t, st)
	rows := ledger(t, st)
	p := Period{Key: "x", From: time.Date(2026, 9, 1, 0, 0, 0, 0, almaty), To: time.Date(2026, 10, 1, 0, 0, 0, 0, almaty)}
	prev := Period{From: time.Date(2026, 8, 1, 0, 0, 0, 0, almaty), To: p.From}
	for _, exclude := range [][]string{nil, {"Gifts"}} {
		d, pd := Build(rows, p, exclude), Build(rows, prev, exclude)
		rep := Categories(rows, p, prev, true, exclude)
		if rep.Total != d.Totals.Spend {
			t.Errorf("total %d, dashboard %d", rep.Total, d.Totals.Spend)
		}
		got := map[string]CategoryStat{}
		for _, c := range rep.Stats {
			got[c.Name] = c
			var trend int64
			for _, b := range c.Trend {
				trend += b.Value
			}
			if trend != c.Total {
				t.Errorf("%s: sparkline %d, total %d", c.Name, trend, c.Total)
			}
		}
		for _, b := range d.Categories {
			if g := got[b.Label]; g.Total != b.Value || g.Count != b.Count {
				t.Errorf("exclude %v: %s %d (%d), dashboard %d (%d)", exclude, b.Label, g.Total, g.Count, b.Value, b.Count)
			}
		}
		for _, b := range pd.Categories {
			if got[b.Label].Prev != b.Value {
				t.Errorf("%s: prev %d, dashboard %d", b.Label, got[b.Label].Prev, b.Value)
			}
		}
	}
}

func TestCashFlow(t *testing.T) {
	st := testStore(t)
	seed(t, st)
	ctx := context.Background()
	add := func(key, who string, at time.Time, amount int64, kind string) {
		if _, _, err := st.InsertTx(ctx, store.Tx{ExternalKey: key, OccurredAt: at, AmountMinor: amount * tg, Currency: "KZT",
			MerchantRaw: who, MerchantNorm: merchant.Normalize(who), Kind: kind, Source: store.SourceImport,
			Status: store.StatusInfo, CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	add("s1", kaspi.Salary, day(8, 10), -400_000, kaspi.TopUp)
	add("s2", kaspi.Salary, day(9, 10), -400_000, kaspi.TopUp)
	add("c1", "Client C.", day(9, 15), -50_000, kaspi.TopUp)
	rows := ledger(t, st)
	p := Period{From: time.Date(2026, 8, 1, 0, 0, 0, 0, almaty), To: time.Date(2026, 10, 1, 0, 0, 0, 0, almaty)}
	sal := []store.SalaryPeriod{{From: "2026-01", To: "2026-08", AmountMinor: 450_000 * tg}, {From: "2026-09", AmountMinor: 500_000 * tg}}
	d := Build(rows, p, nil)

	fl := CashFlow(rows, FlowFilter{Period: p, Sources: map[string]bool{"Client C.": true}, Salaries: sal})
	if fl.Salary != 800_000*tg || fl.Other != 50_000*tg || fl.Income != 850_000*tg || len(fl.Arrivals) != 2 {
		t.Fatalf("income: %+v", fl)
	}
	if fl.Spend != d.Totals.Spend || fl.Out != fl.Spend {
		t.Errorf("spending %d, analytics %d", fl.Spend, d.Totals.Spend)
	}
	if fl.Declared != 950_000*tg || fl.Months[0].Declared != 450_000*tg || fl.Months[1].Declared != 500_000*tg {
		t.Errorf("declared: %d %+v", fl.Declared, fl.Months)
	}
	if fl.Net != fl.Income-fl.Spend || fl.Months[1].Cumulative != fl.Net {
		t.Errorf("net %d, running %d", fl.Net, fl.Months[1].Cumulative)
	}
	// the stated basis takes salaries as stated
	if dec := CashFlow(rows, FlowFilter{Period: p, Salaries: sal, Basis: BasisDeclared}); dec.Income != 950_000*tg {
		t.Errorf("declared basis: %d", dec.Income)
	}
	// with people: the 100,000 transfer to Aigerim goes out; the 300,000 top-up "From Kaspi
	// Deposit" is own money, not a person, so it does not come back as money from people
	pp := CashFlow(rows, FlowFilter{Period: p, Sources: map[string]bool{"Client C.": true}, People: true})
	if pp.PeopleOut != 100_000*tg || pp.PeopleIn != 0 || pp.Out != fl.Spend+100_000*tg {
		t.Errorf("people: out %d in %d total %d", pp.PeopleOut, pp.PeopleIn, pp.Out)
	}
	if c := IncomeCandidates(rows, map[string]bool{"Client C.": true}, day(9, 30)); len(c) == 0 || c[0].Name != "Client C." || !c[0].Counted {
		t.Errorf("candidates: %+v", c)
	}
}

func TestIncomeKinds(t *testing.T) {
	for d, want := range map[string]bool{"Aigerim A.": true, "Анна-Мария К.": true, "Әлия Б.": true, "Анна Мария Ли Б.": true,
		"С карты другого банка": false, "MFO EXAMPLE LLP": false, "На карту Example Bank*0000": false, "Иван И.,  Example Bank": false} {
		if IsPerson(d) != want {
			t.Errorf("IsPerson(%q) = %v", d, !want)
		}
	}
	if !IsOwnMoney("С карты другого банка") || !IsOwnMoney(`по номеру счета АО "Банк"`) || IsOwnMoney("Aigerim A.") {
		t.Error("own money")
	}
	if !IsLoan("MFO EXAMPLE LLP") || !IsLoan("На карту другого банка LOAN PAYMENT") || IsLoan("Aigerim A.") {
		t.Error("loans")
	}
}

// A salary stated with an end is current while the month is inside it; Auto counts it for a
// month nothing arrived in yet.
func TestStatedSalaryAuto(t *testing.T) {
	ps := []store.SalaryPeriod{{From: "2026-10", To: "2026-12", AmountMinor: 2_300_000 * tg, Employer: "Acme"}}
	if amt, cur := CurrentSalary(ps, "2026-10"); amt != 2_300_000*tg || len(cur) != 1 {
		t.Errorf("current: %d %+v", amt, cur)
	}
	if amt, _ := CurrentSalary(ps, "2027-01"); amt != 0 {
		t.Errorf("after the end: %d", amt)
	}
	st := testStore(t)
	seed(t, st)
	rows := ledger(t, st)
	oct := Period{Key: "2026-10", From: time.Date(2026, 10, 1, 0, 0, 0, 0, almaty), To: time.Date(2026, 10, 6, 0, 0, 0, 0, almaty)}
	if fl := CashFlow(rows, FlowFilter{Period: oct, Salaries: ps}); fl.Income != 2_300_000*tg || !fl.Months[0].Stated || fl.Basis != BasisAuto {
		t.Errorf("auto: %+v", fl)
	}
	if fl := CashFlow(rows, FlowFilter{Period: oct, Salaries: ps, Basis: BasisCard}); fl.Income != 0 {
		t.Errorf("card: %d", fl.Income)
	}
}

func TestRecentPeriods(t *testing.T) {
	now := time.Date(2026, 10, 7, 15, 30, 0, 0, almaty) // a Wednesday
	first := time.Date(2021, 10, 4, 0, 0, 0, 0, almaty)
	byKey := map[string]Period{}
	for _, p := range Recent(now, first) {
		byKey[p.Key] = p
	}
	day := func(p Period) string { return p.From.Format("2006-01-02") + "/" + p.To.Format("2006-01-02") }
	for key, want := range map[string]string{
		"today": "2026-10-07/2026-10-08",
		"week":  "2026-10-05/2026-10-08", // from Monday
		"month": "2026-10-01/2026-10-08",
	} {
		if got := day(byKey[key]); got != want {
			t.Errorf("%s: %s, want %s", key, got, want)
		}
	}
	for key, want := range map[string]string{
		"today": "2026-10-06/2026-10-07", // yesterday
		"week":  "2026-09-28/2026-10-01", // the same Monday to Wednesday a week before
		"month": "2026-09-01/2026-09-08", // the same seven days of September
	} {
		if got := day(byKey[key].Prev()); got != want {
			t.Errorf("prev %s: %s, want %s", key, got, want)
		}
	}
	if IsMonth(byKey["month"]) || !IsMonth(Period{Key: "2026-09"}) {
		t.Error("IsMonth")
	}
}

// Trends add up to the cash flow and to the spending they come from.
func TestTrends(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	food, _ := st.FindCategory(ctx, "Groceries")
	home, _ := st.FindCategory(ctx, "Home")
	k := 0
	add := func(at time.Time, raw, kind string, amount int64, cat int64) {
		k++
		id, _, err := st.InsertTx(ctx, store.Tx{ExternalKey: fmt.Sprint(k), OccurredAt: at, AmountMinor: amount * tg, Currency: "KZT",
			MerchantRaw: raw, MerchantNorm: merchant.Normalize(raw), Kind: kind, Source: store.SourceImport, Status: store.StatusInfo, CreatedAt: at})
		if err != nil {
			t.Fatal(err)
		}
		if cat != 0 {
			st.CloseTx(ctx, id, []store.Split{{CategoryID: cat, AmountMinor: amount * tg}}, false, at)
		}
	}
	// 30 months from mid-2024: salary 100,000 a month; groceries 30,000, home 10,000 rising to 40,000 in the last year
	start := time.Date(2024, 3, 15, 12, 0, 0, 0, almaty)
	for i := range 30 {
		at := start.AddDate(0, i, 0)
		add(at, kaspi.Salary, kaspi.TopUp, -100_000, 0)
		add(at.AddDate(0, 0, 1), "MAGNUM", kaspi.Purchase, 30_000, food.ID)
		h := int64(10_000)
		if i >= 18 {
			h = 40_000
		}
		add(at.AddDate(0, 0, 2), "IKEA", kaspi.Purchase, h, home.ID)
	}
	rows := ledger(t, st)
	first, last, _ := Bounds(append([]store.LedgerRow(nil), rows...))
	for _, r := range rows {
		if r.At.Before(first) {
			first = time.Date(r.At.Year(), r.At.Month(), r.At.Day(), 0, 0, 0, 0, almaty)
		}
	}
	tr := BuildTrends(rows, FlowFilter{}, first, last)

	var inc, out int64
	for _, y := range tr.Years {
		inc += y.Income
		out += y.Out
	}
	if inc != tr.Flow.Income || out != tr.Flow.Out || tr.Flow.Income != 3_000_000*tg {
		t.Fatalf("years add up to the flow: %d/%d, %d/%d", inc, tr.Flow.Income, out, tr.Flow.Out)
	}
	if y := tr.Years[1]; y.Year != 2025 || y.Partial || !y.HasRate || y.Rate != float64(y.Net)*100/float64(y.Income) {
		t.Errorf("2025: %+v", y)
	}
	if !tr.Years[0].Partial || !tr.Years[len(tr.Years)-1].Partial {
		t.Error("first and last years are partial")
	}
	for i, cy := range tr.Structure {
		var sum int64
		for _, p := range cy.Parts {
			sum += p.Value
		}
		if sum != cy.Total || cy.Total != tr.Years[i].Out {
			t.Errorf("structure %d: parts %d, total %d, spent %d", cy.Year, sum, cy.Total, tr.Years[i].Out)
		}
	}
	// home went from 10,000 to 40,000 a month while income stayed: faster than income
	if !tr.HasGrowth || tr.IncomeGrowth.Pct != 0 {
		t.Fatalf("growth: %v %+v", tr.HasGrowth, tr.IncomeGrowth)
	}
	if g := tr.Growth[0]; g.Name != "Home" || !g.FasterIncome || g.Now != 480_000*tg {
		t.Errorf("lifestyle inflation: %+v", g)
	}
	for _, g := range tr.Growth {
		if g.Name == "Groceries" && g.FasterIncome {
			t.Error("groceries did not grow")
		}
	}
	// seasons: every month of the year averaged; the average month is total over full months
	if len(tr.Seasons) != 12 || tr.AvgMonth == 0 {
		t.Fatalf("seasons: %+v", tr.Seasons)
	}
	// the rolling average starts after 12 full months and equals the mean of the window
	var got Rolling
	for _, r := range tr.Rolling {
		if r.HasAvg {
			got = r
			break
		}
	}
	if got.Avg != 40_000*tg {
		t.Errorf("first 12-month average: %+v", got)
	}
}

func TestWealth(t *testing.T) {
	d := func(y int, m time.Month, day int) time.Time { return time.Date(y, m, day, 0, 0, 0, 0, almaty) }
	v := func(t time.Time, a int64) store.HoldingValue { return store.HoldingValue{On: t, Amount: a * tg} }
	series := []Series{
		{Name: "Deposit", Kind: "deposit", Liquid: true, Points: []store.HoldingValue{v(d(2025, 1, 1), 1_000_000), v(d(2026, 1, 1), 2_000_000)}},
		{Name: "Flat", Kind: "property", Points: []store.HoldingValue{v(d(2025, 6, 1), 30_000_000)}},
		{Name: "Mortgage", Kind: "mortgage", Debt: true, Points: []store.HoldingValue{v(d(2025, 6, 1), 20_000_000), v(d(2026, 6, 1), 18_000_000)}},
	}
	series = append(series, CardSeries([]store.CardBalance{{Account: "*1111", On: d(2026, 9, 1), Amount: 100_000 * tg}, {Account: "*1111", On: d(2026, 9, 30), Amount: 300_000 * tg}})...)
	if s := series[3]; !s.Auto || !s.Liquid || len(s.Points) != 2 {
		t.Fatalf("card series: %+v", s)
	}
	rows := []store.LedgerRow{
		{At: d(2026, 3, 5), Amount: 150_000 * tg, Currency: "KZT", Kind: kaspi.Transfer, Merchant: "На карту другого банка LOAN PAYMENT", Status: store.StatusInfo},
		{At: d(2026, 4, 5), Amount: 200_000 * tg, Currency: "KZT", Kind: kaspi.Purchase, Merchant: "BANK", Status: store.StatusDone,
			Categories: []string{"Mortgage", "Home"}, Splits: []int64{120_000 * tg, 80_000 * tg}},
		{At: d(2026, 4, 6), Amount: 50_000 * tg, Currency: "KZT", Kind: kaspi.Purchase, Merchant: "MAGNUM", Status: store.StatusDone, Categories: []string{"Groceries"}, Splits: []int64{50_000 * tg}},
		{At: d(2024, 4, 5), Amount: 99_000 * tg, Currency: "KZT", Kind: kaspi.Transfer, Merchant: "LOAN PAYMENT", Status: store.StatusInfo}, // over a year ago
	}
	flow := Flow{Income: 6_000_000 * tg, Out: 2_400_000 * tg, Months: make([]MonthFlow, 12)}
	rep := BuildWealth(series, rows, []string{"Mortgage"}, flow, d(2026, 10, 8))

	// now: deposit 2M + flat 30M + card 300K − mortgage 18M
	if rep.Now.Assets != 32_300_000*tg || rep.Now.Debts != 18_000_000*tg || rep.Now.Net != 14_300_000*tg || rep.Now.Liquid != 2_300_000*tg {
		t.Errorf("now: %+v", rep.Now)
	}
	// a year ago (8 Oct 2025): deposit 1M + flat 30M − mortgage 20M
	if !rep.HasYearAgo || rep.YearAgo.Net != 11_000_000*tg || !rep.Change.OK {
		t.Errorf("year ago: %+v", rep.YearAgo)
	}
	// cushion: 2.3M liquid / 200K a month
	if !rep.HasCushion || rep.Cushion != 11.5 {
		t.Errorf("cushion: %v", rep.Cushion)
	}
	// debt: the LOAN PAYMENT transfer 150K + the mortgage part 120K, not the old one
	if rep.DebtPaid != 270_000*tg || rep.DebtLoad != 4.5 || len(rep.Lenders) != 2 {
		t.Errorf("debt: %d %v %+v", rep.DebtPaid, rep.DebtLoad, rep.Lenders)
	}
	// monthly points from the first value to now, the last one partial
	if p := rep.Points; len(p) != 22 || !p[len(p)-1].Partial || p[0].Month != d(2025, 1, 1) {
		t.Errorf("points: %d", len(p))
	}
}
