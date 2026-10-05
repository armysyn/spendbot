package analytics

import (
	"sort"
	"time"

	"spendbot/internal/store"
)

// CategoryStat is a category's spending over a period, for the categories page.
type CategoryStat struct {
	Name   string
	Total  int64
	Count  int64
	Avg    int64   // per operation
	Share  float64 // of the period's spending, %
	Pct    float64 // relative to the largest category
	Prev   int64   // the same category over the previous period
	Trend  []Bar   // spending by month (or day, week, year — see TrendUnit) for a sparkline
	Top    []Merchant
	Pseudo bool // Cash or Uncategorized: no category behind it
}

// CategoryReport is every category with spending over a period.
type CategoryReport struct {
	Stats     []CategoryStat
	Total     int64
	TrendUnit string
}

// buckets splits a period for sparklines: days up to a month, weeks up to a quarter, months up
// to two years, years beyond.
func buckets(p Period) (unit string, starts []time.Time) {
	days := p.Days()
	loc := p.From.Location()
	var t time.Time
	var step func(time.Time) time.Time
	switch {
	case days <= 31:
		unit, t, step = "day", p.From, func(t time.Time) time.Time { return t.AddDate(0, 0, 1) }
	case days <= 92:
		unit = "week"
		t = p.From.AddDate(0, 0, -((int(p.From.Weekday()) + 6) % 7))
		step = func(t time.Time) time.Time { return t.AddDate(0, 0, 7) }
	case days <= 731:
		unit, t = "month", time.Date(p.From.Year(), p.From.Month(), 1, 0, 0, 0, 0, loc)
		step = func(t time.Time) time.Time { return t.AddDate(0, 1, 0) }
	default:
		unit, t = "year", time.Date(p.From.Year(), 1, 1, 0, 0, 0, 0, loc)
		step = func(t time.Time) time.Time { return t.AddDate(1, 0, 0) }
	}
	for ; t.Before(p.To); t = step(t) {
		starts = append(starts, t)
	}
	return unit, starts
}

func bucketLabel(unit string, t time.Time) string {
	switch unit {
	case "day":
		return t.Format("2 Jan")
	case "week":
		return "week of " + t.Format("2 Jan")
	case "month":
		return t.Format("Jan 2006")
	}
	return t.Format("2006")
}

// Categories computes per-category spending over p, compared with prev when hasPrev.
// exclude leaves savings out the same way the analytics page does.
func Categories(all []store.LedgerRow, p, prev Period, hasPrev bool, exclude []string) CategoryReport {
	unit, starts := buckets(p)
	rep := CategoryReport{TrendUnit: unit}
	type agg struct {
		stat      CategoryStat
		trend     []int64
		merchants map[string]*Merchant
	}
	by := map[string]*agg{}
	get := func(name string) *agg {
		a, ok := by[name]
		if !ok {
			a = &agg{stat: CategoryStat{Name: name, Pseudo: name == CashCategory || name == Uncategorized},
				trend: make([]int64, len(starts)), merchants: map[string]*Merchant{}}
			by[name] = a
		}
		return a
	}
	for _, r := range spendInPeriod(all, p, exclude) {
		i := sort.Search(len(starts), func(i int) bool { return starts[i].After(r.At) }) - 1
		add := func(cat string, amount int64) {
			a := get(cat)
			a.stat.Total += amount
			a.stat.Count++
			if i >= 0 {
				a.trend[i] += amount
			}
			key := merchantKey(r)
			m, ok := a.merchants[key]
			if !ok {
				m = &Merchant{Key: key, Name: merchantName(r)}
				a.merchants[key] = m
			}
			m.Total += amount
			if amount > 0 {
				m.Count++
			}
		}
		if len(r.Categories) == 0 {
			if r.Kind == cash {
				add(CashCategory, r.Amount)
			} else {
				add(Uncategorized, r.Amount)
			}
			continue
		}
		for j, c := range r.Categories {
			add(c, r.Splits[j])
		}
	}
	if hasPrev {
		for _, r := range spendInPeriod(all, prev, exclude) {
			if len(r.Categories) == 0 {
				if r.Kind == cash {
					get(CashCategory).stat.Prev += r.Amount
				} else {
					get(Uncategorized).stat.Prev += r.Amount
				}
				continue
			}
			for j, c := range r.Categories {
				get(c).stat.Prev += r.Splits[j]
			}
		}
	}
	var max int64
	for _, a := range by {
		rep.Total += a.stat.Total
		if a.stat.Total > max {
			max = a.stat.Total
		}
	}
	for _, a := range by {
		st := a.stat
		if st.Count > 0 {
			st.Avg = st.Total / st.Count
		}
		if rep.Total > 0 {
			st.Share = float64(st.Total) * 100 / float64(rep.Total)
		}
		if max > 0 && st.Total > 0 {
			st.Pct = float64(st.Total) * 100 / float64(max)
		}
		for i, v := range a.trend {
			to := p.To
			if i+1 < len(starts) {
				to = starts[i+1]
			}
			st.Trend = append(st.Trend, Bar{Label: bucketLabel(unit, starts[i]), Value: v, From: maxTime(starts[i], p.From), To: minTime(to, p.To)})
		}
		scale(st.Trend, st.Total)
		for _, m := range a.merchants {
			st.Top = append(st.Top, *m)
		}
		sort.Slice(st.Top, func(i, j int) bool {
			if st.Top[i].Total != st.Top[j].Total {
				return st.Top[i].Total > st.Top[j].Total
			}
			return st.Top[i].Name < st.Top[j].Name
		})
		if len(st.Top) > 5 {
			st.Top = st.Top[:5]
		}
		for i := range st.Top {
			if st.Total > 0 {
				st.Top[i].Share = float64(st.Top[i].Total) * 100 / float64(st.Total)
			}
		}
		rep.Stats = append(rep.Stats, st)
	}
	sort.Slice(rep.Stats, func(i, j int) bool {
		if rep.Stats[i].Total != rep.Stats[j].Total {
			return rep.Stats[i].Total > rep.Stats[j].Total
		}
		return rep.Stats[i].Name < rep.Stats[j].Name
	})
	return rep
}
