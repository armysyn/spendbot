package analytics

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"spendbot/internal/kaspi"
	"spendbot/internal/store"
)

// Prev is the period to compare with: the previous calendar month cut to the same number
// of days for a month (so a month in progress is compared with the same days of the last
// one), otherwise the same number of days right before.
func (p Period) Prev() Period {
	days := p.Days()
	if isMonthKey(p.Key) {
		m := time.Date(p.From.Year(), p.From.Month(), 1, 0, 0, 0, 0, p.From.Location())
		from := m.AddDate(0, -1, 0)
		return Period{Key: from.Format("2006-01"), Title: MonthTitle(from), From: from, To: minTime(from.AddDate(0, 0, days), m)}
	}
	return Period{Key: "prev", Title: "previous " + p.Title, From: p.From.AddDate(0, 0, -days), To: p.From}
}

func isMonthKey(k string) bool {
	_, err := time.Parse("2006-01", k)
	return err == nil
}

// DayPeriod returns the one-day period of t.
func DayPeriod(t time.Time) Period {
	d := startOfDay(t)
	return Period{Key: "custom", Title: d.Format("2 Jan 2006"), From: d, To: d.AddDate(0, 0, 1)}
}

// Change compares a number with the same number of the previous period.
type Change struct {
	Prev int64
	Diff int64
	Pct  float64 // relative change, %; meaningless when !OK
	OK   bool    // the previous value is positive, so the percentage makes sense
}

func Compare(cur, prev int64) Change {
	c := Change{Prev: prev, Diff: cur - prev}
	if prev > 0 {
		c.OK = true
		c.Pct = float64(cur-prev) * 100 / float64(prev)
	}
	return c
}

