package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"spendbot/internal/kaspi"
	"spendbot/internal/store"
)

func seedOps(t *testing.T, st *store.Store) (purchase, cash int64) {
	t.Helper()
	ctx := context.Background()
	food, _ := st.FindCategory(ctx, "Groceries")
	ops := []struct {
		day            int
		raw, kind, sts string
		amount         int64
	}{
		{1, "MAGNUM", kaspi.Purchase, store.StatusReview, 450000},
		{2, "MAGNUM #12", kaspi.Purchase, store.StatusReview, 120000},
		{3, "ATM", kaspi.Withdrawal, store.StatusReview, 2000000},
		{4, "Adam S.", kaspi.Transfer, store.StatusInfo, 5000000},
		{5, "Adam S.", kaspi.TopUp, store.StatusInfo, -1000000},
		{40, "COFFEE BOOM", kaspi.Purchase, store.StatusPending, 150000},
	}
	var ids []int64
	for i, o := range ops {
		at := time.Date(2026, 8, 1, 12, 0, 0, 0, almaty).AddDate(0, 0, o.day)
		id, _, err := st.InsertTx(ctx, store.Tx{ExternalKey: itoa(int64(i)), OccurredAt: at, AmountMinor: o.amount, Currency: "KZT",
			MerchantRaw: o.raw, MerchantNorm: strings.ToLower(strings.Fields(o.raw)[0]), Kind: o.kind, Source: store.SourceImport,
			Status: o.sts, CreatedAt: at})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	st.CloseTx(ctx, ids[5], []store.Split{{CategoryID: food.ID, AmountMinor: 150000}}, true, time.Now())
	return ids[0], ids[2]
}

// Every page renders to the end with data: a template error would cut the page short.
func TestPagesRender(t *testing.T) {
	h, st, _ := setup(t)
	for _, empty := range []bool{true, false} {
		if !empty {
			seedOps(t, st)
		}
		for _, p := range []string{"/ui", "/ui/analytics", "/ui/analytics?period=2026-08", "/ui/analytics?period=30d&nosave=1",
			"/ui/operations", "/ui/operations?type=all", "/ui/operations?type=transfers", "/ui/operations?cat=Cash&period=all",
			"/ui/operations?from=2026-08-02&to=2026-08-04&q=magnum", "/ui/operations?m=magnum", "/ui/categories",
			"/ui/transfers", "/ui/transfers?name=Adam+S."} {
			w := do(h, "GET", p, nil, "", true)
			if w.Code != http.StatusOK || !strings.HasSuffix(strings.TrimSpace(w.Body.String()), "</html>") {
				t.Errorf("empty=%v %s: %d, cut short:\n%s", empty, p, w.Code, tail(w.Body.String()))
			}
		}
	}
}

func tail(s string) string {
	if len(s) > 300 {
		return s[len(s)-300:]
	}
	return s
}

func post(h http.Handler, path string, form url.Values) *httptest.ResponseRecorder {
	return do(h, "POST", path, strings.NewReader(form.Encode()), "application/x-www-form-urlencoded", true)
}

func TestOperationsPage(t *testing.T) {
	h, st, k := setup(t)
	ctx := context.Background()
	purchase, cash := seedOps(t, st)
	body := do(h, "GET", "/ui/operations?period=all", nil, "", true).Body.String()
	for _, want := range []string{"Magnum", "Cash withdrawal", "Coffee Boom", "no category"} {
		if !strings.Contains(body, want) {
			t.Errorf("operations page lacks %q", want)
		}
	}
	if strings.Contains(body, "Adam S.") {
		t.Error("a transfer without a category is not spending")
	}

	// one purchase — all purchases of the merchant follow and the merchant is remembered
	home, _ := st.FindCategory(ctx, "Home")
	w := post(h, "/ui/op/"+itoa(purchase)+"/category", url.Values{"category": {itoa(home.ID)}, "all": {"1"}, "back": {"/ui/operations?m=magnum"}})
	if w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/ui/operations?") || k.n != 1 {
		t.Fatalf("edit: %d %s", w.Code, w.Header().Get("Location"))
	}
	if r, ok, _ := st.Rule(ctx, "magnum"); !ok || r.CategoryID != home.ID {
		t.Errorf("rule %+v", r)
	}
	// cash gets a new category without teaching any merchant rule
	post(h, "/ui/op/"+itoa(cash)+"/category", url.Values{"new_category": {"Savings box"}, "back": {"https://evil.example/"}})
	box, err := st.FindCategory(ctx, "savings box")
	if err != nil {
		t.Fatal("new category not created")
	}
	if sp, _ := st.Splits(ctx, cash); len(sp) != 1 || sp[0].CategoryID != box.ID {
		t.Errorf("cash split %+v", sp)
	}
	// not spending
	post(h, "/ui/op/"+itoa(purchase)+"/category", url.Values{"category": {"none"}})
	if tx, _ := st.GetTx(ctx, purchase); tx.Status != store.StatusIgnored {
		t.Errorf("status %s", tx.Status)
	}
	// incoming money has no category
	w = post(h, "/ui/op/"+itoa(purchase+4)+"/category", url.Values{"category": {itoa(home.ID)}})
	if !strings.Contains(w.Header().Get("Location"), "msg=") {
		t.Errorf("top-up edit: %s", w.Header().Get("Location"))
	}

	csv := do(h, "GET", "/ui/operations.csv?period=all", nil, "", true)
	if csv.Header().Get("Content-Type") != "text/csv; charset=utf-8" || !strings.Contains(csv.Body.String(), "Coffee Boom") ||
		!strings.HasPrefix(csv.Body.String(), "date,time,merchant") {
		t.Errorf("csv:\n%s", csv.Body.String())
	}
}

