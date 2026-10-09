package analytics_test

import (
	"testing"
	"time"

	. "spendbot/internal/analytics"
	"spendbot/internal/kaspi"
	"spendbot/internal/store"
)

func passRow(id int64, at time.Time, kind, details string, amount int64) store.LedgerRow {
	return store.LedgerRow{TxID: id, At: at, Amount: amount * tg, Currency: "KZT", Merchant: details,
		Kind: kind, Status: store.StatusInfo, Source: store.SourceImport}
}

func passCash(id int64, at time.Time, amount int64) store.LedgerRow {
	r := passRow(id, at, kaspi.Withdrawal, "", amount)
	r.Status = store.StatusReview
	return r
}

func TestMarkPassThrough(t *testing.T) {
	day0 := time.Date(2026, 3, 2, 0, 0, 0, 0, almaty) // statements give dates only
	rows := []store.LedgerRow{
		// covered whole by own money from another bank the day before
		passRow(1, day0, kaspi.TopUp, "С карты другого банка", -1_000_000),
		passCash(2, day0.AddDate(0, 0, 1), 1_000_000),
		// a cash deposit and its withdrawal on one day; the withdrawal is listed first
		passCash(3, day0.AddDate(0, 0, 3), 200_000),
		passRow(4, day0.AddDate(0, 0, 3), kaspi.TopUp, "В Kaspi Банкомате", -200_000),
		// salary is income: cash from it is spending
		passRow(5, day0.AddDate(0, 0, 10), kaspi.TopUp, kaspi.Salary, -500_000),
		passCash(6, day0.AddDate(0, 0, 10), 100_000),
		// money from a person, partly cashed out
		passRow(7, day0.AddDate(0, 0, 20), kaspi.TopUp, "Aigerim A.", -300_000),
		passCash(8, day0.AddDate(0, 0, 21), 120_000),
		// not covered whole: stays spending, and does not use up the money before it
		passCash(9, day0.AddDate(0, 0, 22), 400_000),
		// money put on the card too long ago does not cover it
		passRow(10, day0.AddDate(0, 1, 0), kaspi.TopUp, "С карты другого банка", -50_000),
		passCash(11, day0.AddDate(0, 1, 8), 50_000),
		// a withdrawal the person gave a category stays spending
		passRow(12, day0.AddDate(0, 2, 0), kaspi.TopUp, "С карты другого банка", -70_000),
		passCash(13, day0.AddDate(0, 2, 0), 70_000),
	}
	rows[len(rows)-1].Categories, rows[len(rows)-1].Splits = []string{"Rent"}, []int64{70_000 * tg}
	MarkPassThrough(rows, nil)

	want := map[int64]int64{1: 1_000_000, 2: 1_000_000, 3: 200_000, 4: 200_000, 7: 120_000, 8: 120_000}
	for _, r := range rows {
		if r.PassThrough != want[r.TxID]*tg {
			t.Errorf("tx %d: passed %d, want %d", r.TxID, r.PassThrough/tg, want[r.TxID])
		}
		if r.Kind == kaspi.Withdrawal && IsSpend(r) == (want[r.TxID] > 0) {
			t.Errorf("tx %d: spending %v", r.TxID, IsSpend(r))
		}
	}
	all := Period{From: day0, To: day0.AddDate(1, 0, 0)}
	if sum, n := PassedThrough(rows, all); sum != 1_320_000*tg || n != 3 {
		t.Errorf("passed through %d in %d", sum/tg, n)
	}
	// spending and its drill-downs leave the passed cash out
	d := Build(rows, all, nil)
	if d.Totals.Cash != (100_000+400_000+50_000+70_000)*tg {
		t.Errorf("cash %d", d.Totals.Cash/tg)
	}
	if ops := Operations(rows, Filter{Period: all, Merchant: CashKey}); ops.Total != d.Totals.Cash {
		t.Errorf("cash operations %d", ops.Total/tg)
	}
	// money from a person that left as cash is not money from people
	fl := CashFlow(rows, FlowFilter{Period: all, People: true})
	if fl.PeopleIn != 180_000*tg || fl.Passed != 1_320_000*tg || fl.PassedN != 3 {
		t.Errorf("people in %d, passed %d in %d", fl.PeopleIn/tg, fl.Passed/tg, fl.PassedN)
	}
	if counted, skipped := Withdrawals(rows, all); counted != 6 || skipped != 0 {
		t.Errorf("withdrawals %d counted, %d skipped", counted, skipped)
	}

	// an income source is not passing through, and marking again starts over
	MarkPassThrough(rows, map[string]bool{"С карты другого банка": true})
	if rows[0].PassThrough != 0 || rows[1].PassThrough != 0 || !IsSpend(rows[1]) {
		t.Errorf("income source: %+v %+v", rows[0], rows[1])
	}
}
