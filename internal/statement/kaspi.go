// Package statement parses bank statements. Amounts are read from the PDF text by code,
// not by a model: models get digits wrong.
package statement

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/ledongthuc/pdf"

	"spendbot/internal/kaspi"
	"spendbot/internal/money"
)

// Op is one operation from a statement.
type Op struct {
	Date        time.Time // day of the operation; statements have no time
	AmountMinor int64     // signed: purchases negative, incoming money positive
	Currency    string
	Kind        string // as printed in the statement, see internal/kaspi
	Details     string // merchant or recipient
	Foreign     string // amount in the purchase currency when it is not tenge: "- 5,00 USD"
	Seq         int    // position of the operation within its day in the statement
}

// Statement is a parsed statement.
type Statement struct {
	Account string // card number, such as *1234
	Holder  string // the account holder's full name from the cover; empty when not found
	// Opening and Closing are the card balances at the start and the end of the period, as the
	// statement prints them ("available on"); HasBalance is false for a statement without them.
	Opening, Closing int64
	HasBalance       bool
	From, To         time.Time
	Ops              []Op
	// Totals from the header summary by Kaspi label (see kaspi.Summaries), in tiyn.
	Summary map[string]int64
}

// Verify checks parsed operations against the totals in the statement header. A mismatch
// means parsing lost or duplicated rows. Foreign-currency amounts are moved to Foreign, so
// the kind of such purchases is plain and exact matching is enough.
func (s Statement) Verify() error {
	var errs []error
	for _, c := range kaspi.Summaries {
		want, ok := s.Summary[c.Label]
		if !ok {
			continue
		}
		var got int64
		for _, o := range s.Ops {
			if o.Kind == c.Kind {
				got += o.AmountMinor
			}
		}
		if got != want {
			errs = append(errs, fmt.Errorf("%s: statement %s, parsed %s", c.English,
				money.Format(want, money.DefaultCurrency), money.Format(got, money.DefaultCurrency)))
		}
	}
	return errors.Join(errs...)
}

var (
	dateRe   = regexp.MustCompile(`^\d{2}\.\d{2}\.\d{2}$`)
	amountRe = regexp.MustCompile(`^[+-]\s?[\d\x{00a0} ]+,\d{2}\s?(₸|\$|€|₽)$`)
	periodRe = regexp.MustCompile(kaspi.PeriodPattern)
	cardRe   = regexp.MustCompile(`^\*\d{4}$`)
	holderRe = regexp.MustCompile(kaspi.HolderPattern)
	balRe    = regexp.MustCompile(kaspi.BalancePattern)
)

// segment is a continuous piece of text on a page line.
type segment struct {
	x, end, y float64
	s         string
}

// ParseKaspi parses a Kaspi Gold PDF statement.
func ParseKaspi(r io.ReaderAt, size int64, loc *time.Location) (Statement, error) {
	reader, err := pdf.NewReader(r, size)
	if err != nil {
		return Statement{}, fmt.Errorf("not a PDF or the file is damaged: %w", err)
	}
	var lines [][]segment
	for i := 1; i <= reader.NumPage(); i++ {
		p := reader.Page(i)
		if p.V.IsNull() {
			continue
		}
		lines = append(lines, pageLines(p)...)
	}
	return parseKaspiLines(lines, loc)
}

// ParseKaspiBytes does the same for a file in memory (web upload).
func ParseKaspiBytes(b []byte, loc *time.Location) (Statement, error) {
	return ParseKaspi(bytes.NewReader(b), int64(len(b)), loc)
}

// pageLines joins page characters into text pieces and groups them into lines top to bottom.
func pageLines(p pdf.Page) [][]segment {
	var segs []segment
	for _, t := range p.Content().Text {
		if n := len(segs); n > 0 && segs[n-1].y == t.Y && t.X >= segs[n-1].x && t.X-segs[n-1].end < 6 {
			segs[n-1].s += t.S
			segs[n-1].end = t.X + t.W
			continue
		}
		segs = append(segs, segment{x: t.X, end: t.X + t.W, y: t.Y, s: t.S})
	}
	sort.SliceStable(segs, func(i, j int) bool {
		if segs[i].y != segs[j].y {
			return segs[i].y > segs[j].y
		}
		return segs[i].x < segs[j].x
	})
	var lines [][]segment
	for _, s := range segs {
		s.s = strings.TrimSpace(s.s)
		if s.s == "" {
			continue
		}
		if n := len(lines); n > 0 && lines[n-1][0].y == s.y {
			lines[n-1] = append(lines[n-1], s)
			continue
		}
		lines = append(lines, []segment{s})
	}
	return lines
}

