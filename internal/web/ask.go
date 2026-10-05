package web

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"spendbot/internal/analytics"
	"spendbot/internal/llm"
	"spendbot/internal/money"
	"spendbot/internal/store"
)

// The operations page takes a request in plain words ("between 20k and 50k, biggest first")
// and turns it into filters on top of the current ones. The model only translates words into
// filters: it never sees the operations and never computes a number — the page does.

// WithAI sets the model that understands requests on the operations page; nil — rules only.
func (s *Server) WithAI(p llm.Provider) *Server {
	s.ai = p
	return s
}

func (s *Server) aiReady() bool { return s.ai != nil && s.ai.Available() }

// intent is a request turned into filters. Empty fields leave the current filter alone.
type intent struct {
	Min, Max   int64 // tiyn; -1 — not mentioned
	From, To   string
	Kind, Dir  string
	Category   string
	Text       string
	Repeat     string
	TimesMin   int // number of operations with a person, the transfers page only; 0 — no bound
	TimesMax   int
	Sort       string
	Group      string
	Clear      []string
	understood bool
}

func newIntent() intent { return intent{Min: -1, Max: -1} }

// ---- rules: amounts and a few phrases, no model needed ----

var (
	numRe   = regexp.MustCompile(`(?i)(\d[\d\s\x{00a0}]*(?:[.,]\d+)?)\s*(тысяч\p{L}*|тыс\.?|thousand|млн|million|mln|k|к|m|м)?(?:\s*(?:₸|тг|тенге|kzt|tenge))?`)
	monthRe = regexp.MustCompile(`(?i)(январ|феврал|март|апрел|ма[йя]|июн|июл|август|сентябр|октябр|ноябр|декабр|jan|feb|mar|apr|may|jun|jul|aug|sep|oct|nov|dec|год|year)\p{L}*\s*$`)
	moreRe  = regexp.MustCompile(`(?i)(больше|более|свыше|выше|от|над|не меньше|минимум|over|above|more than|at least|from|min|>=?|≥)\s*$`)
	lessRe  = regexp.MustCompile(`(?i)(меньше|менее|ниже|до|под|не больше|максимум|under|below|less than|at most|up to|max|<=?|≤)\s*$`)
	rangeRe = regexp.MustCompile(`(?i)(между|от|between|from)\s*$`)
)

type number struct {
	value      float64 // in tenge
	unit       string  // "k", "млн"…; empty — plain tenge
	start, end int
}

func unitScale(u string) float64 {
	u = strings.ToLower(strings.TrimSuffix(u, "."))
	switch {
	case u == "":
		return 1
	case strings.HasPrefix(u, "млн"), u == "million", u == "mln", u == "m", u == "м":
		return 1e6
	}
	return 1e3
}

// parseAmount reads "20000", "20 000", "20k", "1.5m", "20 тыс" as tenge.
func parseAmount(s string) (float64, bool) {
	m := numRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil || strings.TrimSpace(m[0]) != strings.TrimSpace(s) {
		return 0, false
	}
	return numValue(m[1], m[2])
}

func numValue(digits, unit string) (float64, bool) {
	d := strings.NewReplacer(" ", "", " ", "", ",", ".").Replace(digits)
	v, err := strconv.ParseFloat(d, 64)
	if err != nil {
		return 0, false
	}
	return v * unitScale(unit), true
}

// timesRe finds counts: "больше 5 раз", "at least 3 times", "2 раза".
var timesRe = regexp.MustCompile(`(?i)(больше|более|свыше|over|more than|не меньше|at least|от|меньше|менее|under|less than|fewer than|не больше|at most|до)?\s*(\d+)\s*(раза|раз|times|переводов|перевода|переводы|перевод|операций|операции|операция|платежей|платежа|transfers|transfer|operations|operation|payments)([^\p{L}]|$)`)

// yearRe finds a whole year: "в 2026 году", "за 2025", "in 2026".
var yearRe = regexp.MustCompile(`(?i)(?:^|\s)(?:в|за|in|during|for)\s+((?:19|20)\d\d)(?:\s*(?:году|год|г\.?))?(?:[^\d]|$)`)

