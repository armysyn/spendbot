package web

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"spendbot/internal/analytics"
	"spendbot/internal/kaspi"
	"spendbot/internal/merchant"
	"spendbot/internal/money"
	"spendbot/internal/store"
)

// Page is what the layout needs: the title, the active menu item and the number of open questions.
type Page struct {
	Title  string
	Nav    string
	Open   int    // merchants waiting for an answer in open batches
	Issues int    // open issues
	Path   string // this page with its query, for "report a problem"
	// NoPassword warns that anyone on the network can open the page; SignedIn shows "sign out".
	NoPassword bool
	SignedIn   bool
	Bare       bool   // no menu: the sign-in page
	Account    string // whose account, shown when there are several
}

func (p *Page) page() *Page { return p }

type pager interface{ page() *Page }

// openQuestions counts merchants in open batches for the menu badge.
func (s *Server) openQuestions(ctx context.Context) int {
	bs, err := s.st.Batches(ctx, false, 100)
	if err != nil {
		return 0
	}
	n := 0
	for _, b := range bs {
		n += b.Items
	}
	return n
}

func (s *Server) show(w http.ResponseWriter, r *http.Request, name, title, nav string, data pager) {
	p := data.page()
	p.Title, p.Nav, p.Open, p.Path = title, nav, s.openQuestions(r.Context()), r.URL.RequestURI()
	p.Issues, _, _ = s.st.IssueCounts(r.Context())
	v := viewerFrom(r.Context())
	p.NoPassword, p.SignedIn = v.Open, v.SessionID != ""
	if v.Accounts > 1 {
		p.Account = v.Account.Name
	}
	s.render(w, name, data)
}

// link builds a page URL from key-value pairs, skipping empty values.
func link(path string, kv ...string) string {
	q := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			q.Set(kv[i], kv[i+1])
		}
	}
	if len(q) == 0 {
		return path
	}
	return path + "?" + q.Encode()
}

// change is a comparison with the previous period for the page: spending going up is bad.
type change struct {
	Text  string
	Class string // up, down or flat
	Title string
}

func changeOf(c analytics.Change) change {
	if !c.OK {
		return change{}
	}
	out := change{Title: "previous period: " + money.Format(c.Prev, money.DefaultCurrency)}
	switch {
	case c.Pct >= 100:
		// "↑ 1214%" reads badly: past double, show how many times bigger
		ratio := 1 + c.Pct/100
		prec := 1
		if ratio >= 10 {
			prec = 0
		}
		out.Text, out.Class = "↑ ×"+strconv.FormatFloat(ratio, 'f', prec, 64), "up"
	case c.Pct >= 0.5:
		out.Text, out.Class = "↑ "+pctString(c.Pct), "up"
	case c.Pct <= -0.5:
		out.Text, out.Class = "↓ "+pctString(-c.Pct), "down"
	default:
		out.Text, out.Class = "≈ same", "flat"
	}
	return out
}

// bounds are the first and last days of any operation: transfers after the last purchase
// must still be inside "all time" on the operations and transfers pages.
func bounds(rows []store.LedgerRow) (first, last time.Time, ok bool) {
	for _, r := range rows {
		if !ok || r.At.Before(first) {
			first = r.At
		}
		if !ok || r.At.After(last) {
			last = r.At
		}
		ok = true
	}
	day := func(t time.Time) time.Time { return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()) }
	return day(first), day(last), ok
}

// periods are the period choices: today, this week and this month by the clock, then the
// ranges and months built from the data.
func (s *Server) periods(first, last time.Time) []analytics.Period {
	return append(analytics.Recent(s.now().In(s.loc), first), analytics.Periods(first, last)...)
}

// latestMonth is the calendar month of the last day with data.
func latestMonth(ps []analytics.Period) (analytics.Period, bool) {
	for _, p := range ps {
		if len(p.Key) == 7 {
			return p, true
		}
	}
	return analytics.Period{}, false
}

// ---- home ----

type homeSummary struct {
	Month     analytics.Period
	D         analytics.Dashboard
	Change    change
	HasPrev   bool
	PrevTitle string
	Pace      int64 // projected month total while the month is in progress
	Recent    []analytics.Op
	Flow      analytics.Flow // income and what went out this month
}

