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

func ruleIntent(text string) intent {
	in := newIntent()
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
	case has("входящ", "поступлен", "получил", "пришл", "incoming", "received", "money in"):
		set(&in.Dir, analytics.DirIn)
	case has("исходящ", "отправил", "перевел", "перевёл", "outgoing", "money out"):
		set(&in.Dir, analytics.DirOut)
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
  "sort": "new" | "old" | "big" (largest amount first) | "small";
  "group": "month" | "week" | "day" | "weekday" | "category" | "merchant" | "kind";
  "clear": list of filters to remove, from "amount", "period", "category", "text", "merchant", "direction", "sort", "group".
The request may be in Russian, Kazakh or English. It refines the current filters: keep what it does not mention.
Only fill a key the request clearly asks for: never add a period, sorting or grouping on your own.`

func (s *Server) modelIntent(ctx context.Context, text string, cur url.Values, cats []string) (intent, error) {
	in := newIntent()
	user, _ := json.Marshal(map[string]any{
		"request":         text,
		"today":           s.now().In(s.loc).Format("2006-01-02 (Monday)"),
		"current_filters": describe(cur),
		"categories":      cats,
	})
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	raw, err := s.ai.JSON(ctx, askSystem, string(user))
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
	"sort":      {"сначала", "сортир", "отсорт", "крупн", "самы", "наибол", "мелк", "дорог", "дешев", "стар", "нов", "первы", "first", "sort", "big", "larg", "small", "old", "new", "expensive", "cheap", "top", "топ"},
	"group":     {"по ", "сгрупп", "группир", "разбей", "разбив", "помесяч", "понедел", "by ", "group", "per ", "monthly", "weekly", "daily", "breakdown"},
	"period":    {"январ", "феврал", "март", "апрел", "мая", "май", "июн", "июл", "август", "сентябр", "октябр", "ноябр", "декабр", "вчера", "сегодня", "недел", "месяц", "год", "дн", "прошл", "этот", "этом", "текущ", "с ", "после", "jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec", "today", "yesterday", "week", "month", "year", "day", "since", "after", "before", "last", "this", "20"},
	"direction": {"получ", "входящ", "пришл", "приход", "поступ", "отправ", "перев", "исходящ", "расход", "receiv", "incoming", "sent", "send", "outgoing", " in", " out"},
	"kind":      {"перевод", "трат", "покуп", "расход", "все операц", "всё", "transfer", "spend", "purchase", "all "},
	"clear":     {"сброс", "очист", "убери", "без ", "удали", "remove", "clear", "reset", "without", "drop"},
}

func hasCue(text, key string) bool {
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
		"m": "merchant", "q": "text", "min": "min_amount", "max": "max_amount", "dir": "direction", "sort": "sort", "group": "group"} {
		if v := q.Get(k); v != "" {
			out[name] = v
		}
	}
	return out
}

// ---- applying ----

func tenge(t int64) string { return strconv.FormatInt(t/100, 10) }

// apply puts an intent on top of the query and returns what changed, in words.
func (s *Server) apply(q url.Values, in intent, cats []store.Category) []string {
	var done []string
	for _, c := range in.Clear {
		switch c {
		case "amount":
			q.Del("min")
			q.Del("max")
		case "period":
			q.Del("period")
			q.Del("from")
			q.Del("to")
		case "category":
			q.Del("cat")
		case "text":
			q.Del("q")
		case "merchant":
			q.Del("m")
		case "direction":
			q.Del("dir")
		case "sort", "group":
			q.Del(c)
		default:
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
	switch in.Dir {
	case analytics.DirIn, analytics.DirOut:
		q.Set("dir", in.Dir)
		if in.Dir == analytics.DirIn && q.Get("type") == analytics.TypeSpend || in.Dir == analytics.DirIn && q.Get("type") == "" {
			q.Set("type", analytics.TypeAll) // money in is never spending
		}
		done = append(done, map[string]string{"in": "money in", "out": "money out"}[in.Dir])
	}
	if t := strings.Join(strings.Fields(in.Text), " "); t != "" && len([]rune(t)) <= 60 {
		q.Set("q", t)
		done = append(done, "“"+t+"”")
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

var sortTitles = map[string]string{
	analytics.SortNew: "newest first", analytics.SortOld: "oldest first",
	analytics.SortBig: "largest first", analytics.SortSmall: "smallest first",
}

// ask turns a request in plain words into filters and opens the operations page with them.
func (s *Server) ask(w http.ResponseWriter, r *http.Request) {
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
		http.Redirect(w, r, "/ui/operations?"+q.Encode(), http.StatusSeeOther)
	}
	if text == "" {
		back("Type what to find, for example: between 20k and 50k, largest first.")
		return
	}
	if len([]rune(text)) > 300 {
		text = string([]rune(text)[:300])
	}
	cats, err := s.st.Categories(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	rules := ruleIntent(text)
	in, by := rules, "rules"
	if s.aiReady() {
		names := make([]string, 0, len(cats)+2)
		for _, c := range cats {
			names = append(names, c.Name)
		}
		names = append(names, analytics.CashCategory, analytics.Uncategorized)
		model, err := s.modelIntent(ctx, text, q, names)
		if err != nil {
			s.log.Warn("web: ask model", "err", err)
		} else {
			// The model understands the rest; amounts the rules read are exact and win.
			in, by = ground(model, text), "model"
			if rules.Min >= 0 || rules.Max >= 0 {
				in.Min, in.Max = rules.Min, rules.Max
			}
			for _, f := range []struct {
				dst *string
				v   string
			}{{&in.Sort, rules.Sort}, {&in.Group, rules.Group}, {&in.Dir, rules.Dir}} {
				if *f.dst == "" {
					*f.dst = f.v
				}
			}
			in.Clear = append(in.Clear, rules.Clear...)
		}
	}
	done := s.apply(q, in, cats)
	if len(done) == 0 {
		back("Could not turn that into filters. Try: between 20k and 50k · over 100k · largest first · by month · only September.")
		return
	}
	who := "Understood by the model"
	if by == "rules" {
		who = "Understood without the model"
	}
	back(who + ": " + strings.Join(done, " · ") + ".")
}