// countIntent reads counts and blanks them out, so "5 раз" is not read as 5 ₸.
func countIntent(in *intent, text string) string {
	for _, m := range timesRe.FindAllStringSubmatchIndex(text, -1) {
		n, err := strconv.Atoi(text[m[4]:m[5]])
		if err != nil || n <= 0 || n > 10000 {
			continue
		}
		switch strings.ToLower(sub(text, m[2], m[3])) {
		case "больше", "более", "свыше", "over", "more than":
			in.TimesMin, in.TimesMax = n+1, 0
		case "не меньше", "at least", "от":
			in.TimesMin, in.TimesMax = n, 0
		case "меньше", "менее", "under", "less than", "fewer than":
			in.TimesMin, in.TimesMax = 0, max(n-1, 1)
		case "не больше", "at most", "до":
			in.TimesMin, in.TimesMax = 0, n
		default:
			in.TimesMin, in.TimesMax = n, n
		}
		in.understood = true
		text = text[:m[0]] + strings.Repeat(" ", m[7]-m[0]) + text[m[7]:]
	}
	return text
}

func ruleIntent(text string) intent {
	in := newIntent()
	text = countIntent(&in, text)
	if m := yearRe.FindStringSubmatchIndex(text); m != nil {
		y := text[m[2]:m[3]]
		in.From, in.To, in.understood = y+"-01-01", y+"-12-31", true
		text = text[:m[2]] + strings.Repeat(" ", m[3]-m[2]) + text[m[3]:]
	}
	low := strings.ToLower(text)
	var nums []number
	for _, m := range numRe.FindAllStringSubmatchIndex(text, -1) {
		v, ok := numValue(text[m[2]:m[3]], sub(text, m[4], m[5]))
		if !ok {
			continue
		}
		before := text[:m[0]]
		// "September 2026", "2026 год" are dates, not amounts
		if m[4] < 0 && v >= 1990 && v <= 2100 && (monthRe.MatchString(before) || strings.HasPrefix(strings.TrimSpace(strings.ToLower(text[m[1]:])), "год")) {
			continue
		}
		nums = append(nums, number{value: v, unit: sub(text, m[4], m[5]), start: m[0], end: m[1]})
	}
	tiyn := func(v float64) int64 { return int64(math.Round(v * 100)) }
	switch {
	case len(nums) >= 2:
		a, b := nums[0], nums[1]
		between := strings.ToLower(text[a.end:b.start])
		if rangeRe.MatchString(text[:a.start]) || strings.ContainsAny(between, "-–—") ||
			strings.Contains(between, " и ") || strings.Contains(between, "and") || strings.Contains(between, "до") || strings.Contains(between, "to") {
			// "20-50k": the unit of the second number applies to the first one too
			if a.unit == "" && b.unit != "" {
				a.value *= unitScale(b.unit)
			}
			lo, hi := min(a.value, b.value), max(a.value, b.value)
			in.Min, in.Max, in.understood = tiyn(lo), tiyn(hi), true
		}
	case len(nums) == 1:
		n := nums[0]
		before := text[:n.start]
		switch {
		case lessRe.MatchString(before):
			in.Max, in.understood = tiyn(n.value), true
		case moreRe.MatchString(before):
			in.Min, in.understood = tiyn(n.value), true
		}
	}
	has := func(words ...string) bool {
		for _, w := range words {
			if strings.Contains(low, w) {
				return true
			}
		}
		return false
	}
	set := func(field *string, v string) { *field, in.understood = v, true }
	switch {
	case has("минус", "долг", "должен", "должна", "должны", "в плюсе", "balance", "owe"):
		set(&in.Sort, analytics.PeopleBalance)
	case has("чаще", "most often", "most transfers", "больше всего раз", "больше всего переводов"):
		set(&in.Sort, analytics.PeopleCount)
	case has("больше всего", "most"):
		if in.Min < 0 && in.Max < 0 {
			set(&in.Sort, analytics.SortBig)
		}
	case has("крупн", "больш", "дорог", "biggest", "largest", "expensive"):
		if in.Min < 0 && in.Max < 0 || has("сначала", "first", "сортир", "sort") {
			set(&in.Sort, analytics.SortBig)
		}
	case has("мелк", "маленьк", "дешев", "smallest", "cheapest"):
		set(&in.Sort, analytics.SortSmall)
	case has("старые", "сначала стар", "oldest"):
		set(&in.Sort, analytics.SortOld)
	}
	switch {
	case has("по дням недели", "by weekday", "дни недели"):
		set(&in.Group, "weekday")
	case has("по месяц", "by month", "monthly", "помесячно"):
		set(&in.Group, "month")
	case has("по недел", "by week", "weekly"):
		set(&in.Group, "week")
	case has("по дням", "by day", "daily"):
		set(&in.Group, "day")
	case has("по категори", "by category"):
		set(&in.Group, "category")
	case has("по магазин", "по продав", "по людям", "по получател", "by merchant", "by person", "by shop"):
		set(&in.Group, "merchant")
	}
	switch {
	case has("входящ", "поступлен", "получил", "пришл", "присл", "скинул мне", "скинули мне", "перевел мне", "перевёл мне", "перевели мне", "incoming", "received", "sent me", "money in"):
		set(&in.Dir, analytics.DirIn)
	case has("исходящ", "отправ", "перевел", "перевёл", "outgoing", "money out"):
		set(&in.Dir, analytics.DirOut)
	case has("кому", "to whom"):
		set(&in.Dir, analytics.DirOut)
	}
	switch {
	case has("только один раз", "один раз", "единожды", "однократ", "only once", "just once", "a single time"):
		set(&in.Repeat, analytics.RepeatOnce)
	case has("впервые", "первый раз", "в первый", "первые переводы", "новым получател", "новые получател", "first time", "for the first", "new recipient", "new people"):
		set(&in.Repeat, analytics.RepeatFirst)
	}
	if has("сбрось", "очисти", "убери все", "clear", "reset") {
		in.Clear, in.understood = []string{"amount", "period", "category", "text", "merchant", "direction", "sort", "group"}, true
	}
	return in
}