func (s *Server) homeSummary(ctx context.Context) (*homeSummary, error) {
	rows, err := s.st.Ledger(ctx, s.loc)
	if err != nil {
		return nil, err
	}
	first, last, ok := bounds(rows)
	if !ok {
		return nil, nil
	}
	h := &homeSummary{}
	ps := analytics.Periods(first, last)
	if m, ok := latestMonth(ps); ok {
		h.Month = m
		h.D = analytics.Build(rows, m, nil)
		names, err := s.st.IncomeSources(ctx)
		if err != nil {
			return nil, err
		}
		savings, err := s.st.SavingsNames(ctx)
		if err != nil {
			return nil, err
		}
		salaries, err := s.st.SalaryPeriods(ctx)
		if err != nil {
			return nil, err
		}
		sources := map[string]bool{}
		for _, n := range names {
			sources[n] = true
		}
		h.Flow = analytics.CashFlow(rows, analytics.FlowFilter{Period: m, Exclude: savings, Sources: sources, Salaries: salaries, People: true})
		prev := m.Prev()
		if h.HasPrev = !prev.From.Before(first); h.HasPrev {
			h.Change = changeOf(analytics.Compare(h.D.Totals.Spend, analytics.Build(rows, prev, nil).Totals.Spend))
			h.PrevTitle = prev.Title
		}
		monthStart := time.Date(m.From.Year(), m.From.Month(), 1, 0, 0, 0, 0, m.From.Location())
		full := monthStart.AddDate(0, 1, 0)
		if days := m.Days(); m.To.Before(full) && days >= 3 && monthStart.Equal(m.From) {
			total := int(full.Sub(monthStart).Hours()/24 + 0.5)
			h.Pace = h.D.Totals.Spend * int64(total) / int64(days)
		}
	}
	all := analytics.Period{Key: "all", From: first, To: last.AddDate(0, 0, 1)}
	ops := analytics.Operations(rows, analytics.Filter{Period: all, Type: analytics.TypeAll}).Ops
	if len(ops) > 8 {
		ops = ops[:8]
	}
	h.Recent = ops
	return h, nil
}

// ---- operations ----

type dayGroup struct {
	Date  time.Time
	Total int64 // spending of the day among the shown operations
	Ops   []analytics.Op
}

type operationsData struct {
	Page
	Flash        string
	Empty        bool
	Periods      []analytics.Period
	Current      analytics.Period
	Custom       bool // the period is a from–to range, not one of Periods
	From, To     string
	Type         string
	Category     string
	Merchant     string // merchant key
	MerchantName string
	Query        string
	Min, Max     string // amount bounds as typed, tenge
	Dir          string
	Repeat       string
	Sort         string
	Group        string
	Groups       []analytics.Group
	Groupings    []struct{ Key, Title string }
	Stats        analytics.Stats
	Flat         bool // a plain list: sorted by amount or oldest first, not by day
	Ask          string
	AI           bool // a model understands requests; otherwise the box is off
	AIWhy        string
	State        string
	NoSavings    bool
	Categories   []store.Category
	Ops          analytics.Ops
	Days         []dayGroup
	Shown        int
	More         string // link to show more, empty when everything is shown
	Picker       picker
	Back         string // this page, to come back after an edit
	CSV          string
}

const opsPage = 300

// filterFrom reads the operations filter from the query string.
func filterFrom(q url.Values, ps []analytics.Period, loc *time.Location) (analytics.Filter, bool) {
	f := analytics.Filter{Type: q.Get("type"), Category: q.Get("cat"), Merchant: q.Get("m"), Query: strings.TrimSpace(q.Get("q"))}
	bound := func(k string) int64 {
		if v, ok := parseAmount(q.Get(k)); ok && v > 0 {
			return int64(v*100 + 0.5)
		}
		return 0
	}
	f.Min, f.Max = bound("min"), bound("max")
	if f.Min > 0 && f.Max > 0 && f.Min > f.Max {
		f.Min, f.Max = f.Max, f.Min
	}
	if d := q.Get("dir"); d == analytics.DirIn || d == analytics.DirOut {
		f.Dir = d
	}
	if r := q.Get("repeat"); r == analytics.RepeatFirst || r == analytics.RepeatOnce {
		f.Repeat = r
	}
	switch f.Type {
	case analytics.TypeSpend, analytics.TypeTransfers, analytics.TypeAll:
	default:
		f.Type = analytics.TypeSpend
	}
	for _, p := range ps {
		if p.Key == "all" {
			f.Period = p
		}
	}
	for _, p := range ps {
		if p.Key == q.Get("period") {
			f.Period = p
		}
	}
	if p, ok := customPeriod(q, loc); ok {
		f.Period = p
		return f, true
	}
	return f, false
}

