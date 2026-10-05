package analytics_test

import (
	"testing"
	"time"

	. "spendbot/internal/analytics"
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