func sub(s string, i, j int) string {
	if i < 0 || j < 0 {
		return ""
	}
	return s[i:j]
}

// ---- the model ----

const askSystem = `You turn a request about a list of bank card operations into filters for that list.
You never compute sums or counts: the program does that from your filters. Reply with one JSON object.
Amounts are in Kazakhstani tenge (₸): "20k", "20к", "20 тыс" = 20000; "1.5m", "1,5 млн" = 1500000.
Keys (leave a key out when the request does not mention it):
  "min_amount": number, tenge, inclusive;  "max_amount": number, tenge, inclusive;
  "from": "YYYY-MM-DD";  "to": "YYYY-MM-DD", inclusive;
  "kind": "spend" (spending) | "transfers" (transfers to and from people) | "all";
  "direction": "out" (money spent or sent) | "in" (money received);
  "category": one name from the given categories, exactly as written;
  "text": words to find in the merchant or person name;
  "repeat": "first" (the first operation ever with that person or merchant) | "once" (the only operation ever with them);
  "sort": "new" | "old" | "big" (largest amount first) | "small";
  "group": "month" | "week" | "day" | "weekday" | "category" | "merchant" | "kind";
  "clear": list of filters to remove, from "amount", "period", "category", "text", "merchant", "direction", "repeat", "sort", "group".
The request may be in Russian, Kazakh or English. It refines the current filters: keep what it does not mention.
Only fill a key the request clearly asks for: never add a period, sorting or grouping on your own.`

