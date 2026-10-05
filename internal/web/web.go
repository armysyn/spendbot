// Package web is the page on the home network: merchant question batches, insights,
// analytics and statement uploads. HTTP Basic password if set, cross-origin protection from stdlib.
package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"spendbot/internal/analysis"
	"spendbot/internal/analytics"
	"spendbot/internal/clickhouse"
	"spendbot/internal/importer"
	"spendbot/internal/kaspi"
	"spendbot/internal/llm"
	"spendbot/internal/merchant"
	"spendbot/internal/money"
	"spendbot/internal/statement"
	"spendbot/internal/store"
)

//go:embed templates/*.html
var templates embed.FS

const maxUpload = 20 << 20

// Kicker starts analysis right away instead of waiting for the ticker (after imports and answers).
type Kicker interface{ Kick() }

type Server struct {
	st       *store.Store
	kick     Kicker // may be nil when analysis is off
	password []byte
	minHits  int
	// batchMaxAge is when a partial batch gets made, for the hint on the home page.
	batchMaxAge time.Duration
	loc         *time.Location
	log         *slog.Logger
	now         func() time.Time
	tmpl        *template.Template
	ch          *clickhouse.Client // nil — analytics reads SQLite
	settings    *Settings          // nil — no settings page
	ai          llm.Provider       // understands requests on the operations page; nil — rules only
	pull        pullState
	pullMu      sync.Mutex
}

// WithAnalytics makes the analytics page read from ClickHouse.
func (s *Server) WithAnalytics(ch *clickhouse.Client) *Server {
	s.ch = ch
	return s
}

func New(st *store.Store, kick Kicker, password string, minHits int, batchMaxAge time.Duration, loc *time.Location, log *slog.Logger) *Server {
	s := &Server{st: st, kick: kick, password: []byte(password), minHits: minHits, batchMaxAge: batchMaxAge,
		loc: loc, log: log, now: time.Now}
	s.tmpl = template.Must(template.New("").Funcs(template.FuncMap{
		"kzt":      func(m int64) string { return money.Format(m, money.DefaultCurrency) },
		"money":    money.Format,
		"date":     func(t time.Time) string { return t.In(loc).Format("2 Jan 2006") },
		"datetime": func(t time.Time) string { return t.In(loc).Format("2 Jan 15:04") },
		"merchant": merchant.Display,
		"kind":     kaspi.KindName,
		"plural":   plural,
		"int":      func(n int64) int { return int(n) },
		"has":      func(xs []string, x string) bool { return slices.Contains(xs, x) },
		"gb":       func(b int64) string { return strconv.FormatFloat(float64(b)/1e9, 'f', 1, 64) },
		"compact":  compact,
		"last":     func(i, n int) bool { return i == n-1 },
		"inc":      func(i int) int { return i + 1 },
		"sub":      func(a, b int64) int64 { return a - b },
		"neg":      func(m int64) int64 { return -m },
		"pct":      func(f float64) string { return pctString(f) },
		"width":    func(f float64) template.CSS { return template.CSS(fmt.Sprintf("%.2f%%", f)) },
		"wday":     func(t time.Time) string { return weekdayShort[t.Weekday()] },
		"link":     link,
		"now":      func() time.Time { return s.now().In(loc) },
		"ymd":      func(t time.Time) string { return t.Format("2006-01-02") },
		"lastDay":  func(t time.Time) string { return t.AddDate(0, 0, -1).Format("2006-01-02") },
		"hm":       func(t time.Time) string { return t.Format("15:04") },
		"dayTitle": dayTitle,
		"chg":      func(c analytics.Change) change { return changeOf(c) },
		"cmp":      analytics.Compare,
		"abs": func(m int64) int64 {
			if m < 0 {
				return -m
			}
			return m
		},
		"initial": func(s string) string {
			for _, r := range s {
				return strings.ToUpper(string(r))
			}
			return "·"
		},
		"hue": func(s string) int {
			h := 0
			for _, r := range s {
				h = (h*31 + int(r)) % 360
			}
			return h
		},
		"join": strings.Join,
		"dict": func(kv ...any) map[string]any {
			m := map[string]any{}
			for i := 0; i+1 < len(kv); i += 2 {
				m[kv[i].(string)] = kv[i+1]
			}
			return m
		},
		"purchase": func(k string) bool { return k == kaspi.Purchase },
		"cols":     cols,
		"swap":     swap,
		"swapAt":   swapAt,
		"subi":     func(a, b int) int { return a - b },
		"add":      func(a, b int) int { return a + b },
		"title": func(s string) string {
			if s == "" {
				return s
			}
			return strings.ToUpper(s[:1]) + s[1:]
		},
		"avg": func(total int64, n int) int64 {
			if n == 0 {
				return 0
			}
			return total / int64(n)
		},
		"hasTime": func(t time.Time) bool { return t.Hour() != 0 || t.Minute() != 0 },
		"istr":    func(n int64) string { return strconv.FormatInt(n, 10) },
		"bool1": func(b bool) string {
			if b {
				return "1"
			}
			return ""
		},
	}).ParseFS(templates, "templates/*.html"))
	return s
}

