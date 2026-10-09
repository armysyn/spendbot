// Package analytics computes the numbers of the analytics page. The math runs in Go over
// operation rows, so the source — SQLite or ClickHouse — does not affect the result. All
// blocks share one definition of spending, and the total is cross-checked: sums by month,
// weekday and category must equal it, and sums by operation kind must equal the totals of
// imported statements.
package analytics

import (
	"fmt"
	"sort"
	"time"
	"unicode/utf8"

	"spendbot/internal/kaspi"
	"spendbot/internal/merchant"
	"spendbot/internal/store"
)

// Names of the pseudo-categories and the merchant row for cash.
const (
	CashCategory  = "Cash"
	Uncategorized = "Uncategorized"
	CashMerchant  = "Cash withdrawal"
)

const (
	cash     = kaspi.Withdrawal
	transfer = kaspi.Transfer
)

// IsSpend reports spending: a purchase (refunds negative) or a cash withdrawal in tenge,
// unless the person skipped it or the cash only passed through the card (MarkPassThrough).
// Transfers to people, top-ups and incoming money are not spending — except transfers the
// person gave a category (rent and such): those are done. Wallet payments and manual entries
// have no statement kind.
func IsSpend(r store.LedgerRow) bool {
	if r.Currency != "KZT" || r.Status == store.StatusIgnored || r.PassThrough > 0 && r.Kind == cash {
		return false
	}
	switch r.Kind {
	case kaspi.Purchase, cash:
		return true
	case "":
		return r.Source == store.SourceWallet || r.Source == store.SourceManual
	case transfer:
		return r.Status == store.StatusDone
	}
	return false
}

// Period is the half-open interval [From, To) in the report time zone.
type Period struct {
	Key   string
	Title string
	From  time.Time
	To    time.Time
}

func (p Period) contains(t time.Time) bool { return !t.Before(p.From) && t.Before(p.To) }

// Days is the number of calendar days in the period.
func (p Period) Days() int {
	return int(p.To.Sub(p.From).Hours()/24 + 0.5)
}

func startOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// Bounds returns the first and last day with spending (periods are built from the data:
// a statement may end before today).
func Bounds(rows []store.LedgerRow) (first, last time.Time, ok bool) {
	for _, r := range rows {
		if !IsSpend(r) {
			continue
		}
		if !ok || r.At.Before(first) {
			first = r.At
		}
		if !ok || r.At.After(last) {
			last = r.At
		}
		ok = true
	}
	return startOfDay(first), startOfDay(last), ok
}

func MonthTitle(t time.Time) string { return t.Format("January 2006") }

// Periods are the period choices built from the data, up to the last day with spending.
// Months go from the latest to the earliest.
func Periods(first, last time.Time) []Period {
	end := last.AddDate(0, 0, 1)
	clamp := func(t time.Time) time.Time {
		if t.Before(first) {
			return first
		}
		return t
	}
	ps := []Period{
		{Key: "30d", Title: "30 days", From: clamp(end.AddDate(0, 0, -30)), To: end},
		{Key: "90d", Title: "90 days", From: clamp(end.AddDate(0, 0, -90)), To: end},
		{Key: "12m", Title: "12 months", From: clamp(end.AddDate(-1, 0, 0)), To: end},
		{Key: "all", Title: "all time", From: first, To: end},
	}
	for m := time.Date(last.Year(), last.Month(), 1, 0, 0, 0, 0, last.Location()); m.AddDate(0, 1, 0).After(first); m = m.AddDate(0, -1, 0) {
		ps = append(ps, Period{Key: m.Format("2006-01"), Title: MonthTitle(m), From: clamp(m), To: minTime(m.AddDate(0, 1, 0), end)})
	}
	return ps
}

// Recent are the periods around today: the day itself, the calendar week from Monday and the
// calendar month from the 1st, each up to the end of today. They follow the clock, not the
// data: if the last statement ends earlier, they show what is imported so far.
func Recent(now, first time.Time) []Period {
	today := startOfDay(now)
	end := today.AddDate(0, 0, 1)
	clamp := func(t time.Time) time.Time {
		if t.Before(first) && first.Before(end) {
			return first
		}
		return t
	}
	monday := today.AddDate(0, 0, -((int(today.Weekday()) + 6) % 7))
	month := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, today.Location())
	return []Period{
		{Key: "today", Title: "today", From: today, To: end},
		{Key: "week", Title: "this week", From: clamp(monday), To: end},
		{Key: "month", Title: "this month", From: clamp(month), To: end},
	}
}