func (s *Server) modelIntent(ctx context.Context, system, text string, cur url.Values, cats []string) (intent, error) {
	in := newIntent()
	user, _ := json.Marshal(map[string]any{
		"request":         text,
		"today":           s.now().In(s.loc).Format("2006-01-02 (Monday)"),
		"current_filters": describe(cur),
		"categories":      cats,
	})
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	raw, err := s.ai.JSON(ctx, system, string(user))
	if err != nil {
		return in, err
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return in, fmt.Errorf("model answer is not JSON: %w", err)
	}
	str := func(k string) string {
		switch v := m[k].(type) {
		case string:
			return strings.TrimSpace(v)
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64)
		}
		return ""
	}
	amount := func(k string) int64 {
		v := str(k)
		if v == "" {
			return -1
		}
		t, ok := parseAmount(v)
		if !ok || t < 0 {
			return -1
		}
		return int64(math.Round(t * 100))
	}
	in.Min, in.Max = amount("min_amount"), amount("max_amount")
	in.From, in.To = str("from"), str("to")
	in.Kind, in.Dir, in.Category, in.Text, in.Sort, in.Group = str("kind"), str("direction"), str("category"), str("text"), str("sort"), str("group")
	in.Repeat = str("repeat")
	count := func(k string) int {
		n, err := strconv.Atoi(strings.SplitN(str(k), ".", 2)[0])
		if err != nil || n < 0 || n > 10000 {
			return 0
		}
		return n
	}
	in.TimesMin, in.TimesMax = count("times_min"), count("times_max")
	if cl, ok := m["clear"].([]any); ok {
		for _, c := range cl {
			if c, ok := c.(string); ok {
				in.Clear = append(in.Clear, c)
			}
		}
	}
	return in, nil
}

// Cues a request must contain for the model's answer to set a filter: a small model sometimes
// adds a grouping, a sort or today's date nobody asked for.
var cues = map[string][]string{
	"sort":      {"чаще", "баланс", "долг", "должн", "алфавит", "имени", "often", "balance", "owe", "name", "сначала", "сортир", "отсорт", "крупн", "самы", "наибол", "мелк", "дорог", "дешев", "стар", "нов", "первы", "first", "sort", "big", "larg", "small", "old", "new", "expensive", "cheap", "top", "топ"},
	"group":     {"по ", "сгрупп", "группир", "разбей", "разбив", "помесяч", "понедел", "by ", "group", "per ", "monthly", "weekly", "daily", "breakdown"},
	"period":    {"январ", "феврал", "март", "апрел", "мая", "май", "июн", "июл", "август", "сентябр", "октябр", "ноябр", "декабр", "вчера", "сегодня", "недел", "месяц", "год", "дней", "день", "дня", "прошл", "этот", "этом", "текущ", "с ", "после", "jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec", "today", "yesterday", "week", "month", "year", "day", "since", "after", "before", "last", "this"},
	"direction": {"присл", "кому", "мне", "получ", "входящ", "пришл", "приход", "поступ", "отправ", "перев", "исходящ", "расход", "receiv", "incoming", "sent", "send", "outgoing", " in", " out"},
	"kind":      {"перевод", "трат", "покуп", "расход", "все операц", "всё", "transfer", "spend", "purchase", "all "},
	"repeat":    {"впервые", "перв", "один раз", "единожды", "однократ", "нов", "first", "once", "single", "new"},
	"times":     {"раз", "times", "once", "один", "single", "перевод", "операц", "платеж", "transfer", "operation", "payment"},
	"clear":     {"сброс", "очист", "убери", "без ", "удали", "remove", "clear", "reset", "without", "drop"},
}

// dateRe finds a year or a date in a request: "2026", "05.10", "5/10".
var dateRe = regexp.MustCompile(`(^|[^\d])((19|20)\d\d|\d{1,2}[./]\d{1,2})([^\d]|$)`)

func hasCue(text, key string) bool {
	if key == "period" && dateRe.MatchString(text) {
		return true
	}
	low := " " + strings.ToLower(text) + " "
	for _, c := range cues[key] {
		if strings.Contains(low, c) {
			return true
		}
	}
	return false
}

// ground drops what the model set without a cue in the request.
func ground(in intent, text string) intent {
	if !hasCue(text, "sort") {
		in.Sort = ""
	}
	if !hasCue(text, "group") {
		in.Group = ""
	}
	if !hasCue(text, "period") {
		in.From, in.To = "", ""
	}
	if !hasCue(text, "direction") {
		in.Dir = ""
	}
	if !hasCue(text, "kind") {
		in.Kind = ""
	}
	if !hasCue(text, "repeat") {
		in.Repeat = ""
	}
	if !hasCue(text, "times") {
		in.TimesMin, in.TimesMax = 0, 0
	}
	if !hasCue(text, "clear") {
		in.Clear = nil
	}
	// searched words must come from the request itself
	if in.Text != "" && !strings.Contains(analytics.Fold(text), analytics.Fold(in.Text)) {
		in.Text = ""
	}
	return in
}