func parseKaspiLines(lines [][]segment, loc *time.Location) (Statement, error) {
	st := Statement{Summary: map[string]int64{}}
	// The details column comes from the table header. The header is on the first page only,
	// so operation rows are recognized by shape (date + amount), not by position under it.
	detailsX := 0.0
	var lastY float64 // y of the last operation row; -1 — nothing to continue
	lastY = -1
	seqByDay := map[string]int{}
	balances := map[time.Time]int64{}

	for li, line := range lines {
		text := joinLine(line)
		for _, m := range balRe.FindAllStringSubmatch(text, -1) {
			on, err := time.ParseInLocation("02.01.06", m[1], loc)
			v, _, perr := money.Parse(m[2])
			if err == nil && perr == nil {
				balances[on] = v
			}
		}
		if m := holderRe.FindStringSubmatch(text); m != nil && st.Holder == "" {
			st.Holder = strings.Join(strings.Fields(m[1]), " ")
		}
		if m := periodRe.FindStringSubmatch(text); m != nil && st.From.IsZero() {
			st.From, _ = time.ParseInLocation("02.01.06", m[1], loc)
			st.To, _ = time.ParseInLocation("02.01.06", m[2], loc)
		}
		for _, s := range line {
			if cardRe.MatchString(s.s) && st.Account == "" {
				st.Account = s.s
			}
		}
		if len(st.Ops) == 0 {
			collectSummary(line, st.Summary)
		}
		if isHeader(line) {
			for _, s := range line {
				if strings.HasPrefix(s.s, kaspi.ColDetails) {
					detailsX = s.x
				}
			}
			lastY = -1
			continue
		}

		if isRow(line) {
			op, err := parseRow(line, detailsColumn(detailsX), loc)
			if err != nil {
				return st, fmt.Errorf("line %d %q: %w", li+1, text, err)
			}
			key := op.Date.Format("2006-01-02")
			seqByDay[key]++
			op.Seq = seqByDay[key]
			st.Ops = append(st.Ops, op)
			lastY = line[0].y
			continue
		}

		// Wrapped text of the previous operation: no date, inside the table columns, right
		// below it on the same page (on a new page y is large again and the gap is negative).
		if gap := lastY - line[0].y; lastY >= 0 && line[0].x > 100 && gap > 0 && gap < 20 {
			last := &st.Ops[len(st.Ops)-1]
			for _, s := range line {
				if s.x >= detailsColumn(detailsX)-5 {
					last.Details = strings.TrimSpace(last.Details + " " + s.s)
				} else {
					last.Kind = strings.TrimSpace(last.Kind + " " + s.s)
				}
			}
			lastY = line[0].y
			continue
		}
		// Page footers, footnotes, header — nothing wraps after them.
		lastY = -1
	}
	if len(st.Ops) == 0 {
		return st, errors.New("no operations found: is this a Kaspi Gold PDF statement?")
	}
	// The foreign-currency amount often wraps to the next line, so it is split off after joining.
	for i := range st.Ops {
		if m := foreignRe.FindStringSubmatch(st.Ops[i].Kind); m != nil {
			st.Ops[i].Kind, st.Ops[i].Foreign = m[1], m[2]
		}
	}
	// balances are matched to the period's ends once both are known
	o, okO := balances[st.From]
	c, okC := balances[st.To]
	if okO && okC {
		st.Opening, st.Closing, st.HasBalance = o, c, true
	}
	return st, nil
}

// isRow: an operation row starts with a date in the left column and contains an amount.
func isRow(line []segment) bool {
	if len(line) < 3 || !dateRe.MatchString(line[0].s) || line[0].x > 100 {
		return false
	}
	for _, s := range line[1:] {
		if amountRe.MatchString(s.s) {
			return true
		}
	}
	return false
}

// detailsColumn is where the details column starts; without a header the usual position
// in Kaspi statements is used.
func detailsColumn(x float64) float64 {
	if x == 0 {
		return 295
	}
	return x
}

var summaryLabels = func() map[string]bool {
	m := map[string]bool{}
	for _, s := range kaspi.Summaries {
		m[s.Label] = true
	}
	return m
}()

// foreignRe matches a purchase in another currency, "<kind> (- 5,00 USD)"; the tenge amount is already converted by the bank.
var foreignRe = regexp.MustCompile(`^(.*?)\s*\(([+-]\s?[\d\x{00a0} ]+,\d{2}\s?[A-Z]{3})\)$`)

// collectSummary finds label → amount pairs on a header line.
func collectSummary(line []segment, into map[string]int64) {
	for i := 0; i+1 < len(line); i++ {
		label := line[i].s
		if !summaryLabels[label] {
			continue
		}
		if !amountRe.MatchString(line[i+1].s) {
			continue
		}
		if v, _, err := money.Parse(line[i+1].s); err == nil {
			into[label] = v
		}
	}
}

func isHeader(line []segment) bool {
	var have [4]bool
	for _, s := range line {
		switch {
		case s.s == kaspi.ColDate:
			have[0] = true
		case s.s == kaspi.ColAmount:
			have[1] = true
		case strings.HasPrefix(s.s, kaspi.ColKind):
			have[2] = true
		case strings.HasPrefix(s.s, kaspi.ColDetails):
			have[3] = true
		}
	}
	return have[0] && have[1] && have[2] && have[3]
}

func parseRow(line []segment, detailsX float64, loc *time.Location) (Op, error) {
	var op Op
	d, err := time.ParseInLocation("02.01.06", line[0].s, loc)
	if err != nil {
		return op, err
	}
	op.Date = d
	var kind, details []string
	for _, s := range line[1:] {
		switch {
		case op.AmountMinor == 0 && amountRe.MatchString(s.s):
			minor, cur, err := money.Parse(s.s)
			if err != nil {
				return op, err
			}
			op.AmountMinor, op.Currency = minor, cur
		case s.x >= detailsX-5:
			details = append(details, s.s)
		default:
			kind = append(kind, s.s)
		}
	}
	if op.AmountMinor == 0 {
		return op, errors.New("amount not found")
	}
	op.Kind = strings.Join(kind, " ")
	op.Details = strings.Join(details, " ")
	return op, nil
}

func joinLine(line []segment) string {
	parts := make([]string, len(line))
	for i, s := range line {
		parts[i] = s.s
	}
	return strings.Join(parts, " ")
}

// IsSpending reports whether the operation gets categorized: purchases (refunds included —
// they reduce the merchant's category) and cash withdrawals.
func (o Op) IsSpending() bool {
	return o.Kind == kaspi.Purchase || (o.Kind == kaspi.Withdrawal && o.AmountMinor < 0)
}

// IsRefund reports a purchase refund: money came back to the card.
func (o Op) IsRefund() bool { return o.Kind == kaspi.Purchase && o.AmountMinor > 0 }