func TestSafeBack(t *testing.T) {
	for in, want := range map[string]string{"/ui/operations?x=1": "/ui/operations?x=1", "https://evil.example": "/ui", "//evil": "/ui", "": "/ui"} {
		if got := safeBack(in, "/ui"); got != want {
			t.Errorf("%q → %q", in, got)
		}
	}
}

func TestCategoryActions(t *testing.T) {
	h, st, _ := setup(t)
	ctx := context.Background()
	seedOps(t, st)
	act := func(kv ...string) string {
		f := url.Values{}
		for i := 0; i+1 < len(kv); i += 2 {
			f.Set(kv[i], kv[i+1])
		}
		w := post(h, "/ui/categories", f)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("%v: %d", kv, w.Code)
		}
		return w.Header().Get("Location")
	}
	act("action", "add", "name", "  Coffee  ")
	coffee, err := st.FindCategory(ctx, "Coffee")
	if err != nil {
		t.Fatal("not added")
	}
	act("action", "rename", "id", itoa(coffee.ID), "name", "Cafe")
	if c, _ := st.Category(ctx, coffee.ID); c.Name != "Cafe" {
		t.Errorf("rename: %+v", c)
	}
	// renaming to an existing name merges
	food, _ := st.FindCategory(ctx, "Groceries")
	act("action", "rename", "id", itoa(food.ID), "name", "cafe")
	if _, err := st.Category(ctx, food.ID); err != store.ErrNotFound {
		t.Error("groceries must be merged into Cafe")
	}
	if r, _, _ := st.Rule(ctx, "coffee"); r.CategoryID != coffee.ID {
		t.Errorf("rule follows the merge: %+v", r)
	}
	act("action", "savings", "id", itoa(coffee.ID))
	if names, _ := st.SavingsNames(ctx); len(names) != 1 || names[0] != "Cafe" {
		t.Errorf("savings %v", names)
	}
	act("action", "archive", "id", itoa(coffee.ID))
	if c, _ := st.Category(ctx, coffee.ID); !c.Archived {
		t.Error("archive")
	}
	act("action", "restore", "id", itoa(coffee.ID))
	if c, _ := st.Category(ctx, coffee.ID); c.Archived {
		t.Error("restore")
	}
	if loc := act("action", "merge", "id", itoa(coffee.ID), "into", itoa(coffee.ID)); !strings.Contains(loc, "msg=") {
		t.Error("merge into itself must be refused")
	}
}

func TestRuleIntent(t *testing.T) {
	cases := []struct {
		text      string
		min, max  int64 // tenge, -1 — none
		sort, grp string
	}{
		{"вытащи мне между 20к и 50к", 20000, 50000, "", ""},
		{"between 20k and 50k", 20000, 50000, "", ""},
		{"от 20 000 до 50 000 ₸", 20000, 50000, "", ""},
		{"20-50к", 20000, 50000, "", ""},
		{"больше 100к", 100000, -1, "", ""},
		{"не больше 1,5 млн", -1, 1500000, "", ""},
		{"under 5000", -1, 5000, "", ""},
		{"самые крупные по месяцам", -1, -1, "big", "month"},
		{"за сентябрь 2026 по категориям", -1, -1, "", "category"},
	}
	for text, want := range map[string]string{
		"между 20000 и 50000, кому впервые отправлялось": "first",
		"к кому отправлял только один раз":               "once",
		"sent only once, 20k-50k": "once",
		"first time transfers":    "first",
	} {
		if got := ruleIntent(text).Repeat; got != want {
			t.Errorf("%q: repeat %q, want %q", text, got, want)
		}
	}
	for _, c := range cases {
		in := ruleIntent(c.text)
		toT := func(v int64) int64 {
			if v < 0 {
				return -1
			}
			return v / 100
		}
		if toT(in.Min) != c.min || toT(in.Max) != c.max || in.Sort != c.sort || in.Group != c.grp {
			t.Errorf("%q: min %d max %d sort %q group %q", c.text, toT(in.Min), toT(in.Max), in.Sort, in.Group)
		}
	}
}