// customPeriod reads a range picked in the calendar: ?from=2026-09-01&to=2026-09-15, both
// days included. A single day has from == to.
func customPeriod(q url.Values, loc *time.Location) (analytics.Period, bool) {
	from, err1 := time.ParseInLocation("2006-01-02", q.Get("from"), loc)
	to, err2 := time.ParseInLocation("2006-01-02", q.Get("to"), loc)
	if err1 != nil || err2 != nil || to.Before(from) {
		return analytics.Period{}, false
	}
	p := analytics.Period{Key: "custom", From: from, To: to.AddDate(0, 0, 1), Title: from.Format("2 Jan 2006")}
	if !to.Equal(from) {
		p.Title += " – " + to.Format("2 Jan 2006")
	}
	return p, true
}

// picker is what the calendar needs: the page, the other filters to keep, the range shown,
// the span of the data and the days with spending (marked with a dot).
type picker struct {
	Path, State string
	From, To    string // the range shown, both days included
	Min, Max    string // the first and the last day of the data
	Days        string // days with spending, comma-separated
	Custom      bool
	Label       string // the picked range for the button: "8 Sep – 21 Sep 2026"
}

// rangeLabel is a short range: "8 Sep 2026", "8 – 21 Sep 2026", "28 Aug – 3 Sep 2026",
// "28 Dec 2025 – 3 Jan 2026".
func rangeLabel(from, to time.Time) string {
	switch {
	case from.Equal(to):
		return from.Format("2 Jan 2006")
	case from.Year() != to.Year():
		return from.Format("2 Jan 2006") + " – " + to.Format("2 Jan 2006")
	case from.Month() != to.Month():
		return from.Format("2 Jan") + " – " + to.Format("2 Jan 2006")
	}
	return from.Format("2") + " – " + to.Format("2 Jan 2006")
}

func newPicker(path string, q url.Values, cur analytics.Period, custom bool, rows []store.LedgerRow) picker {
	sq := cloneValues(q)
	for _, k := range []string{"period", "from", "to", "msg", "limit", "ask"} {
		sq.Del(k)
	}
	pk := picker{Path: path, State: sq.Encode(), Custom: custom,
		From: cur.From.Format("2006-01-02"), To: cur.To.AddDate(0, 0, -1).Format("2006-01-02"),
		Label: rangeLabel(cur.From, cur.To.AddDate(0, 0, -1))}
	if first, last, ok := bounds(rows); ok {
		pk.Min, pk.Max = first.Format("2006-01-02"), last.Format("2006-01-02")
	}
	seen := map[string]bool{}
	var days []string
	for _, r := range rows {
		if analytics.IsSpend(r) && r.Amount > 0 {
			if d := r.At.Format("2006-01-02"); !seen[d] {
				seen[d] = true
				days = append(days, d)
			}
		}
	}
	pk.Days = strings.Join(days, ",")
	return pk
}

func (s *Server) operations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d := &operationsData{Flash: r.URL.Query().Get("msg")}
	rows, err := s.st.Ledger(ctx, s.loc)
	if err != nil {
		s.fail(w, err)
		return
	}
	first, last, ok := bounds(rows)
	if !ok {
		d.Empty = true
		s.show(w, r, "operations.html", "Operations", "operations", d)
		return
	}
	q := r.URL.Query()
	d.Periods = s.periods(first, last)
	f, custom := filterFrom(q, d.Periods, s.loc)
	if d.NoSavings = q.Get("nosave") == "1"; d.NoSavings && f.Type == analytics.TypeSpend {
		if f.Exclude, err = s.st.SavingsNames(ctx); err != nil {
			s.fail(w, err)
			return
		}
	}
	d.Current, d.Custom = f.Period, custom
	if custom {
		d.From, d.To = q.Get("from"), q.Get("to")
	}
	d.Picker = newPicker("/ui/operations", q, d.Current, custom, rows)
	d.Type, d.Category, d.Merchant, d.Query = f.Type, f.Category, f.Merchant, f.Query
	d.Min, d.Max, d.Dir, d.Repeat, d.Sort, d.Group = q.Get("min"), q.Get("max"), f.Dir, f.Repeat, q.Get("sort"), q.Get("group")
	d.Groupings, d.Ask, d.AI, d.AIWhy = analytics.Groupings, q.Get("ask"), s.aiReady(), s.aiWhy(ctx)
	sq := cloneValues(q)
	for _, k := range []string{"msg", "limit", "ask"} {
		sq.Del(k)
	}
	d.State = sq.Encode()
	if d.Categories, err = s.st.Categories(ctx); err != nil {
		s.fail(w, err)
		return
	}
	d.Ops = analytics.Operations(rows, f)
	if _, ok := sortTitles[d.Sort]; !ok {
		d.Sort = analytics.SortNew
	}
	analytics.SortOps(d.Ops.Ops, d.Sort)
	d.Flat = d.Sort != analytics.SortNew
	d.Stats = analytics.StatsOf(d.Ops.Ops)
	if d.Group != "" {
		d.Groups = analytics.GroupOps(d.Ops.Ops, d.Group)
		if len(d.Groups) == 0 {
			d.Group = ""
		}
	}
	if d.Merchant != "" && len(d.Ops.Ops) > 0 {
		d.MerchantName = d.Ops.Ops[0].Merchant
	}
	limit := opsPage
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > limit {
		limit = n
	}
	shown := d.Ops.Ops
	if len(shown) > limit {
		shown = shown[:limit]
		mq := cloneValues(q)
		mq.Set("limit", strconv.Itoa(limit+opsPage))
		d.More = "/ui/operations?" + mq.Encode()
	}
	d.Shown = len(shown)
	for _, o := range shown {
		day := time.Date(o.At.Year(), o.At.Month(), o.At.Day(), 0, 0, 0, 0, o.At.Location())
		if n := len(d.Days); n == 0 || !d.Days[n-1].Date.Equal(day) {
			d.Days = append(d.Days, dayGroup{Date: day})
		}
		g := &d.Days[len(d.Days)-1]
		g.Ops = append(g.Ops, o)
		if o.Spend {
			g.Total += o.Amount
		}
	}
	d.Back = r.URL.RequestURI()
	cq := cloneValues(q)
	cq.Del("limit")
	cq.Del("msg")
	d.CSV = "/ui/operations.csv?" + cq.Encode()
	s.show(w, r, "operations.html", "Operations", "operations", d)
}

