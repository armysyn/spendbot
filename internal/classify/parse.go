package classify

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"spendbot/internal/money"
	"spendbot/internal/store"
)

// Item is one part of a free-text answer: "gift for mom 2000".
type Item struct {
	Words     []string
	Amount    int64 // in minor units; 0 — no amount given
	Currency  string
	HasAmount bool
}

var (
	// Part separators: comma, semicolon, newline, " + ", " plus " and their Russian equivalents.
	itemSep = regexp.MustCompile(`(?i)\s*(?:[,;\n]|\s\+\s|\splus\s|\sплюс\s|\sи ещё\s|\sи еще\s)\s*`)
	// amount: "2000", "2 000", "1 250,50", "-500", with an optional currency
	amountRe = regexp.MustCompile(`(?i)(?:^|\s)((?:[$€₸₽]\s?)?-?\d[\d\x{00a0}\x{202f} ]*(?:[.,]\d{1,2})?)\s*(₸|тг|тенге|\$|€|₽)?(?:\s|$)`)
	// Words that carry no meaning in an answer, English and Russian.
	fillers = map[string]bool{
		"for": true, "on": true, "at": true, "to": true, "the": true, "a": true, "and": true, "plus": true, "&": true,
		"на": true, "за": true, "в": true, "это": true, "плюс": true, "и": true, "ещё": true, "еще": true,
	}
)

// ParseItems splits free text into parts with optional amounts.
// A comma inside a number ("1 250,50") does not start a new part.
func ParseItems(text string) ([]Item, error) {
	var items []Item
	for _, part := range splitItems(text) {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		var it Item
		if locs := amountRe.FindAllStringSubmatchIndex(part, -1); locs != nil {
			loc := locs[len(locs)-1]
			raw := part[loc[2]:loc[3]]
			if loc[4] >= 0 {
				raw += " " + part[loc[4]:loc[5]]
			}
			minor, cur, err := money.Parse(raw)
			if err != nil {
				return nil, fmt.Errorf("cannot read amount %q", strings.TrimSpace(raw))
			}
			if minor < 0 {
				return nil, fmt.Errorf("a part cannot be negative: %q", strings.TrimSpace(raw))
			}
			it.Amount, it.Currency, it.HasAmount = minor, cur, true
			part = part[:loc[0]] + " " + part[loc[1]:]
		}
		for _, w := range strings.Fields(strings.ToLower(part)) {
			w = strings.Trim(w, ".!?:—-«»\"'()")
			if w != "" && !fillers[w] {
				it.Words = append(it.Words, w)
			}
		}
		if len(it.Words) == 0 && !it.HasAmount {
			continue
		}
		items = append(items, it)
	}
	if len(items) == 0 {
		return nil, errors.New("empty answer")
	}
	return items, nil
}

// splitItems splits on separators but keeps a comma between digits.
func splitItems(text string) []string {
	const guard = "\x00"
	protected := decimalComma.ReplaceAllString(text, "$1"+guard+"$2")
	parts := itemSep.Split(protected, -1)
	for i := range parts {
		parts[i] = strings.ReplaceAll(parts[i], guard, ",")
	}
	return parts
}

var decimalComma = regexp.MustCompile(`(\d),(\d{1,2}\b)`)

// MatchCategory finds a category from the words of a part and returns it with the leftover
// words (the note). A word matches a category word when they share a prefix of at least
// max(4, len-2) runes: "gift" → "Gifts", "restaurant" → "Cafes & restaurants".
func MatchCategory(words []string, cats []store.Category) (store.Category, []string, bool) {
	// First the full multi-word name, "cafes & restaurants". Filler words ("&", "and") are
	// already removed from words, so they are removed from the name too.
	joined := " " + strings.Join(words, " ") + " "
	for _, c := range cats {
		var nameWords []string
		for _, w := range strings.Fields(strings.ToLower(c.Name)) {
			if !fillers[w] {
				nameWords = append(nameWords, w)
			}
		}
		name := " " + strings.Join(nameWords, " ") + " "
		if len(nameWords) > 1 && strings.Contains(joined, name) {
			return c, strings.Fields(strings.Replace(joined, name, " ", 1)), true
		}
	}
	for i, w := range words {
		var found []store.Category
		for _, c := range cats {
			for _, cw := range strings.Fields(strings.ToLower(c.Name)) {
				if fillers[cw] {
					continue
				}
				if similar(w, cw) {
					found = append(found, c)
					break
				}
			}
		}
		if len(found) == 1 {
			rest := append(append([]string{}, words[:i]...), words[i+1:]...)
			return found[0], rest, true
		}
	}
	return store.Category{}, words, false
}

func similar(a, b string) bool {
	if a == b {
		return true
	}
	la, lb := utf8.RuneCountInString(a), utf8.RuneCountInString(b)
	need := max(4, min(la, lb)-2)
	ra, rb := []rune(a), []rune(b)
	n := 0
	for n < len(ra) && n < len(rb) && ra[n] == rb[n] {
		n++
	}
	return n >= need
}

// RuleSplits parses an answer without an LLM: "groceries", "groceries, gift for mom 2000".
// One part without an amount gets the remainder. If the transaction amount is unknown (0),
// every part needs an amount.
func RuleSplits(text string, total int64, currency string, cats []store.Category) ([]store.Split, error) {
	items, err := ParseItems(text)
	if err != nil {
		return nil, err
	}
	var parts []part
	for _, it := range items {
		c, rest, ok := MatchCategory(it.Words, cats)
		if !ok {
			return nil, fmt.Errorf("unknown category %q", strings.Join(it.Words, " "))
		}
		parts = append(parts, part{cat: c.ID, amount: it.Amount, has: it.HasAmount, note: strings.Join(rest, " ")})
	}
	return distribute(parts, total, currency)
}

type part struct {
	cat    int64
	amount int64
	has    bool
	note   string
}

// distribute checks that the parts add up to the transaction amount and hands out the remainder.
func distribute(parts []part, total int64, currency string) ([]store.Split, error) {
	var sum int64
	open := -1
	for i, p := range parts {
		if p.has {
			sum += p.amount
			continue
		}
		if open >= 0 {
			return nil, errors.New("only one category may be given without an amount")
		}
		open = i
	}
	switch {
	case total == 0 && open >= 0:
		return nil, errors.New("the payment amount is unknown, give an amount for every category")
	case total == 0:
		// the payment amount is unknown — the parts define it and CloseTx takes their sum
	case open >= 0:
		rest := total - sum
		if rest <= 0 {
			return nil, fmt.Errorf("the parts (%s) already exceed the payment", money.Format(sum, currency))
		}
		parts[open].amount = rest
	case sum != total:
		return nil, fmt.Errorf("amounts do not add up: %s of %s", money.Format(sum, currency), money.Format(total, currency))
	}
	out := make([]store.Split, 0, len(parts))
	for _, p := range parts {
		out = append(out, store.Split{CategoryID: p.cat, AmountMinor: p.amount, Note: p.note})
	}
	return out, nil
}