// Register mounts the pages on mux. Everything under /ui needs the password if one is set.
func (s *Server) Register(mux *http.ServeMux) {
	protect := http.NewCrossOriginProtection()
	protect.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.log.Warn("web: cross-origin request denied", "method", r.Method, "path", r.URL.Path,
			"origin", r.Header.Get("Origin"), "sec_fetch_site", r.Header.Get("Sec-Fetch-Site"))
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
	}))
	ui := http.NewServeMux()
	ui.HandleFunc("GET /ui", s.index)
	ui.HandleFunc("GET /ui/analytics", s.analytics)
	ui.HandleFunc("GET /ui/operations", s.operations)
	ui.HandleFunc("GET /ui/operations.csv", s.operationsCSV)
	ui.HandleFunc("POST /ui/operations/ask", s.askOn("operations"))
	ui.HandleFunc("POST /ui/transfers/ask", s.askOn("transfers"))
	ui.HandleFunc("POST /ui/op/{id}/category", s.opCategory)
	ui.HandleFunc("GET /ui/categories", s.categories)
	ui.HandleFunc("POST /ui/categories", s.categoryAction)
	ui.HandleFunc("POST /ui/savings", s.savings)
	ui.HandleFunc("GET /ui/transfers", s.transfers)
	if s.settings != nil {
		ui.HandleFunc("GET /ui/settings", s.settingsPage)
		ui.HandleFunc("POST /ui/settings/model", s.chooseModel)
		ui.HandleFunc("GET /ui/settings/pull", s.pullStatus)
	}
	ui.HandleFunc("POST /ui/transfers/category", s.transferCategory)
	ui.HandleFunc("GET /ui/batch/{id}", s.batch)
	ui.HandleFunc("POST /ui/batch/{id}", s.answer)
	ui.HandleFunc("POST /ui/import", s.importPDF)
	ui.HandleFunc("POST /ui/insight/{id}/dismiss", s.dismiss)
	h := s.auth(protect.Handler(ui))
	mux.Handle("/ui", h)
	mux.Handle("/ui/", h)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui", http.StatusFound)
	})
}