// describe is the current filter in words, for the model.
func describe(q url.Values) map[string]string {
	out := map[string]string{}
	for k, name := range map[string]string{"type": "kind", "period": "period", "from": "from", "to": "to", "cat": "category",
		"m": "merchant", "q": "text", "min": "min_amount", "max": "max_amount", "dir": "direction", "repeat": "repeat", "sort": "sort", "group": "group",
		"nmin": "times_min", "nmax": "times_max", "new": "repeat_first"} {
		if v := q.Get(k); v != "" {
			out[name] = v
		}
	}
	return out
}

// ---- applying ----

func tenge(t int64) string { return strconv.FormatInt(t/100, 10) }

// applyCommon puts what both pages share on top of the query — clearing, amount, period,
// direction, text — and returns what changed, in words. Other keys to clear are returned.
func (s *Server) applyCommon(q url.Values, in intent) (done, rest []string) {
	for _, c := range in.Clear {
		switch c {
		case "amount":
			q.Del("min")
			q.Del("max")
		case "period":
			q.Del("period")
			q.Del("from")
			q.Del("to")
		case "text":
			q.Del("q")
		case "direction":
			q.Del("dir")
		case "sort":
			q.Del("sort")
		default:
			rest = append(rest, c)
			continue
		}
		done = append(done, "removed "+c)
	}
	if in.Min >= 0 && in.Max >= 0 && in.Min > in.Max {
		in.Min, in.Max = in.Max, in.Min
	}
	kzt := func(t int64) string { return money.Format(t, money.DefaultCurrency) }
	switch {
	case in.Min > 0 && in.Max > 0:
		q.Set("min", tenge(in.Min))
		q.Set("max", tenge(in.Max))
		done = append(done, "amount "+kzt(in.Min)+" – "+kzt(in.Max))
	case in.Min > 0:
		q.Set("min", tenge(in.Min))
		q.Del("max")
		done = append(done, "amount from "+kzt(in.Min))
	case in.Max > 0:
		q.Set("max", tenge(in.Max))
		q.Del("min")
		done = append(done, "amount up to "+kzt(in.Max))
	}
	day := func(v string) (time.Time, bool) {
		t, err := time.ParseInLocation("2006-01-02", v, s.loc)
		return t, err == nil
	}
	from, okF := day(in.From)
	to, okT := day(in.To)
	if okF || okT {
		if !okF {
			from = time.Date(2000, 1, 1, 0, 0, 0, 0, s.loc)
		}
		if !okT {
			to = s.now().In(s.loc)
		}
		if to.Before(from) {
			from, to = to, from
		}
		q.Del("period")
		q.Set("from", from.Format("2006-01-02"))
		q.Set("to", to.Format("2006-01-02"))
		done = append(done, from.Format("2 Jan 2006")+" – "+to.Format("2 Jan 2006"))
	}
	if in.Dir == analytics.DirIn || in.Dir == analytics.DirOut {
		q.Set("dir", in.Dir)
		done = append(done, map[string]string{"in": "money in", "out": "money out"}[in.Dir])
	}
	if t := strings.Join(strings.Fields(in.Text), " "); t != "" && len([]rune(t)) <= 60 {
		q.Set("q", t)
		done = append(done, "“"+t+"”")
	}
	return done, rest
}

