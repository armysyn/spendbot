package bot

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"spendbot/internal/classify"
	"spendbot/internal/config"
	"spendbot/internal/store"
)

type sent struct {
	id int
	Message
}

type fakeMessenger struct {
	mu     sync.Mutex
	nextID int
	sent   []sent
	edits  map[int]Message
	files  []string
	acks   []string
}

func (f *fakeMessenger) Send(_ context.Context, m Message) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.sent = append(f.sent, sent{f.nextID, m})
	return f.nextID, nil
}

func (f *fakeMessenger) Edit(_ context.Context, id int, m Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.edits == nil {
		f.edits = map[int]Message{}
	}
	f.edits[id] = m
	return nil
}

func (f *fakeMessenger) AnswerCallback(_ context.Context, _, text string) error {
	f.acks = append(f.acks, text)
	return nil
}

func (f *fakeMessenger) SendFile(_ context.Context, name string, data []byte, _ string) error {
	f.files = append(f.files, name+"\n"+string(data))
	return nil
}

func (f *fakeMessenger) last() sent { return f.sent[len(f.sent)-1] }

type env struct {
	t   *testing.T
	ctx context.Context
	st  *store.Store
	msg *fakeMessenger
	a   *Agent
	now time.Time
}

var almaty, _ = time.LoadLocation("Asia/Almaty")

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cls := classify.New(st, nil, 3, almaty, log)
	msg := &fakeMessenger{}
	quiet, _ := config.ParseQuietHours("23:00-08:00")
	a := New(st, cls, msg, Options{Location: almaty, Quiet: quiet, BatchWindow: 10 * time.Minute,
		BatchThreshold: 3, ReminderAfter: 3 * time.Hour}, log)
	e := &env{t: t, ctx: ctx, st: st, msg: msg, a: a, now: time.Date(2026, 10, 5, 14, 32, 0, 0, almaty)}
	a.now = func() time.Time { return e.now }
	return e
}

