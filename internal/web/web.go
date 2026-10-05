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
	ModelHint string // the model is not ready — hint to open the settings
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
	s.render(w, "index.html", d)
}

type batchData struct {
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
	s.render(w, "batch.html", batchData{ID: id, Items: items, Categories: cats, Answered: answered})
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
		s.render(w, "import.html", importData{Err: "Choose a PDF statement (up to 20 MB)."})
		return
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		s.render(w, "import.html", importData{Err: "Could not read the file."})
		return
	}
	st, err := statement.ParseKaspiBytes(b, s.loc)
	if err != nil {
		s.render(w, "import.html", importData{File: hdr.Filename, Err: "Could not parse the statement: " + err.Error()})
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
	s.render(w, "import.html", importData{File: hdr.Filename, Result: res, From: st.From, To: st.To})
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
		s.render(w, "analytics.html", data)
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
	stmts, err := s.st.Statements(ctx, s.loc)
	if err != nil {
		s.fail(w, err)
		return
	}
	for _, si := range stmts {
		data.Recs = append(data.Recs, analytics.Reconcile(rows, si))
	}
	if len(data.D.Problems) > 0 {
		s.log.Error("web: analytics invariants broken", "problems", data.D.Problems)
	}
	s.render(w, "analytics.html", data)
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
	Categories []store.Category
	Flash      string
	Query      string
	Results    []store.Counterparty
	Total      int // total number of people
	Name       string
	Person     *store.Counterparty
	Ops        []store.Tx
}

// transfers searches people money was sent to or received from. Without a query it shows
// the largest by turnover; ?name= shows all operations with a person.
func (s *Server) transfers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	all, err := s.st.Counterparties(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	d := transfersData{Query: strings.TrimSpace(r.URL.Query().Get("q")), Total: len(all), Name: r.URL.Query().Get("name")}
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
		d.Flash = r.URL.Query().Get("msg")
		s.render(w, "transfers.html", d)
		return
	}
	terms := strings.Fields(fold(d.Query))
	for _, c := range all {
		name := fold(c.Name)
		match := true
		for _, t := range terms {
			if !strings.Contains(name, t) {
				match = false
				break
			}
		}
		if match {
			d.Results = append(d.Results, c)
		}
		if d.Query == "" && len(d.Results) == 30 {
			break
		}
	}
	s.render(w, "transfers.html", d)
}

// fold prepares a name for search: lower case, Kazakh letters mapped to Russian ones
// ("Әлия" is found by "алия"), ё to е, dots to spaces.
var foldReplacer = strings.NewReplacer(
	"ә", "а", "ə", "а", "ғ", "г", "қ", "к", "ң", "н", "ө", "о", "ұ", "у", "ү", "у", "һ", "х", "і", "и", "ё", "е", ".", " ",
)

func fold(s string) string { return foldReplacer.Replace(strings.ToLower(s)) }

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
