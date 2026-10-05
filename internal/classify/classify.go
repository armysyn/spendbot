// Package classify decides which category a transaction belongs to. The chain goes from
// cheap to expensive: merchant memory → LLM guess → asking a person.
package classify

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"spendbot/internal/llm"
	"spendbot/internal/merchant"
	"spendbot/internal/money"
	"spendbot/internal/store"
)

type Rules interface {
	Rule(ctx context.Context, norm string) (store.Rule, bool, error)
}

type Classifier struct {
	rules   Rules
	llm     llm.Provider // nil — no model
	minHits int
	loc     *time.Location
	log     *slog.Logger
}

func New(rules Rules, provider llm.Provider, minHits int, loc *time.Location, log *slog.Logger) *Classifier {
	return &Classifier{rules: rules, llm: provider, minHits: minHits, loc: loc, log: log}
}

func (c *Classifier) llmReady() bool { return c.llm != nil && c.llm.Available() }

// Decision is the outcome of classifying a new transaction.
type Decision struct {
	Auto  int64  // category to set without asking (memory, hits ≥ minHits)
	Guess int64  // guess for the first button
	By    string // rule | llm | "" — where the guess came from, for logs
}

// Decide walks the chain. LLM errors are not fatal: there is just no guess.
func (c *Classifier) Decide(ctx context.Context, tx store.Tx, cats []store.Category) (Decision, error) {
	if tx.MerchantNorm != "" {
		r, ok, err := c.rules.Rule(ctx, tx.MerchantNorm)
		if err != nil {
			return Decision{}, err
		}
		if ok && r.Hits >= c.minHits {
			return Decision{Auto: r.CategoryID, By: "rule"}, nil
		}
		if ok {
			return Decision{Guess: r.CategoryID, By: "rule"}, nil
		}
	}
	if !c.llmReady() || tx.AmountMinor == 0 {
		return Decision{}, nil
	}
	id, err := c.llmGuess(ctx, tx, cats)
	if err != nil {
		c.log.Warn("llm guess failed", "tx_id", tx.ID, "err", err)
		return Decision{}, nil
	}
	return Decision{Guess: id, By: "llm"}, nil
}

const guessSystem = `You help track personal spending in Kazakhstan. Given the merchant name, amount and time,
pick exactly one category from the list, using its name exactly as written. Answer with JSON only:
{"category": "<name from the list>", "confidence": <0..1>}. Use a low confidence when unsure.`

func (c *Classifier) llmGuess(ctx context.Context, tx store.Tx, cats []store.Category) (int64, error) {
	user := fmt.Sprintf("Merchant: %s\nAmount: %s\nTime: %s\nCard: %s\nCategories: %s",
		merchant.Display(tx.MerchantRaw), money.Format(tx.AmountMinor, tx.Currency),
		tx.OccurredAt.In(c.loc).Format("Mon 15:04"), tx.Card, categoryList(cats))
	out, err := c.llm.JSON(ctx, guessSystem, user)
	if err != nil {
		return 0, err
	}
	var resp struct {
		Category   string  `json:"category"`
		Confidence float64 `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return 0, fmt.Errorf("invalid JSON: %w", err)
	}
	cat, ok := byName(cats, resp.Category)
	if !ok {
		return 0, fmt.Errorf("category %q is not in the list", resp.Category)
	}
	if resp.Confidence < 0.3 {
		return 0, fmt.Errorf("low confidence %.2f", resp.Confidence)
	}
	return cat.ID, nil
}

// ParseAnswer turns a free-text answer into splits. Answers made only of categories and
// amounts ("groceries", "groceries, gifts 2000") are parsed by rules. If there are other
// words and the model is available, the model parses it: rules may get such text wrong.
// The code always checks that the amounts add up; otherwise it returns an error with a
// message for the user.
func (c *Classifier) ParseAnswer(ctx context.Context, tx store.Tx, text string, cats []store.Category) ([]store.Split, error) {
	ruleSplits, ruleErr := RuleSplits(text, tx.AmountMinor, tx.Currency, cats)
	if !c.llmReady() || (ruleErr == nil && !hasNotes(ruleSplits)) {
		return ruleSplits, ruleErr
	}
	splits, err := c.llmSplits(ctx, tx, text, cats)
	if err != nil {
		c.log.Warn("llm splits failed", "tx_id", tx.ID, "err", err)
		return ruleSplits, ruleErr
	}
	return splits, nil
}

func hasNotes(splits []store.Split) bool {
	for _, s := range splits {
		if s.Note != "" {
			return true
		}
	}
	return false
}

const splitsSystem = `You parse a person's answer about what a payment was for. Split the payment into parts
by categories from the list, using the names exactly as written. Give amounts in major currency units (tenge,
dollars) as numbers. One part may have a null amount — it gets the remainder. Keep the note short (who for, what).
Answer with JSON only: {"splits": [{"category": "<name from the list>", "amount": <number or null>, "note": "<text>"}]}`

func (c *Classifier) llmSplits(ctx context.Context, tx store.Tx, text string, cats []store.Category) ([]store.Split, error) {
	amount := "unknown"
	if tx.AmountMinor != 0 {
		amount = money.Format(tx.AmountMinor, tx.Currency)
	}
	user := fmt.Sprintf("Payment: %s at %s\nCategories: %s\nAnswer: %s",
		amount, merchant.Display(tx.MerchantRaw), categoryList(cats), text)
	out, err := c.llm.JSON(ctx, splitsSystem, user)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Splits []struct {
			Category string   `json:"category"`
			Amount   *float64 `json:"amount"`
			Note     string   `json:"note"`
		} `json:"splits"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	if len(resp.Splits) == 0 || len(resp.Splits) > 10 {
		return nil, fmt.Errorf("invalid number of parts: %d", len(resp.Splits))
	}
	var parts []part
	for _, s := range resp.Splits {
		cat, ok := byName(cats, s.Category)
		if !ok {
			return nil, fmt.Errorf("category %q is not in the list", s.Category)
		}
		p := part{cat: cat.ID, note: strings.TrimSpace(s.Note)}
		if s.Amount != nil && *s.Amount != 0 {
			if *s.Amount < 0 || *s.Amount > 1e10 {
				return nil, fmt.Errorf("invalid amount %v", *s.Amount)
			}
			p.amount, p.has = int64(math.Round(*s.Amount*100)), true
		}
		parts = append(parts, p)
	}
	// If the model gave amounts for all parts but they do not add up, distribute rejects them.
	return distribute(parts, tx.AmountMinor, tx.Currency)
}

func categoryList(cats []store.Category) string {
	names := make([]string, len(cats))
	for i, c := range cats {
		names[i] = c.Name
	}
	return strings.Join(names, ", ")
}

func byName(cats []store.Category, name string) (store.Category, bool) {
	for _, c := range cats {
		if strings.EqualFold(c.Name, strings.TrimSpace(name)) {
			return c, true
		}
	}
	return store.Category{}, false
}
