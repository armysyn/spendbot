package analysis

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"spendbot/internal/analytics"
	"spendbot/internal/kaspi"
	"spendbot/internal/merchant"
	"spendbot/internal/money"
	"spendbot/internal/store"
)

// Thresholds below which an insight is not worth attention (tiyn).
const (
	minAnomaly  = 20_000_00
	minNewBig   = 50_000_00
	minTrendAdd = 15_000_00

	minTipsOps  = 50 // tips only when there is enough data to draw on
	minTipsDays = 60
)

// subscriptionCategories get the "cancel if unused" hint; Russian names cover existing databases.
var subscriptionCategories = map[string]bool{
	"": true, "Phone & subscriptions": true, "Entertainment": true, "Связь и подписки": true, "Развлечения": true,
}

// spendRows returns spending for insights: the analytics page definition without refunds.
// Code does the math; the model only gets finished numbers.
func spendRows(all []store.LedgerRow) []store.LedgerRow {
	var out []store.LedgerRow
	for _, r := range all {
		if analytics.IsSpend(r) && r.Amount > 0 {
			out = append(out, r)
		}
	}
	return out
}

// part is a piece of a transaction: category and amount; without parts — Uncategorized.
type part struct {
	r   store.LedgerRow
	cat string
	amt int64
}

func parts(rows []store.LedgerRow) []part {
	var out []part
	for _, r := range rows {
		if len(r.Categories) == 0 {
			out = append(out, part{r, analytics.Uncategorized, r.Amount})
			continue
		}
		for i, c := range r.Categories {
			out = append(out, part{r, c, r.Splits[i]})
		}
	}
	return out
}

// median works like ClickHouse quantileExact(0.5): for an even count, the upper of the two middle values.
func median(xs []int64) int64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]int64(nil), xs...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}

func name(r store.LedgerRow) string {
	if r.Merchant == "" {
		return ""
	}
	return merchant.Display(r.Merchant)
}

// shortest returns the shortest merchant name, alphabetical on ties.
func shortest(names []string) string {
	best := ""
	for i, n := range names {
		ln, lb := utf8.RuneCountInString(n), utf8.RuneCountInString(best)
		if i == 0 || ln < lb || (ln == lb && n < best) {
			best = n
		}
	}
	return best
}

// Insights recomputes insights, stores new ones and returns how many are new.
func (a *Analyzer) Insights(ctx context.Context) (int, error) {
	all, err := a.ledger(ctx)
	if err != nil {
		return 0, err
	}
	rows := spendRows(all)
	anchor, ok := anchorOf(rows)
	if !ok {
		return 0, nil
	}
	var ins []store.Insight
	ins = append(ins, a.recurring(rows, anchor)...)
	ins = append(ins, a.anomalies(rows, anchor)...)
	ins = append(ins, a.duplicates(rows, anchor)...)
	ins = append(ins, a.trends(rows, anchor)...)
	added := 0
	for _, in := range ins {
		ok, err := a.st.AddInsight(ctx, in, a.now())
		if err != nil {
			return added, err
		}
		if ok {
			added++
		}
	}
	return added, nil
}

// anchorOf is the time of the latest spending. A statement may be old, so "the last month"
// is counted from the data, not from today.
func anchorOf(rows []store.LedgerRow) (time.Time, bool) {
	var last time.Time
	for _, r := range rows {
		if r.At.After(last) {
			last = r.At
		}
	}
	return last, len(rows) > 0
}

func kzt(minor int64) string { return money.Format(minor, money.DefaultCurrency) }

func round(minor float64) int64 {
	// to hundreds of tenge: "≈ 4,500 ₸", not "≈ 4,487.33 ₸"
	return int64(math.Round(minor/10000)) * 10000
}

func monthKey(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location())
}

