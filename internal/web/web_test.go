package web

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"spendbot/internal/kaspi"
	"spendbot/internal/store"
)

var almaty, _ = time.LoadLocation("Asia/Almaty")

type kicks struct{ n int }

func (k *kicks) Kick() { k.n++ }

func setup(t *testing.T) (http.Handler, *store.Store, *kicks) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	k := &kicks{}
	mux := http.NewServeMux()
	New(st, k, "secret", 3, 30*time.Minute, almaty, slog.New(slog.NewTextHandler(io.Discard, nil))).Register(mux)
	return mux, st, k
}

func do(h http.Handler, method, path string, body io.Reader, ctype string, auth bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, body)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if auth {
		req.SetBasicAuth("me", "secret")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestAuth(t *testing.T) {
	h, _, _ := setup(t)
	// a page asks to sign in, a form post is refused
	if w := do(h, "GET", "/ui/income?period=all", nil, "", false); w.Code != http.StatusSeeOther ||
		w.Header().Get("Location") != "/ui/login?next=%2Fui%2Fincome%3Fperiod%3Dall" {
		t.Fatalf("no password: %d %s", w.Code, w.Header().Get("Location"))
	}
	if w := do(h, "POST", "/ui/insight/1/dismiss", nil, "", false); w.Code != http.StatusUnauthorized {
		t.Fatalf("post without password: %d", w.Code)
	}
	// scripts may still send the password with Basic auth
	if w := do(h, "GET", "/ui", nil, "", true); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Upload a statement") {
		t.Fatalf("index: %d", w.Code)
	}
	if w := do(h, "GET", "/", nil, "", false); w.Code != http.StatusFound {
		t.Fatalf("root redirect: %d", w.Code)
	}
}

func TestNoPasswordMeansOpen(t *testing.T) {
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mux := http.NewServeMux()
	New(st, nil, "", 3, time.Minute, almaty, slog.New(slog.NewTextHandler(io.Discard, nil))).Register(mux)
	if w := do(mux, "GET", "/ui", nil, "", false); w.Code != http.StatusOK {
		t.Fatalf("open page: %d", w.Code)
	}
}

func TestCrossOriginPostRejected(t *testing.T) {
	h, _, _ := setup(t)
	req := httptest.NewRequest("POST", "/ui/insight/1/dismiss", nil)
	req.SetBasicAuth("me", "secret")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-site post: %d", w.Code)
	}
}

func TestBatchFormFlow(t *testing.T) {
	h, st, k := setup(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, almaty)
	for i, m := range []string{"asanov", "boom", "cosmo"} {
		st.InsertTx(ctx, store.Tx{ExternalKey: m, OccurredAt: now, AmountMinor: int64(1000 * (i + 1)), Currency: "KZT",
			MerchantRaw: strings.ToUpper(m), MerchantNorm: m, Source: store.SourceImport, Status: store.StatusReview, CreatedAt: now})
		st.AddMerchantQuestion(ctx, m, strings.ToUpper(m), 0, now)
	}
	id, _ := st.CreateBatch(ctx, 10, now)

	w := do(h, "GET", "/ui/batch/"+itoa(id), nil, "", true)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Asanov") || !strings.Contains(w.Body.String(), "norm_2") {
		t.Fatalf("batch page: %d", w.Code)
	}

	food, _ := st.FindCategory(ctx, "Groceries")
	form := url.Values{
		"norm_0": {"cosmo"}, "cat_0": {itoa(food.ID)}, "note_0": {"shop"},
		"norm_1": {"boom"}, "cat_1": {"ignore"},
		"norm_2": {"asanov"}, "cat_2": {""}, "new_2": {"  Rent  "},
	}
	w = do(h, "POST", "/ui/batch/"+itoa(id), strings.NewReader(form.Encode()), "application/x-www-form-urlencoded", true)
	if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "msg=") {
		t.Fatalf("answer: %d %s", w.Code, w.Body.String())
	}
	rent, err := st.FindCategory(ctx, "rent")
	if err != nil {
		t.Fatal("new category not created")
	}
	if r, ok, _ := st.Rule(ctx, "asanov"); !ok || r.CategoryID != rent.ID {
		t.Fatalf("rule for new category: %+v", r)
	}
	if ms, _ := st.ReviewMerchants(ctx); len(ms) != 0 {
		t.Fatalf("left in review: %+v", ms)
	}
	if k.n != 1 {
		t.Fatal("analysis must be kicked after answers")
	}
}