// IsMonth reports a calendar month period ("2026-09"); the rest are ranges such as 30 days.
func IsMonth(p Period) bool { return isMonthKey(p.Key) }

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

// Totals are the headline numbers of a period (tiyn).
type Totals struct {
	Spend         int64 // net purchases + cash + transfers counted as spending
	Purchases     int64 // net purchases (refunds subtracted)
	PurchaseCount int64 // number of purchases (refunds excluded)
	PurchaseGross int64 // purchases before refunds
	Refunds       int64 // refunds, as a positive number
	Cash          int64
	CashCount     int64
	// TransferSpend is transfers to people marked as spending (rent and such).
	TransferSpend      int64
	TransferSpendCount int64
	Uncategorized      int64 // spending without a category
	TransfersOut       int64 // transfers to people without a category — not spending, for reference
	Income             int64 // card top-ups, for reference
}

func (t Totals) AvgCheck() int64 {
	if t.PurchaseCount == 0 {
		return 0
	}
	return t.PurchaseGross / t.PurchaseCount
}

// Bar is one row or column of a chart.
type Bar struct {
	Label string
	Sub   string // small caption: operation count, average check
	Value int64
	Count int64
	Avg   int64 // for weekdays — the average per such day
	Sum   int64 // for check sizes: the sum of purchases in the bucket (Value is their count)
	Pct   float64
	Share float64 // share of the period's spending, %
	// From and To bound the column in time (months and trend), for drill-down links.
	From, To time.Time
}

type Merchant struct {
	Key   string // normalized name, CashKey for cash, empty when unnamed
	Name  string
	Count int64
	Total int64
	Avg   int64
	Share float64
	Pct   float64
}

type Day struct {
	Date        time.Time
	Total       int64
	Count       int64
	TopMerchant string
	TopAmount   int64
	Pct         float64
}

type Purchase struct {
	Key      string // normalized merchant name
	Date     time.Time
	Merchant string
	Amount   int64
	Category string
}

type Dashboard struct {
	Period     Period
	Excluded   []string // categories excluded from spending (savings)
	Totals     Totals
	PerDay     int64 // average per day over the period
	Months     []Bar
	Trend      []Bar  // by day for short periods, by week for longer ones
	TrendUnit  string // "day" or "week"
	Weekdays   []Bar  // by total, descending
	Categories []Bar
	Merchants  []Merchant
	TopDays    []Day
	Checks     []Bar // check size distribution
	Biggest    []Purchase
	// Problems lists broken internal invariants; empty when everything adds up.
	Problems []string
}

// display is the merchant name for the page: "MAGNUM CASH&CARRY" → "Magnum Cash&Carry".
func display(raw string) string {
	if raw == "" {
		return ""
	}
	return merchant.Display(raw)
}

func isPurchase(r store.LedgerRow) bool { return r.Kind != cash && r.Kind != transfer }

// spendInPeriod returns the period's spending with exclude subtracted: a transaction fully
// in excluded categories disappears, a split one loses only the excluded parts.
func spendInPeriod(rows []store.LedgerRow, p Period, exclude []string) []store.LedgerRow {
	ex := map[string]bool{}
	for _, c := range exclude {
		ex[c] = true
	}
	var out []store.LedgerRow
	for _, r := range rows {
		if !IsSpend(r) || !p.contains(r.At) {
			continue
		}
		if len(ex) > 0 && len(r.Categories) > 0 {
			var cats []string
			var splits []int64
			amount := r.Amount
			for i, c := range r.Categories {
				if ex[c] {
					amount -= r.Splits[i]
					continue
				}
				cats, splits = append(cats, c), append(splits, r.Splits[i])
			}
			if len(cats) == 0 {
				continue
			}
			r.Amount, r.Categories, r.Splits = amount, cats, splits
		}
		out = append(out, r)
	}
	return out
}

// Build computes all blocks of the page for a period. exclude lists categories not to count
// as spending (savings); empty counts everything.
func Build(all []store.LedgerRow, p Period, exclude []string) Dashboard {
	d := Dashboard{Period: p, Excluded: exclude}
	rows := spendInPeriod(all, p, exclude)
	d.totals(all, rows)
	d.months(rows)
	d.trend(rows)
	d.weekdays(rows)
	d.categories(rows)
	d.merchants(rows)
	d.topDays(rows)
	d.checks(rows)
	d.biggest(rows)
	if days := p.Days(); days > 0 {
		d.PerDay = d.Totals.Spend / int64(days)
	}
	d.Problems = d.invariants()
	return d
}