// recurring finds regular payments: a merchant in 3+ different months of the year with an
// almost constant amount (spread < 10%: subscriptions have 0–5%, shops with similar checks
// 15–20%). It also finds price increases: the last payment noticeably above the usual one.
func (a *Analyzer) recurring(rows []store.LedgerRow, anchor time.Time) []store.Insight {
	lastAt := map[string]time.Time{} // the merchant's latest payment of all time
	for _, r := range rows {
		if r.At.After(lastAt[r.MerchantNorm]) {
			lastAt[r.MerchantNorm] = r.At
		}
	}
	since := anchor.AddDate(-1, 0, 0)
	byNorm := map[string][]store.LedgerRow{}
	for _, r := range rows {
		if r.MerchantNorm != "" && r.At.After(since) {
			byNorm[r.MerchantNorm] = append(byNorm[r.MerchantNorm], r)
		}
	}
	type cand struct {
		norm, name, cat string
		n, months, span int
		total, last     int64
		before          float64
	}
	var cands []cand
	for norm, rs := range byNorm {
		months := map[time.Time]bool{}
		var total int64
		var names []string
		first, last := rs[0], rs[0]
		cats := map[string]int{}
		var before []int64
		for _, r := range rs {
			months[monthKey(r.At)] = true
			total += r.Amount
			names = append(names, name(r))
			if r.At.Before(first.At) {
				first = r
			}
			if r.At.After(last.At) || (r.At.Equal(last.At) && r.TxID > last.TxID) {
				last = r
			}
			c := ""
			if len(r.Categories) > 0 {
				c = r.Categories[0]
			}
			cats[c]++
			if r.At.Before(lastAt[norm]) {
				before = append(before, r.Amount)
			}
		}
		n := len(rs)
		if len(months) < 3 || n > 2*len(months) {
			continue
		}
		avg := float64(total) / float64(n)
		var sq float64
		for _, r := range rs {
			sq += (float64(r.Amount) - avg) * (float64(r.Amount) - avg)
		}
		if math.Sqrt(sq/float64(n))/avg >= 0.1 {
			continue
		}
		span := (last.At.Year()-first.At.Year())*12 + int(last.At.Month()-first.At.Month()) + 1
		cat, best := "", -1
		for c, k := range cats {
			if k > best || (k == best && c < cat) {
				cat, best = c, k
			}
		}
		cands = append(cands, cand{norm: norm, name: shortest(names), cat: cat, n: n, months: len(months), span: span,
			total: total, last: last.Amount, before: float64(median(before))})
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].total != cands[j].total {
			return cands[i].total > cands[j].total
		}
		return cands[i].norm < cands[j].norm
	})
	if len(cands) > 30 {
		cands = cands[:30]
	}
	var out []store.Insight
	for _, c := range cands {
		// Divide by the whole span, not by months with payments: otherwise charges on the
		// 31st and the 1st look like two payments in one month.
		perMonth := round(float64(c.total) / float64(max(c.span, c.months)))
		body := fmt.Sprintf("%d payments over %d months, %s in total for the year.", c.n, c.months, kzt(c.total))
		if subscriptionCategories[c.cat] {
			body += " If it is a subscription you do not use, consider cancelling it."
		}
		out = append(out, store.Insight{
			Key:   "recurring:" + c.norm,
			Kind:  "recurring",
			Title: fmt.Sprintf("Recurring payment: %s ≈ %s a month", c.name, kzt(perMonth)),
			Body:  body,
		})
		if c.before > 0 && float64(c.last) > c.before*1.08 && c.last-int64(c.before) >= 50000 {
			pct := (float64(c.last)/c.before - 1) * 100
			out = append(out, store.Insight{
				Key:   fmt.Sprintf("price_up:%s:%d", c.norm, c.last),
				Kind:  "price_up",
				Title: fmt.Sprintf("Price went up: %s — was %s, now %s (+%.0f%%)", c.name, kzt(int64(c.before)), kzt(c.last), pct),
				Body:  "A recurring payment grew. Check the plan or look for an alternative.",
			})
		}
	}
	return out
}

