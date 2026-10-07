package analytics

import (
	"sort"
	"time"

	"spendbot/internal/store"
)

// Trends are the long view over all the data: savings rate by year, income against what went
// out, spending structure by year, categories growing faster than income, seasonality and the
// 12-month rolling average. Income and "went out" are the cash flow of the Income page, so the
// numbers agree; structure, seasonality and the rolling average are spending only.

// YearRate is a calendar year of the cash flow.
type YearRate struct {
	Year    int
	Partial bool // the data covers only part of the year
	Months  int
	Income  int64
	Out     int64
	Net     int64
	Rate    float64 // Net / Income, %; meaningless when !HasRate
	HasRate bool
	Pct     float64 // |Rate| relative to the largest, for bars
}

// CatPart is a category's share of a year's spending.
type CatPart struct {
	Name  string
	Value int64
	Share float64 // of the year's spending, %
}

// CatYear is the spending structure of a year: the top categories over all years, then the rest.
type CatYear struct {
	Year    int
	Partial bool
	Total   int64
	Parts   []CatPart
}

// Growth compares a category over the last 12 months with the 12 months before.
type Growth struct {
	Name         string
	Before, Now  int64
	Pct          float64 // growth, %; meaningless when !OK
	OK           bool    // Before is positive
	Share        float64 // of the last 12 months' spending, %
	FasterIncome bool    // grew faster than income and matters (over 2% of spending)
}

// Season is a month of the year averaged over the years that have it in full.
type Season struct {
	Month time.Month
	Avg   int64
	Years int
	Index float64 // Avg relative to the average month, %: 135 — a third more than usual
	Pct   float64 // for bars
}

// Rolling is a month's spending and the average of the 12 months ending with it.
type Rolling struct {
	Month   time.Time
	Spend   int64
	Avg     int64 // 0 until 12 months of data
	HasAvg  bool
	Partial bool
}

type Trends struct {
	Flow         Flow // month by month, as on the Income page
	Years        []YearRate
	Top          []string // categories in the structure, the largest first; the rest is OtherCategory
	Structure    []CatYear
	Growth       []Growth
	IncomeGrowth Change // income, last 12 months against the 12 before
	HasGrowth    bool   // there are 24 months of data
	Seasons      []Season
	AvgMonth     int64 // the average full month of spending, for seasonality
	Rolling      []Rolling
	Last12       Change // spending, last 12 months against the 12 before
}

// OtherCategory collects the categories outside the top in the structure.
const OtherCategory = "Other"

// TopCategories is how many categories the structure shows before "Other".
const TopCategories = 7

// BuildTrends computes the trends from first to last (days with data, both included).
func BuildTrends(all []store.LedgerRow, f FlowFilter, first, last time.Time) Trends {
	end := startOfDay(last).AddDate(0, 0, 1)
	f.Period = Period{Key: "all", From: startOfDay(first), To: end}
	t := Trends{Flow: CashFlow(all, f)}
	t.years()
	t.structure(all, f)
	t.growth(all, f, end)
	t.seasons(all, f)
	t.rolling()
	return t
}

func (t *Trends) years() {
	idx := map[int]int{}
	for _, m := range t.Flow.Months {
		y := m.Month.Year()
		i, ok := idx[y]
		if !ok {
			i = len(t.Years)
			idx[y] = i
			t.Years = append(t.Years, YearRate{Year: y})
		}
		yr := &t.Years[i]
		yr.Months++
		yr.Partial = yr.Partial || m.Partial
		yr.Income += m.Income
		yr.Out += m.Out
		yr.Net += m.Net
	}
	var max float64
	for i := range t.Years {
		yr := &t.Years[i]
		yr.Partial = yr.Partial || yr.Months < 12
		if yr.Income > 0 {
			yr.HasRate = true
			yr.Rate = float64(yr.Net) * 100 / float64(yr.Income)
			if a := abs64f(yr.Rate); a > max {
				max = a
			}
		}
	}
	for i := range t.Years {
		if max > 0 && t.Years[i].HasRate {
			t.Years[i].Pct = abs64f(t.Years[i].Rate) * 100 / max
		}
	}
}