func (d *Dashboard) invariants() []string {
	var out []string
	sum := func(bars []Bar) int64 {
		var s int64
		for _, b := range bars {
			s += b.Value
		}
		return s
	}
	for _, c := range []struct {
		name string
		bars []Bar
	}{{"month", d.Months}, {"trend", d.Trend}, {"weekday", d.Weekdays}, {"category", d.Categories}} {
		if s := sum(c.bars); s != d.Totals.Spend {
			out = append(out, fmt.Sprintf("sum by %s %d does not equal the total %d", c.name, s, d.Totals.Spend))
		}
	}
	if d.Totals.Purchases+d.Totals.Cash+d.Totals.TransferSpend != d.Totals.Spend {
		out = append(out, "purchases + cash + transfer spending do not equal the total")
	}
	return out
}

func (d *Dashboard) totals(all, rows []store.LedgerRow) {
	t := &d.Totals
	for _, r := range rows {
		t.Spend += r.Amount
		switch r.Kind {
		case cash:
			t.Cash += r.Amount
			t.CashCount++
		case transfer:
			t.TransferSpend += r.Amount
			t.TransferSpendCount++
		default:
			t.Purchases += r.Amount
			if r.Amount > 0 {
				t.PurchaseCount++
				t.PurchaseGross += r.Amount
			} else {
				t.Refunds -= r.Amount
			}
		}
		if len(r.Categories) == 0 && r.Kind != cash {
			t.Uncategorized += r.Amount
		}
	}
	// Transfers to people and top-ups are not spending: they are counted over all operations.
	for _, r := range all {
		if r.Currency != "KZT" || !d.Period.contains(r.At) {
			continue
		}
		switch {
		case r.Kind == transfer && r.Status != store.StatusDone:
			t.TransfersOut += r.Amount
		case r.Kind == kaspi.TopUp:
			t.Income -= r.Amount
		}
	}
}

func shortMonth(m time.Time) string {
	return m.Format("Jan 06")
}

func (d *Dashboard) months(rows []store.LedgerRow) {
	type agg struct{ total, n int64 }
	byMonth := map[time.Time]agg{}
	for _, r := range rows {
		m := time.Date(r.At.Year(), r.At.Month(), 1, 0, 0, 0, 0, r.At.Location())
		a := byMonth[m]
		a.total += r.Amount
		a.n++
		byMonth[m] = a
	}
	// Every month of the period, empty ones too — an empty column is information as well.
	loc := d.Period.From.Location()
	for m := time.Date(d.Period.From.Year(), d.Period.From.Month(), 1, 0, 0, 0, 0, loc); m.Before(d.Period.To); m = m.AddDate(0, 1, 0) {
		a := byMonth[m]
		sub := fmt.Sprintf("%d operations", a.n)
		if m.Before(d.Period.From) || m.AddDate(0, 1, 0).After(d.Period.To) {
			sub += " · partial month"
		}
		d.Months = append(d.Months, Bar{Label: shortMonth(m), Sub: sub, Value: a.total, Count: a.n,
			From: m, To: m.AddDate(0, 1, 0)})
	}
	scale(d.Months, d.Totals.Spend)
}

var weekdayNames = map[time.Weekday]string{
	time.Monday: "Monday", time.Tuesday: "Tuesday", time.Wednesday: "Wednesday", time.Thursday: "Thursday",
	time.Friday: "Friday", time.Saturday: "Saturday", time.Sunday: "Sunday",
}

func (d *Dashboard) weekdays(rows []store.LedgerRow) {
	var total, n [7]int64
	for _, r := range rows {
		total[r.At.Weekday()] += r.Amount
		n[r.At.Weekday()]++
	}
	// The average divides by the number of such days in the period's calendar, not by days
	// with spending: otherwise a weekday with one big purchase would look the most expensive.
	var occur [7]int64
	for t := d.Period.From; t.Before(d.Period.To); t = t.AddDate(0, 0, 1) {
		occur[t.Weekday()]++
	}
	for wd := time.Sunday; wd <= time.Saturday; wd++ {
		b := Bar{Label: weekdayNames[wd], Value: total[wd], Count: n[wd]}
		if occur[wd] > 0 {
			b.Avg = total[wd] / occur[wd]
		}
		b.Sub = fmt.Sprintf("%d such days · %d operations", occur[wd], n[wd])
		d.Weekdays = append(d.Weekdays, b)
	}
	sort.SliceStable(d.Weekdays, func(i, j int) bool { return d.Weekdays[i].Value > d.Weekdays[j].Value })
	scale(d.Weekdays, d.Totals.Spend)
}

