// Package report builds text summaries and CSV exports. All sums come from SQL.
package report

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"spendbot/internal/merchant"
	"spendbot/internal/money"
	"spendbot/internal/store"
)

// Range is the half-open interval [From, To) with a title for messages.
type Range struct {
	From, To time.Time
	Title    string
}

func startOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func startOfWeek(t time.Time) time.Time {
	d := startOfDay(t)
	return d.AddDate(0, 0, -((int(d.Weekday()) + 6) % 7))
}

func Day(now time.Time) Range {
	from := startOfDay(now)
	return Range{From: from, To: from.AddDate(0, 0, 1), Title: "today, " + from.Format("Jan 2")}
}

func Week(now time.Time) Range {
	from := startOfWeek(now)
	to := from.AddDate(0, 0, 7)
	return Range{From: from, To: to, Title: "week of " + from.Format("Jan 2") + "–" + to.AddDate(0, 0, -1).Format("Jan 2")}
}

func LastWeek(now time.Time) Range { return Week(startOfWeek(now).AddDate(0, 0, -1)) }

func Month(now time.Time) Range {
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	return Range{From: from, To: from.AddDate(0, 1, 0), Title: from.Format("January 2006")}
}

// ParseRange understands "today", "week", "month", "last week", "last month" and
// "2026-09"; Russian words are accepted too. An empty argument means def.
func ParseRange(arg string, now time.Time, def func(time.Time) Range) (Range, error) {
	arg = strings.ToLower(strings.Join(strings.Fields(arg), " "))
	switch arg {
	case "":
		return def(now), nil
	case "today", "day", "сегодня", "день":
		return Day(now), nil
	case "week", "неделя", "нед":
		return Week(now), nil
	case "month", "месяц", "мес":
		return Month(now), nil
	case "last week", "прошлая неделя":
		return LastWeek(now), nil
	case "last month", "прошлый месяц":
		return Month(Month(now).From.AddDate(0, -1, 0)), nil
	}
	if t, err := time.ParseInLocation("2006-01", arg, now.Location()); err == nil {
		return Month(t), nil
	}
	return Range{}, fmt.Errorf("unknown period %q: try week, month, last week, last month or 2026-09", arg)
}

// Summary lists spending by category with a total per currency.
func Summary(r Range, totals []store.CategoryTotal) string {
	if len(totals) == 0 {
		return fmt.Sprintf("Summary: %s\nNo spending.", r.Title)
	}
	byCur := map[string][]store.CategoryTotal{}
	for _, t := range totals {
		byCur[t.Currency] = append(byCur[t.Currency], t)
	}
	currencies := make([]string, 0, len(byCur))
	for c := range byCur {
		currencies = append(currencies, c)
	}
	sort.Slice(currencies, func(i, j int) bool {
		if (currencies[i] == money.DefaultCurrency) != (currencies[j] == money.DefaultCurrency) {
			return currencies[i] == money.DefaultCurrency
		}
		return currencies[i] < currencies[j]
	})

	var b strings.Builder
	fmt.Fprintf(&b, "Summary: %s", r.Title)
	for _, cur := range currencies {
		rows := byCur[cur]
		var sum int64
		var count int
		var open *store.CategoryTotal
		for i := range rows {
			if rows[i].Category == "" {
				open = &rows[i]
				continue
			}
			sum += rows[i].Minor
			count += rows[i].Count
		}
		fmt.Fprintf(&b, "\n\nTotal: %s", money.Format(sum, cur))
		for _, t := range rows {
			if t.Category == "" {
				continue
			}
			pct := 0.0
			if sum != 0 {
				pct = float64(t.Minor) * 100 / float64(sum)
			}
			fmt.Fprintf(&b, "\n%s — %s · %.0f%%", t.Category, money.Format(t.Minor, cur), pct)
		}
		if open != nil {
			fmt.Fprintf(&b, "\nUncategorized — %s (%d) · /pending", money.Format(open.Minor, cur), open.Count)
		}
	}
	return b.String()
}

// CSV exports one row per spending part, amounts in major units with a decimal point.
func CSV(rows []store.ExportRow, loc *time.Location) []byte {
	var buf bytes.Buffer
	buf.WriteString("\uFEFF") // BOM so that Excel opens UTF-8 correctly
	w := csv.NewWriter(&buf)
	w.Write([]string{"id", "date", "time", "amount", "currency", "category", "merchant", "card", "source", "status", "note"})
	for _, r := range rows {
		t := r.Tx.OccurredAt.In(loc)
		note := strings.TrimSpace(strings.Join([]string{r.Tx.Note, r.SplitNote}, " "))
		w.Write([]string{
			strconv.FormatInt(r.Tx.ID, 10), t.Format("2006-01-02"), t.Format("15:04"),
			decimal(r.AmountMinor), r.Tx.Currency, r.Category, displayMerchant(r.Tx), r.Tx.Card,
			r.Tx.Source, r.Tx.Status, note,
		})
	}
	w.Flush()
	return buf.Bytes()
}

func displayMerchant(t store.Tx) string {
	if t.MerchantRaw == "" {
		return ""
	}
	return merchant.Display(t.MerchantRaw)
}

func decimal(minor int64) string {
	sign := ""
	if minor < 0 {
		sign, minor = "-", -minor
	}
	return fmt.Sprintf("%s%d.%02d", sign, minor/100, minor%100)
}