type fakeAI struct{ reply string }

func (f fakeAI) Available() bool { return true }
func (f fakeAI) JSON(context.Context, string, string) (string, error) {
	return f.reply, nil
}

// The request refines the current filters: a person picked before stays, the amount is added.
func TestAskKeepsFilters(t *testing.T) {
	st, err := store.Open(context.Background(), t.TempDir()+"/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	seedOps(t, st)
	mux := http.NewServeMux()
	srv := New(st, nil, "", 3, time.Minute, almaty, slogDiscard())
	// the model misreads the amount; the rules' exact reading wins, the rest comes from the model
	srv.WithAI(fakeAI{`{"min_amount": 20, "max_amount": 50, "sort": "big", "category": "No Such"}`}).Register(mux)
	form := url.Values{"prompt": {"между 20к и 50к, сначала крупные"}, "state": {"type=transfers&q=Adam+S."}}
	w := do(mux, "POST", "/ui/operations/ask", strings.NewReader(form.Encode()), "application/x-www-form-urlencoded", false)
	loc, _ := url.Parse(w.Header().Get("Location"))
	q := loc.Query()
	if q.Get("type") != "transfers" || q.Get("q") != "Adam S." || q.Get("min") != "20000" || q.Get("max") != "50000" ||
		q.Get("sort") != "big" || q.Get("cat") != "" || !strings.Contains(q.Get("msg"), "20,000") {
		t.Fatalf("redirect %s", loc)
	}
	page := do(mux, "GET", loc.String(), nil, "", false).Body.String()
	if !strings.Contains(page, "50,000") || !strings.HasSuffix(strings.TrimSpace(page), "</html>") {
		t.Fatal("filtered page")
	}
	// Adam S. sent 50,000 and received 10,000: only the transfer is in the range
	if !strings.Contains(page, `<div class="value num">1</div>`) {
		t.Errorf("one operation expected in 20k–50k")
	}
}

func TestGroundPeriod(t *testing.T) {
	model := intent{From: "2026-10-06", To: "2026-10-06", Min: -1, Max: -1}
	for text, keep := range map[string]bool{
		"между 20000 и 50000, кому впервые отправлялось": false,
		"только один раз":       false,
		"за сентябрь":           true,
		"в 2025":                true,
		"с 05.10":               true,
		"over 20000 last month": true,
	} {
		if got := ground(model, text).From != ""; got != keep {
			t.Errorf("%q: period kept %v", text, got)
		}
	}
}

func slogDiscard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestPeopleRequests(t *testing.T) {
	for text, want := range map[string][2]int{
		"кому отправлял больше 5 раз": {6, 0},
		"at least 3 times": {3, 0},
		"2 раза":           {2, 2},
		"меньше 3 раз между 20к и 50к":        {0, 2},
		"кому больше 5 переводов в 2026 году": {6, 0},
	} {
		in := ruleIntent(text)
		if in.TimesMin != want[0] || in.TimesMax != want[1] {
			t.Errorf("%q: times %d–%d, want %v", text, in.TimesMin, in.TimesMax, want)
		}
	}
	for text, want := range map[string][3]string{
		"кто прислал мне больше всего":        {"in", "big", ""},
		"с кем у меня самый большой минус":    {"", "balance", ""},
		"кому больше 5 переводов в 2026 году": {"out", "", "2026-01-01"},
		"кому чаще всего отправлял":           {"out", "count", ""},
	} {
		in := ruleIntent(text)
		if in.Dir != want[0] || in.Sort != want[1] || in.From != want[2] {
			t.Errorf("%q: dir %q sort %q from %q, want %v", text, in.Dir, in.Sort, in.From, want)
		}
	}
	if in := ruleIntent("кому больше 5 переводов в 2026 году"); in.Min >= 0 {
		t.Errorf("a count or a year read as an amount: %d", in.Min)
	}
	// "5 раз" is a count, not 5 ₸; the amount next to it is still read
	if in := ruleIntent("больше 5 раз"); in.Min >= 0 {
		t.Errorf("count read as amount: %d", in.Min)
	}
	if in := ruleIntent("меньше 3 раз между 20к и 50к"); in.Min != 20000*100 || in.Max != 50000*100 {
		t.Errorf("amount next to a count: %d–%d", in.Min, in.Max)
	}

	h, st, _ := setup(t)
	seedOps(t, st)
	ctx := context.Background()
	at := time.Date(2026, 9, 20, 12, 0, 0, 0, almaty)
	for i, a := range []int64{3000000, 3500000} { // Bob B. twice, 30,000 and 35,000
		st.InsertTx(ctx, store.Tx{ExternalKey: "bob" + itoa(int64(i)), OccurredAt: at.AddDate(0, 0, i), AmountMinor: a, Currency: "KZT",
			MerchantRaw: "Bob B.", MerchantNorm: "bob b", Kind: kaspi.Transfer, Source: store.SourceImport, Status: store.StatusInfo, CreatedAt: at})
	}
	st.InsertTx(ctx, store.Tx{ExternalKey: "cat", OccurredAt: at, AmountMinor: 4000000, Currency: "KZT",
		MerchantRaw: "Cara C.", MerchantNorm: "cara c", Kind: kaspi.Transfer, Source: store.SourceImport, Status: store.StatusInfo, CreatedAt: at})
	// sent only once, 20k–50k: Cara (40,000 once); not Bob (twice), not Adam (50,000 + received)
	w := post(h, "/ui/transfers/ask", url.Values{"prompt": {"кому отправлял только один раз между 20к и 50к"}, "state": {""}})
	loc, _ := url.Parse(w.Header().Get("Location"))
	if loc.Path != "/ui/transfers" || loc.Query().Get("nmin") != "1" || loc.Query().Get("nmax") != "1" || loc.Query().Get("min") != "20000" {
		t.Fatalf("redirect %s", loc)
	}
	page := do(h, "GET", loc.String(), nil, "", true).Body.String()
	if !strings.Contains(page, "Cara C.") || strings.Contains(page, ">Bob B.<") || !strings.HasSuffix(strings.TrimSpace(page), "</html>") {
		t.Errorf("people page: cara %v bob %v loc %s", strings.Contains(page, "Cara C."), strings.Contains(page, ">Bob B.<"), loc)
	}
	// sorting and the direction switch render
	for _, p := range []string{"/ui/transfers?dir=in&sort=big", "/ui/transfers?sort=balance&nmin=2", "/ui/transfers?new=1&period=30d"} {
		if b := do(h, "GET", p, nil, "", true).Body.String(); !strings.HasSuffix(strings.TrimSpace(b), "</html>") {
			t.Errorf("%s cut short", p)
		}
	}
}

func TestCategoriesPage(t *testing.T) {
	h, st, _ := setup(t)
	seedOps(t, st)
	for _, p := range []string{"/ui/categories", "/ui/categories?period=all&unused=1", "/ui/categories?period=2026-08&sort=grew",
		"/ui/categories?q=groc&min=100&savings=none", "/ui/categories?from=2026-08-01&to=2026-09-30&sort=name"} {
		b := do(h, "GET", p, nil, "", true).Body.String()
		if !strings.HasSuffix(strings.TrimSpace(b), "</html>") {
			t.Errorf("%s cut short:\n%s", p, tail(b))
		}
	}
	all := do(h, "GET", "/ui/categories?period=all", nil, "", true).Body.String()
	for _, want := range []string{"Groceries", "Cash", "Uncategorized", "1,500"} {
		if !strings.Contains(all, want) {
			t.Errorf("categories page lacks %q", want)
		}
	}
	if strings.Contains(all, "cat=Beauty") {
		t.Error("unused categories are hidden by default")
	}
	if !strings.Contains(do(h, "GET", "/ui/categories?period=all&unused=1", nil, "", true).Body.String(), "cat=Beauty") {
		t.Error("unused categories show with the switch")
	}
	w := post(h, "/ui/categories/ask", url.Values{"prompt": {"что выросло больше всего, без сбережений"}, "state": {"period=12m"}})
	loc, _ := url.Parse(w.Header().Get("Location"))
	if loc.Path != "/ui/categories" || loc.Query().Get("sort") != "grew" || loc.Query().Get("savings") != "none" || loc.Query().Get("period") != "12m" {
		t.Errorf("ask: %s", loc)
	}
}
