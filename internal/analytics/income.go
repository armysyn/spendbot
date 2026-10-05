package analytics

import (
	"regexp"
	"sort"
	"strings"
	"time"

	"spendbot/internal/kaspi"
	"spendbot/internal/store"
)

// IsSalary reports a salary arrival: Kaspi prints "Зарплата" ("salary") as the details of
// such a top-up, so no guessing is needed.
func IsSalary(r store.LedgerRow) bool {
	return r.Kind == kaspi.TopUp && r.Amount < 0 && strings.HasPrefix(Fold(strings.TrimSpace(r.Merchant)), Fold(kaspi.Salary))
}

// personRe matches how Kaspi shows a person: first name(s) and the initial of the surname,
// "Aigerim A." or "Анна-Мария К.".
var personRe = regexp.MustCompile(`^\p{L}+(?:[ -]\p{L}+)* \p{L}\.$`)

// IsPerson reports whether the details of a transfer or top-up name a person, not a bank,
// a card in another bank or a lender.
func IsPerson(details string) bool { return personRe.MatchString(strings.TrimSpace(details)) }

// IsOwnMoney reports a top-up of the person's own money from elsewhere.
func IsOwnMoney(details string) bool {
	d := Fold(strings.TrimSpace(details))
	for _, p := range kaspi.OwnTopUps {
		if strings.HasPrefix(d, Fold(p)) {
			return true
		}
	}
	return false
}

// IsLoan reports money from or to a lender.
func IsLoan(details string) bool {
	d := strings.ToUpper(details)
	for _, w := range kaspi.LoanWords {
		if strings.Contains(d, w) {
			return true
		}
	}
	return false
}

// Income bases for FlowFilter.Basis.
const (
	BasisCard     = "card"     // salary as it arrived on the card
	BasisDeclared = "declared" // salary as stated, for salaries paid to another bank
)

// FlowFilter sets up a cash flow.
type FlowFilter struct {
	Period   Period
	Exclude  []string        // savings categories not counted as spending
	Sources  map[string]bool // top-up senders counted as income besides salary
	Salaries []store.SalaryPeriod
	Basis    string
	// People counts money between you and people as well: transfers to them (without a
	// category — those with one are already spending) minus top-ups from them that are not
	// income. Only operations naming a person count: own money moved between banks and
	// lenders are neither. Without it only spending leaves.
	People bool
}

// MonthFlow is a month of the cash flow; money in tiyn, positive.
type MonthFlow struct {
	Month      time.Time
	From, To   time.Time // the part of the month inside the period
	Label      string
	Partial    bool
	Salary     int64 // arrived on the card as salary
	SalaryN    int
	Declared   int64 // the stated salary for the month
	Other      int64 // from income sources
	Income     int64 // counted income: salary on the chosen basis plus other income
	Spend      int64
	PeopleOut  int64 // sent to people, not spending
	PeopleIn   int64 // received from people, not income
	Out        int64 // Spend, plus PeopleOut − PeopleIn when people count
	Net        int64 // Income − Out: what was left, negative — a deficit
	Cumulative int64 // Net added up from the start of the period
	Rate       float64
	IncPct     float64 // bar heights relative to the largest income or spending
	SpendPct   float64
	CumPct     float64 // |Cumulative| relative to the largest |Cumulative|
}

// Flow is a cash flow over a period.
type Flow struct {
	Months    []MonthFlow
	Arrivals  []Op // salary arrivals, newest first
	Income    int64
	Salary    int64
	Declared  int64
	Other     int64
	Spend     int64
	PeopleOut int64
	PeopleIn  int64
	Out       int64
	Net       int64
	Rate      float64 // Net / Income, %
	AvgIncome int64   // per month
	AvgSpend  int64   // of Out
	Deficit   int     // months that ended in the red
	Basis     string
	People    bool
}

// DeclaredFor sums the stated salaries covering a month ('YYYY-MM'); an open end runs on.
func DeclaredFor(ps []store.SalaryPeriod, month string) int64 {
	var sum int64
	for _, p := range ps {
		if p.From <= month && (p.To == "" || month <= p.To) {
			sum += p.AmountMinor
		}
	}
	return sum
}

// CurrentSalary is the stated salary of the latest month with one, and its periods.
func CurrentSalary(ps []store.SalaryPeriod) (amount int64, current []store.SalaryPeriod) {
	for _, p := range ps {
		if p.Current() {
			amount += p.AmountMinor
			current = append(current, p)
		}
	}
	return amount, current
}