func (d *Dashboard) categories(rows []store.LedgerRow) {
	type agg struct{ total, n int64 }
	byCat := map[string]agg{}
	add := func(cat string, amount int64) {
		a := byCat[cat]
		a.total += amount
		a.n++
		byCat[cat] = a
	}
	// A split transaction counts by parts. A cash withdrawal with a category (savings, say)
	// goes to that category; one without a category goes to "Cash".
	for _, r := range rows {
		if len(r.Categories) == 0 {
			if r.Kind == cash {
				add(CashCategory, r.Amount)
			} else {
				add(Uncategorized, r.Amount)
			}
			continue
		}
		for i, c := range r.Categories {
			add(c, r.Splits[i])
		}
	}
	for c, a := range byCat {
		d.Categories = append(d.Categories, Bar{Label: c, Value: a.total, Count: a.n, Sub: fmt.Sprintf("%d operations", a.n)})
	}
	sort.Slice(d.Categories, func(i, j int) bool {
		if d.Categories[i].Value != d.Categories[j].Value {
			return d.Categories[i].Value > d.Categories[j].Value
		}
		return d.Categories[i].Label < d.Categories[j].Label
	})
	scale(d.Categories, d.Totals.Spend)
}

// shorter prefers the shorter merchant name ("Magnum" over "Magnum #12"), alphabetical on ties.
func shorter(a, b string) bool {
	la, lb := utf8.RuneCountInString(a), utf8.RuneCountInString(b)
	if la != lb {
		return la < lb
	}
	return a < b
}

func (d *Dashboard) merchants(rows []store.LedgerRow) {
	// Merchants are grouped by normalized name ("MAGNUM CASH&CARRY #12" and "Magnum Cash & Carry"
	// are one merchant); all ATMs are one "Cash withdrawal" row.
	type agg struct {
		name     string
		n, total int64
	}
	byKey := map[string]*agg{}
	var keys []string
	for _, r := range rows {
		key, name := r.MerchantNorm, display(r.Merchant)
		switch {
		case r.Kind == cash:
			key, name = CashKey, CashMerchant
		case key == "":
			name = "Unnamed"
		}
		a, ok := byKey[key]
		if !ok {
			a = &agg{name: name}
			byKey[key] = a
			keys = append(keys, key)
		} else if key != CashKey && key != "" && shorter(name, a.name) {
			a.name = name
		}
		a.total += r.Amount
		if r.Amount > 0 {
			a.n++
		}
	}
	sort.SliceStable(keys, func(i, j int) bool {
		a, b := byKey[keys[i]], byKey[keys[j]]
		if a.total != b.total {
			return a.total > b.total
		}
		return a.name < b.name
	})
	if len(keys) > 25 {
		keys = keys[:25]
	}
	var max int64
	if len(keys) > 0 {
		max = byKey[keys[0]].total
	}
	for _, k := range keys {
		a := byKey[k]
		m := Merchant{Key: k, Name: a.name, Count: a.n, Total: a.total}
		if a.n > 0 {
			m.Avg = a.total / a.n
		}
		if d.Totals.Spend > 0 {
			m.Share = float64(a.total) * 100 / float64(d.Totals.Spend)
		}
		if max > 0 && a.total > 0 {
			m.Pct = float64(a.total) * 100 / float64(max)
		}
		d.Merchants = append(d.Merchants, m)
	}
}

func (d *Dashboard) topDays(rows []store.LedgerRow) {
	byDay := map[time.Time]*Day{}
	for _, r := range rows {
		day := startOfDay(r.At)
		x, ok := byDay[day]
		if !ok {
			x = &Day{Date: day}
			byDay[day] = x
		}
		x.Total += r.Amount
		x.Count++
		name := display(r.Merchant)
		if r.Kind == cash {
			name = CashMerchant
		}
		if x.Count == 1 || r.Amount > x.TopAmount {
			x.TopMerchant, x.TopAmount = name, r.Amount
		}
	}
	for _, x := range byDay {
		d.TopDays = append(d.TopDays, *x)
	}
	sort.Slice(d.TopDays, func(i, j int) bool {
		if d.TopDays[i].Total != d.TopDays[j].Total {
			return d.TopDays[i].Total > d.TopDays[j].Total
		}
		return d.TopDays[i].Date.Before(d.TopDays[j].Date)
	})
	if len(d.TopDays) > 10 {
		d.TopDays = d.TopDays[:10]
	}
	for i := range d.TopDays {
		if top := d.TopDays[0].Total; top > 0 && d.TopDays[i].Total > 0 {
			d.TopDays[i].Pct = float64(d.TopDays[i].Total) * 100 / float64(top)
		}
	}
}