// apply puts an intent on top of the operations page query and returns what changed, in words.
func (s *Server) apply(q url.Values, in intent, cats []store.Category) []string {
	done, rest := s.applyCommon(q, in)
	for _, c := range rest {
		switch c {
		case "category":
			q.Del("cat")
		case "merchant":
			q.Del("m")
		case "repeat", "group":
			q.Del(c)
		default:
			continue
		}
		done = append(done, "removed "+c)
	}
	if in.Category != "" {
		name := ""
		for _, c := range cats {
			if strings.EqualFold(c.Name, in.Category) {
				name = c.Name
			}
		}
		for _, p := range []string{analytics.CashCategory, analytics.Uncategorized} {
			if strings.EqualFold(p, in.Category) {
				name = p
			}
		}
		if name != "" {
			q.Set("cat", name)
			q.Set("type", analytics.TypeSpend)
			done = append(done, "category "+name)
		}
	}
	switch in.Kind {
	case analytics.TypeSpend, analytics.TypeTransfers, analytics.TypeAll:
		if q.Get("type") != in.Kind && (in.Category == "" || in.Kind == analytics.TypeSpend) {
			q.Set("type", in.Kind)
			if in.Kind != analytics.TypeSpend {
				q.Del("cat")
			}
			done = append(done, map[string]string{"spend": "spending", "transfers": "transfers", "all": "all operations"}[in.Kind])
		}
	}
	if q.Get("dir") == analytics.DirIn && (q.Get("type") == analytics.TypeSpend || q.Get("type") == "") {
		q.Set("type", analytics.TypeAll) // money in is never spending
	}
	switch in.Repeat {
	case analytics.RepeatFirst:
		q.Set("repeat", in.Repeat)
		done = append(done, "first operation with each merchant or person")
	case analytics.RepeatOnce:
		q.Set("repeat", in.Repeat)
		done = append(done, "only one operation with them ever")
	}
	if slices.Contains([]string{analytics.SortNew, analytics.SortOld, analytics.SortBig, analytics.SortSmall}, in.Sort) {
		q.Set("sort", in.Sort)
		done = append(done, sortTitles[in.Sort])
	}
	for _, g := range analytics.Groupings {
		if g.Key == in.Group {
			q.Set("group", g.Key)
			done = append(done, "grouped by "+g.Title)
		}
	}
	return done
}

// applyPeople puts an intent on top of the transfers page query: amounts and counts are
// totals per person on the chosen side.
func (s *Server) applyPeople(q url.Values, in intent) []string {
	done, rest := s.applyCommon(q, in)
	for _, c := range rest {
		switch c {
		case "repeat", "times":
			q.Del("new")
			q.Del("nmin")
			q.Del("nmax")
		default:
			continue
		}
		done = append(done, "removed "+c)
	}
	switch in.Repeat {
	case analytics.RepeatOnce:
		in.TimesMin, in.TimesMax = 1, 1
	case analytics.RepeatFirst:
		q.Set("new", "1")
		done = append(done, "people new in the period")
	}
	if in.TimesMax > 0 && in.TimesMin > in.TimesMax {
		in.TimesMin, in.TimesMax = in.TimesMax, in.TimesMin
	}
	switch {
	case in.TimesMin > 0 && in.TimesMin == in.TimesMax:
		q.Set("nmin", strconv.Itoa(in.TimesMin))
		q.Set("nmax", strconv.Itoa(in.TimesMax))
		done = append(done, fmt.Sprintf("exactly %d %s", in.TimesMin, plural(in.TimesMin, "operation", "operations")))
	case in.TimesMin > 0 || in.TimesMax > 0:
		q.Del("nmin")
		q.Del("nmax")
		if in.TimesMin > 0 {
			q.Set("nmin", strconv.Itoa(in.TimesMin))
			done = append(done, fmt.Sprintf("at least %d operations", in.TimesMin))
		}
		if in.TimesMax > 0 {
			q.Set("nmax", strconv.Itoa(in.TimesMax))
			done = append(done, fmt.Sprintf("at most %d operations", in.TimesMax))
		}
	}
	if t, ok := peopleSorts[in.Sort]; ok && in.Sort != "" {
		q.Set("sort", in.Sort)
		done = append(done, t)
	}
	return done
}

var peopleSorts = map[string]string{
	analytics.PeopleTurnover: "by turnover", analytics.PeopleBig: "largest total first",
	analytics.PeopleSmall: "smallest total first", analytics.PeopleCount: "most operations first",
	analytics.PeopleNew: "latest first", analytics.PeopleOld: "earliest first",
	analytics.PeopleBalance: "sent the most beyond what came back first", analytics.PeopleName: "by name",
}