// CashFlow adds up income and spending by month.
func CashFlow(all []store.LedgerRow, f FlowFilter) Flow {
	fl := Flow{Basis: f.Basis, People: f.People}
	if fl.Basis != BasisDeclared {
		fl.Basis = BasisCard
	}
	p := f.Period
	loc := p.From.Location()
	index := map[string]int{}
	for m := time.Date(p.From.Year(), p.From.Month(), 1, 0, 0, 0, 0, loc); m.Before(p.To); m = m.AddDate(0, 1, 0) {
		next := m.AddDate(0, 1, 0)
		mf := MonthFlow{Month: m, From: maxTime(m, p.From), To: minTime(next, p.To), Label: m.Format("Jan 2006"),
			Partial: m.Before(p.From) || next.After(p.To), Declared: DeclaredFor(f.Salaries, m.Format("2006-01"))}
		index[m.Format("2006-01")] = len(fl.Months)
		fl.Months = append(fl.Months, mf)
	}
	month := func(t time.Time) *MonthFlow {
		if i, ok := index[t.Format("2006-01")]; ok {
			return &fl.Months[i]
		}
		return nil
	}
	for _, r := range all {
		if r.Currency != "KZT" || !p.contains(r.At) {
			continue
		}
		m := month(r.At)
		if m == nil {
			continue
		}
		switch {
		case IsSalary(r):
			m.Salary -= r.Amount
			m.SalaryN++
			fl.Arrivals = append(fl.Arrivals, Op{TxID: r.TxID, At: r.At, Merchant: r.Merchant, Kind: r.Kind, Amount: r.Amount, Full: r.Amount})
		case r.Kind == kaspi.TopUp && r.Amount < 0 && f.Sources[r.Merchant]:
			m.Other -= r.Amount
		case r.Kind == kaspi.TopUp && r.Amount < 0 && IsPerson(r.Merchant):
			m.PeopleIn -= r.Amount
		case r.Kind == kaspi.Transfer && r.Status != store.StatusDone && IsPerson(r.Merchant):
			m.PeopleOut += r.Amount
		}
	}
	for _, r := range spendInPeriod(all, p, f.Exclude) {
		if m := month(r.At); m != nil {
			m.Spend += r.Amount
		}
	}
	var maxBar, maxCum int64
	for i := range fl.Months {
		m := &fl.Months[i]
		salary := m.Salary
		if fl.Basis == BasisDeclared {
			salary = m.Declared
		}
		m.Income = salary + m.Other
		m.Out = m.Spend
		if f.People {
			m.Out += m.PeopleOut - m.PeopleIn
		}
		m.Net = m.Income - m.Out
		if m.Income > 0 {
			m.Rate = float64(m.Net) * 100 / float64(m.Income)
		}
		fl.Net += m.Net
		m.Cumulative = fl.Net
		fl.Income += m.Income
		fl.Salary += m.Salary
		fl.Declared += m.Declared
		fl.Other += m.Other
		fl.Spend += m.Spend
		fl.PeopleOut += m.PeopleOut
		fl.PeopleIn += m.PeopleIn
		fl.Out += m.Out
		if m.Net < 0 {
			fl.Deficit++
		}
		maxBar = max(maxBar, m.Income, m.Out)
		maxCum = max(maxCum, abs(m.Cumulative))
	}
	for i := range fl.Months {
		m := &fl.Months[i]
		if maxBar > 0 {
			m.IncPct = float64(m.Income) * 100 / float64(maxBar)
			m.SpendPct = float64(max(m.Out, 0)) * 100 / float64(maxBar)
		}
		if maxCum > 0 {
			m.CumPct = float64(abs(m.Cumulative)) * 100 / float64(maxCum)
		}
	}
	if fl.Income > 0 {
		fl.Rate = float64(fl.Net) * 100 / float64(fl.Income)
	}
	if n := int64(len(fl.Months)); n > 0 {
		fl.AvgIncome, fl.AvgSpend = fl.Income/n, fl.Out/n
	}
	SortOps(fl.Arrivals, SortNew)
	return fl
}

// IncomeCandidate is a sender of regular top-ups that may be income (a client, a tenant).
type IncomeCandidate struct {
	Name    string
	Months  int // distinct months with a top-up over the last year of data
	Count   int
	Total   int64
	Median  int64
	Last    time.Time
	Counted bool // already counted as income
}

// IncomeCandidates finds senders who topped the card up in at least three different months
// over the last year of data, salary aside; counted sources are always listed. Largest first.
func IncomeCandidates(all []store.LedgerRow, sources map[string]bool, last time.Time) []IncomeCandidate {
	since := last.AddDate(-1, 0, 0)
	type agg struct {
		c       IncomeCandidate
		months  map[string]bool
		amounts []int64
	}
	by := map[string]*agg{}
	for _, r := range all {
		if r.Kind != kaspi.TopUp || r.Amount >= 0 || r.Merchant == "" || IsSalary(r) || r.At.Before(since) ||
			IsOwnMoney(r.Merchant) || IsLoan(r.Merchant) {
			continue
		}
		a, ok := by[r.Merchant]
		if !ok {
			a = &agg{c: IncomeCandidate{Name: r.Merchant, Counted: sources[r.Merchant]}, months: map[string]bool{}}
			by[r.Merchant] = a
		}
		a.months[r.At.Format("2006-01")] = true
		a.amounts = append(a.amounts, -r.Amount)
		a.c.Count++
		a.c.Total -= r.Amount
		if r.At.After(a.c.Last) {
			a.c.Last = r.At
		}
	}
	var out []IncomeCandidate
	for _, a := range by {
		a.c.Months = len(a.months)
		if a.c.Months < 3 && !a.c.Counted {
			continue
		}
		sort.Slice(a.amounts, func(i, j int) bool { return a.amounts[i] < a.amounts[j] })
		a.c.Median = a.amounts[len(a.amounts)/2]
		out = append(out, a.c)
	}
	for name := range sources {
		if _, ok := by[name]; !ok {
			out = append(out, IncomeCandidate{Name: name, Counted: true})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Counted != out[j].Counted {
			return out[i].Counted
		}
		if out[i].Total != out[j].Total {
			return out[i].Total > out[j].Total
		}
		return out[i].Name < out[j].Name
	})
	return out
}