// Check size buckets in tiyn; the upper bound is exclusive.
var checkBuckets = []struct {
	label string
	upTo  int64
}{
	{"under 1K ₸", 1_000_00}, {"1–5K ₸", 5_000_00}, {"5–20K ₸", 20_000_00},
	{"20–50K ₸", 50_000_00}, {"50K ₸ and more", 1 << 62},
}

func (d *Dashboard) checks(rows []store.LedgerRow) {
	n := make([]int64, len(checkBuckets))
	sum := make([]int64, len(checkBuckets))
	var all int64
	for _, r := range rows {
		if !isPurchase(r) || r.Amount <= 0 {
			continue
		}
		for i, b := range checkBuckets {
			if r.Amount < b.upTo {
				n[i]++
				sum[i] += r.Amount
				all++
				break
			}
		}
	}
	var max int64
	for _, c := range n {
		if c > max {
			max = c
		}
	}
	for i, b := range checkBuckets {
		bar := Bar{Label: b.label, Value: n[i], Count: n[i], Sum: sum[i], Sub: fmt.Sprintf("%d purchases", n[i])}
		if all > 0 {
			bar.Share = float64(n[i]) * 100 / float64(all)
		}
		if max > 0 {
			bar.Pct = float64(n[i]) * 100 / float64(max)
		}
		d.Checks = append(d.Checks, bar)
	}
}

func (d *Dashboard) biggest(rows []store.LedgerRow) {
	var ps []store.LedgerRow
	for _, r := range rows {
		if isPurchase(r) && r.Amount > 0 {
			ps = append(ps, r)
		}
	}
	sort.SliceStable(ps, func(i, j int) bool { return ps[i].Amount > ps[j].Amount })
	if len(ps) > 10 {
		ps = ps[:10]
	}
	for _, r := range ps {
		cat := Uncategorized
		if len(r.Categories) > 0 {
			cat = r.Categories[0]
		}
		d.Biggest = append(d.Biggest, Purchase{Key: r.MerchantNorm, Date: r.At, Merchant: display(r.Merchant), Amount: r.Amount, Category: cat})
	}
}

// scale sets bar lengths relative to the maximum and shares of the period's spending.
func scale(bars []Bar, total int64) {
	var max int64
	for _, b := range bars {
		if b.Value > max {
			max = b.Value
		}
	}
	for i := range bars {
		if max > 0 && bars[i].Value > 0 {
			bars[i].Pct = float64(bars[i].Value) * 100 / float64(max)
		}
		if total > 0 {
			bars[i].Share = float64(bars[i].Value) * 100 / float64(total)
		}
	}
}

// Reconciliation compares a statement's header totals with the database, one line per total.
type Reconciliation struct {
	Statement store.StatementInfo
	Lines     []ReconLine
	OK        bool
}

type ReconLine struct {
	Label     string // English name of the header total
	Statement int64  // as printed in the statement, signed
	Database  int64
	OK        bool
}

// Reconcile compares the statement header totals with sums of operations of the same kind
// over the statement period — regardless of status, because the bank counts everything.
func Reconcile(rows []store.LedgerRow, si store.StatementInfo) Reconciliation {
	p := Period{From: si.From, To: si.To.AddDate(0, 0, 1)}
	byKind := map[string]int64{}
	for _, r := range rows {
		if r.Kind != "" && p.contains(r.At) {
			byKind[r.Kind] -= r.Amount // statements show spending as negative
		}
	}
	rec := Reconciliation{Statement: si, OK: true}
	for _, sm := range kaspi.Summaries {
		want, ok := si.Summary[sm.Label]
		if !ok {
			continue
		}
		line := ReconLine{Label: sm.English, Statement: want, Database: byKind[sm.Kind]}
		line.OK = line.Statement == line.Database
		rec.OK = rec.OK && line.OK
		rec.Lines = append(rec.Lines, line)
	}
	return rec
}
