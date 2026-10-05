package web

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"spendbot/internal/analytics"
	"spendbot/internal/store"
)

// The income page: stated salaries over a life, salary arrivals Kaspi marks itself, other
// income sources, and what was left of income after spending month by month.

type incomeData struct {
	Page
	Flash       string
	Empty       bool
	Periods     []analytics.Period
	Current     analytics.Period
	Custom      bool
	From, To    string
	WithSavings bool // count savings categories as spending
	People      bool // money between you and people counts
	Basis       string
	Flow        analytics.Flow
	Salaries    []store.SalaryPeriod
	SalaryNow   int64
	Employers   []string
	LastArrival *analytics.Op
	Candidates  []analytics.IncomeCandidate
	ThisMonth   string // 'YYYY-MM', a default for the forms
	State       string
	ShownMonths []analytics.MonthFlow // newest first, for the table
}

func (s *Server) income(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	d := &incomeData{Flash: q.Get("msg"), WithSavings: q.Get("withsave") == "1", Basis: q.Get("basis"), People: q.Get("people") != "0",
		ThisMonth: s.now().In(s.loc).Format("2006-01")}
	var err error
	if d.Salaries, err = s.st.SalaryPeriods(ctx); err != nil {
		s.fail(w, err)
		return
	}
	var current []store.SalaryPeriod
	d.SalaryNow, current = analytics.CurrentSalary(d.Salaries)
	for _, p := range current {
		if p.Employer != "" {
			d.Employers = append(d.Employers, p.Employer)
		}
	}
	names, err := s.st.IncomeSources(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	sources := map[string]bool{}
	for _, n := range names {
		sources[n] = true
	}
	rows, err := s.st.Ledger(ctx, s.loc)
	if err != nil {
		s.fail(w, err)
		return
	}
	sq := cloneValues(q)
	sq.Del("msg")
	d.State = sq.Encode()
	first, last, ok := bounds(rows)
	if !ok {
		d.Empty = true
		s.show(w, r, "income.html", "Income", "income", d)
		return
	}
	d.Periods = analytics.Periods(first, last)
	if q.Get("period") == "" && q.Get("from") == "" {
		q.Set("period", "12m")
	}
	f, custom := filterFrom(q, d.Periods, s.loc)
	d.Current, d.Custom = f.Period, custom
	if custom {
		d.From, d.To = q.Get("from"), q.Get("to")
	}
	var exclude []string
	if !d.WithSavings {
		if exclude, err = s.st.SavingsNames(ctx); err != nil {
			s.fail(w, err)
			return
		}
	}
	d.Flow = analytics.CashFlow(rows, analytics.FlowFilter{Period: d.Current, Exclude: exclude, Sources: sources,
		Salaries: d.Salaries, Basis: d.Basis, People: d.People})
	d.Basis = d.Flow.Basis
	for i := len(d.Flow.Months) - 1; i >= 0; i-- {
		d.ShownMonths = append(d.ShownMonths, d.Flow.Months[i])
	}
	// the latest salary over all time, not only the period
	all := analytics.CashFlow(rows, analytics.FlowFilter{Period: analytics.Period{From: first, To: last.AddDate(0, 0, 1)}})
	if len(all.Arrivals) > 0 {
		d.LastArrival = &all.Arrivals[0]
	}
	d.Candidates = analytics.IncomeCandidates(rows, sources, last)
	s.show(w, r, "income.html", "Income", "income", d)
}

// parseMonth reads 'YYYY-MM'.
func parseMonth(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if _, err := time.Parse("2006-01", v); err != nil {
		return "", false
	}
	return v, true
}

// incomeAction saves and deletes stated salaries and marks income sources.
func (s *Server) incomeAction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	back := safeBack(r.PostFormValue("back"), "/ui/income")
	done := func(msg string) { http.Redirect(w, r, withMsg(back, msg), http.StatusSeeOther) }
	id, _ := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	switch r.PostFormValue("action") {
	case "salary_save":
		from, ok := parseMonth(r.PostFormValue("from"))
		if !ok {
			done("Pick the first month of this salary.")
			return
		}
		to := ""
		if v := strings.TrimSpace(r.PostFormValue("to")); v != "" {
			if to, ok = parseMonth(v); !ok || to < from {
				done("The last month must be the same as the first one or later; leave it empty for the current salary.")
				return
			}
		}
		amount, ok := parseAmount(r.PostFormValue("amount"))
		if !ok || amount <= 0 {
			done("Type the monthly salary, for example 450000 or 450k.")
			return
		}
		p := store.SalaryPeriod{ID: id, From: from, To: to, AmountMinor: int64(amount*100 + 0.5),
			Employer: trimTo(r.PostFormValue("employer"), 60), Note: trimTo(r.PostFormValue("note"), 200)}
		if _, err := s.st.SaveSalaryPeriod(ctx, p); err != nil {
			s.fail(w, err)
			return
		}
		done("Salary saved.")
	case "salary_delete":
		if err := s.st.DeleteSalaryPeriod(ctx, id); err != nil {
			s.fail(w, err)
			return
		}
		done("Salary period deleted.")
	case "source_on", "source_off":
		name := strings.TrimSpace(r.PostFormValue("name"))
		on := r.PostFormValue("action") == "source_on"
		if err := s.st.SetIncomeSource(ctx, name, on, s.now()); err != nil {
			s.fail(w, err)
			return
		}
		if on {
			done("Top-ups from " + name + " now count as income.")
		} else {
			done("Top-ups from " + name + " no longer count as income.")
		}
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
	}
}

func trimTo(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