func TestImportUpload(t *testing.T) {
	pdf, err := os.ReadFile("../../statements/gold_statement.pdf")
	if err != nil {
		t.Skip("no statement in statements/")
	}
	h, st, k := setup(t)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("pdf", "gold_statement.pdf")
	fw.Write(pdf)
	mw.Close()
	w := do(h, "POST", "/ui/import", &body, mw.FormDataContentType(), true)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Operations in the statement: <b>6</b>") {
		t.Fatalf("import: %d\n%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "do not match") || k.n != 1 {
		t.Fatal("verify failed or analysis not kicked")
	}
	if ms, _ := st.ReviewMerchants(context.Background()); len(ms) == 0 {
		t.Fatal("purchases must wait for review")
	}

	w = do(h, "POST", "/ui/import", strings.NewReader("x"), "multipart/form-data; boundary=zzz", true)
	if !strings.Contains(w.Body.String(), "Choose a PDF") {
		t.Fatalf("bad upload: %s", w.Body.String())
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestTransfersSearch(t *testing.T) {
	h, st, _ := setup(t)
	ctx := context.Background()
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, almaty)
	for i, op := range []struct {
		name, kind string
		amount     int64
	}{{"Әлия Б.", kaspi.Transfer, 500000}, {"Әлия Б.", kaspi.TopUp, -200000}, {"Данияр К.", kaspi.Transfer, 100000}, {"Shop", kaspi.Purchase, 999}} {
		st.InsertTx(ctx, store.Tx{ExternalKey: itoa(int64(i)), OccurredAt: at, AmountMinor: op.amount, Currency: "KZT",
			MerchantRaw: op.name, Kind: op.kind, Source: store.SourceImport, Status: store.StatusInfo, CreatedAt: at})
	}
	w := do(h, "GET", "/ui/transfers?q="+url.QueryEscape("алия"), nil, "", true) // the Kazakh "ә" is found as "а"
	body := w.Body.String()
	if w.Code != 200 || !strings.Contains(body, "Әлия Б.") || strings.Contains(body, "Данияр") {
		t.Fatalf("search: %d\n%s", w.Code, body)
	}
	w = do(h, "GET", "/ui/transfers?name="+url.QueryEscape("Әлия Б."), nil, "", true)
	body = w.Body.String()
	for _, want := range []string{"5,000\u00a0₸", "2,000\u00a0₸", "-3,000\u00a0₸", "sent more than received"} {
		if !strings.Contains(body, want) {
			t.Errorf("person page lacks %q", want)
		}
	}
	if strings.Contains(do(h, "GET", "/ui/transfers", nil, "", true).Body.String(), "Shop") {
		t.Error("purchases are not transfers")
	}
}

func TestSavingsForm(t *testing.T) {
	h, st, _ := setup(t)
	ctx := context.Background()
	inv, _ := st.AddCategory(ctx, "Investments")
	other, _ := st.FindCategory(ctx, "Home")
	form := url.Values{"savings": {itoa(other.ID)}, "period": {"12m"}}
	w := do(h, "POST", "/ui/savings", strings.NewReader(form.Encode()), "application/x-www-form-urlencoded", true)
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/ui/analytics?nosave=1&period=12m" {
		t.Fatalf("savings: %d %s", w.Code, w.Header().Get("Location"))
	}
	names, _ := st.SavingsNames(ctx)
	if len(names) != 1 || names[0] != "Home" {
		t.Fatalf("savings names %v (Investments %d must be unmarked)", names, inv)
	}
}

func TestTransferCategoryForm(t *testing.T) {
	h, st, _ := setup(t)
	ctx := context.Background()
	at := time.Date(2026, 9, 29, 12, 0, 0, 0, almaty)
	id, _, _ := st.InsertTx(ctx, store.Tx{ExternalKey: "r", OccurredAt: at, AmountMinor: 26000000, Currency: "KZT",
		MerchantRaw: "Adam S.", MerchantNorm: "adam s", Kind: kaspi.Transfer, Source: store.SourceImport, Status: store.StatusInfo, CreatedAt: at})
	rent, _ := st.AddCategory(ctx, "Rent")
	post := func(cat string) *httptest.ResponseRecorder {
		form := url.Values{"name": {"Adam S."}, "category": {cat}}
		return do(h, "POST", "/ui/transfers/category", strings.NewReader(form.Encode()), "application/x-www-form-urlencoded", true)
	}
	if w := post(itoa(rent)); w.Code != http.StatusSeeOther {
		t.Fatalf("post: %d %s", w.Code, w.Body.String())
	}
	if tx, _ := st.GetTx(ctx, id); tx.Status != store.StatusDone {
		t.Fatalf("status %s", tx.Status)
	}
	page := do(h, "GET", "/ui/transfers?name="+url.QueryEscape("Adam S."), nil, "", true).Body.String()
	if !strings.Contains(page, "Now: spending in \"Rent\"") {
		t.Fatal("current category not shown")
	}
	post("")
	if tx, _ := st.GetTx(ctx, id); tx.Status != store.StatusInfo {
		t.Fatalf("undo status %s", tx.Status)
	}

	// an own category is created and wins over the selected one
	form := url.Values{"name": {"Adam S."}, "category": {""}, "new_category": {"  Family "}}
	do(h, "POST", "/ui/transfers/category", strings.NewReader(form.Encode()), "application/x-www-form-urlencoded", true)
	fam, err := st.FindCategory(ctx, "family")
	if err != nil {
		t.Fatal("custom category not created")
	}
	if r, ok, _ := st.Rule(ctx, "adam s"); !ok || r.CategoryID != fam.ID {
		t.Fatalf("rule %+v", r)
	}
}