// anomalies finds spending far above the usual for its category and new merchants with large
// amounts. A cash withdrawal is not a purchase, so comparing it with category checks makes no sense.
func (a *Analyzer) anomalies(rows []store.LedgerRow, anchor time.Time) []store.Insight {
	ps := parts(rows)
	byCat := map[string][]int64{}
	for _, p := range ps {
		byCat[p.cat] = append(byCat[p.cat], p.amt)
	}
	med := map[string]int64{}
	for c, xs := range byCat {
		med[c] = median(xs)
	}
	since := anchor.AddDate(0, 0, -90)
	var hits []part
	for _, p := range ps {
		if len(byCat[p.cat]) >= 5 && p.amt > 4*med[p.cat] && p.amt >= minAnomaly && p.r.At.After(since) && p.r.Kind != kaspi.Withdrawal {
			hits = append(hits, p)
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].amt > hits[j].amt })
	if len(hits) > 10 {
		hits = hits[:10]
	}
	var out []store.Insight
	for _, p := range hits {
		out = append(out, store.Insight{
			Key:   fmt.Sprintf("anomaly:%d", p.r.TxID),
			Kind:  "anomaly",
			Title: fmt.Sprintf("Unusually large purchase: %s · %s, %s", kzt(p.amt), name(p.r), p.r.At.Format("2 Jan 2006")),
			Body:  fmt.Sprintf("%s usually costs about %s. Check that this is not a mistake or fraud.", p.cat, kzt(med[p.cat])),
		})
	}

	type agg struct {
		names   []string
		maxAmt  int64
		firstAt time.Time
		n       int
	}
	byNorm := map[string]*agg{}
	for _, r := range rows {
		if r.MerchantNorm == "" || r.Kind == kaspi.Withdrawal {
			continue
		}
		g, ok := byNorm[r.MerchantNorm]
		if !ok {
			g = &agg{firstAt: r.At}
			byNorm[r.MerchantNorm] = g
		}
		g.names = append(g.names, name(r))
		g.n++
		if r.Amount > g.maxAmt {
			g.maxAmt = r.Amount
		}
		if r.At.Before(g.firstAt) {
			g.firstAt = r.At
		}
	}
	type nb struct {
		norm string
		g    *agg
	}
	var news []nb
	for norm, g := range byNorm {
		if g.firstAt.After(anchor.AddDate(0, 0, -30)) && g.maxAmt >= minNewBig && g.n <= 2 {
			news = append(news, nb{norm, g})
		}
	}
	sort.Slice(news, func(i, j int) bool {
		if news[i].g.maxAmt != news[j].g.maxAmt {
			return news[i].g.maxAmt > news[j].g.maxAmt
		}
		return news[i].norm < news[j].norm
	})
	if len(news) > 5 {
		news = news[:5]
	}
	for _, x := range news {
		out = append(out, store.Insight{
			Key:   "newbig:" + x.norm,
			Kind:  "anomaly",
			Title: fmt.Sprintf("New merchant with a large amount: %s · %s", shortest(x.g.names), kzt(x.g.maxAmt)),
			Body:  fmt.Sprintf("First payment on %s. If you do not recognize it, check your card.", x.g.firstAt.Format("2 Jan 2006")),
		})
	}
	return out
}