func abs64f(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

// byCategory sums spending per category the way the categories chart does.
func byCategory(rows []store.LedgerRow, add func(cat string, amount int64, r store.LedgerRow)) {
	for _, r := range rows {
		if len(r.Categories) == 0 {
			if r.Kind == cash {
				add(CashCategory, r.Amount, r)
			} else {
				add(Uncategorized, r.Amount, r)
			}
			continue
		}
		for i, c := range r.Categories {
			add(c, r.Splits[i], r)
		}
	}
}

func (t *Trends) structure(all []store.LedgerRow, f FlowFilter) {
	total := map[string]int64{}
	byYear := map[int]map[string]int64{}
	byCategory(spendInPeriod(all, f.Period, f.Exclude), func(cat string, amount int64, r store.LedgerRow) {
		total[cat] += amount
		y := r.At.Year()
		if byYear[y] == nil {
			byYear[y] = map[string]int64{}
		}
		byYear[y][cat] += amount
	})
	for c, v := range total {
		if v > 0 {
			t.Top = append(t.Top, c)
		}
	}
	sort.Slice(t.Top, func(i, j int) bool {
		if total[t.Top[i]] != total[t.Top[j]] {
			return total[t.Top[i]] > total[t.Top[j]]
		}
		return t.Top[i] < t.Top[j]
	})
	if len(t.Top) > TopCategories {
		t.Top = t.Top[:TopCategories]
	}
	inTop := map[string]bool{}
	for _, c := range t.Top {
		inTop[c] = true
	}
	for _, yr := range t.Years {
		cy := CatYear{Year: yr.Year, Partial: yr.Partial}
		var other int64
		for c, v := range byYear[yr.Year] {
			cy.Total += v
			if !inTop[c] {
				other += v
			}
		}
		for _, c := range t.Top {
			cy.Parts = append(cy.Parts, CatPart{Name: c, Value: byYear[yr.Year][c]})
		}
		cy.Parts = append(cy.Parts, CatPart{Name: OtherCategory, Value: other})
		for i := range cy.Parts {
			if cy.Total > 0 && cy.Parts[i].Value > 0 {
				cy.Parts[i].Share = float64(cy.Parts[i].Value) * 100 / float64(cy.Total)
			}
		}
		t.Structure = append(t.Structure, cy)
	}
}

// growth compares the last 12 months with the 12 before, for income and every category.
func (t *Trends) growth(all []store.LedgerRow, f FlowFilter, end time.Time) {
	now := Period{From: end.AddDate(-1, 0, 0), To: end}
	before := Period{From: end.AddDate(-2, 0, 0), To: now.From}
	if before.From.Before(f.Period.From) {
		return
	}
	t.HasGrowth = true
	income := func(p Period) int64 {
		g := f
		g.Period = p
		return CashFlow(all, g).Income
	}
	t.IncomeGrowth = Compare(income(now), income(before))
	sums := func(p Period) (map[string]int64, int64) {
		out := map[string]int64{}
		var total int64
		byCategory(spendInPeriod(all, p, f.Exclude), func(cat string, amount int64, _ store.LedgerRow) {
			out[cat] += amount
			total += amount
		})
		return out, total
	}
	nowBy, nowTotal := sums(now)
	beforeBy, beforeTotal := sums(before)
	t.Last12 = Compare(nowTotal, beforeTotal)
	names := map[string]bool{}
	for c := range nowBy {
		names[c] = true
	}
	for c := range beforeBy {
		names[c] = true
	}
	for c := range names {
		g := Growth{Name: c, Before: beforeBy[c], Now: nowBy[c]}
		ch := Compare(g.Now, g.Before)
		g.Pct, g.OK = ch.Pct, ch.OK
		if nowTotal > 0 {
			g.Share = float64(g.Now) * 100 / float64(nowTotal)
		}
		incomePct := t.IncomeGrowth.Pct
		if !t.IncomeGrowth.OK {
			incomePct = 0
		}
		g.FasterIncome = g.Share >= 2 && (g.OK && g.Pct > incomePct || !g.OK && g.Now > 0)
		if g.Now > 0 || g.Before > 0 {
			t.Growth = append(t.Growth, g)
		}
	}
	sort.Slice(t.Growth, func(i, j int) bool {
		a, b := t.Growth[i], t.Growth[j]
		if a.FasterIncome != b.FasterIncome {
			return a.FasterIncome
		}
		if da, db := a.Now-a.Before, b.Now-b.Before; da != db {
			return da > db
		}
		return a.Name < b.Name
	})
}

// seasons averages each month of the year over the years that have it in full.
func (t *Trends) seasons(all []store.LedgerRow, f FlowFilter) {
	spend := map[time.Time]int64{}
	for _, r := range spendInPeriod(all, f.Period, f.Exclude) {
		spend[time.Date(r.At.Year(), r.At.Month(), 1, 0, 0, 0, 0, r.At.Location())] += r.Amount
	}
	var sum [13]int64
	var n [13]int
	var all12 int64
	var months int
	for _, m := range t.Flow.Months {
		if m.Partial {
			continue
		}
		v := spend[m.Month]
		sum[m.Month.Month()] += v
		n[m.Month.Month()]++
		all12 += v
		months++
	}
	if months == 0 {
		return
	}
	t.AvgMonth = all12 / int64(months)
	var max int64
	for mo := time.January; mo <= time.December; mo++ {
		s := Season{Month: mo, Years: n[mo]}
		if n[mo] > 0 {
			s.Avg = sum[mo] / int64(n[mo])
			if t.AvgMonth > 0 {
				s.Index = float64(s.Avg) * 100 / float64(t.AvgMonth)
			}
		}
		if s.Avg > max {
			max = s.Avg
		}
		t.Seasons = append(t.Seasons, s)
	}
	for i := range t.Seasons {
		if max > 0 && t.Seasons[i].Avg > 0 {
			t.Seasons[i].Pct = float64(t.Seasons[i].Avg) * 100 / float64(max)
		}
	}
}

// rolling is spending per month with the average of the 12 months ending there. A partial
// first month would pull the average down, so the window starts with the first full month.
func (t *Trends) rolling() {
	ms := t.Flow.Months
	start := 0
	if len(ms) > 0 && ms[0].Partial {
		start = 1
	}
	var window int64
	for i, m := range ms {
		r := Rolling{Month: m.Month, Spend: m.Spend, Partial: m.Partial}
		if i >= start {
			window += m.Spend
			if i-start >= 12 {
				window -= ms[i-12].Spend
			}
			if i-start >= 11 && !m.Partial {
				r.Avg, r.HasAvg = window/12, true
			}
		}
		t.Rolling = append(t.Rolling, r)
	}
}
