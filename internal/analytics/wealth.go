package analytics

import (
	"sort"
	"strings"
	"time"

	"spendbot/internal/kaspi"
	"spendbot/internal/store"
)

// Wealth is net worth over time, the cushion in months and the debt load.
//
//   - Net worth on a date = every asset's latest value on or before it − every debt's.
//     The Kaspi Gold balance comes by itself from the statements (start and end of each).
//   - Cushion = liquid assets now / what went out in an average month over the last 12.
//   - Debt load = what went to lenders over the last 12 months / income over them. Lenders
//     are recognised by their names (LOAN, KREDIT, MFO…) and by categories marked as debt
//     payments.

// Series is a holding's values over time.
type Series struct {
	Name   string
	Kind   string
	Debt   bool
	Liquid bool
	Auto   bool // from statements, not typed in
	Points []store.HoldingValue
}

// At is the series' latest value on or before t.
func (s Series) At(t time.Time) (int64, bool) {
	i := sort.Search(len(s.Points), func(i int) bool { return s.Points[i].On.After(t) })
	if i == 0 {
		return 0, false
	}
	return s.Points[i-1].Amount, true
}

// WealthPoint is net worth at the end of a month.
type WealthPoint struct {
	Month   time.Time // the month; the values are at its last day
	Assets  int64
	Debts   int64
	Liquid  int64
	Net     int64
	Partial bool // the current month: values up to today
}

type Lender struct {
	Name   string
	Amount int64
	Count  int
}

type WealthReport struct {
	Points      []WealthPoint
	HasData     bool // at least one asset or debt has a value, or a statement printed a balance
	Today       time.Time
	Now         WealthPoint
	YearAgo     WealthPoint
	HasYearAgo  bool
	Change      Change // net worth against a year ago
	Cushion     float64
	HasCushion  bool
	MonthOut    int64 // what went out in an average month over the last 12
	DebtPaid    int64 // to lenders over the last 12 months
	Income12    int64
	DebtLoad    float64 // %
	HasDebtLoad bool
	Lenders     []Lender
}

// CardSeries turns statement balances into one liquid series per card.
func CardSeries(bs []store.CardBalance) []Series {
	by := map[string]*Series{}
	var order []string
	for _, b := range bs {
		s, ok := by[b.Account]
		if !ok {
			s = &Series{Name: "Kaspi Gold " + b.Account, Kind: "cash", Liquid: true, Auto: true}
			by[b.Account] = s
			order = append(order, b.Account)
		}
		s.Points = append(s.Points, store.HoldingValue{On: b.On, Amount: b.Amount})
	}
	var out []Series
	for _, a := range order {
		s := by[a]
		sort.SliceStable(s.Points, func(i, j int) bool { return s.Points[i].On.Before(s.Points[j].On) })
		out = append(out, *s)
	}
	return out
}

// isLoanPayment reports money paid to a lender, and how much of it.
func loanPart(r store.LedgerRow, debtCats map[string]bool) int64 {
	if r.Amount <= 0 || r.Currency != "KZT" || r.Status == store.StatusIgnored {
		return 0
	}
	switch r.Kind {
	case kaspi.Purchase, kaspi.Transfer, "":
	default:
		return 0
	}
	var part int64
	for i, c := range r.Categories {
		if debtCats[c] {
			part += r.Splits[i]
		}
	}
	if part > 0 {
		return part
	}
	if IsLoan(r.Merchant) {
		return r.Amount
	}
	return 0
}

// BuildWealth computes the report up to today. flow is the cash flow of the last 12 months
// (for the cushion and the debt load).
func BuildWealth(series []Series, all []store.LedgerRow, debtCats []string, flow Flow, today time.Time) WealthReport {
	rep := WealthReport{Today: today}
	loc := today.Location()
	var first time.Time
	for _, s := range series {
		if len(s.Points) > 0 && (first.IsZero() || s.Points[0].On.Before(first)) {
			first = s.Points[0].On
		}
	}
	value := func(at time.Time) WealthPoint {
		var p WealthPoint
		for _, s := range series {
			v, ok := s.At(at)
			if !ok {
				continue
			}
			if s.Debt {
				p.Debts += v
			} else {
				p.Assets += v
				if s.Liquid {
					p.Liquid += v
				}
			}
		}
		p.Net = p.Assets - p.Debts
		return p
	}
	rep.Now.Month = today
	if !first.IsZero() {
		rep.HasData = true
		cur := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, loc)
		for m := time.Date(first.Year(), first.Month(), 1, 0, 0, 0, 0, loc); !m.After(cur); m = m.AddDate(0, 1, 0) {
			end := m.AddDate(0, 1, -1)
			partial := false
			if end.After(today) {
				end, partial = today, true
			}
			p := value(end)
			p.Month, p.Partial = m, partial
			rep.Points = append(rep.Points, p)
		}
		rep.Now = value(today)
		rep.Now.Month = today
		ago := today.AddDate(-1, 0, 0)
		if !ago.Before(first) {
			rep.YearAgo, rep.HasYearAgo = value(ago), true
			rep.YearAgo.Month = ago
			rep.Change = Compare(rep.Now.Net, rep.YearAgo.Net)
			if rep.YearAgo.Net <= 0 {
				rep.Change.OK = false
			}
		}
	}
	if n := int64(len(flow.Months)); n > 0 {
		rep.MonthOut = flow.Out / n
	}
	// the cushion means something only once some money is known to be at hand
	if rep.MonthOut > 0 && rep.Now.Liquid > 0 {
		rep.Cushion, rep.HasCushion = float64(rep.Now.Liquid)/float64(rep.MonthOut), true
	}
	cats := map[string]bool{}
	for _, c := range debtCats {
		cats[c] = true
	}
	from := today.AddDate(-1, 0, 0)
	lenders := map[string]*Lender{}
	for _, r := range all {
		if r.At.Before(from) || r.At.After(today.AddDate(0, 0, 1)) {
			continue
		}
		if part := loanPart(r, cats); part > 0 {
			rep.DebtPaid += part
			name := strings.TrimSpace(r.Merchant)
			if name == "" {
				name = "Unnamed"
			}
			l := lenders[name]
			if l == nil {
				l = &Lender{Name: display(name)}
				lenders[name] = l
			}
			l.Amount += part
			l.Count++
		}
	}
	for _, l := range lenders {
		rep.Lenders = append(rep.Lenders, *l)
	}
	sort.Slice(rep.Lenders, func(i, j int) bool { return rep.Lenders[i].Amount > rep.Lenders[j].Amount })
	rep.Income12 = flow.Income
	if rep.Income12 > 0 {
		rep.DebtLoad, rep.HasDebtLoad = float64(rep.DebtPaid)*100/float64(rep.Income12), true
	}
	return rep
}