// duplicates finds the same amount at the same merchant on the same day — a possible double charge.
func (a *Analyzer) duplicates(rows []store.LedgerRow, anchor time.Time) []store.Insight {
	type key struct {
		norm   string
		day    string
		amount int64
	}
	groups := map[key][]store.LedgerRow{}
	since := anchor.AddDate(0, 0, -90)
	for _, r := range rows {
		if r.MerchantNorm != "" && r.Amount >= 5_000_00 && r.At.After(since) {
			k := key{r.MerchantNorm, r.At.Format("2006-01-02"), r.Amount}
			groups[k] = append(groups[k], r)
		}
	}
	var keys []key
	for k, rs := range groups {
		if len(rs) >= 2 {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].amount != keys[j].amount {
			return keys[i].amount > keys[j].amount
		}
		return keys[i].day+keys[i].norm < keys[j].day+keys[j].norm
	})
	if len(keys) > 10 {
		keys = keys[:10]
	}
	var out []store.Insight
	for _, k := range keys {
		rs := groups[k]
		var names []string
		for _, r := range rs {
			names = append(names, name(r))
		}
		d, _ := time.Parse("2006-01-02", k.day)
		out = append(out, store.Insight{
			Key:   fmt.Sprintf("dup:%s:%s:%d", k.norm, k.day, k.amount),
			Kind:  "duplicate",
			Title: fmt.Sprintf("Possible double charge: %d × %s · %s, %s", len(rs), kzt(k.amount), shortest(names), d.Format("2 Jan 2006")),
			Body:  "The same amount at the same merchant on the same day. If there was only one purchase, dispute the charge with the bank.",
		})
	}
	return out
}

// trends finds categories that grew in the last full month against the average of the three
// months before it and against the same month a year ago.
func (a *Analyzer) trends(rows []store.LedgerRow, anchor time.Time) []store.Insight {
	month := monthKey(anchor)
	// The last full month: if the data does not end on the last day, the current month is partial.
	if anchor.AddDate(0, 0, 1).Month() == anchor.Month() {
		month = month.AddDate(0, -1, 0)
	}
	prevFrom, yearAgo := month.AddDate(0, -3, 0), month.AddDate(-1, 0, 0)
	type agg struct {
		cur, prev, yago int64
		prevMonths      map[time.Time]bool
	}
	byCat := map[string]*agg{}
	for _, p := range parts(rows) {
		if p.cat == analytics.Uncategorized {
			continue
		}
		g, ok := byCat[p.cat]
		if !ok {
			g = &agg{prevMonths: map[time.Time]bool{}}
			byCat[p.cat] = g
		}
		m := monthKey(p.r.At)
		switch {
		case m.Equal(month):
			g.cur += p.amt
		case !m.Before(prevFrom) && m.Before(month):
			g.prev += p.amt
			g.prevMonths[m] = true
		case m.Equal(yearAgo):
			g.yago += p.amt
		}
	}
	cats := make([]string, 0, len(byCat))
	for c := range byCat {
		cats = append(cats, c)
	}
	sort.Slice(cats, func(i, j int) bool {
		if byCat[cats[i]].cur != byCat[cats[j]].cur {
			return byCat[cats[i]].cur > byCat[cats[j]].cur
		}
		return cats[i] < cats[j]
	})
	monthName := month.Format("01.2006")
	var out []store.Insight
	for _, c := range cats {
		g := byCat[c]
		prev3 := float64(g.prev) / 3
		if len(g.prevMonths) == 3 && prev3 > 0 && float64(g.cur) > prev3*1.3 && float64(g.cur)-prev3 >= minTrendAdd {
			out = append(out, store.Insight{
				Key:  fmt.Sprintf("trend:%s:%s", c, month.Format("2006-01")),
				Kind: "trend",
				Title: fmt.Sprintf("%s grew: %s in %s against %s on average over 3 months (+%.0f%%)",
					c, kzt(g.cur), monthName, kzt(round(prev3)), (float64(g.cur)/prev3-1)*100),
				Body: "See which purchases drove it: /stat " + month.Format("2006-01") + " in the bot or the analytics page for that month.",
			})
		}
		if g.yago > 0 && float64(g.cur) > float64(g.yago)*1.3 && g.cur-g.yago >= minTrendAdd {
			out = append(out, store.Insight{
				Key:  fmt.Sprintf("yoy:%s:%s", c, month.Format("2006-01")),
				Kind: "trend",
				Title: fmt.Sprintf("%s: %s in %s against %s a year ago (+%.0f%%)",
					c, kzt(g.cur), monthName, kzt(g.yago), (float64(g.cur)/float64(g.yago)-1)*100),
				Body: "Growth above inflation is worth a look.",
			})
		}
	}
	return out
}