const askPeopleSystem = `You turn a request about a list of people into filters for that list. The list shows,
per person, the total of bank transfers sent to them and the total of money received from them.
You never compute sums or counts: the program does that from your filters. Reply with one JSON object.
Amounts are in Kazakhstani tenge (₸): "20k", "20к", "20 тыс" = 20000; "1.5m", "1,5 млн" = 1500000.
Keys (leave a key out when the request does not mention it):
  "direction": "out" (people money was sent to) | "in" (people money came from);
  "min_amount", "max_amount": number, tenge, inclusive — the person's total on that side;
  "times_min", "times_max": integers — the number of transfers on that side ("only once" = 1 and 1);
  "repeat": "first" — people whose first transfer ever falls in the period;
  "from": "YYYY-MM-DD";  "to": "YYYY-MM-DD", inclusive — which operations count;
  "text": words to find in the name;
  "sort": "big" | "small" (by the total) | "count" | "new" (latest first) | "old" | "balance" | "name";
  "clear": list of filters to remove, from "amount", "period", "text", "direction", "times", "repeat", "sort".
The request may be in Russian, Kazakh or English. It refines the current filters: keep what it does not mention.
Only fill a key the request clearly asks for: never add a period or sorting on your own.`

var sortTitles = map[string]string{
	analytics.SortNew: "newest first", analytics.SortOld: "oldest first",
	analytics.SortBig: "largest first", analytics.SortSmall: "smallest first",
}

// askOn makes the handler that turns a request in plain words into filters and opens the
// page with them: "operations" or "transfers".
func (s *Server) askOn(page string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { s.ask(w, r, page) }
}

func (s *Server) ask(w http.ResponseWriter, r *http.Request, page string) {
	ctx := r.Context()
	text := strings.TrimSpace(r.PostFormValue("prompt"))
	q, err := url.ParseQuery(r.PostFormValue("state"))
	if err != nil {
		q = url.Values{}
	}
	for _, k := range []string{"msg", "limit", "ask"} {
		q.Del(k)
	}
	back := func(msg string) {
		q.Set("msg", msg)
		if text != "" {
			q.Set("ask", text)
		}
		http.Redirect(w, r, "/ui/"+page+"?"+q.Encode(), http.StatusSeeOther)
	}
	if text == "" {
		back("Type what to find, for example: between 20k and 50k, largest first.")
		return
	}
	if len([]rune(text)) > 300 {
		text = string([]rune(text)[:300])
	}
	var cats []store.Category
	system := askPeopleSystem
	if page == "operations" {
		system = askSystem
		if cats, err = s.st.Categories(ctx); err != nil {
			s.fail(w, err)
			return
		}
	}
	rules := ruleIntent(text)
	in, by := rules, "rules"
	if s.aiReady() {
		var names []string
		if page == "operations" {
			for _, c := range cats {
				names = append(names, c.Name)
			}
			names = append(names, analytics.CashCategory, analytics.Uncategorized)
		}
		model, err := s.modelIntent(ctx, system, text, q, names)
		if err != nil {
			s.log.Warn("web: ask model", "err", err)
		} else {
			// The model understands the rest; amounts and counts the rules read are exact and win.
			in, by = ground(model, text), "model"
			if rules.Min >= 0 || rules.Max >= 0 {
				in.Min, in.Max = rules.Min, rules.Max
			}
			if rules.TimesMin > 0 || rules.TimesMax > 0 {
				in.TimesMin, in.TimesMax = rules.TimesMin, rules.TimesMax
			}
			// so are the keywords the rules found: a small model mixes up "the most" and "the balance"
			for _, f := range []struct {
				dst *string
				v   string
			}{{&in.Sort, rules.Sort}, {&in.Group, rules.Group}, {&in.Dir, rules.Dir}, {&in.Repeat, rules.Repeat},
				{&in.From, rules.From}, {&in.To, rules.To}} {
				if f.v != "" {
					*f.dst = f.v
				}
			}
			in.Clear = append(in.Clear, rules.Clear...)
		}
	}
	var done []string
	if page == "operations" {
		done = s.apply(q, in, cats)
	} else {
		done = s.applyPeople(q, in)
	}
	if len(done) == 0 {
		back("Could not turn that into filters. Try: between 20k and 50k · over 100k · largest first · only once · only September.")
		return
	}
	who := "Understood by the model"
	if by == "rules" {
		who = "Understood without the model"
	}
	back(who + ": " + strings.Join(done, " · ") + ".")
}
