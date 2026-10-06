package statement

import (
	"os"
	"strings"
	"testing"
	"time"

	"spendbot/internal/kaspi"
)

var almaty, _ = time.LoadLocation("Asia/Almaty")

// line builds a page line: pairs of x and text at height y. The text imitates what a
// Kaspi PDF contains, so it is in Russian.
func line(y float64, parts ...any) []segment {
	var out []segment
	for i := 0; i+1 < len(parts); i += 2 {
		x := float64(parts[i].(int))
		out = append(out, segment{x: x, end: x + 10, y: y, s: parts[i+1].(string)})
	}
	return out
}

func header(y float64) []segment {
	return line(y, 59, kaspi.ColDate, 133, kaspi.ColAmount, 243, kaspi.ColKind, 300, kaspi.ColDetails)
}

// Two pages: an operation wraps to the next line, and the page footer and a footnote must
// not end up in the details of the last operation. The table header is on page one only.
func TestParseKaspiLines(t *testing.T) {
	lines := [][]segment{
		line(700, 40, "АО «Kaspi Bank» подтверждает, что Иванов  Иван Иванович ИИН 000000000000"),
		line(654, 40, "по Kaspi Gold за период с 01.01.26 по 31.12.26"),
		line(600, 300, "*1234"),
		line(484, 43, "Покупки", 200, "- 7 182,70 ₸"),
		line(470, 43, "Переводы", 200, "- 5 000,00 ₸"),
		header(333),
		line(317, 51, "01.01.26", 139, "- 1 230,00 ₸", 257, kaspi.Purchase, 300, "ИП ''АЛМАЗ''"),
		line(301, 51, "01.01.26", 132, "+ 10 000,00 ₸", 222, "Поступление со", 300, "С Kaspi Депозита"),
		line(285, 236, "своего счета"),
		line(202, 40, "- Сумма заблокирована. Банк ожидает подтверждения от платежной системы."),
		line(27, 40, "АО «Kaspi Bank», БИК CASPKZKA, www.kaspi.kz"),
		// page 2 has no table header, like a real statement
		line(815, 40, "Приложение к Справке №1 от 05 октября 2026"),
		line(764, 51, "02.01.26", 139, "- 3 410,00 ₸", 257, kaspi.Purchase, 300, "SMALL"),
		line(748, 300, "MARKET ALMATY"),
		line(732, 51, "02.01.26", 139, "- 5 000,00 ₸", 257, kaspi.Transfer, 300, "Айгерим А."),
		line(716, 51, "02.01.26", 139, "- 2 542,70 ₸", 257, kaspi.Purchase, 300, "OPENAI"),
		line(700, 220, "(- 5,00 USD)"), // the foreign amount wraps
		line(27, 40, "АО «Kaspi Bank», БИК CASPKZKA, www.kaspi.kz"),
	}
	st, err := parseKaspiLines(lines, almaty)
	if err != nil {
		t.Fatal(err)
	}
	if st.Holder != "Иванов Иван Иванович" {
		t.Errorf("holder: %q", st.Holder)
	}
	if st.Account != "*1234" || st.From.Format("2006-01-02") != "2026-01-01" || st.To.Format("2006-01-02") != "2026-12-31" {
		t.Fatalf("header: %+v", st)
	}
	want := []Op{
		{AmountMinor: -123000, Kind: kaspi.Purchase, Details: "ИП ''АЛМАЗ''", Seq: 1},
		{AmountMinor: 1000000, Kind: kaspi.FromOwn, Details: "С Kaspi Депозита", Seq: 2},
		{AmountMinor: -341000, Kind: kaspi.Purchase, Details: "SMALL MARKET ALMATY", Seq: 1},
		{AmountMinor: -500000, Kind: kaspi.Transfer, Details: "Айгерим А.", Seq: 2},
		{AmountMinor: -254270, Kind: kaspi.Purchase, Details: "OPENAI", Seq: 3},
	}
	if len(st.Ops) != len(want) {
		t.Fatalf("ops: %+v", st.Ops)
	}
	for i, w := range want {
		g := st.Ops[i]
		if g.AmountMinor != w.AmountMinor || g.Kind != w.Kind || g.Details != w.Details || g.Seq != w.Seq || g.Currency != "KZT" {
			t.Errorf("op %d: got %+v want %+v", i, g, w)
		}
	}
	if err := st.Verify(); err != nil {
		t.Fatal(err)
	}
	if st.Ops[4].Foreign != "- 5,00 USD" {
		t.Errorf("foreign amount: %q", st.Ops[4].Foreign)
	}
	if !st.Ops[0].IsSpending() || st.Ops[1].IsSpending() || st.Ops[3].IsSpending() {
		t.Error("IsSpending")
	}
	refund := Op{Kind: kaspi.Purchase, AmountMinor: 500000}
	if !refund.IsSpending() || !refund.IsRefund() || st.Ops[0].IsRefund() {
		t.Error("refund must be categorized with purchases")
	}

	// a lost row is caught by the header totals
	st.Ops = st.Ops[1:]
	if err := st.Verify(); err == nil || !strings.Contains(err.Error(), "Purchases") {
		t.Fatalf("verify must fail: %v", err)
	}
}

func TestParseKaspiNoOps(t *testing.T) {
	if _, err := parseKaspiLines([][]segment{line(10, 40, "just text")}, almaty); err == nil {
		t.Fatal("want error")
	}
}

// Real statements live in statements/ and are not committed; without them the test is skipped.
func TestParseRealStatements(t *testing.T) {
	files, _ := os.ReadDir("../../statements")
	n := 0
	for _, f := range files {
		if !strings.HasSuffix(f.Name(), ".pdf") {
			continue
		}
		b, err := os.ReadFile("../../statements/" + f.Name())
		if err != nil {
			t.Fatal(err)
		}
		st, err := ParseKaspiBytes(b, almaty)
		if err != nil {
			t.Errorf("%s: %v", f.Name(), err)
			continue
		}
		if err := st.Verify(); err != nil {
			t.Errorf("%s: %v", f.Name(), err)
		}
		t.Logf("%s: %d operations, %s – %s", f.Name(), len(st.Ops), st.From.Format("2006-01-02"), st.To.Format("2006-01-02"))
		n++
	}
	if n == 0 {
		t.Skip("no statements in statements/")
	}
}