// auth requires the password when set; without one the page is open to the network.
func (s *Server) auth(next http.Handler) http.Handler {
	if len(s.password) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, pass, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(pass), s.password) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="spendbot", charset="UTF-8"`)
			http.Error(w, "password required (WEB_PASSWORD)", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.log.Error("web: render", "template", name, "err", err)
	}
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	s.log.Error("web", "err", err)
	http.Error(w, "internal error, see the logs", http.StatusInternalServerError)
}

type indexData struct {
	Page
	Summary   *homeSummary // nil — no data yet
	ModelHint string       // the model is not ready — hint to open the settings
	Open      []store.Batch
	Answered  []store.Batch
	Insights  []store.Insight
	Waiting   int       // questions waiting for a batch
	BatchAt   time.Time // when a partial batch will be made
	Flash     string
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var d indexData
	var err error
	if d.Open, err = s.st.Batches(ctx, false, 100); err != nil {
		s.fail(w, err)
		return
	}
	if d.Answered, err = s.st.Batches(ctx, true, 5); err != nil {
		s.fail(w, err)
		return
	}
	if d.Insights, err = s.st.Insights(ctx, false, 100); err != nil {
		s.fail(w, err)
		return
	}
	n, oldest, err := s.st.UnbatchedCount(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	d.Waiting, d.BatchAt = n, oldest.Add(s.batchMaxAge)
	d.Flash = r.URL.Query().Get("msg")
	d.ModelHint = s.modelHint(ctx)
	if d.Summary, err = s.homeSummary(ctx); err != nil {
		s.fail(w, err)
		return
	}
	s.show(w, r, "index.html", "Home", "home", &d)
}

type batchData struct {
	Page
	ID         int64
	Items      []store.BatchItem
	Categories []store.Category
	Answered   bool
}

func (s *Server) batch(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	items, err := s.st.BatchItems(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	if len(items) == 0 {
		http.Redirect(w, r, "/ui?msg="+urlq("The batch is empty or already answered."), http.StatusSeeOther)
		return
	}
	cats, err := s.st.Categories(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	answered := true
	for _, it := range items {
		if it.Status == "open" {
			answered = false
		}
	}
	s.show(w, r, "batch.html", fmt.Sprintf("Batch #%d", id), "home", &batchData{ID: id, Items: items, Categories: cats, Answered: answered})
}

// answer applies a whole batch. Fields of row i: norm_i, cat_i ("", "ignore" or a category id),
// new_i (a new category, wins over cat_i), note_i.
func (s *Server) answer(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	answers := map[string]store.Answer{}
	for i := 0; ; i++ {
		norm := r.PostFormValue(fmt.Sprintf("norm_%d", i))
		if norm == "" {
			break
		}
		a := store.Answer{Note: strings.TrimSpace(r.PostFormValue(fmt.Sprintf("note_%d", i)))}
		newCat := strings.Join(strings.Fields(r.PostFormValue(fmt.Sprintf("new_%d", i))), " ")
		switch cat := r.PostFormValue(fmt.Sprintf("cat_%d", i)); {
		case newCat != "":
			if a.CategoryID, err = s.ensureCategory(ctx, newCat); err != nil {
				s.fail(w, err)
				return
			}
		case cat == "ignore":
			a.Ignore = true
		case cat != "":
			if a.CategoryID, err = strconv.ParseInt(cat, 10, 64); err != nil {
				http.Error(w, "invalid category", http.StatusBadRequest)
				return
			}
		}
		answers[norm] = a
	}
	n, err := s.st.AnswerBatch(ctx, id, answers, s.minHits, s.now())
	if err != nil {
		s.fail(w, err)
		return
	}
	if s.kick != nil {
		s.kick.Kick()
	}
	msg := fmt.Sprintf("Batch #%d: %d answers saved. Merchants without an answer went back to the queue.", id, n)
	http.Redirect(w, r, "/ui?msg="+urlq(msg), http.StatusSeeOther)
}

func (s *Server) ensureCategory(ctx context.Context, name string) (int64, error) {
	if len([]rune(name)) > 40 {
		name = string([]rune(name)[:40])
	}
	c, err := s.st.FindCategory(ctx, name)
	if err == nil {
		return c.ID, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return 0, err
	}
	return s.st.AddCategory(ctx, name)
}

type importData struct {
	Page
	File   string
	Result importer.Result
	From   time.Time
	To     time.Time
	Err    string
}

func (s *Server) importPDF(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	f, hdr, err := r.FormFile("pdf")
	if err != nil {
		s.show(w, r, "import.html", "Statement import", "home", &importData{Err: "Choose a PDF statement (up to 20 MB)."})
		return
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		s.show(w, r, "import.html", "Statement import", "home", &importData{Err: "Could not read the file."})
		return
	}
	st, err := statement.ParseKaspiBytes(b, s.loc)
	if err != nil {
		s.show(w, r, "import.html", "Statement import", "home", &importData{File: hdr.Filename, Err: "Could not parse the statement: " + err.Error()})
		return
	}
	res, err := importer.Import(r.Context(), s.st, st, s.now())
	if err != nil {
		s.fail(w, err)
		return
	}
	s.log.Info("web: statement imported", "ops", res.Total, "review", res.Review, "auto", res.Auto)
	if s.kick != nil {
		s.kick.Kick()
	}
	s.show(w, r, "import.html", "Statement import", "home", &importData{File: hdr.Filename, Result: res, From: st.From, To: st.To})
}

func (s *Server) dismiss(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.st.DismissInsight(r.Context(), id); err != nil {
		s.fail(w, err)
		return
	}
	http.Redirect(w, r, "/ui#insights", http.StatusSeeOther)
}

func urlq(s string) string { return url.QueryEscape(s) }

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

type analyticsData struct {
	Page
	Prev         analytics.Dashboard
	HasPrev      bool
	PrevPeriod   analytics.Period
	PrevCat      map[string]int64
	Older        string // keys of the neighbouring months for the arrows
	Newer        string
	Heat         *heatmap
	RecOK        int
	Empty        bool
	Source       string           // where the data came from: ClickHouse or SQLite
	Fallback     bool             // ClickHouse is configured but unreachable — SQLite data is shown
	NoSavings    bool             // the "without savings" switch
	SavingsNames []string         // categories marked as savings
	Categories   []store.Category // for editing the marks
	Periods      []analytics.Period
	Current      analytics.Period
	D            analytics.Dashboard
	Recs         []analytics.Reconciliation
}

func (s *Server) analytics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var data analyticsData
	rows, err := s.ledger(ctx, &data)
	if err != nil {
		s.fail(w, err)
		return
	}
	first, last, ok := analytics.Bounds(rows)
	if !ok {
		data.Empty = true
		s.show(w, r, "analytics.html", "Analytics", "analytics", &data)
		return
	}
	data.Periods = analytics.Periods(first, last)
	data.Current = data.Periods[0]
	for _, p := range data.Periods {
		if p.Key == r.URL.Query().Get("period") {
			data.Current = p
		}
	}
	if data.Categories, err = s.st.Categories(ctx); err != nil {
		s.fail(w, err)
		return
	}
	if data.SavingsNames, err = s.st.SavingsNames(ctx); err != nil {
		s.fail(w, err)
		return
	}
	var exclude []string
	if data.NoSavings = r.URL.Query().Get("nosave") == "1"; data.NoSavings {
		exclude = data.SavingsNames
	}
	data.D = analytics.Build(rows, data.Current, exclude)
	if data.Current.Key != "all" {
		data.PrevPeriod = data.Current.Prev()
		if data.HasPrev = !data.PrevPeriod.From.Before(first); data.HasPrev {
			data.Prev = analytics.Build(rows, data.PrevPeriod, exclude)
			data.PrevCat = map[string]int64{}
			for _, c := range data.Prev.Categories {
				data.PrevCat[c.Label] = c.Value
			}
		}
	}
	for i, p := range data.Periods {
		if p.Key == data.Current.Key && len(p.Key) == 7 {
			if i+1 < len(data.Periods) {
				data.Older = data.Periods[i+1].Key
			}
			if i > 0 && len(data.Periods[i-1].Key) == 7 {
				data.Newer = data.Periods[i-1].Key
			}
		}
	}
	data.Heat = heat(rows, data.Current, exclude)
	stmts, err := s.st.Statements(ctx, s.loc)
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, si := range stmts {
		rec := analytics.Reconcile(rows, si)
		data.Recs = append(data.Recs, rec)
		if rec.OK {
			data.RecOK++
		}
	}
	if len(data.D.Problems) > 0 {
		s.log.Error("web: analytics invariants broken", "problems", data.D.Problems)
	}
	s.show(w, r, "analytics.html", "Analytics", "analytics", &data)
}

// ledger returns operations for analytics: from ClickHouse when configured, otherwise (or when
// it is unreachable) from SQLite. The computation is the same, the source does not change the numbers.
func (s *Server) ledger(ctx context.Context, data *analyticsData) ([]store.LedgerRow, error) {
	if s.ch != nil {
		rows, err := analysis.LedgerFromClickHouse(ctx, s.ch, s.loc)
		if err == nil {
			data.Source = "ClickHouse"
			return rows, nil
		}
		s.log.Warn("web: clickhouse unavailable, using sqlite", "err", err)
		data.Fallback = true
	}
	data.Source = "SQLite"
	return s.st.Ledger(ctx, s.loc)
}

var weekdayShort = map[time.Weekday]string{
	time.Monday: "Mon", time.Tuesday: "Tue", time.Wednesday: "Wed", time.Thursday: "Thu",
	time.Friday: "Fri", time.Saturday: "Sat", time.Sunday: "Sun",
}

// compact is a short amount for chart labels: "850 ₸", "123K", "1.2M".
// The exact amount is in the tooltip and the table.
func compact(minor int64) string {
	t := float64(minor) / 100
	sign := ""
	if t < 0 {
		sign, t = "−", -t
	}
	switch {
	case t >= 1e6:
		return sign + strconv.FormatFloat(t/1e6, 'f', 1, 64) + "M"
	case t >= 1e4:
		return sign + strconv.FormatFloat(t/1e3, 'f', 0, 64) + "K"
	case t >= 1e3:
		return sign + strconv.FormatFloat(t/1e3, 'f', 1, 64) + "K"
	}
	return sign + strconv.FormatFloat(t, 'f', 0, 64) + " ₸"
}

func pctString(f float64) string {
	switch {
	case f > 0 && f < 0.1:
		return "<0.1%"
	case f < 10:
		return strconv.FormatFloat(f, 'f', 1, 64) + "%"
	}
	return strconv.FormatFloat(f, 'f', 0, 64) + "%"
}

// savings stores the savings marks and returns to analytics.
func (s *Server) savings(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	var ids []int64
	for _, v := range r.PostForm["savings"] {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			http.Error(w, "invalid category", http.StatusBadRequest)
			return
		}
		ids = append(ids, id)
	}
	if err := s.st.SetSavings(r.Context(), ids); err != nil {
		s.fail(w, err)
		return
	}
	back := "/ui/analytics?nosave=1"
	if p := r.PostFormValue("period"); p != "" {
		back += "&period=" + url.QueryEscape(p)
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

type transfersData struct {
	Page
	Categories []store.Category
	Flash      string
	Query      string
	Total      int // total number of people
	Name       string
	Person     *store.Counterparty
	Ops        []store.Tx
	// the list of people
	Empty          bool
	People         []analytics.Person
	Shown          int
	More           string
	Cats           map[string]string // person → category their transfers count in
	Sent, Received int64
	Periods        []analytics.Period
	Current        analytics.Period
	Custom         bool
	From, To       string
	Dir            string
	Min, Max       string
	TimesMin       string
	TimesMax       string
	New            bool
	Sort           string
	Sorts          []struct{ Key, Title string }
	Ask            string
	AI             bool
	State          string
}

const peoplePage = 50

// transfers lists people money was sent to or received from, with filters and requests in
// plain words; ?name= shows all operations with a person.
func (s *Server) transfers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	all, err := s.st.Counterparties(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	q := r.URL.Query()
	d := transfersData{Query: strings.TrimSpace(q.Get("q")), Total: len(all), Name: q.Get("name"), Flash: q.Get("msg")}
	if d.Name != "" {
		for i := range all {
			if all[i].Name == d.Name {
				d.Person = &all[i]
			}
		}
		if d.Ops, err = s.st.CounterpartyOps(ctx, d.Name); err != nil {
			s.fail(w, err)
			return
		}
		if d.Categories, err = s.st.Categories(ctx); err != nil {
			s.fail(w, err)
			return
		}
		s.show(w, r, "transfers.html", d.Name, "transfers", &d)
		return
	}
	d.Cats = map[string]string{}
	for _, c := range all {
		if c.Category != "" {
			d.Cats[c.Name] = c.Category
		}
	}
	rows, err := s.st.Ledger(ctx, s.loc)
	if err != nil {
		s.fail(w, err)
		return
	}
	first, last, ok := bounds(rows)
	if !ok {
		d.Empty = true
		s.show(w, r, "transfers.html", "Transfers", "transfers", &d)
		return
	}
	d.Periods = analytics.Periods(first, last)
	of, custom := filterFrom(q, d.Periods, s.loc)
	f := analytics.PeopleFilter{Period: of.Period, Dir: of.Dir, Query: d.Query, Min: of.Min, Max: of.Max,
		New: q.Get("new") == "1", Sort: q.Get("sort")}
	f.TimesMin, _ = strconv.Atoi(q.Get("nmin"))
	f.TimesMax, _ = strconv.Atoi(q.Get("nmax"))
	if _, ok := peopleSorts[f.Sort]; !ok {
		f.Sort = analytics.PeopleTurnover
	}
	d.Current, d.Custom = f.Period, custom
	if custom {
		d.From, d.To = q.Get("from"), q.Get("to")
	}
	d.Dir, d.Min, d.Max, d.New, d.Sort = f.Dir, q.Get("min"), q.Get("max"), f.New, f.Sort
	if f.TimesMin > 0 {
		d.TimesMin = strconv.Itoa(f.TimesMin)
	}
	if f.TimesMax > 0 {
		d.TimesMax = strconv.Itoa(f.TimesMax)
	}
	for _, k := range []string{analytics.PeopleTurnover, analytics.PeopleBig, analytics.PeopleSmall, analytics.PeopleCount,
		analytics.PeopleNew, analytics.PeopleOld, analytics.PeopleBalance, analytics.PeopleName} {
		d.Sorts = append(d.Sorts, struct{ Key, Title string }{k, peopleSorts[k]})
	}
	d.Ask, d.AI = q.Get("ask"), s.aiReady()
	sq := cloneValues(q)
	for _, k := range []string{"msg", "limit", "ask"} {
		sq.Del(k)
	}
	d.State = sq.Encode()
	d.People = analytics.People(rows, f)
	for _, p := range d.People {
		d.Sent += p.Sent
		d.Received += p.Received
	}
	limit := peoplePage
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > limit {
		limit = n
	}
	d.Shown = min(len(d.People), limit)
	if len(d.People) > limit {
		mq := cloneValues(q)
		mq.Set("limit", strconv.Itoa(limit+peoplePage))
		d.More = "/ui/transfers?" + mq.Encode()
	}
	s.show(w, r, "transfers.html", "Transfers", "transfers", &d)
}

// transferCategory makes transfers to a person spending in a category or turns them back into non-spending.
func (s *Server) transferCategory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		http.Error(w, "no name", http.StatusBadRequest)
		return
	}
	norm := merchant.Normalize(name)
	cat := r.PostFormValue("category")
	// An own category wins over the selected one and is created if it does not exist yet.
	if newCat := strings.Join(strings.Fields(r.PostFormValue("new_category")), " "); newCat != "" {
		id, err := s.ensureCategory(ctx, newCat)
		if err != nil {
			s.fail(w, err)
			return
		}
		cat = strconv.FormatInt(id, 10)
	}
	var msg string
	switch cat {
	case "":
		if err := s.st.UncategorizeTransfers(ctx, name, norm); err != nil {
			s.fail(w, err)
			return
		}
		msg = "Transfers are no longer counted as spending."
	default:
		id, err := strconv.ParseInt(cat, 10, 64)
		if err != nil {
			http.Error(w, "invalid category", http.StatusBadRequest)
			return
		}
		c, err := s.st.Category(ctx, id)
		if err != nil {
			s.fail(w, err)
			return
		}
		n, err := s.st.CategorizeTransfers(ctx, name, norm, id, s.minHits, s.now())
		if err != nil {
			s.fail(w, err)
			return
		}
		msg = fmt.Sprintf("%d transfers moved to \"%s\". Transfers from future statements will be categorized automatically.", n, c.Name)
	}
	if s.kick != nil {
		s.kick.Kick()
	}
	http.Redirect(w, r, "/ui/transfers?name="+url.QueryEscape(name)+"&msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

// modelHint says what is wrong with the local model; empty when it is fine or turned off.
func (s *Server) modelHint(ctx context.Context) string {
	if s.settings == nil || s.settings.Model == nil || s.settings.Ollama == nil {
		return ""
	}
	if !s.settings.Ollama.Running(ctx) {
		return "Ollama is not running — without it there are no category guesses or tips."
	}
	models, err := s.settings.Ollama.Models(ctx)
	if err == nil && !slices.Contains(models, s.settings.Model.Model()) {
		return "Model " + s.settings.Model.Model() + " is not downloaded yet."
	}
	return ""
}

// dayTitle is a day header in lists: "Today", "Yesterday" or "Mon, 6 Oct".
func dayTitle(t, now time.Time) string {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, t.Location())
	switch d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()); {
	case d.Equal(today):
		return "Today"
	case d.Equal(today.AddDate(0, 0, -1)):
		return "Yesterday"
	case d.Year() == today.Year():
		return d.Format("Mon, 2 Jan")
	}
	return t.Format("Mon, 2 Jan 2006")
}
