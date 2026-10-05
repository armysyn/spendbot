package report

import (
	"strings"
	"testing"
	"time"

	"spendbot/internal/store"
)

var almaty, _ = time.LoadLocation("Asia/Almaty")

// Monday, 5 October 2026
var now = time.Date(2026, 10, 5, 14, 0, 0, 0, almaty)

func TestRanges(t *testing.T) {
	cases := []struct {
		arg      string
		from, to string
	}{
		{"", "2026-10-01", "2026-11-01"},
		{"week", "2026-10-05", "2026-10-12"},
		{"last week", "2026-09-28", "2026-10-05"},
		{"month", "2026-10-01", "2026-11-01"},
		{"Last  month", "2026-09-01", "2026-10-01"},
		{"2026-02", "2026-02-01", "2026-03-01"},
		{"today", "2026-10-05", "2026-10-06"},
		{"неделя", "2026-10-05", "2026-10-12"}, // Russian input still works
	}
	for _, c := range cases {
		r, err := ParseRange(c.arg, now, Month)
		if err != nil {
			t.Errorf("%q: %v", c.arg, err)
			continue
		}
		if r.From.Format("2006-01-02") != c.from || r.To.Format("2006-01-02") != c.to || r.From.Location() != almaty {
			t.Errorf("%q: %s – %s", c.arg, r.From, r.To)
		}
	}
	if _, err := ParseRange("yesterday", now, Month); err == nil {
		t.Error("unknown range must fail")
	}
	// Sunday belongs to the week that started on Monday
	if w := Week(time.Date(2026, 10, 11, 23, 0, 0, 0, almaty)); w.From.Day() != 5 {
		t.Errorf("sunday week starts %s", w.From)
	}
	if got := Month(now).Title; got != "October 2026" {
		t.Errorf("month title %q", got)
	}
}

func TestSummary(t *testing.T) {
	got := Summary(Month(now), []store.CategoryTotal{
		{Category: "Groceries", Currency: "KZT", Minor: 300000, Count: 3},
		{Category: "Taxi", Currency: "KZT", Minor: 100000, Count: 2},
		{Category: "", Currency: "KZT", Minor: 4500, Count: 1},
		{Category: "Subscriptions", Currency: "USD", Minor: 1250, Count: 1},
	})
	for _, want := range []string{"October 2026", "Total: 4,000\u00a0₸", "Groceries — 3,000\u00a0₸ · 75%",
		"Uncategorized — 45\u00a0₸ (1)", "Total: 12.50\u00a0$"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q:\n%s", want, got)
		}
	}
	if strings.Index(got, "₸") > strings.Index(got, "$") {
		t.Error("KZT must go first")
	}
}

func TestCSV(t *testing.T) {
	out := string(CSV([]store.ExportRow{{
		Tx:       store.Tx{ID: 7, OccurredAt: time.Date(2026, 10, 5, 9, 32, 0, 0, time.UTC), Currency: "KZT", MerchantRaw: "MAGNUM, CASH", Source: "wallet", Status: "done"},
		Category: "Gifts", AmountMinor: 200000, SplitNote: "for mom",
	}}, almaty))
	want := "7,2026-10-05,14:32,2000.00,KZT,Gifts,\"Magnum, Cash\",,wallet,done,for mom\n"
	if !strings.HasPrefix(out, "\ufeffid,") || !strings.HasSuffix(out, want) {
		t.Fatalf("csv:\n%s", out)
	}
}
