package web

import (
	"fmt"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"spendbot/internal/analytics"
	"spendbot/internal/store"
)

// The net worth page: what is owned and owed with values over time, the cushion and the debt load.

type holdingRow struct {
	store.Holding
	Debt   bool
	Latest store.HoldingValue
	Has    bool
}

type wealthData struct {
	Page
	Flash     string
	R         analytics.WealthReport
	Assets    []holdingRow
	Debts     []holdingRow
	Archived  []holdingRow
	Cards     []analytics.Series
	Chart     template.HTML
	Today     string
	AssetKind []string
	DebtKind  []string
	DebtCats  []string
}

var kindTitles = map[string]string{
	"cash": "Cash", "deposit": "Deposit", "investment": "Investments", "property": "Property", "vehicle": "Vehicle",
	"asset": "Other asset", "loan": "Loan", "mortgage": "Mortgage", "credit": "Credit card", "debt": "Other debt",
}

func (s *Server) wealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := s.now().In(s.loc)
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, s.loc)
	d := &wealthData{Flash: r.URL.Query().Get("msg"), Today: today.Format("2006-01-02"), AssetKind: store.AssetKinds, DebtKind: store.DebtKinds}
	hs, err := s.st.Holdings(ctx, s.loc)
	if err != nil {
		s.fail(w, err)
		return
	}
	var series []analytics.Series
	for _, h := range hs {
		row := holdingRow{Holding: h, Debt: store.IsDebtKind(h.Kind)}
		if n := len(h.Values); n > 0 {
			row.Latest, row.Has = h.Values[n-1], true
		}
		switch {
		case h.Archived:
			d.Archived = append(d.Archived, row)
		case row.Debt:
			d.Debts = append(d.Debts, row)
		default:
			d.Assets = append(d.Assets, row)
		}
		series = append(series, analytics.Series{Name: h.Name, Kind: h.Kind, Debt: row.Debt, Liquid: h.Liquid, Points: h.Values})
	}
	cards, err := s.st.CardBalances(ctx, s.loc)
	if err != nil {
		s.fail(w, err)
		return
	}
	d.Cards = analytics.CardSeries(cards)
	series = append(series, d.Cards...)

	rows, err := s.st.Ledger(ctx, s.loc)
	if err != nil {
		s.fail(w, err)
		return
	}
	f := analytics.FlowFilter{People: true, Period: analytics.Period{From: today.AddDate(-1, 0, 0), To: today.AddDate(0, 0, 1)}}
	if f.Exclude, err = s.st.SavingsNames(ctx); err != nil {
		s.fail(w, err)
		return
	}
	names, err := s.st.IncomeSources(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	f.Sources = map[string]bool{}
	for _, n := range names {
		f.Sources[n] = true
	}
	if f.Salaries, err = s.st.SalaryPeriods(ctx); err != nil {
		s.fail(w, err)
		return
	}
	if d.DebtCats, err = s.st.DebtCategories(ctx); err != nil {
		s.fail(w, err)
		return
	}
	d.R = analytics.BuildWealth(series, rows, d.DebtCats, analytics.CashFlow(rows, f), today)
	d.Chart = wealthSVG(d.R.Points)
	s.show(w, r, "wealth.html", "Net worth", "wealth", d)
}