// trend splits the period into days (up to two months) or Monday weeks. Every slot is
// present, empty ones too, and slots are cut to the period.
func (d *Dashboard) trend(rows []store.LedgerRow) {
	p := d.Period
	if p.Days() <= 0 {
		return
	}
	d.TrendUnit = "day"
	start := p.From
	step := func(t time.Time) time.Time { return t.AddDate(0, 0, 1) }
	if p.Days() > 62 {
		d.TrendUnit = "week"
		start = p.From.AddDate(0, 0, -((int(p.From.Weekday()) + 6) % 7))
		step = func(t time.Time) time.Time { return t.AddDate(0, 0, 7) }
	}
	for t := start; t.Before(p.To); t = step(t) {
		b := Bar{From: maxTime(t, p.From), To: minTime(step(t), p.To)}
		if d.TrendUnit == "day" {
			b.Label = t.Format("2 Jan")
		} else {
			b.Label = "week of " + b.From.Format("2 Jan")
		}
		d.Trend = append(d.Trend, b)
	}
	for _, r := range rows {
		i := sort.Search(len(d.Trend), func(i int) bool { return r.At.Before(d.Trend[i].To) })
		if i < len(d.Trend) {
			d.Trend[i].Value += r.Amount
			d.Trend[i].Count++
		}
	}
	for i := range d.Trend {
		d.Trend[i].Sub = fmt.Sprintf("%d operations", d.Trend[i].Count)
	}
	scale(d.Trend, d.Totals.Spend)
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// Operation types for Filter.Type.
const (
	TypeSpend     = "spend"     // spending as the analytics page counts it
	TypeTransfers = "transfers" // transfers to people and top-ups from them
	TypeAll       = "all"       // everything
)

// CashKey is the merchant key of cash withdrawals in Filter.Merchant: all ATMs are one merchant.
const CashKey = "#cash"

// Filter selects operations for the operations page.
type Filter struct {
	Period   Period
	Type     string   // TypeSpend (default), TypeTransfers or TypeAll
	Category string   // a category name, CashCategory or Uncategorized; spending only
	Merchant string   // a normalized merchant name or CashKey
	Query    string   // words that must all appear in the merchant or category name
	Exclude  []string // savings categories not counted as spending
}

// Op is a row of the operations page.
type Op struct {
	TxID        int64
	At          time.Time
	Merchant    string
	MerchantKey string
	Kind        string
	Status      string
	Source      string
	Amount      int64 // spending positive; with a category filter, only that category's part
	Full        int64 // the whole operation
	Categories  []string
	Spend       bool
}

// Editable reports whether the category of the operation can be changed: purchases, cash
// and transfers can, incoming money cannot.
func (o Op) Editable() bool {
	switch o.Kind {
	case "", kaspi.Purchase, cash, transfer:
		return true
	}
	return false
}

// Ops is the filtered list with its sum. For spending, the sum equals the number the
// analytics page shows for the same category, merchant or column.
type Ops struct {
	Ops   []Op
	Total int64
	In    int64 // incoming money among them, as a positive number
}

func merchantKey(r store.LedgerRow) string {
	if r.Kind == cash {
		return CashKey
	}
	return r.MerchantNorm
}

func merchantName(r store.LedgerRow) string {
	switch {
	case r.Kind == cash:
		return CashMerchant
	case r.Merchant == "":
		return "Unnamed"
	}
	return display(r.Merchant)
}

// Operations returns the operations matching f, newest first.
func Operations(all []store.LedgerRow, f Filter) Ops {
	var rows []store.LedgerRow
	spend := f.Type == "" || f.Type == TypeSpend
	if spend {
		rows = spendInPeriod(all, f.Period, f.Exclude)
	} else {
		for _, r := range all {
			if !f.Period.contains(r.At) {
				continue
			}
			if f.Type == TypeTransfers && r.Kind != transfer && r.Kind != kaspi.TopUp {
				continue
			}
			rows = append(rows, r)
		}
	}
	terms := strings.Fields(Fold(f.Query))
	var out Ops
	for _, r := range rows {
		if f.Merchant != "" && merchantKey(r) != f.Merchant {
			continue
		}
		amount := r.Amount
		if spend && f.Category != "" {
			amount = categoryPart(r, f.Category)
			if amount == 0 && !inCategory(r, f.Category) {
				continue
			}
		}
		o := Op{TxID: r.TxID, At: r.At, Merchant: merchantName(r), MerchantKey: merchantKey(r), Kind: r.Kind,
			Status: r.Status, Source: r.Source, Amount: amount, Full: r.Amount, Categories: r.Categories,
			Spend: spend || IsSpend(r)}
		if len(terms) > 0 && !matches(o, terms) {
			continue
		}
		out.Ops = append(out.Ops, o)
		if amount < 0 && !o.Spend {
			out.In -= amount
		} else {
			out.Total += amount
		}
	}
	sort.SliceStable(out.Ops, func(i, j int) bool {
		if !out.Ops[i].At.Equal(out.Ops[j].At) {
			return out.Ops[i].At.After(out.Ops[j].At)
		}
		return out.Ops[i].TxID > out.Ops[j].TxID
	})
	return out
}

// inCategory reports whether a spending row belongs to a category, pseudo-categories included.
func inCategory(r store.LedgerRow, cat string) bool {
	if len(r.Categories) == 0 {
		if r.Kind == cash {
			return cat == CashCategory
		}
		return cat == Uncategorized
	}
	for _, c := range r.Categories {
		if c == cat {
			return true
		}
	}
	return false
}

// categoryPart is how much of a spending row is in a category — the same split the
// categories chart uses.
func categoryPart(r store.LedgerRow, cat string) int64 {
	if len(r.Categories) == 0 {
		if inCategory(r, cat) {
			return r.Amount
		}
		return 0
	}
	var sum int64
	for i, c := range r.Categories {
		if c == cat {
			sum += r.Splits[i]
		}
	}
	return sum
}

func matches(o Op, terms []string) bool {
	hay := Fold(o.Merchant + " " + strings.Join(o.Categories, " "))
	for _, t := range terms {
		if !strings.Contains(hay, t) {
			return false
		}
	}
	return true
}

// Fold prepares text for search: lower case, Kazakh letters mapped to Russian ones
// ("Әлия" is found by "алия"), ё to е, dots to spaces.
func Fold(s string) string { return foldReplacer.Replace(strings.ToLower(s)) }

var foldReplacer = strings.NewReplacer(
	"ә", "а", "ə", "а", "ғ", "г", "қ", "к", "ң", "н", "ө", "о", "ұ", "у", "ү", "у", "һ", "х", "і", "и", "ё", "е", ".", " ",
)

// CategoryUsage is a category's spending over a period, for the categories page.
type CategoryUsage struct {
	Total int64
	Count int64
}

// UsageByCategory sums spending per category over a period (savings included).
func UsageByCategory(all []store.LedgerRow, p Period) map[string]CategoryUsage {
	out := map[string]CategoryUsage{}
	for _, r := range spendInPeriod(all, p, nil) {
		for i, c := range r.Categories {
			u := out[c]
			u.Total += r.Splits[i]
			u.Count++
			out[c] = u
		}
	}
	return out
}
