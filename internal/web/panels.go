package web

import (
	"net/http"
	"net/url"
	"sort"
	"time"

	"spendbot/internal/analytics"
	"spendbot/internal/store"
)

// Side panels: a click on a chart piece — a category in a year, a merchant, a day — opens a
// panel with what happened there and a link to the operations with the filters set. The panel
// is an HTML fragment the page fetches; without JavaScript the links go to the full pages.

type panelData struct {
	Title    string
	Sub      string
	Period   analytics.Period
	Prev     analytics.Period
	HasPrev  bool
	Total    int64
	PrevTot  int64
	Share    float64 // of the period's spending, %
	Count    int
	Avg      int64
	Trend    []analytics.Bar
	Unit     string
	Top      []analytics.Merchant
	Cats     []analytics.Group
	Ops      []analytics.Op
	Biggest  []analytics.Op
	Link     string // the operations with the filters set
	Page     string // a page with more, when there is one
	PageName string
	Empty    bool
}

// panelPeriod reads from/to (a picked range, both days included) or a period key; the
// default is all the data.
func (s *Server) panelPeriod(q url.Values, rows []store.LedgerRow) (analytics.Period, bool) {
	first, last, ok := bounds(rows)
	if !ok {
		return analytics.Period{}, false
	}
	if p, ok := customPeriod(q, s.loc); ok {
		return p, true
	}
	ps := s.periods(first, last)
	for _, p := range ps {
		if p.Key == q.Get("period") {
			return p, true
		}
	}
	for _, p := range ps {
		if p.Key == "all" {
			return p, true
		}
	}
	return analytics.Period{}, false
}

func periodLink(p analytics.Period) []string {
	if p.Key != "custom" && p.Key != "" {
		return []string{"period", p.Key}
	}
	return []string{"from", p.From.Format("2006-01-02"), "to", p.To.AddDate(0, 0, -1).Format("2006-01-02")}
}

func (s *Server) panel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	rows, err := s.rows(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	d := &panelData{}
	p, ok := s.panelPeriod(q, rows)
	if !ok {
		d.Empty = true
		s.renderPanel(w, d)
		return
	}
	d.Period = p
	var exclude []string
	if q.Get("nosave") == "1" {
		if exclude, err = s.st.SavingsNames(ctx); err != nil {
			s.fail(w, err)
			return
		}
	}
	first, _, _ := bounds(rows)
	if p.Key != "all" {
		d.Prev = p.Prev()
		d.HasPrev = !d.Prev.From.Before(first)
	}
	nosave := ""
	if len(exclude) > 0 {
		nosave = "1"
	}
	pl := periodLink(p)
	switch r.PathValue("kind") {
	case "category":
		name := q.Get("name")
		rep := analytics.Categories(rows, p, d.Prev, d.HasPrev, exclude)
		d.Title, d.Unit = name, rep.TrendUnit
		for _, st := range rep.Stats {
			if st.Name == name {
				d.Total, d.PrevTot, d.Share, d.Count, d.Avg = st.Total, st.Prev, st.Share, int(st.Count), st.Avg
				d.Trend, d.Top = st.Trend, st.Top
			}
		}
		ops := analytics.Operations(rows, analytics.Filter{Period: p, Category: name, Exclude: exclude}).Ops
		d.Biggest = biggest(ops, 5)
		d.Link = link("/ui/operations", append([]string{"cat", name, "nosave", nosave}, pl...)...)
		d.Page, d.PageName = link("/ui/categories", pl...), "Categories"
	case "merchant":
		key := q.Get("m")
		ops := analytics.Operations(rows, analytics.Filter{Period: p, Merchant: key, Exclude: exclude, Type: q.Get("type")}).Ops
		if len(ops) > 0 {
			d.Title = ops[0].Merchant
		}
		for _, o := range ops {
			d.Total += o.Amount
		}
		d.Count = len(ops)
		if d.Count > 0 {
			d.Avg = d.Total / int64(d.Count)
		}
		if d.HasPrev {
			for _, o := range analytics.Operations(rows, analytics.Filter{Period: d.Prev, Merchant: key, Exclude: exclude, Type: q.Get("type")}).Ops {
				d.PrevTot += o.Amount
			}
		}
		if all := analytics.Operations(rows, analytics.Filter{Period: p, Exclude: exclude}).Total; all > 0 {
			d.Share = float64(d.Total) * 100 / float64(all)
		}
		d.Trend, d.Unit = monthBars(analytics.GroupOps(ops, "month")), "month"
		d.Cats = analytics.GroupOps(ops, "category")
		d.Ops = ops[:min(len(ops), 8)]
		d.Biggest = biggest(ops, 5)
		d.Link = link("/ui/operations", append([]string{"m", key, "type", q.Get("type"), "nosave", nosave}, pl...)...)
	case "day":
		day, err := time.ParseInLocation("2006-01-02", q.Get("d"), s.loc)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		d.Period, d.HasPrev = analytics.DayPeriod(day), false
		d.Title = dayTitle(day, s.now().In(s.loc))
		spend := analytics.Operations(rows, analytics.Filter{Period: d.Period, Exclude: exclude})
		d.Total, d.Count = spend.Total, len(spend.Ops)
		d.Ops = analytics.Operations(rows, analytics.Filter{Period: d.Period, Type: analytics.TypeAll}).Ops
		d.Cats = analytics.GroupOps(spend.Ops, "category")
		ds := day.Format("2006-01-02")
		d.Link = link("/ui/operations", "from", ds, "to", ds, "type", "all")
	default:
		http.NotFound(w, r)
		return
	}
	if d.Title == "" {
		d.Title = "Nothing here"
	}
	s.renderPanel(w, d)
}

func (s *Server) renderPanel(w http.ResponseWriter, d *panelData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := s.tmpl.ExecuteTemplate(w, "panel", d); err != nil {
		s.log.Error("web: render panel", "err", err)
	}
}

func biggest(ops []analytics.Op, n int) []analytics.Op {
	out := append([]analytics.Op(nil), ops...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Amount > out[j].Amount })
	return out[:min(len(out), n)]
}

// monthBars turns month groups into bars for the panel's sparkline.
func monthBars(gs []analytics.Group) []analytics.Bar {
	var max int64
	for _, g := range gs {
		max = maxInt64(max, g.Out)
	}
	var out []analytics.Bar
	for _, g := range gs {
		b := analytics.Bar{Label: g.Label, Value: g.Out, From: g.From, To: g.To}
		if max > 0 {
			b.Pct = float64(g.Out) * 100 / float64(max)
		}
		out = append(out, b)
	}
	return out
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