func cloneValues(q url.Values) url.Values {
	out := url.Values{}
	for k, v := range q {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// operationsCSV exports the filtered operations for a spreadsheet.
func (s *Server) operationsCSV(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := s.st.Ledger(ctx, s.loc)
	if err != nil {
		s.fail(w, err)
		return
	}
	var ops []analytics.Op
	if first, last, ok := bounds(rows); ok {
		f, _ := filterFrom(r.URL.Query(), s.periods(first, last), s.loc)
		if r.URL.Query().Get("nosave") == "1" && f.Type == analytics.TypeSpend {
			if f.Exclude, err = s.st.SavingsNames(ctx); err != nil {
				s.fail(w, err)
				return
			}
		}
		ops = analytics.Operations(rows, f).Ops
		analytics.SortOps(ops, r.URL.Query().Get("sort"))
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="spendbot-operations.csv"`)
	cw := csv.NewWriter(w)
	cw.Write([]string{"date", "time", "merchant", "kind", "category", "amount", "spending"})
	for _, o := range ops {
		cat := strings.Join(o.Categories, " + ")
		cw.Write([]string{o.At.Format("2006-01-02"), o.At.Format("15:04"), o.Merchant, kaspi.KindName(o.Kind), cat,
			strconv.FormatFloat(float64(o.Amount)/100, 'f', 2, 64), strconv.FormatBool(o.Spend)})
	}
	cw.Flush()
}

// safeBack keeps redirects on this site's pages.
func safeBack(back, fallback string) string {
	if strings.HasPrefix(back, "/ui") && !strings.HasPrefix(back, "//") {
		return back
	}
	return fallback
}

func withMsg(back, msg string) string {
	u, err := url.Parse(back)
	if err != nil {
		return back
	}
	q := u.Query()
	q.Set("msg", msg)
	u.RawQuery = q.Encode()
	return u.String()
}

// opCategory changes the category of one operation, or of all purchases from its merchant.
// Fields: category ("none" — not spending, or a category id), new_category (wins), all ("1").
func (s *Server) opCategory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	back := safeBack(r.PostFormValue("back"), "/ui/operations")
	tx, err := s.st.GetTx(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		s.fail(w, err)
		return
	}
	switch tx.Kind {
	case "", kaspi.Purchase, kaspi.Withdrawal, kaspi.Transfer:
	default:
		http.Redirect(w, r, withMsg(back, "Incoming money has no category."), http.StatusSeeOther)
		return
	}
	cat := r.PostFormValue("category")
	if newCat := strings.Join(strings.Fields(r.PostFormValue("new_category")), " "); newCat != "" {
		cid, err := s.ensureCategory(ctx, newCat)
		if err != nil {
			s.fail(w, err)
			return
		}
		cat = strconv.FormatInt(cid, 10)
	}
	name := tx.MerchantRaw
	if tx.Kind == kaspi.Withdrawal {
		name = analytics.CashMerchant
	}
	var msg string
	switch cat {
	case "":
		http.Redirect(w, r, withMsg(back, "Pick a category."), http.StatusSeeOther)
		return
	case "none":
		if err := s.st.UncategorizeTx(ctx, id); err != nil {
			s.fail(w, err)
			return
		}
		msg = "Not counted as spending any more: " + displayName(name) + "."
	default:
		cid, err := strconv.ParseInt(cat, 10, 64)
		if err != nil {
			http.Error(w, "invalid category", http.StatusBadRequest)
			return
		}
		c, err := s.st.Category(ctx, cid)
		if err != nil {
			s.fail(w, err)
			return
		}
		all := r.PostFormValue("all") == "1" && tx.MerchantNorm != "" && (tx.Kind == "" || tx.Kind == kaspi.Purchase)
		switch {
		case all:
			n, err := s.st.RecategorizeMerchant(ctx, tx.MerchantNorm, cid, s.minHits, s.now())
			if err != nil {
				s.fail(w, err)
				return
			}
			msg = fmt.Sprintf("%d %s from %s moved to \"%s\"; new ones will follow.", n, plural(n, "purchase", "purchases"), displayName(name), c.Name)
		case tx.AmountMinor == 0:
			http.Redirect(w, r, withMsg(back, "The amount of this operation is unknown — answer it in Telegram."), http.StatusSeeOther)
			return
		default:
			learn := tx.Kind != kaspi.Withdrawal
			if _, err := s.st.CloseTx(ctx, id, []store.Split{{CategoryID: cid, AmountMinor: tx.AmountMinor}}, learn, s.now()); err != nil {
				s.fail(w, err)
				return
			}
			msg = displayName(name) + " → \"" + c.Name + "\"."
		}
	}
	if s.kick != nil {
		s.kick.Kick()
	}
	http.Redirect(w, r, withMsg(back, msg), http.StatusSeeOther)
}

func displayName(raw string) string {
	if raw == "" {
		return "the operation"
	}
	return merchant.Display(raw)
}

// ---- categories ----

type categoryRow struct {
	store.Category
	Stat   analytics.CategoryStat
	Change change
	Rules  int
}

type categoriesData struct {
	Page
	Flash      string
	Empty      bool
	Rows       []categoryRow
	Archived   []categoryRow
	Options    []store.Category
	Periods    []analytics.Period
	Current    analytics.Period
	PrevPeriod analytics.Period
	HasPrev    bool
	Custom     bool
	From, To   string
	Query      string
	Min, Max   string
	Savings    string // "" — all, "only", "none"
	Unused     bool   // show categories without spending in the period
	NoSavings  bool
	Sort       string
	Sorts      []struct{ Key, Title string }
	Total      int64
	Used       int // categories with spending
	Top        *categoryRow
	Grew       *categoryRow // the largest increase against the previous period
	Fell       *categoryRow
	TrendUnit  string
	Ask        string
	AI         bool
	AIWhy      string
	State      string
	Picker     picker
}

var categorySorts = []struct{ Key, Title string }{
	{"", "Largest first"}, {"small", "Smallest first"}, {"grew", "Grew the most"}, {"fell", "Fell the most"},
	{"count", "Most operations"}, {"avg", "Largest average"}, {"name", "By name"},
}

func (s *Server) categories(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	d := &categoriesData{Flash: q.Get("msg"), Query: strings.TrimSpace(q.Get("q")), Savings: q.Get("savings"),
		Unused: q.Get("unused") == "1", NoSavings: q.Get("nosave") == "1", Sort: q.Get("sort"), Ask: q.Get("ask"), AI: s.aiReady(), AIWhy: s.aiWhy(ctx), Sorts: categorySorts}
	cats, err := s.st.AllCategories(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	rules, err := s.st.CategoryRules(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	rows, err := s.st.Ledger(ctx, s.loc)
	if err != nil {
		s.fail(w, err)
		return
	}
	sq := cloneValues(q)
	for _, k := range []string{"msg", "ask"} {
		sq.Del(k)
	}
	d.State = sq.Encode()
	var rep analytics.CategoryReport
	first, last, ok := analytics.Bounds(rows)
	if ok {
		d.Periods = s.periods(first, last)
		if q.Get("period") == "" && q.Get("from") == "" {
			q.Set("period", "12m")
		}
		f, custom := filterFrom(q, d.Periods, s.loc)
		d.Current, d.Custom = f.Period, custom
		if custom {
			d.From, d.To = q.Get("from"), q.Get("to")
		}
		d.Picker = newPicker("/ui/categories", q, d.Current, custom, rows)
		var exclude []string
		if d.NoSavings {
			if exclude, err = s.st.SavingsNames(ctx); err != nil {
				s.fail(w, err)
				return
			}
		}
		if d.Current.Key != "all" {
			d.PrevPeriod = d.Current.Prev()
			d.HasPrev = !d.PrevPeriod.From.Before(first)
		}
		rep = analytics.Categories(rows, d.Current, d.PrevPeriod, d.HasPrev, exclude)
		d.TrendUnit = rep.TrendUnit
	} else {
		d.Empty = true
	}
	stats := map[string]analytics.CategoryStat{}
	for _, st := range rep.Stats {
		stats[st.Name] = st
	}
	min, max := int64(0), int64(0)
	if v, ok := parseAmount(q.Get("min")); ok {
		min, d.Min = int64(v*100+0.5), q.Get("min")
	}
	if v, ok := parseAmount(q.Get("max")); ok {
		max, d.Max = int64(v*100+0.5), q.Get("max")
	}
	terms := strings.Fields(analytics.Fold(d.Query))
	keep := func(name string, savings bool, st analytics.CategoryStat) bool {
		for _, t := range terms {
			if !strings.Contains(analytics.Fold(name), t) {
				return false
			}
		}
		switch {
		case d.Savings == "only" && !savings, d.Savings == "none" && savings:
			return false
		case min > 0 && st.Total < min, max > 0 && st.Total > max:
			return false
		}
		return true
	}
	seen := map[string]bool{}
	for _, c := range cats {
		st := stats[c.Name]
		st.Name = c.Name
		seen[c.Name] = true
		row := categoryRow{Category: c, Stat: st, Rules: rules[c.ID]}
		if d.HasPrev {
			row.Change = changeOf(analytics.Compare(st.Total, st.Prev))
		}
		if c.Archived {
			if keep(c.Name, c.Savings, st) {
				d.Archived = append(d.Archived, row)
			}
			continue
		}
		d.Options = append(d.Options, c)
		if st.Total == 0 && !d.Unused {
			continue
		}
		if keep(c.Name, c.Savings, st) {
			d.Rows = append(d.Rows, row)
		}
	}
	// Cash and Uncategorized have no category behind them but are part of spending.
	for _, st := range rep.Stats {
		if seen[st.Name] || st.Total == 0 && !d.Unused {
			continue
		}
		row := categoryRow{Category: store.Category{Name: st.Name}, Stat: st}
		if d.HasPrev {
			row.Change = changeOf(analytics.Compare(st.Total, st.Prev))
		}
		if keep(st.Name, false, st) {
			d.Rows = append(d.Rows, row)
		}
	}
	for _, row := range d.Rows {
		d.Total += row.Stat.Total
		if row.Stat.Total > 0 {
			d.Used++
		}
	}
	growth := func(r categoryRow) int64 { return r.Stat.Total - r.Stat.Prev }
	sort.SliceStable(d.Rows, func(i, j int) bool {
		a, b := d.Rows[i].Stat, d.Rows[j].Stat
		growth := func(st analytics.CategoryStat) int64 { return st.Total - st.Prev }
		switch d.Sort {
		case "small":
			if a.Total != b.Total {
				return a.Total < b.Total
			}
		case "grew":
			if growth(a) != growth(b) {
				return growth(a) > growth(b)
			}
		case "fell":
			if growth(a) != growth(b) {
				return growth(a) < growth(b)
			}
		case "count":
			if a.Count != b.Count {
				return a.Count > b.Count
			}
		case "avg":
			if a.Avg != b.Avg {
				return a.Avg > b.Avg
			}
		case "name":
			return a.Name < b.Name
		}
		if a.Total != b.Total {
			return a.Total > b.Total
		}
		return a.Name < b.Name
	})
	for i := range d.Rows {
		row := &d.Rows[i]
		if d.Top == nil || row.Stat.Total > d.Top.Stat.Total {
			d.Top = row
		}
		if d.HasPrev && growth(*row) > 0 && (d.Grew == nil || growth(*row) > growth(*d.Grew)) {
			d.Grew = row
		}
		if d.HasPrev && growth(*row) < 0 && (d.Fell == nil || growth(*row) < growth(*d.Fell)) {
			d.Fell = row
		}
	}
	s.show(w, r, "categories.html", "Categories", "categories", d)
}

// categoryAction adds, renames, merges, archives and restores categories and marks savings.
func (s *Server) categoryAction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	back := func(msg string) { http.Redirect(w, r, "/ui/categories?msg="+urlq(msg), http.StatusSeeOther) }
	name := strings.Join(strings.Fields(r.PostFormValue("name")), " ")
	if len([]rune(name)) > 40 {
		name = string([]rune(name)[:40])
	}
	id, _ := strconv.ParseInt(r.PostFormValue("id"), 10, 64)
	var c store.Category
	if r.PostFormValue("action") != "add" {
		var err error
		if c, err = s.st.Category(ctx, id); errors.Is(err, store.ErrNotFound) {
			back("No such category.")
			return
		} else if err != nil {
			s.fail(w, err)
			return
		}
	}
	var err error
	var msg string
	switch r.PostFormValue("action") {
	case "add":
		if name == "" {
			back("Type a name for the new category.")
			return
		}
		_, err = s.st.AddCategory(ctx, name)
		msg = "Category \"" + name + "\" added."
	case "rename":
		if name == "" || name == c.Name {
			back("Nothing to change.")
			return
		}
		other, ferr := s.st.FindCategory(ctx, name)
		switch {
		case ferr == nil && other.ID != c.ID:
			err = s.st.MergeCategory(ctx, c.ID, other.ID)
			msg = "\"" + other.Name + "\" already exists — \"" + c.Name + "\" was merged into it."
		case ferr != nil && !errors.Is(ferr, store.ErrNotFound):
			err = ferr
		default:
			err = s.st.RenameCategory(ctx, c.ID, name)
			msg = "\"" + c.Name + "\" is now \"" + name + "\"."
		}
	case "merge":
		into, perr := strconv.ParseInt(r.PostFormValue("into"), 10, 64)
		if perr != nil || into == c.ID {
			back("Pick another category to merge into.")
			return
		}
		target, terr := s.st.Category(ctx, into)
		if terr != nil {
			back("No such category.")
			return
		}
		err = s.st.MergeCategory(ctx, c.ID, into)
		msg = "\"" + c.Name + "\" merged into \"" + target.Name + "\"."
	case "archive":
		err = s.st.ArchiveCategory(ctx, c.ID)
		msg = "\"" + c.Name + "\" archived: hidden from choices, old operations keep it."
	case "restore":
		err = s.st.RestoreCategory(ctx, c.ID)
		msg = "\"" + c.Name + "\" restored."
	case "debt":
		err = s.st.SetDebtCategory(ctx, c.ID, !c.Debt)
		if c.Debt {
			msg = "\"" + c.Name + "\" no longer counts as debt payments."
		} else {
			msg = "\"" + c.Name + "\" counts as debt payments for the debt load on the Net worth page."
		}
	case "savings":
		var ids []int64
		cats, cerr := s.st.AllCategories(ctx)
		if cerr != nil {
			s.fail(w, cerr)
			return
		}
		for _, x := range cats {
			if x.Savings != (x.ID == c.ID) {
				ids = append(ids, x.ID)
			}
		}
		err = s.st.SetSavings(ctx, ids)
		if c.Savings {
			msg = "\"" + c.Name + "\" is spending again."
		} else {
			msg = "\"" + c.Name + "\" is savings: the \"without savings\" switch leaves it out."
		}
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	if s.kick != nil {
		s.kick.Kick()
	}
	back(msg)
}

// ---- calendar heatmap ----

type heatCell struct {
	Date  time.Time
	Value int64
	Level int
	In    bool // inside the period
}

type heatmap struct {
	Weeks  [][7]heatCell
	Months []heatMonth
	Max    int64
}

type heatMonth struct {
	Col   int
	Label string
}

// heat lays the period's daily spending out as weeks (columns) of days, Monday first.
// The shade is the quartile of the day among days with spending.
func heat(rows []store.LedgerRow, p analytics.Period, exclude []string) *heatmap {
	if days := p.Days(); days < 28 || days > 400 {
		return nil
	}
	byDay := dayTotals(rows, p, exclude)
	return layoutHeat(byDay, p, p, quartiles(byDay))
}

// dayTotals is spending per day over a period.
func dayTotals(rows []store.LedgerRow, p analytics.Period, exclude []string) map[time.Time]int64 {
	byDay := map[time.Time]int64{}
	for _, o := range analytics.Operations(rows, analytics.Filter{Period: p, Exclude: exclude}).Ops {
		d := time.Date(o.At.Year(), o.At.Month(), o.At.Day(), 0, 0, 0, 0, o.At.Location())
		byDay[d] += o.Amount
	}
	return byDay
}

// quartiles are the shade bounds: days with spending split into four equal groups.
func quartiles(byDay map[time.Time]int64) [3]int64 {
	var vals []int64
	for _, v := range byDay {
		if v > 0 {
			vals = append(vals, v)
		}
	}
	sort.Slice(vals, func(i, j int) bool { return vals[i] < vals[j] })
	q := func(f float64) int64 {
		if len(vals) == 0 {
			return 0
		}
		return vals[int(f*float64(len(vals)-1))]
	}
	return [3]int64{q(.25), q(.5), q(.75)}
}

// layoutHeat lays the grid's days out as weeks (columns) of seven, Monday first; only days of
// the data period p are shaded, the rest of the grid stays blank.
func layoutHeat(byDay map[time.Time]int64, grid, p analytics.Period, cut [3]int64) *heatmap {
	h := &heatmap{}
	start := grid.From.AddDate(0, 0, -((int(grid.From.Weekday()) + 6) % 7))
	lastMonth := -1
	for t := start; t.Before(grid.To); t = t.AddDate(0, 0, 7) {
		var wk [7]heatCell
		for i := range 7 {
			d := t.AddDate(0, 0, i)
			c := heatCell{Date: d, Value: byDay[d], In: !d.Before(p.From) && d.Before(p.To)}
			if c.In && c.Value > h.Max {
				h.Max = c.Value
			}
			if c.In && c.Value > 0 {
				c.Level = 1
				for _, x := range cut {
					if c.Value > x {
						c.Level++
					}
				}
			}
			wk[i] = c
			if !d.Before(grid.From) && d.Before(grid.To) && int(d.Month()) != lastMonth {
				lastMonth = int(d.Month())
				// a label needs room: skip it when the previous one is in the same or the last column
				if n := len(h.Months); n == 0 || len(h.Weeks)-h.Months[n-1].Col >= 3 {
					h.Months = append(h.Months, heatMonth{Col: len(h.Weeks), Label: d.Format("Jan")})
				}
			}
		}
		h.Weeks = append(h.Weeks, wk)
	}
	return h
}

// yearHeat is one calendar year of the all-time heatmap.
type yearHeat struct {
	Year  int
	Total int64
	Days  int // days with spending
	H     *heatmap
}

// heatYears lays every year out with one scale for all of them, so the years compare.
func heatYears(rows []store.LedgerRow, p analytics.Period, exclude []string) []yearHeat {
	byDay := dayTotals(rows, p, exclude)
	cut := quartiles(byDay)
	var out []yearHeat
	loc := p.From.Location()
	for y := p.To.AddDate(0, 0, -1).Year(); y >= p.From.Year(); y-- {
		yp := analytics.Period{From: time.Date(y, 1, 1, 0, 0, 0, 0, loc), To: time.Date(y+1, 1, 1, 0, 0, 0, 0, loc)}
		yh := yearHeat{Year: y, H: layoutHeat(byDay, yp, p, cut)}
		for d, v := range byDay {
			if d.Year() == y && v > 0 {
				yh.Total += v
				yh.Days++
			}
		}
		out = append(out, yh)
	}
	return out
}

// ---- columns chart ----

type column struct {
	analytics.Bar
	Href  string
	Axis  string // label under the column; empty when labels would collide
	Shown bool   // the value is printed above the column
}

type columns struct {
	Label  string
	Cols   []column
	Avg    int64
	AvgPct float64
	Short  bool
}

// cols prepares a columns chart: links for drill-down (target "month" opens the month on the
// analytics page, otherwise the operations of the column), a sparse axis and the average line.
func cols(bars []analytics.Bar, target, nosave, label string, short bool) columns {
	c := columns{Label: label, Short: short}
	n := len(bars)
	if n == 0 {
		return c
	}
	every := (n + 7) / 8
	var sum, max int64
	for _, b := range bars {
		sum += b.Value
		if b.Value > max {
			max = b.Value
		}
	}
	c.Avg = sum / int64(n)
	if max > 0 && n > 1 && c.Avg > 0 {
		c.AvgPct = float64(c.Avg) * 100 / float64(max)
	}
	for i, b := range bars {
		col := column{Bar: b, Shown: b.Value > 0 && (b.Value == max || i == n-1)}
		if target == "month" {
			col.Href = link("/ui/analytics", "period", b.From.Format("2006-01"), "nosave", nosave)
		} else {
			col.Href = link("/ui/operations", "from", b.From.Format("2006-01-02"), "to", b.To.AddDate(0, 0, -1).Format("2006-01-02"), "nosave", nosave)
		}
		if i%every == 0 {
			col.Axis = b.Label
			if target != "month" && len(b.Label) > 7 {
				col.Axis = b.From.Format("2 Jan")
			}
		}
		c.Cols = append(c.Cols, col)
	}
	return c
}

// swap is the operations page with the current filters (an encoded query) where the given keys
// are replaced; an empty value removes a key.
func swap(state string, kv ...string) string { return swapAt("/ui/operations", state, kv...) }

// swapAt is swap for any page.
func swapAt(path, state string, kv ...string) string {
	q, _ := url.ParseQuery(state)
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "" {
			q.Del(kv[i])
		} else {
			q.Set(kv[i], kv[i+1])
		}
	}
	if len(q) == 0 {
		return path
	}
	return path + "?" + q.Encode()
}
