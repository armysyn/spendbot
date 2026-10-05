package analysis

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"spendbot/internal/classify"
	"spendbot/internal/kaspi"
	"spendbot/internal/store"
)

type fakeLLM struct {
	reply string
	calls int
}

func (f *fakeLLM) Available() bool { return true }
func (f *fakeLLM) JSON(context.Context, string, string) (string, error) {
	f.calls++
	return f.reply, nil
}

type notes struct{ texts []string }

func (n *notes) Notify(_ context.Context, t string) { n.texts = append(n.texts, t) }

// newAnalyzer runs analysis without ClickHouse: insights come from SQLite, as on Windows.
func newAnalyzer(t *testing.T, l *fakeLLM) (*Analyzer, *store.Store, *notes) {
	st := testStore(t)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cls := classify.New(st, l, 3, almaty, log)
	n := &notes{}
	a := New(st, nil, cls, l, n, Options{Location: almaty, PublicURL: "http://mac:8080", BatchSize: 2,
		BatchMaxAge: time.Hour, Interval: time.Minute}, log)
	return a, st, n
}

// A year of synthetic spending with known patterns.
func seedYear(t *testing.T, st *store.Store) {
	ctx := context.Background()
	cat := func(name string) int64 {
		c, err := st.FindCategory(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		return c.ID
	}
	subs, food, cafe := cat("Phone & subscriptions"), cat("Groceries"), cat("Cafes & restaurants")
	k := 0
	add := func(at time.Time, merchant string, amount int64, catID int64) {
		k++
		id, _, err := st.InsertTx(ctx, store.Tx{ExternalKey: fmt.Sprint(k), OccurredAt: at, AmountMinor: amount, Currency: "KZT",
			MerchantRaw: merchant, MerchantNorm: strings.ToLower(merchant), Source: store.SourceImport, Kind: kaspi.Purchase, Status: store.StatusPending, CreatedAt: at})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.CloseTx(ctx, id, []store.Split{{CategoryID: catID, AmountMinor: amount}}, false, at); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Date(2025, 10, 1, 12, 0, 0, 0, almaty)
	for m := 0; m < 12; m++ {
		month := start.AddDate(0, m, 0)
		price := int64(4_990_00)
		if m == 11 {
			price = 5_990_00 // price increase in the last month
		}
		add(month.AddDate(0, 0, 4), "NETFLIX", price, subs)
		for d := 0; d < 8; d++ {
			add(month.AddDate(0, 0, d*3), "MAGNUM", 9_000_00+int64(d)*10_00, food)
		}
		cafeSum := int64(20_000_00)
		if m == 10 {
			cafeSum = 60_000_00 // cafes grow in the last full month (August 2026)
		}
		add(month.AddDate(0, 0, 10), "COFFEE BOOM", cafeSum, cafe)
	}
	// a shop with similar but not identical checks in different months is not a subscription
	for m, amt := range []int64{24_980_00, 40_030_00, 28_990_00} {
		add(start.AddDate(0, m+2, 5), "BERSHKA", amt, cat("Clothing"))
	}
	// an anomaly in groceries and a double charge
	add(time.Date(2026, 9, 20, 12, 0, 0, 0, almaty), "MAGNUM", 85_000_00, food)
	add(time.Date(2026, 9, 21, 12, 0, 0, 0, almaty), "SULPAK", 7_500_00, cat("Home"))
	add(time.Date(2026, 9, 21, 12, 5, 0, 0, almaty), "SULPAK", 7_500_00, cat("Home"))
}

func TestInsights(t *testing.T) {
	ctx := context.Background()
	l := &fakeLLM{reply: `{"tips":[{"title":"Review subscriptions","body":"Netflix got pricier."},{"title":"Cafes","body":"Less coffee to go."}]}`}
	a, st, n := newAnalyzer(t, l)
	seedYear(t, st)
	a.Cycle(ctx)

	ins, err := st.Insights(ctx, false, 100)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[string][]string{}
	for _, in := range ins {
		kinds[in.Kind] = append(kinds[in.Kind], in.Title)
	}
	check := func(kind, substr string) {
		t.Helper()
		for _, title := range kinds[kind] {
			if strings.Contains(title, substr) {
				return
			}
		}
		t.Errorf("no %s insight with %q; got %v", kind, substr, kinds)
	}
	check("recurring", "Netflix")
	check("price_up", "Netflix")
	check("anomaly", "85,000\u00a0₸")
	check("duplicate", "Sulpak")
	check("trend", "Cafes & restaurants")
	check("tip", "Review subscriptions")
	for _, title := range kinds["recurring"] {
		if strings.Contains(title, "Magnum") || strings.Contains(title, "Bershka") {
			t.Errorf("groceries are not a recurring payment: %s", title)
		}
	}
	if len(n.texts) != 1 || !strings.Contains(n.texts[0], "New insights") || !strings.Contains(n.texts[0], "http://mac:8080/ui") {
		t.Fatalf("notify: %v", n.texts)
	}

	// a second pass repeats neither insights nor notifications; tips come once per month of data
	calls := l.calls
	a.Cycle(ctx)
	again, _ := st.Insights(ctx, false, 100)
	if len(again) != len(ins) || len(n.texts) != 1 || l.calls != calls {
		t.Fatalf("second cycle: insights %d→%d, notes %d, llm calls %d→%d", len(ins), len(again), len(n.texts), calls, l.calls)
	}
}

func TestNoTipsOnLittleData(t *testing.T) {
	ctx := context.Background()
	l := &fakeLLM{reply: `{"tips":[{"title":"Tip","body":"..."}]}`}
	a, st, _ := newAnalyzer(t, l)
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, almaty)
	st.InsertTx(ctx, store.Tx{ExternalKey: "1", OccurredAt: at, AmountMinor: 180000, Currency: "KZT", MerchantRaw: "YANDEX GO",
		MerchantNorm: "yandex go", Source: store.SourceWallet, Status: store.StatusPending, CreatedAt: at})
	a.Cycle(ctx)
	if l.calls != 0 {
		t.Fatalf("tips must wait for enough data, llm called %d times", l.calls)
	}
}

func TestQuestionsAndBatches(t *testing.T) {
	ctx := context.Background()
	l := &fakeLLM{reply: `{"category":"Groceries","confidence":0.9}`}
	a, st, n := newAnalyzer(t, l)
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, almaty)
	for i, m := range []string{"IP ASANOV", "IP ASANOV", "TOO KOMFORT", "SHOP 3"} {
		st.InsertTx(ctx, store.Tx{ExternalKey: fmt.Sprint(i), OccurredAt: at, AmountMinor: int64(1000 * (i + 1)), Currency: "KZT",
			MerchantRaw: m, MerchantNorm: strings.ToLower(m), Source: store.SourceImport, Status: store.StatusReview, CreatedAt: at})
	}
	a.now = func() time.Time { return at }
	a.Cycle(ctx)

	open, _ := st.Batches(ctx, false, 10)
	if len(open) != 1 || open[0].Items != 2 {
		t.Fatalf("batches: %+v (one full batch of 2, the third question waits)", open)
	}
	items, _ := st.BatchItems(ctx, open[0].ID)
	food, _ := st.FindCategory(ctx, "Groceries")
	if items[0].GuessCategoryID != food.ID {
		t.Fatalf("guess not stored: %+v", items[0])
	}
	if len(n.texts) != 1 || !strings.Contains(n.texts[0], fmt.Sprintf("/ui/batch/%d", open[0].ID)) {
		t.Fatalf("notify: %v", n.texts)
	}

	// an hour later the partial batch is made too; batches pile up
	a.now = func() time.Time { return at.Add(2 * time.Hour) }
	a.Cycle(ctx)
	if open, _ := st.Batches(ctx, false, 10); len(open) != 2 {
		t.Fatalf("batches accumulate: %+v", open)
	}

	// a merchant that got a rule after import is categorized without a question
	st.InsertTx(ctx, store.Tx{ExternalKey: "z", OccurredAt: at, AmountMinor: 500, Currency: "KZT", MerchantRaw: "NEW",
		MerchantNorm: "new", Source: store.SourceImport, Status: store.StatusReview, CreatedAt: at})
	w, _, _ := st.InsertTx(ctx, store.Tx{ExternalKey: "w", OccurredAt: at, AmountMinor: 700, Currency: "KZT", MerchantNorm: "new",
		Source: store.SourceWallet, CreatedAt: at})
	st.CloseTx(ctx, w, []store.Split{{CategoryID: food.ID, AmountMinor: 700}}, true, at)
	a.Cycle(ctx)
	if ms, _ := st.ReviewMerchants(ctx); len(ms) != 0 {
		t.Fatalf("merchant with rule must be closed: %+v", ms)
	}
}
