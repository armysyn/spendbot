// Package money parses amounts as Apple Wallet and Kaspi print them ("4 500,00 ₸",
// "$12.50") into minor units (tiyn, cents) and formats them back.
package money

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

const DefaultCurrency = "KZT"

var ErrEmpty = errors.New("empty amount")

// Currency symbols and codes, including Russian abbreviations that appear in Kazakh
// sources. Order matters: longer marks are checked before shorter ones.
var currencyMarks = []struct{ mark, code string }{
	{"KZT", "KZT"}, {"USD", "USD"}, {"EUR", "EUR"}, {"RUB", "RUB"}, {"GBP", "GBP"}, {"TRY", "TRY"},
	{"тенге", "KZT"}, {"тг", "KZT"}, {"руб", "RUB"},
	{"₸", "KZT"}, {"$", "USD"}, {"€", "EUR"}, {"₽", "RUB"}, {"£", "GBP"}, {"₺", "TRY"},
}

// Parse turns an amount string into minor units and a currency code.
// The default currency is KZT. The sign is kept (refunds are negative).
func Parse(s string) (minor int64, currency string, err error) {
	rest := strings.TrimSpace(s)
	if rest == "" {
		return 0, "", ErrEmpty
	}
	currency = DefaultCurrency
	// Digits and separators are unaffected by case, so the rest of the work uses upper.
	upper := strings.ToUpper(rest)
	rest = upper
	for _, cm := range currencyMarks {
		mark := strings.ToUpper(cm.mark)
		if i := strings.Index(upper, mark); i >= 0 {
			currency = cm.code
			rest = upper[:i] + upper[i+len(mark):]
			break
		}
	}

	var b strings.Builder
	neg := false
	for _, r := range rest {
		switch {
		case r >= '0' && r <= '9', r == ',', r == '.':
			b.WriteRune(r)
		case r == '-' || r == '−' || r == '+':
			if b.Len() > 0 {
				return 0, "", fmt.Errorf("sign in the middle of amount %q", s)
			}
			neg = r != '+'
		case r == '\'' || unicode.IsSpace(r) || r == ' ' || r == ' ':
			// thousands separators: space, no-break space, narrow no-break space, apostrophe
		default:
			return 0, "", fmt.Errorf("unexpected character %q in amount %q", r, s)
		}
	}
	num := b.String()
	if num == "" {
		return 0, "", fmt.Errorf("no digits in amount %q", s)
	}

	intPart, frac, err := splitDecimal(num)
	if err != nil {
		return 0, "", fmt.Errorf("%w: %q", err, s)
	}
	if intPart == "" {
		intPart = "0"
	}
	units, err := strconv.ParseInt(intPart, 10, 64)
	if err != nil || units > 1e12 {
		return 0, "", fmt.Errorf("amount too large %q", s)
	}
	for len(frac) < 2 {
		frac += "0"
	}
	cents, _ := strconv.ParseInt(frac, 10, 64)
	minor = units*100 + cents
	if minor == 0 {
		return 0, "", fmt.Errorf("zero amount %q", s)
	}
	if neg {
		minor = -minor
	}
	return minor, currency, nil
}

// splitDecimal decides which of "," and "." is the decimal mark and which separates thousands.
func splitDecimal(num string) (intPart, frac string, err error) {
	lastComma, lastDot := strings.LastIndex(num, ","), strings.LastIndex(num, ".")
	sep := -1
	switch {
	case lastComma >= 0 && lastDot >= 0:
		// Both marks: the rightmost is decimal, as in 1.234,56 or 1,234.56.
		sep = max(lastComma, lastDot)
	case lastComma >= 0 || lastDot >= 0:
		i := max(lastComma, lastDot)
		mark := num[i]
		tail := len(num) - i - 1
		// A single mark used once and followed by 1–2 digits is decimal;
		// "4,500" and "1.234" are thousands.
		if strings.Count(num, string(mark)) == 1 && tail <= 2 {
			sep = i
		}
	}
	if sep >= 0 {
		intPart, frac = num[:sep], num[sep+1:]
		if len(frac) > 2 || strings.ContainsAny(frac, ",.") {
			return "", "", errors.New("more than two decimal places")
		}
	} else {
		intPart = num
	}
	groups := strings.FieldsFunc(intPart, func(r rune) bool { return r == ',' || r == '.' })
	for i, g := range groups {
		if (i > 0 && len(g) != 3) || (i == 0 && len(groups) > 1 && len(g) > 3) {
			return "", "", errors.New("invalid digit grouping")
		}
	}
	return strings.Join(groups, ""), frac, nil
}

var symbols = map[string]string{"KZT": "₸", "USD": "$", "EUR": "€", "RUB": "₽", "GBP": "£", "TRY": "₺"}

// Format prints an amount as "4,500 ₸" or "12.50 $"; zero cents are omitted. A no-break
// space keeps the symbol on the same line as the number.
func Format(minor int64, currency string) string {
	sign := ""
	if minor < 0 {
		sign, minor = "-", -minor
	}
	units, cents := minor/100, minor%100
	s := strconv.FormatInt(units, 10)
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteRune(',')
		}
		b.WriteRune(r)
	}
	out := sign + b.String()
	if cents != 0 {
		out += fmt.Sprintf(".%02d", cents)
	}
	sym, ok := symbols[currency]
	if !ok {
		sym = currency
	}
	return out + " " + sym
}
