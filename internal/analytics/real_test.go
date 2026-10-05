package analytics_test

import (
	"context"
	"fmt"
	"os"
	"reflect"
	. "spendbot/internal/analytics"
	"testing"
	"time"

	"spendbot/internal/analysis"
	"spendbot/internal/clickhouse"
	"spendbot/internal/importer"
	"spendbot/internal/statement"
)

// A real yearly statement (statements/, not committed): after import every header total
// matches the database to the tiyn, and purchases on the page equal the bank's total.
func TestRealStatementReconciles(t *testing.T) {
	b, err := os.ReadFile("../../statements/gold_statement_year.pdf")
	if err != nil {
		t.Skip("no yearly statement in statements/")
	}
	st := testStore(t)
	ctx := context.Background()
	s, err := statement.ParseKaspiBytes(b, almaty)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := importer.Import(ctx, st, s, time.Now()); err != nil {
		t.Fatal(err)
	}
	rows := ledger(t, st)
	infos, _ := st.Statements(ctx, almaty)
	rec := Reconcile(rows, infos[0])
	for _, l := range rec.Lines {
		if !l.OK {
			t.Errorf("%s: statement %d, database %d", l.Label, l.Statement, l.Database)
		}
	}
	if len(rec.Lines) < 5 {
		t.Errorf("too few summary lines: %+v", rec.Lines)
	}
	d := Build(rows, Period{From: s.From, To: s.To.AddDate(0, 0, 1)}, nil)
	if len(d.Problems) > 0 {
		t.Fatalf("invariants: %v", d.Problems)
	}
	if d.Totals.Purchases != -s.Summary["Покупки"] || d.Totals.Cash != -s.Summary["Снятия"] {
		t.Errorf("purchases %d (bank %d), cash %d (bank %d)", d.Totals.Purchases, -s.Summary["Покупки"], d.Totals.Cash, -s.Summary["Снятия"])
	}
}

// The source does not affect the numbers: the same data via SQLite and via ClickHouse gives
// the same analytics page down to the last field.
func TestClickHouseSameAsSQLite(t *testing.T) {
	url := os.Getenv("CLICKHOUSE_TEST_URL")
	if url == "" {
		t.Skip("CLICKHOUSE_TEST_URL is not set")
	}
	ctx := context.Background()
	ch := clickhouse.New(url, fmt.Sprintf("analytics_test_%d", time.Now().UnixNano()))
	if err := analysis.Migrate(ctx, ch, "Asia/Almaty"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ch.ExecRaw(context.Background(), "DROP DATABASE IF EXISTS "+ch.DB()) })

	st := testStore(t)
	seed(t, st)
	time.Sleep(3 * time.Second) // sync takes rows older than two seconds
	if _, err := analysis.Sync(ctx, st, ch, time.Now()); err != nil {
		t.Fatal(err)
	}
	fromCH, err := analysis.LedgerFromClickHouse(ctx, ch, almaty)
	if err != nil {
		t.Fatal(err)
	}
	p := Period{From: time.Date(2026, 8, 1, 0, 0, 0, 0, almaty), To: time.Date(2026, 10, 1, 0, 0, 0, 0, almaty)}
	for _, ex := range [][]string{nil, {"Gifts"}} {
		a, b := Build(ledger(t, st), p, ex), Build(fromCH, p, ex)
		if !reflect.DeepEqual(a, b) {
			t.Errorf("exclude %v: SQLite and ClickHouse differ:\nsqlite %+v\nclickhouse %+v", ex, a.Totals, b.Totals)
		}
	}
}