// Tips asks the local model for saving tips based on finished numbers, once per month of
// data: the insight key is tied to the month of the latest spending.
func (a *Analyzer) Tips(ctx context.Context) (int, error) {
	if a.llm == nil || !a.llm.Available() {
		return 0, nil
	}
	all, err := a.ledger(ctx)
	if err != nil {
		return 0, err
	}
	rows := spendRows(all)
	anchor, ok := anchorOf(rows)
	if !ok {
		return 0, nil
	}
	month := anchor.Format("2006-01")
	if done, err := a.st.GetKV(ctx, "tips:"+month); err != nil || done != "" {
		return 0, err
	}
	// With a couple of weeks or a dozen purchases tips come out made up — wait for data.
	first := anchor
	for _, r := range rows {
		if r.At.Before(first) {
			first = r.At
		}
	}
	if len(rows) < minTipsOps || anchor.Sub(first) < minTipsDays*24*time.Hour {
		return 0, nil
	}

	type catRow struct {
		Cat   string  `json:"cat"`
		Month float64 `json:"per_month"`
		Share float64 `json:"share"`
	}
	since := anchor.AddDate(0, -3, 0)
	sums := map[string]int64{}
	var total int64
	for _, p := range parts(rows) {
		if p.r.At.After(since) {
			sums[p.cat] += p.amt
			total += p.amt
		}
	}
	var cats []catRow
	for c, s := range sums {
		share := 0.0
		if total > 0 {
			share = float64(s) / float64(total)
		}
		cats = append(cats, catRow{Cat: c, Month: float64(s) / 3 / 100, Share: share})
	}
	sort.Slice(cats, func(i, j int) bool { return cats[i].Month > cats[j].Month })
	if len(cats) > 12 {
		cats = cats[:12]
	}
	recent, err := a.st.Insights(ctx, false, 30)
	if err != nil {
		return 0, err
	}
	var facts []string
	for _, in := range recent {
		if in.Kind != "tip" {
			facts = append(facts, in.Title)
		}
	}
	data, _ := json.Marshal(map[string]any{
		"categories_per_month_tenge": cats,
		"findings":                   facts,
	})

	out, err := a.llm.JSON(ctx, tipsSystem, string(data))
	if err != nil {
		a.log.Warn("analysis: tips llm failed", "err", err)
		return 0, nil
	}
	var resp struct {
		Tips []struct {
			Title string `json:"title"`
			Body  string `json:"body"`
		} `json:"tips"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		a.log.Warn("analysis: tips invalid json", "err", err)
		return 0, nil
	}
	added := 0
	for i, t := range resp.Tips {
		if i >= 5 || strings.TrimSpace(t.Title) == "" {
			break
		}
		ok, err := a.st.AddInsight(ctx, store.Insight{
			Key: fmt.Sprintf("tip:%s:%d", month, i), Kind: "tip",
			Title: clip(t.Title, 140), Body: clip(t.Body, 600),
		}, a.now())
		if err != nil {
			return added, err
		}
		if ok {
			added++
		}
	}
	return added, a.st.SetKV(ctx, "tips:"+month, a.now().Format(time.RFC3339))
}

const tipsSystem = `You are a personal finance assistant. You get a person's spending by category (tenge per month,
share of all spending) and findings (subscriptions, growing categories, anomalies). Give 3–5 concrete tips on how
to save. Use only the data in the request; do not invent numbers or merchants. Write in English, briefly.
Answer with JSON only: {"tips": [{"title": "<up to 100 characters>", "body": "<2–3 sentences>"}]}`

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
