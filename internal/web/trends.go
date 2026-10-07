package web

import (
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"spendbot/internal/analytics"
	"spendbot/internal/money"
)

// The trends page: the long view over all the data. Line charts are SVG drawn here; every
// month has an invisible hit area with a tooltip.

type trendsData struct {
	Page
	Empty       bool
	People      bool
	WithSavings bool
	State       string
	T           analytics.Trends
	Last12Rate  analytics.YearRate // the last 12 months as one "year"
	Lines       template.HTML
	Rolling     template.HTML
	Slots       []template.CSS // colour per structure part, in order
}

// structureSlots are the categorical colours for the structure (validated palette, adjacent
// pairs); "Other" is neutral grey.
var structureSlots = []template.CSS{"var(--cat-1)", "var(--cat-2)", "var(--cat-3)", "var(--cat-4)", "var(--cat-5)", "var(--cat-6)", "var(--cat-7)"}

func (s *Server) trends(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	d := &trendsData{People: q.Get("people") != "0", WithSavings: q.Get("withsave") == "1"}
	sq := cloneValues(q)
	sq.Del("msg")
	d.State = sq.Encode()
	rows, err := s.st.Ledger(ctx, s.loc)
	if err != nil {
		s.fail(w, err)
		return
	}
	first, last, ok := bounds(rows)
	if !ok {
		d.Empty = true
		s.show(w, r, "trends.html", "Trends", "trends", d)
		return
	}
	f := analytics.FlowFilter{People: d.People}
	if !d.WithSavings {
		if f.Exclude, err = s.st.SavingsNames(ctx); err != nil {
			s.fail(w, err)
			return
		}
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
	d.T = analytics.BuildTrends(rows, f, first, last)
	ms := d.T.Flow.Months
	from := max(0, len(ms)-12)
	for _, m := range ms[from:] {
		d.Last12Rate.Income += m.Income
		d.Last12Rate.Out += m.Out
		d.Last12Rate.Net += m.Net
		d.Last12Rate.Months++
	}
	if d.Last12Rate.Income > 0 {
		d.Last12Rate.HasRate = true
		d.Last12Rate.Rate = float64(d.Last12Rate.Net) * 100 / float64(d.Last12Rate.Income)
	}
	d.Lines = linesSVG(ms)
	d.Rolling = rollingSVG(d.T.Rolling)
	d.Slots = append(structureSlots[:len(d.T.Top):len(d.T.Top)], "var(--cat-other)")
	s.show(w, r, "trends.html", "Trends", "trends", d)
}

const (
	svgW = 1000.0
	svgH = 240.0
	svgT = 12.0 // room above the highest point
)

func kztText(m int64) string { return money.Format(m, money.DefaultCurrency) }

// linesSVG draws income and what went out by month; months in the red get a red band.
func linesSVG(ms []analytics.MonthFlow) template.HTML {
	n := len(ms)
	if n == 0 {
		return ""
	}
	var top int64 = 1
	for _, m := range ms {
		top = max(top, m.Income, m.Out)
	}
	step := svgW / float64(n)
	x := func(i int) float64 { return step*float64(i) + step/2 }
	y := func(v int64) float64 { return svgH - float64(max(v, 0))*(svgH-svgT)/float64(top) }
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="line-chart" viewBox="0 0 %.0f %.0f" preserveAspectRatio="none" role="img" aria-label="Income and what went out by month">`, svgW, svgH)
	for i, m := range ms {
		if m.Net < 0 {
			fmt.Fprintf(&b, `<rect class="deficit" x="%.1f" y="0" width="%.1f" height="%.0f"/>`, step*float64(i), step, svgH)
		}
	}
	line := func(class string, v func(analytics.MonthFlow) int64) {
		var pts []string
		for i, m := range ms {
			pts = append(pts, fmt.Sprintf("%.1f,%.1f", x(i), y(v(m))))
		}
		fmt.Fprintf(&b, `<polyline class="%s" points="%s"/>`, class, strings.Join(pts, " "))
	}
	line("ln-income", func(m analytics.MonthFlow) int64 { return m.Income })
	line("ln-out", func(m analytics.MonthFlow) int64 { return m.Out })
	for i, m := range ms {
		state := "left " + kztText(m.Net)
		if m.Net < 0 {
			state = "in the red " + kztText(-m.Net)
		}
		tip := fmt.Sprintf("%s%s · income %s · went out %s · %s", m.Label, map[bool]string{true: " (part)"}[m.Partial],
			kztText(m.Income), kztText(m.Out), state)
		fmt.Fprintf(&b, `<rect class="hit" x="%.1f" y="0" width="%.1f" height="%.0f" data-tip="%s"/>`, step*float64(i), step, svgH, template.HTMLEscapeString(tip))
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String()) //nolint:gosec // numbers and escaped text
}

// rollingSVG draws monthly spending as thin bars and the 12-month average as a line.
func rollingSVG(rs []analytics.Rolling) template.HTML {
	n := len(rs)
	if n == 0 {
		return ""
	}
	var top int64 = 1
	for _, r := range rs {
		top = max(top, r.Spend, r.Avg)
	}
	step := svgW / float64(n)
	y := func(v int64) float64 { return svgH - float64(max(v, 0))*(svgH-svgT)/float64(top) }
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="line-chart" viewBox="0 0 %.0f %.0f" preserveAspectRatio="none" role="img" aria-label="Spending by month and the 12-month average">`, svgW, svgH)
	for i, r := range rs {
		h := svgH - y(r.Spend)
		fmt.Fprintf(&b, `<rect class="bar-soft" x="%.1f" y="%.1f" width="%.1f" height="%.1f"/>`, step*float64(i)+step*0.15, y(r.Spend), step*0.7, h)
	}
	var pts []string
	for i, r := range rs {
		if r.HasAvg {
			pts = append(pts, fmt.Sprintf("%.1f,%.1f", step*float64(i)+step/2, y(r.Avg)))
		}
	}
	if len(pts) > 1 {
		fmt.Fprintf(&b, `<polyline class="ln-avg" points="%s"/>`, strings.Join(pts, " "))
	}
	for i, r := range rs {
		tip := r.Month.Format("Jan 2006") + " · spent " + kztText(r.Spend)
		if r.HasAvg {
			tip += " · 12-month average " + kztText(r.Avg)
		}
		fmt.Fprintf(&b, `<rect class="hit" x="%.1f" y="0" width="%.1f" height="%.0f" data-tip="%s"/>`, step*float64(i), step, svgH, template.HTMLEscapeString(tip))
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String()) //nolint:gosec // numbers and escaped text
}
