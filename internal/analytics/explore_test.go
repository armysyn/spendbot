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
