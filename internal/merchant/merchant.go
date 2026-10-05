// Package merchant normalizes merchant names so that "MAGNUM CASH&CARRY #12" and
// "Magnum Cash & Carry" end up in the same memory rule.
package merchant

import (
	"strings"
	"unicode"
)

// Noise words: legal forms (Kazakh, Russian and English), cities, acquirer prefixes.
var noise = map[string]bool{
	"too": true, "тоо": true, "ип": true, "ip": true, "ao": true, "ао": true, "llp": true,
	"llc": true, "ltd": true, "inc": true, "kz": true, "kaz": true, "almaty": true, "алматы": true,
	"astana": true, "астана": true, "shymkent": true, "шымкент": true, "pos": true, "sq": true,
	"www": true, "com": true, "the": true,
}

// Normalize turns a name into a memory key: lower case, no punctuation, store numbers
// or noise words. If nothing is left, it keeps all tokens so the key is never empty.
func Normalize(raw string) string {
	tokens := tokenize(raw)
	var kept []string
	for _, t := range tokens {
		if noise[t] || isNumberish(t) {
			continue
		}
		kept = append(kept, t)
	}
	if len(kept) == 0 {
		kept = tokens
	}
	return strings.Join(kept, " ")
}

func tokenize(raw string) []string {
	return strings.FieldsFunc(strings.ToLower(raw), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// isNumberish: "12", "0042", "n12", "№5" — store numbers.
func isNumberish(t string) bool {
	digits := 0
	for _, r := range t {
		if unicode.IsDigit(r) {
			digits++
		}
	}
	return digits > 0 && digits >= len([]rune(t))-1
}

// Display is a short name for messages: "MAGNUM CASH&CARRY" → "Magnum Cash&Carry".
// Mixed case ("iHerb") is kept as is.
func Display(raw string) string {
	raw = strings.Join(strings.Fields(raw), " ")
	if raw == "" {
		return "unknown merchant"
	}
	if strings.ToUpper(raw) != raw {
		return raw
	}
	var b strings.Builder
	start := true
	for _, r := range raw {
		if start {
			b.WriteRune(unicode.ToUpper(r))
		} else {
			b.WriteRune(unicode.ToLower(r))
		}
		start = !unicode.IsLetter(r) && r != '\''
	}
	return b.String()
}
