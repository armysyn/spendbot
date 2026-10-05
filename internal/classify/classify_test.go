package classify

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"spendbot/internal/store"
)

var cats = []store.Category{
	{ID: 1, Name: "Groceries"}, {ID: 2, Name: "Cafes & restaurants"}, {ID: 3, Name: "Taxi"},
	{ID: 4, Name: "Gifts"}, {ID: 5, Name: "Transport"}, {ID: 6, Name: "Other"},
}

func TestRuleSplits(t *testing.T) {
	cases := []struct {
		text  string
		total int64
		want  []store.Split
	}{
		{"groceries", 450000, []store.Split{{CategoryID: 1, AmountMinor: 450000}}},
		{"Groceries.", 450000, []store.Split{{CategoryID: 1, AmountMinor: 450000}}},
		{"groceries, plus a gift for mom 2000", 450000, []store.Split{
			{CategoryID: 1, AmountMinor: 250000}, {CategoryID: 4, AmountMinor: 200000, Note: "mom"}}},
		{"gift for mom 2 000 ₸ + groceries", 450000, []store.Split{
			{CategoryID: 4, AmountMinor: 200000, Note: "mom"}, {CategoryID: 1, AmountMinor: 250000}}},
		{"groceries 2500; taxi 2000", 450000, []store.Split{
			{CategoryID: 1, AmountMinor: 250000}, {CategoryID: 3, AmountMinor: 200000}}},
		{"restaurant with colleagues", 1000000, []store.Split{{CategoryID: 2, AmountMinor: 1000000, Note: "with colleagues"}}},
		{"cafes and restaurants", 100, []store.Split{{CategoryID: 2, AmountMinor: 100}}},
		{"groceries 1 250,50, taxi", 300000, []store.Split{
			{CategoryID: 1, AmountMinor: 125050}, {CategoryID: 3, AmountMinor: 174950}}},
		// the payment amount was not parsed — it comes from the answer
		{"taxi 1200", 0, []store.Split{{CategoryID: 3, AmountMinor: 120000}}},
	}
	for _, c := range cases {
		got, err := RuleSplits(c.text, c.total, "KZT", cats)
		if err != nil {
			t.Errorf("%q: %v", c.text, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q:\n got %+v\nwant %+v", c.text, got, c.want)
		}
	}
}

// Existing databases have Russian category names and Russian answers; they keep working.
func TestRuleSplitsRussian(t *testing.T) {
	ru := []store.Category{{ID: 1, Name: "Продукты"}, {ID: 4, Name: "Подарки"}}
	got, err := RuleSplits("продукты, плюс подарок маме 2000", 450000, "KZT", ru)
	want := []store.Split{{CategoryID: 1, AmountMinor: 250000}, {CategoryID: 4, AmountMinor: 200000, Note: "маме"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v %v", got, err)
	}
}

func TestRuleSplitsErrors(t *testing.T) {
	cases := []struct {
		text  string
		total int64
		msg   string
	}{
		{"food", 1000, "unknown category"},
		{"groceries 100, taxi 100", 1000000, "do not add up"},
		{"groceries, taxi", 1000, "only one category"},
		{"groceries 20000, taxi", 1000, "already exceed"},
		{"groceries", 0, "amount is unknown"},
		{"  ", 1000, "empty answer"},
	}
	for _, c := range cases {
		_, err := RuleSplits(c.text, c.total, "KZT", cats)
		if err == nil || !strings.Contains(err.Error(), c.msg) {
			t.Errorf("%q: got %v, want %q", c.text, err, c.msg)
		}
	}
}

func TestParseItemsManual(t *testing.T) {
	items, err := ParseItems("taxi 1200")
	if err != nil || len(items) != 1 || items[0].Amount != 120000 || items[0].Words[0] != "taxi" {
		t.Fatalf("%+v %v", items, err)
	}
	items, _ = ParseItems("1500 coffee")
	if items[0].Amount != 150000 || items[0].Words[0] != "coffee" {
		t.Fatalf("%+v", items)
	}
	items, _ = ParseItems("$12.50 netflix")
	if items[0].Amount != 1250 || items[0].Currency != "USD" {
		t.Fatalf("%+v", items)
	}
}

type fakeRules map[string]store.Rule

func (f fakeRules) Rule(_ context.Context, norm string) (store.Rule, bool, error) {
	r, ok := f[norm]
	return r, ok, nil
}

type fakeLLM struct {
	up    bool
	reply string
	err   error
	calls int
}

func (f *fakeLLM) Available() bool { return f.up }
func (f *fakeLLM) JSON(context.Context, string, string) (string, error) {
	f.calls++
	return f.reply, f.err
}

func newC(rules fakeRules, l *fakeLLM) *Classifier {
	var p interface {
		Available() bool
		JSON(context.Context, string, string) (string, error)
	}
	if l != nil {
		p = l
	}
	return New(rules, p, 3, time.UTC, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func tx(norm string, amount int64) store.Tx {
	return store.Tx{ID: 1, MerchantRaw: strings.ToUpper(norm), MerchantNorm: norm, AmountMinor: amount, Currency: "KZT"}
}

func TestDecideChain(t *testing.T) {
	ctx := context.Background()
	rules := fakeRules{"magnum": {CategoryID: 1, Hits: 3}, "coffee boom": {CategoryID: 2, Hits: 1}}

	// memory is confident — the LLM is not asked
	l := &fakeLLM{up: true, reply: `{"category":"Taxi","confidence":0.9}`}
	d, _ := newC(rules, l).Decide(ctx, tx("magnum", 100), cats)
	if d.Auto != 1 || l.calls != 0 {
		t.Fatalf("rule auto: %+v calls=%d", d, l.calls)
	}
	// memory is not confident — its category becomes the first button
	d, _ = newC(rules, l).Decide(ctx, tx("coffee boom", 100), cats)
	if d.Auto != 0 || d.Guess != 2 || l.calls != 0 {
		t.Fatalf("rule guess: %+v", d)
	}
	// unknown merchant — the LLM guesses
	d, _ = newC(rules, l).Decide(ctx, tx("yandex go", 100), cats)
	if d.Guess != 3 || d.By != "llm" {
		t.Fatalf("llm guess: %+v", d)
	}
	// garbage from the model is dropped
	for _, reply := range []string{`not json`, `{"category":"Food","confidence":0.9}`, `{"category":"Taxi","confidence":0.1}`} {
		d, _ = newC(rules, &fakeLLM{up: true, reply: reply}).Decide(ctx, tx("x", 100), cats)
		if d != (Decision{}) {
			t.Errorf("reply %s: %+v", reply, d)
		}
	}
	// the model is down or off — no guess and no call
	off := &fakeLLM{up: false, reply: `{"category":"Taxi","confidence":0.9}`}
	if d, _ = newC(rules, off).Decide(ctx, tx("x", 100), cats); d != (Decision{}) || off.calls != 0 {
		t.Fatalf("unavailable: %+v", d)
	}
	if d, _ = newC(rules, nil).Decide(ctx, tx("x", 100), cats); d != (Decision{}) {
		t.Fatalf("no llm: %+v", d)
	}
}

func TestParseAnswerLLM(t *testing.T) {
	ctx := context.Background()
	text := "bought bread and a gift for mom for two thousand"

	l := &fakeLLM{up: true, reply: `{"splits":[{"category":"Groceries","amount":null,"note":"bread"},{"category":"Gifts","amount":2000,"note":"mom"}]}`}
	got, err := newC(nil, l).ParseAnswer(ctx, tx("magnum", 450000), text, cats)
	want := []store.Split{{CategoryID: 1, AmountMinor: 250000, Note: "bread"}, {CategoryID: 4, AmountMinor: 200000, Note: "mom"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v %v", got, err)
	}

	// a plain answer is parsed by rules, the model is not needed
	l.calls = 0
	if _, err := newC(nil, l).ParseAnswer(ctx, tx("m", 100), "taxi", cats); err != nil || l.calls != 0 {
		t.Fatalf("rule path: %v calls=%d", err, l.calls)
	}

	// the model's amounts do not add up — its answer is dropped and the rule parse stays
	// (the bot shows the result, and /undo reverts it)
	bad := &fakeLLM{up: true, reply: `{"splits":[{"category":"Groceries","amount":100},{"category":"Gifts","amount":2000}]}`}
	got, err = newC(nil, bad).ParseAnswer(ctx, tx("m", 450000), text, cats)
	if err != nil || len(got) != 1 || got[0].CategoryID != 4 || got[0].AmountMinor != 450000 {
		t.Fatalf("sum mismatch fallback: %+v %v", got, err)
	}

	// rules failed and the model is unreachable — the user sees the rule error
	netErr := &fakeLLM{up: true, err: errors.New("timeout")}
	_, err = newC(nil, netErr).ParseAnswer(ctx, tx("m", 450000), "random stuff", cats)
	if err == nil || !strings.Contains(err.Error(), "unknown category") {
		t.Fatalf("fallback error: %v", err)
	}
}