// wealthSVG draws assets, debts and net worth by month.
func wealthSVG(ps []analytics.WealthPoint) template.HTML {
	n := len(ps)
	if n == 0 {
		return ""
	}
	var top, bottom int64 = 1, 0
	for _, p := range ps {
		top = max(top, p.Assets, p.Debts, p.Net)
		bottom = min(bottom, p.Net)
	}
	span := float64(top - bottom)
	step := svgW / float64(n)
	x := func(i int) float64 { return step*float64(i) + step/2 }
	y := func(v int64) float64 { return svgT + (float64(top-v))*(svgH-2*svgT)/span }
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="line-chart" viewBox="0 0 %.0f %.0f" preserveAspectRatio="none" role="img" aria-label="Net worth by month">`, svgW, svgH)
	if bottom < 0 {
		fmt.Fprintf(&b, `<line class="zero" x1="0" x2="%.0f" y1="%.1f" y2="%.1f"/>`, svgW, y(0), y(0))
	}
	// the net worth as an area down to zero, then the three lines
	area := []string{fmt.Sprintf("%.1f,%.1f", x(0), y(0))}
	for i, p := range ps {
		area = append(area, fmt.Sprintf("%.1f,%.1f", x(i), y(p.Net)))
	}
	area = append(area, fmt.Sprintf("%.1f,%.1f", x(n-1), y(0)))
	fmt.Fprintf(&b, `<polygon class="area-net" points="%s"/>`, strings.Join(area, " "))
	line := func(class string, v func(analytics.WealthPoint) int64) {
		var pts []string
		for i, p := range ps {
			pts = append(pts, fmt.Sprintf("%.1f,%.1f", x(i), y(v(p))))
		}
		fmt.Fprintf(&b, `<polyline class="%s" points="%s"/>`, class, strings.Join(pts, " "))
	}
	line("ln-income", func(p analytics.WealthPoint) int64 { return p.Assets })
	line("ln-debt", func(p analytics.WealthPoint) int64 { return p.Debts })
	line("ln-net", func(p analytics.WealthPoint) int64 { return p.Net })
	for i, p := range ps {
		tip := fmt.Sprintf("%s%s · net worth %s · owned %s · owed %s", p.Month.Format("Jan 2006"),
			map[bool]string{true: " (so far)"}[p.Partial], kztText(p.Net), kztText(p.Assets), kztText(p.Debts))
		fmt.Fprintf(&b, `<rect class="hit" x="%.1f" y="0" width="%.1f" height="%.0f" data-tip="%s"/>`, step*float64(i), step, svgH, template.HTMLEscapeString(tip))
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String()) //nolint:gosec // numbers and escaped text
}

// wealthAction adds holdings and their values, edits, archives and deletes them.
func (s *Server) wealthAction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	back := func(msg string) { http.Redirect(w, r, "/ui/wealth?msg="+urlq(msg), http.StatusSeeOther) }
	id, _ := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	day := func() (time.Time, bool) {
		t, err := time.ParseInLocation("2006-01-02", r.PostFormValue("on"), s.loc)
		return t, err == nil && !t.After(s.now().In(s.loc))
	}
	amount := func() (int64, bool) {
		v, ok := parseAmount(r.PostFormValue("amount"))
		return int64(v*100 + 0.5), ok && v >= 0
	}
	switch r.PostFormValue("action") {
	case "add", "edit":
		h := store.Holding{ID: id, Name: trimTo(r.PostFormValue("name"), 60), Kind: r.PostFormValue("kind"),
			Liquid: r.PostFormValue("liquid") == "1", Note: trimTo(r.PostFormValue("note"), 200)}
		if h.Name == "" || !store.ValidKind(h.Kind) {
			back("Give it a name and a kind.")
			return
		}
		if store.IsDebtKind(h.Kind) {
			h.Liquid = false
		}
		if h.ID != 0 {
			if _, err := s.st.SaveHolding(ctx, h, s.now()); err != nil {
				s.fail(w, err)
				return
			}
			back(h.Name + " saved.")
			return
		}
		on, okD := day()
		v, okA := amount()
		if !okD || !okA {
			back("Type its value now and the date (not in the future).")
			return
		}
		nid, err := s.st.SaveHolding(ctx, h, s.now())
		if err == nil {
			err = s.st.SetHoldingValue(ctx, nid, on, v)
		}
		if err != nil {
			s.fail(w, err)
			return
		}
		back(h.Name + " added.")
	case "value":
		on, okD := day()
		v, okA := amount()
		if !okD || !okA {
			back("Type the value and the date (not in the future).")
			return
		}
		if err := s.st.SetHoldingValue(ctx, id, on, v); err != nil {
			s.fail(w, err)
			return
		}
		back("Value saved.")
	case "delete_value":
		vid, _ := strconv.ParseInt(r.PostFormValue("value"), 10, 64)
		if err := s.st.DeleteHoldingValue(ctx, id, vid); err != nil {
			s.fail(w, err)
			return
		}
		back("Value deleted.")
	case "archive", "restore":
		if err := s.st.SetHoldingArchived(ctx, id, r.PostFormValue("action") == "archive"); err != nil {
			s.fail(w, err)
			return
		}
		back(map[string]string{"archive": "Archived: its history stays in the chart.", "restore": "Restored."}[r.PostFormValue("action")])
	case "delete":
		if err := s.st.DeleteHolding(ctx, id); err != nil {
			s.fail(w, err)
			return
		}
		back("Deleted with its history.")
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
	}
}