// pay imitates receiving a Wallet payment and the agent handling it.
func (e *env) pay(merchant string, amount int64) int64 {
	e.t.Helper()
	norm := strings.ToLower(merchant)
	id, _, err := e.st.InsertTx(e.ctx, store.Tx{
		ExternalKey: merchant + e.now.String() + itoa(amount), OccurredAt: e.now, AmountMinor: amount,
		Currency: "KZT", MerchantRaw: merchant, MerchantNorm: norm, Card: "Kaspi Gold",
		Source: store.SourceWallet, CreatedAt: e.now,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	if err := e.a.route(e.ctx, id); err != nil {
		e.t.Fatal(err)
	}
	e.a.Flush(e.ctx)
	return id
}

func (e *env) cat(name string) int64 {
	c, err := e.st.FindCategory(e.ctx, name)
	if err != nil {
		e.t.Fatal(err)
	}
	return c.ID
}

func (e *env) status(id int64) string {
	tx, err := e.st.GetTx(e.ctx, id)
	if err != nil {
		e.t.Fatal(err)
	}
	return tx.Status
}

func (e *env) press(msgID int, data string) {
	e.a.HandleCallback(e.ctx, "cb", msgID, data)
}

func findButton(m Message, text string) (Button, bool) {
	for _, row := range m.Buttons {
		for _, b := range row {
			if strings.Contains(b.Text, text) {
				return b, true
			}
		}
	}
	return Button{}, false
}

func TestQuestionAndButtonTeachMemory(t *testing.T) {
	e := newEnv(t)
	food := e.cat("Groceries")

	// Three confirmations — the fourth payment is categorized automatically.
	for i := range 3 {
		id := e.pay("MAGNUM", int64(1000+i))
		q := e.msg.last()
		if !strings.Contains(q.Text, "what for?") || !strings.Contains(q.Text, "Magnum · Kaspi Gold · ") {
			t.Fatalf("question text: %q", q.Text)
		}
		if e.status(id) != store.StatusAsked {
			t.Fatalf("status %s", e.status(id))
		}
		e.press(q.id, "c:"+itoa(id)+":"+itoa(food))
		if e.status(id) != store.StatusDone || !strings.HasPrefix(e.msg.edits[q.id].Text, "✓") {
			t.Fatalf("after press: %s %q", e.status(id), e.msg.edits[q.id].Text)
		}
		if i == 1 {
			// after two confirmations memory already suggests the first button
			e.now = e.now.Add(time.Hour)
			id := e.pay("MAGNUM", 777)
			if first := e.msg.last().Buttons[0][0]; first.Text != "★ Groceries" {
				t.Fatalf("guess button: %+v", first)
			}
			e.press(e.msg.last().id, "s:"+itoa(id))
		}
		e.now = e.now.Add(time.Hour)
	}

	n := len(e.msg.sent)
	id := e.pay("MAGNUM", 5000)
	if e.status(id) != store.StatusDone || len(e.msg.sent) != n+1 {
		t.Fatalf("auto: status=%s sent=%d", e.status(id), len(e.msg.sent)-n)
	}
	note := e.msg.last()
	if !note.Silent || !strings.Contains(note.Text, "→ Groceries") {
		t.Fatalf("auto note: %+v", note)
	}
	if _, ok := findButton(note.Message, "Fix"); !ok {
		t.Fatal("no fix button")
	}
}

func TestFreeTextReplyWithSplits(t *testing.T) {
	e := newEnv(t)
	id := e.pay("MAGNUM", 450000)
	q := e.msg.last()
	e.a.HandleText(e.ctx, "groceries, plus a gift for mom 2000", q.id)

	if e.status(id) != store.StatusDone {
		t.Fatalf("status %s", e.status(id))
	}
	splits, _ := e.st.Splits(e.ctx, id)
	if len(splits) != 2 || splits[0].AmountMinor != 250000 || splits[1].AmountMinor != 200000 || splits[1].Note != "mom" {
		t.Fatalf("splits %+v", splits)
	}
	if !strings.Contains(e.msg.edits[q.id].Text, "Gifts 2,000\u00a0₸ (mom)") {
		t.Fatalf("edited: %q", e.msg.edits[q.id].Text)
	}
	// a split transaction does not teach merchant memory
	if _, ok, _ := e.st.Rule(e.ctx, "magnum"); ok {
		t.Fatal("split must not teach")
	}
}

func TestBadAnswerReasks(t *testing.T) {
	e := newEnv(t)
	id := e.pay("MAGNUM", 450000)
	e.a.HandleText(e.ctx, "groceries 100, taxi 100", e.msg.last().id)
	if e.status(id) != store.StatusAsked || !strings.Contains(e.msg.last().Text, "do not add up") {
		t.Fatalf("status=%s reply=%q", e.status(id), e.msg.last().Text)
	}
}

func TestOtherButtonThenText(t *testing.T) {
	e := newEnv(t)
	id := e.pay("SHOP", 10000)
	q := e.msg.last()
	other, _ := findButton(q.Message, "Other")
	e.press(q.id, other.Data)
	if len(e.msg.edits[q.id].Buttons) < 5 {
		t.Fatalf("other keyboard: %+v", e.msg.edits[q.id])
	}
	e.a.HandleText(e.ctx, "clothing", 0) // no reply needed — the answer is expected for this transaction
	if e.status(id) != store.StatusDone {
		t.Fatalf("status %s", e.status(id))
	}
}

func TestManualEntryAndUndo(t *testing.T) {
	e := newEnv(t)
	e.a.HandleText(e.ctx, "taxi 1200", 0)
	open, _ := e.st.OpenTxs(e.ctx)
	if len(open) != 0 || !strings.Contains(e.msg.last().Text, "1,200\u00a0₸ · taxi → Taxi") {
		t.Fatalf("open=%d last=%q", len(open), e.msg.last().Text)
	}
	totals, _ := e.st.Totals(e.ctx, e.now.Add(-time.Hour), e.now.Add(time.Hour))
	if len(totals) != 1 || totals[0].Minor != 120000 {
		t.Fatalf("totals %+v", totals)
	}

	e.a.HandleText(e.ctx, "/undo", 0)
	totals, _ = e.st.Totals(e.ctx, e.now.Add(-time.Hour), e.now.Add(time.Hour))
	if len(totals) != 0 || !strings.HasPrefix(e.msg.last().Text, "Undone") {
		t.Fatalf("after undo totals=%+v last=%q", totals, e.msg.last().Text)
	}

	// an unknown word — the entry is saved and the question is asked right away
	e.now = time.Date(2026, 10, 5, 23, 30, 0, 0, almaty) // even during quiet hours: the person is in the chat
	e.a.HandleText(e.ctx, "coffee 900", 0)
	if !strings.Contains(e.msg.last().Text, "what for?") {
		t.Fatalf("manual question: %q", e.msg.last().Text)
	}
}

func TestUndoCategoryReasks(t *testing.T) {
	e := newEnv(t)
	id := e.pay("SHOP", 10000)
	q := e.msg.last()
	e.press(q.id, "c:"+itoa(id)+":"+itoa(e.cat("Clothing")))
	n := len(e.msg.sent)
	e.a.HandleText(e.ctx, "/undo", 0)
	if e.status(id) != store.StatusAsked {
		t.Fatalf("status after undo %s", e.status(id))
	}
	// a new question + the "Undone" reply
	if len(e.msg.sent) != n+2 || !strings.Contains(e.msg.sent[n].Text, "what for?") {
		t.Fatalf("sent after undo: %+v", e.msg.sent[n:])
	}
	if _, ok, _ := e.st.Rule(e.ctx, "shop"); ok {
		t.Fatal("undo must forget the rule")
	}
}

func TestQuietHoursAndBatch(t *testing.T) {
	e := newEnv(t)
	e.now = time.Date(2026, 10, 5, 23, 15, 0, 0, almaty)
	for i := range 3 {
		e.pay("SHOP"+itoa(int64(i)), 1000)
		e.now = e.now.Add(time.Hour)
	}
	if len(e.msg.sent) != 0 {
		t.Fatalf("sent during quiet hours: %d", len(e.msg.sent))
	}
	e.now = time.Date(2026, 10, 6, 8, 0, 0, 0, almaty)
	e.a.Flush(e.ctx)
	if len(e.msg.sent) != 1 || !strings.Contains(e.msg.last().Text, "3 payments piled up") || len(e.msg.last().Buttons) != 3 {
		t.Fatalf("morning batch: %+v", e.msg.sent)
	}
	open, _ := e.st.OpenTxs(e.ctx)
	for _, tx := range open {
		if tx.Status != store.StatusAsked {
			t.Fatalf("tx %d status %s", tx.ID, tx.Status)
		}
	}
	// a button from the batch asks a separate question; answering it closes the transaction
	e.press(e.msg.last().id, e.msg.last().Buttons[0][0].Data)
	q := e.msg.last()
	if !strings.Contains(q.Text, "what for?") {
		t.Fatalf("single question: %q", q.Text)
	}
	e.a.HandleText(e.ctx, "home", q.id)
	if open, _ := e.st.OpenTxs(e.ctx); len(open) != 2 {
		t.Fatalf("open after answer: %d", len(open))
	}
}

func TestBurstGoesToBatch(t *testing.T) {
	e := newEnv(t)
	for i := range 5 {
		e.pay("SHOP"+itoa(int64(i)), 1000)
		e.now = e.now.Add(time.Minute)
	}
	// the first three as separate questions, the rest wait for the window to free up
	if len(e.msg.sent) != 3 {
		t.Fatalf("sent %d", len(e.msg.sent))
	}
	e.now = e.now.Add(10 * time.Minute)
	e.a.Flush(e.ctx)
	if len(e.msg.sent) != 4 || !strings.Contains(e.msg.last().Text, "2 payments piled up") {
		t.Fatalf("after window: %+v", e.msg.sent[3:])
	}
}

func TestReminderOnce(t *testing.T) {
	e := newEnv(t)
	e.pay("SHOP", 1000)
	e.now = e.now.Add(3*time.Hour + time.Minute)
	e.a.Remind(e.ctx)
	e.a.Remind(e.ctx)
	reminders := 0
	for _, s := range e.msg.sent {
		if strings.HasPrefix(s.Text, "Reminder") {
			reminders++
		}
	}
	if reminders != 1 {
		t.Fatalf("reminders %d", reminders)
	}
}

func TestSkipAndUnparsedAmount(t *testing.T) {
	e := newEnv(t)
	id := e.pay("SHOP", 1000)
	e.press(e.msg.last().id, "s:"+itoa(id))
	if e.status(id) != store.StatusIgnored {
		t.Fatalf("status %s", e.status(id))
	}

	bad, _, _ := e.st.InsertTx(e.ctx, store.Tx{ExternalKey: "x", OccurredAt: e.now, Currency: "KZT",
		AmountRaw: "four thousand", MerchantRaw: "SHOP", MerchantNorm: "shop", Source: store.SourceWallet, CreatedAt: e.now})
	e.a.route(e.ctx, bad)
	e.a.Flush(e.ctx)
	q := e.msg.last()
	if !strings.Contains(q.Text, "Could not read the amount \"four thousand\"") {
		t.Fatalf("unparsed question: %q", q.Text)
	}
	e.a.HandleText(e.ctx, "groceries 4000", q.id)
	tx, _ := e.st.GetTx(e.ctx, bad)
	if tx.Status != store.StatusDone || tx.AmountMinor != 400000 {
		t.Fatalf("unparsed closed: %+v", tx)
	}
}

func TestCommands(t *testing.T) {
	e := newEnv(t)
	id := e.pay("SHOP", 450000)
	e.press(e.msg.last().id, "c:"+itoa(id)+":"+itoa(e.cat("Groceries")))
	e.pay("OTHER", 1000)

	e.a.HandleText(e.ctx, "/stat", 0)
	if s := e.msg.last().Text; !strings.Contains(s, "October 2026") || !strings.Contains(s, "Groceries — 4,500\u00a0₸ · 100%") || !strings.Contains(s, "Uncategorized") {
		t.Fatalf("stat: %q", s)
	}
	e.a.HandleText(e.ctx, "/pending", 0)
	if s := e.msg.last(); !strings.Contains(s.Text, "Uncategorized: 1") || len(s.Buttons) != 1 {
		t.Fatalf("pending: %+v", s)
	}
	e.a.HandleText(e.ctx, "/export", 0)
	if len(e.msg.files) != 1 || !strings.HasPrefix(e.msg.files[0], "spend-2026-10.csv") {
		t.Fatalf("export: %v", e.msg.files)
	}
	e.a.HandleText(e.ctx, "/cat add Sports", 0)
	e.a.HandleText(e.ctx, "/cat rename sports = Fitness", 0)
	if _, err := e.st.FindCategory(e.ctx, "fitness"); err != nil {
		t.Fatalf("rename: %v / %q", err, e.msg.last().Text)
	}
	e.a.HandleText(e.ctx, "/cat del Fitness", 0)
	if _, err := e.st.FindCategory(e.ctx, "fitness"); err == nil {
		t.Fatal("archive failed")
	}
	e.a.HandleText(e.ctx, "/stat yesterday", 0)
	if !strings.Contains(e.msg.last().Text, "unknown period") {
		t.Fatalf("bad period: %q", e.msg.last().Text)
	}
}

func TestRecoverRoutesOrphans(t *testing.T) {
	e := newEnv(t)
	e.st.InsertTx(e.ctx, store.Tx{ExternalKey: "o", OccurredAt: e.now, AmountMinor: 1000, Currency: "KZT",
		MerchantRaw: "SHOP", MerchantNorm: "shop", Source: store.SourceWallet, CreatedAt: e.now})
	e.a.Recover(e.ctx)
	e.a.Flush(e.ctx)
	if len(e.msg.sent) != 1 {
		t.Fatalf("recovered sent %d", len(e.msg.sent))
	}
}

func TestPlural(t *testing.T) {
	for n, want := range map[int]string{0: "payments", 1: "payment", 2: "payments", 21: "payments"} {
		if got := plural(n, "payment", "payments"); got != want {
			t.Errorf("%d: %s", n, got)
		}
	}
}
