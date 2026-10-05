// Package bot is the conversation with the person: questions about spending, category
// buttons, commands. The transport (Telegram) hides behind the Messenger interface, so the
// logic is tested without a network.
package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"spendbot/internal/classify"
	"spendbot/internal/config"
	"spendbot/internal/merchant"
	"spendbot/internal/money"
	"spendbot/internal/store"
)

type Messenger interface {
	Send(ctx context.Context, m Message) (msgID int, err error)
	Edit(ctx context.Context, msgID int, m Message) error
	AnswerCallback(ctx context.Context, callbackID, text string) error
	SendFile(ctx context.Context, name string, data []byte, caption string) error
}

type Message struct {
	Text    string
	Buttons [][]Button // nil — no buttons
	Silent  bool
}

type Button struct{ Text, Data string }

type Options struct {
	Location       *time.Location
	Quiet          config.QuietHours
	BatchWindow    time.Duration // window for counting recent questions
	BatchThreshold int           // more questions per window go as a batch
	ReminderAfter  time.Duration
}

type Agent struct {
	st   *store.Store
	cls  *classify.Classifier
	msg  Messenger
	log  *slog.Logger
	opts Options
	now  func() time.Time

	newTx   chan int64
	routeMu sync.Mutex // Run and Recover from the scheduler must not route the same transaction at once
	flushMu sync.Mutex // keeps two Flush calls from sending a question twice

	mu       sync.Mutex
	guesses  map[int64]int64 // tx → guess for the first button
	awaiting int64           // transaction awaiting a free-text answer after "Other"
	undo     []undoEntry
}

type undoEntry struct {
	desc string
	do   func(ctx context.Context) error
}

const maxUndo = 20

func New(st *store.Store, cls *classify.Classifier, msg Messenger, opts Options, log *slog.Logger) *Agent {
	return &Agent{st: st, cls: cls, msg: msg, opts: opts, log: log, now: time.Now,
		newTx: make(chan int64, 256), guesses: map[int64]int64{}}
}

// NewTx is called by ingest after a payment is saved. It does not block: if the queue is
// full, the transaction is picked up by the next Recover pass.
func (a *Agent) NewTx(id int64) {
	select {
	case a.newTx <- id:
	default:
		a.log.Warn("agent: queue full, tx will be recovered later", "tx_id", id)
	}
}

// Run processes new transactions one at a time until ctx is cancelled.
func (a *Agent) Run(ctx context.Context) {
	a.Recover(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-a.newTx:
			if err := a.route(ctx, id); err != nil {
				a.log.Error("agent: route tx", "tx_id", id, "err", err)
			}
			a.Flush(ctx)
		}
	}
}

// Recover routes transactions that were saved but never classified.
func (a *Agent) Recover(ctx context.Context) {
	txs, err := a.st.Unrouted(ctx)
	if err != nil {
		a.log.Error("agent: recover", "err", err)
		return
	}
	for _, t := range txs {
		if err := a.route(ctx, t.ID); err != nil {
			a.log.Error("agent: route tx", "tx_id", t.ID, "err", err)
		}
	}
}

// route is the classification chain: memory → LLM → question queue.
func (a *Agent) route(ctx context.Context, id int64) error {
	a.routeMu.Lock()
	defer a.routeMu.Unlock()
	tx, err := a.st.GetTx(ctx, id)
	if err != nil {
		return err
	}
	if tx.Status != store.StatusPending {
		return nil
	}
	if tx.AmountMinor != 0 {
		cats, err := a.st.Categories(ctx)
		if err != nil {
			return err
		}
		d, err := a.cls.Decide(ctx, tx, cats)
		if err != nil {
			return err
		}
		if d.Auto != 0 {
			a.log.Info("agent: auto category", "tx_id", id, "by", d.By)
			return a.autoClose(ctx, tx, d.Auto)
		}
		if d.Guess != 0 {
			a.log.Info("agent: guess", "tx_id", id, "by", d.By)
			a.mu.Lock()
			a.guesses[id] = d.Guess
			a.mu.Unlock()
		}
	}
	return a.st.EnqueueQuestion(ctx, id)
}

// autoClose sets the category from memory and sends a silent notification with a "Fix" button.
func (a *Agent) autoClose(ctx context.Context, tx store.Tx, catID int64) error {
	splits := []store.Split{{CategoryID: catID, AmountMinor: tx.AmountMinor}}
	closed, err := a.st.CloseTx(ctx, tx.ID, splits, false, a.now())
	if err != nil {
		return err
	}
	a.pushUndo("category for "+a.short(tx), func(ctx context.Context) error { return a.undoClose(ctx, closed) })
	text, err := a.doneText(ctx, tx, splits)
	if err != nil {
		return err
	}
	_, err = a.msg.Send(ctx, Message{Text: text, Silent: true, Buttons: fixButtons(tx.ID)})
	return err
}

// undoClose reverts a close. If the transaction was open before, the question is asked again.
func (a *Agent) undoClose(ctx context.Context, c store.Closed) error {
	if err := a.st.UndoClose(ctx, c, a.now()); err != nil {
		return err
	}
	if c.PrevStatus == store.StatusDone || c.PrevStatus == store.StatusIgnored {
		return nil
	}
	return a.reopen(ctx, c.TxID)
}

// reopen puts the transaction back into the question queue.
func (a *Agent) reopen(ctx context.Context, txID int64) error {
	if err := a.st.SetStatus(ctx, txID, store.StatusPending); err != nil {
		return err
	}
	if err := a.st.DeleteQuestion(ctx, txID); err != nil {
		return err
	}
	return a.st.EnqueueQuestion(ctx, txID)
}

// Flush sends pending questions, respecting quiet hours and batching.
func (a *Agent) Flush(ctx context.Context) {
	a.flushMu.Lock()
	defer a.flushMu.Unlock()
	if err := a.flush(ctx); err != nil {
		a.log.Error("agent: flush", "err", err)
	}
}

func (a *Agent) flush(ctx context.Context) error {
	now := a.now()
	if a.opts.Quiet.Contains(now.In(a.opts.Location)) {
		return nil
	}
	unasked, err := a.st.Unasked(ctx)
	if err != nil || len(unasked) == 0 {
		return err
	}
	recent, err := a.st.CountAskedSince(ctx, now.Add(-a.opts.BatchWindow))
	if err != nil {
		return err
	}
	switch {
	case recent == 0 && len(unasked) > 1:
		// Piled up after quiet hours or a burst of payments — one message with a list.
		return a.sendList(ctx, fmt.Sprintf("%d payments piled up. What were they for?", len(unasked)), unasked, false)
	case recent+len(unasked) <= a.opts.BatchThreshold:
		for _, tx := range unasked {
			if err := a.ask(ctx, tx); err != nil {
				return err
			}
		}
		return nil
	default:
		// Many questions in the window already: wait until it frees up and send a batch.
		return nil
	}
}

// Remind reminds once about unanswered questions.
func (a *Agent) Remind(ctx context.Context) {
	now := a.now()
	if a.opts.Quiet.Contains(now.In(a.opts.Location)) {
		return
	}
	due, err := a.st.DueReminders(ctx, now.Add(-a.opts.ReminderAfter))
	if err != nil || len(due) == 0 {
		if err != nil {
			a.log.Error("agent: reminders", "err", err)
		}
		return
	}
	if err := a.sendList(ctx, fmt.Sprintf("Reminder: %d %s without a category.", len(due), plural(len(due), "payment", "payments")), due, true); err != nil {
		a.log.Error("agent: send reminder", "err", err)
	}
}

// sendList sends one message listing transactions with a button for each.
func (a *Agent) sendList(ctx context.Context, title string, txs []store.Tx, reminder bool) error {
	const limit = 15
	var b strings.Builder
	b.WriteString(title)
	var buttons [][]Button
	ids := make([]int64, 0, len(txs))
	for i, tx := range txs {
		ids = append(ids, tx.ID)
		if i >= limit {
			continue
		}
		fmt.Fprintf(&b, "\n%d. %s", i+1, a.describe(tx))
		buttons = append(buttons, []Button{{Text: fmt.Sprintf("%d. %s", i+1, a.short(tx)), Data: "a:" + itoa(tx.ID)}})
	}
	if len(txs) > limit {
		fmt.Fprintf(&b, "\n…and %d more — /pending", len(txs)-limit)
	}
	msgID, err := a.msg.Send(ctx, Message{Text: b.String(), Buttons: buttons})
	if err != nil {
		return err
	}
	if reminder {
		return a.st.MarkReminded(ctx, a.now(), ids...)
	}
	return a.st.MarkAsked(ctx, msgID, a.now(), ids...)
}

// ask asks about one transaction.
func (a *Agent) ask(ctx context.Context, tx store.Tx) error {
	m, err := a.question(ctx, tx)
	if err != nil {
		return err
	}
	msgID, err := a.msg.Send(ctx, m)
	if err != nil {
		return err
	}
	return a.st.MarkAsked(ctx, msgID, a.now(), tx.ID)
}

// question builds the text and buttons: the guess first, then 3 frequent categories, "Other" and "Skip".
func (a *Agent) question(ctx context.Context, tx store.Tx) (Message, error) {
	if tx.AmountMinor == 0 {
		return Message{
			Text: fmt.Sprintf("Could not read the amount %q · %s\nReply to this message with a category and an amount, e.g.: groceries 4500",
				tx.AmountRaw, merchant.Display(tx.MerchantRaw)),
			Buttons: [][]Button{{{Text: "Skip", Data: "s:" + itoa(tx.ID)}}},
		}, nil
	}
	a.mu.Lock()
	guess := a.guesses[tx.ID]
	a.mu.Unlock()

	var picks []store.Category
	seen := map[int64]bool{}
	if guess != 0 {
		if c, err := a.st.Category(ctx, guess); err == nil && !c.Archived {
			picks = append(picks, c)
			seen[c.ID] = true
		}
	}
	top, err := a.st.TopCategories(ctx, 6, a.now().AddDate(0, 0, -90))
	if err != nil {
		return Message{}, err
	}
	if len(top) < 3 {
		all, err := a.st.Categories(ctx)
		if err != nil {
			return Message{}, err
		}
		top = append(top, all...)
	}
	for _, c := range top {
		if len(picks) >= 4 || (guess == 0 && len(picks) >= 3) {
			break
		}
		if !seen[c.ID] {
			picks = append(picks, c)
			seen[c.ID] = true
		}
	}
	id := itoa(tx.ID)
	var buttons [][]Button
	for i, c := range picks {
		text := c.Name
		if i == 0 && guess != 0 {
			text = "★ " + text
		}
		b := Button{Text: text, Data: "c:" + id + ":" + itoa(c.ID)}
		if i%2 == 0 {
			buttons = append(buttons, []Button{b})
		} else {
			buttons[len(buttons)-1] = append(buttons[len(buttons)-1], b)
		}
	}
	buttons = append(buttons, []Button{{Text: "Other", Data: "o:" + id}, {Text: "Skip", Data: "s:" + id}})
	return Message{Text: a.describe(tx) + " — what for?", Buttons: buttons}, nil
}

// HandleCallback handles button presses. Data format: "action:tx[:category]".
func (a *Agent) HandleCallback(ctx context.Context, callbackID string, msgID int, data string) {
	answer, err := a.callback(ctx, msgID, data)
	if err != nil {
		a.log.Error("agent: callback", "data", data, "err", err)
		answer = "Something went wrong, try again"
		if errors.Is(err, store.ErrNotFound) {
			answer = "Transaction not found"
		}
	}
	if err := a.msg.AnswerCallback(ctx, callbackID, answer); err != nil {
		a.log.Warn("agent: answer callback", "err", err)
	}
}

func (a *Agent) callback(ctx context.Context, msgID int, data string) (string, error) {
	parts := strings.Split(data, ":")
	if len(parts) < 2 {
		return "", fmt.Errorf("unknown button %q", data)
	}
	txID, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil {
		return "", err
	}
	tx, err := a.st.GetTx(ctx, txID)
	if err != nil {
		return "", err
	}

	switch parts[0] {
	case "c": // a category was chosen
		if len(parts) != 3 {
			return "", fmt.Errorf("invalid button %q", data)
		}
		catID, err := strconv.ParseInt(parts[2], 10, 64)
		if err != nil {
			return "", err
		}
		if tx.AmountMinor == 0 {
			return "Send the amount first", nil
		}
		splits := []store.Split{{CategoryID: catID, AmountMinor: tx.AmountMinor}}
		if err := a.closeConfirmed(ctx, tx, splits, msgID); err != nil {
			return "", err
		}
		return "Saved", nil

	case "o": // "Other": all categories and waiting for a free-text answer
		a.setAwaiting(tx.ID)
		cats, err := a.st.Categories(ctx)
		if err != nil {
			return "", err
		}
		var buttons [][]Button
		for i, c := range cats {
			b := Button{Text: c.Name, Data: "c:" + itoa(tx.ID) + ":" + itoa(c.ID)}
			if i%2 == 0 {
				buttons = append(buttons, []Button{b})
			} else {
				buttons[len(buttons)-1] = append(buttons[len(buttons)-1], b)
			}
		}
		buttons = append(buttons, []Button{{Text: "← Back", Data: "b:" + itoa(tx.ID)}})
		text := a.describe(tx) + "\nPick a category or reply with text, e.g.: groceries, gift for mom 2000"
		return "", a.msg.Edit(ctx, msgID, Message{Text: text, Buttons: buttons})

	case "b": // back to the short list
		m, err := a.question(ctx, tx)
		if err != nil {
			return "", err
		}
		return "", a.msg.Edit(ctx, msgID, m)

	case "s": // skip
		prev := tx.Status
		if err := a.st.SetStatus(ctx, tx.ID, store.StatusIgnored); err != nil {
			return "", err
		}
		if err := a.st.DeleteQuestion(ctx, tx.ID); err != nil {
			return "", err
		}
		a.clearAwaiting(tx.ID)
		a.pushUndo("skipping "+a.short(tx), func(ctx context.Context) error {
			if prev == store.StatusDone {
				return a.st.SetStatus(ctx, tx.ID, store.StatusDone)
			}
			return a.reopen(ctx, tx.ID)
		})
		return "Skipped", a.msg.Edit(ctx, msgID, Message{Text: a.describe(tx) + "\n— skipped, will not appear in summaries"})

	case "f": // "Fix": show the buttons again
		m, err := a.question(ctx, tx)
		if err != nil {
			return "", err
		}
		return "", a.msg.Edit(ctx, msgID, m)

	case "a": // ask about one transaction from a list
		if tx.Status != store.StatusPending && tx.Status != store.StatusAsked {
			return "Already categorized", nil
		}
		return "", a.ask(ctx, tx)
	}
	return "", fmt.Errorf("unknown button %q", data)
}

// closeConfirmed saves an answer confirmed by a person, teaches merchant memory and
// replaces the question with the result.
func (a *Agent) closeConfirmed(ctx context.Context, tx store.Tx, splits []store.Split, msgID int) error {
	if msgID == 0 {
		var err error
		if msgID, err = a.st.QuestionMessage(ctx, tx.ID); err != nil {
			return err
		}
	}
	closed, err := a.st.CloseTx(ctx, tx.ID, splits, true, a.now())
	if err != nil {
		return err
	}
	a.clearAwaiting(tx.ID)
	a.mu.Lock()
	delete(a.guesses, tx.ID)
	a.mu.Unlock()
	a.pushUndo("category for "+a.short(tx), func(ctx context.Context) error { return a.undoClose(ctx, closed) })
	if tx.AmountMinor == 0 {
		for _, s := range splits {
			tx.AmountMinor += s.AmountMinor
		}
	}
	text, err := a.doneText(ctx, tx, splits)
	if err != nil {
		return err
	}
	done := Message{Text: text, Buttons: fixButtons(tx.ID)}
	if msgID != 0 {
		if err := a.msg.Edit(ctx, msgID, done); err == nil {
			return nil
		}
	}
	_, err = a.msg.Send(ctx, done)
	return err
}

// HandleText handles commands, answers to questions and manual entries.
func (a *Agent) HandleText(ctx context.Context, text string, replyTo int) {
	text = strings.TrimSpace(text)
	var reply string
	var err error
	if strings.HasPrefix(text, "/") {
		reply, err = a.command(ctx, text)
	} else {
		reply, err = a.freeText(ctx, text, replyTo)
	}
	if err != nil {
		a.log.Error("agent: text", "err", err)
		reply = "Something went wrong, try again."
	}
	if reply == "" {
		return
	}
	if _, err := a.msg.Send(ctx, Message{Text: reply}); err != nil {
		a.log.Error("agent: send reply", "err", err)
	}
}

func (a *Agent) freeText(ctx context.Context, text string, replyTo int) (string, error) {
	// 1. A reply to the question message.
	if replyTo != 0 {
		id, err := a.st.TxByMessage(ctx, replyTo)
		if err == nil {
			return a.answer(ctx, id, text)
		}
		if !errors.Is(err, store.ErrNotFound) {
			return "", err
		}
	}
	// 2. After "Other", an answer for a specific transaction is expected.
	a.mu.Lock()
	awaiting := a.awaiting
	a.mu.Unlock()
	if awaiting != 0 {
		return a.answer(ctx, awaiting, text)
	}
	// 3. "taxi 1200" — a manual entry.
	items, err := classify.ParseItems(text)
	if err == nil && len(items) == 1 && items[0].HasAmount && len(items[0].Words) > 0 {
		return a.manual(ctx, items[0])
	}
	// 4. An answer without a reply when exactly one question is open.
	open, err := a.st.OpenTxs(ctx)
	if err != nil {
		return "", err
	}
	var asked []store.Tx
	for _, t := range open {
		if t.Status == store.StatusAsked {
			asked = append(asked, t)
		}
	}
	if len(asked) == 1 {
		return a.answer(ctx, asked[0].ID, text)
	}
	if len(asked) > 1 {
		return "Not sure which payment this is for. Reply to the question message or use a button in /pending.\nTo add a manual entry: taxi 1200", nil
	}
	return "To add a manual entry, send: taxi 1200\nCommands: /pending, /stat, /cat, /undo, /export, /help", nil
}

// answer parses a free-text answer about a transaction and closes it.
func (a *Agent) answer(ctx context.Context, txID int64, text string) (string, error) {
	tx, err := a.st.GetTx(ctx, txID)
	if err != nil {
		return "", err
	}
	cats, err := a.st.Categories(ctx)
	if err != nil {
		return "", err
	}
	splits, err := a.cls.ParseAnswer(ctx, tx, text, cats)
	if err != nil {
		return fmt.Sprintf("Did not get that: %s.\nList categories separated by commas, with amounts for all but one: groceries, gifts 2000\nCategories: %s",
			err, categoryNames(cats)), nil
	}
	if err := a.closeConfirmed(ctx, tx, splits, 0); err != nil {
		if errors.Is(err, store.ErrSplitSum) {
			return "The amounts do not add up to the payment, try again.", nil
		}
		return "", err
	}
	return "", nil
}

// manual saves an entry typed as text: "taxi 1200".
func (a *Agent) manual(ctx context.Context, it classify.Item) (string, error) {
	note := strings.Join(it.Words, " ")
	now := a.now()
	tx := store.Tx{
		OccurredAt: now, AmountMinor: it.Amount, Currency: it.Currency, AmountRaw: "",
		MerchantRaw: note, MerchantNorm: merchant.Normalize(note), Source: store.SourceManual,
		Status: store.StatusPending, CreatedAt: now,
	}
	id, _, err := a.st.InsertTx(ctx, tx)
	if err != nil {
		return "", err
	}
	tx.ID = id
	a.pushUndo("manual entry "+a.short(tx), func(ctx context.Context) error { return a.st.DeleteTx(ctx, id) })

	cats, err := a.st.Categories(ctx)
	if err != nil {
		return "", err
	}
	// The category is in the text ("taxi 1200") — the person named it, so it is confirmed
	// and merchant memory learns. /undo deletes the whole entry.
	if c, rest, ok := classify.MatchCategory(it.Words, cats); ok {
		splits := []store.Split{{CategoryID: c.ID, AmountMinor: tx.AmountMinor, Note: strings.Join(rest, " ")}}
		return a.closeManual(ctx, tx, splits, true)
	}
	d, err := a.cls.Decide(ctx, tx, cats)
	if err != nil {
		return "", err
	}
	if d.Auto != 0 {
		return a.closeManual(ctx, tx, []store.Split{{CategoryID: d.Auto, AmountMinor: tx.AmountMinor}}, false)
	}
	if d.Guess != 0 {
		a.mu.Lock()
		a.guesses[id] = d.Guess
		a.mu.Unlock()
	}
	// The person is in the chat right now — ask immediately, ignoring quiet hours and batching.
	return "", a.ask(ctx, tx)
}

func (a *Agent) closeManual(ctx context.Context, tx store.Tx, splits []store.Split, learn bool) (string, error) {
	if _, err := a.st.CloseTx(ctx, tx.ID, splits, learn, a.now()); err != nil {
		return "", err
	}
	text, err := a.doneText(ctx, tx, splits)
	if err != nil {
		return "", err
	}
	_, err = a.msg.Send(ctx, Message{Text: text, Buttons: fixButtons(tx.ID)})
	return "", err
}

func (a *Agent) pushUndo(desc string, do func(ctx context.Context) error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.undo = append(a.undo, undoEntry{desc: desc, do: do})
	if len(a.undo) > maxUndo {
		a.undo = a.undo[len(a.undo)-maxUndo:]
	}
}

func (a *Agent) popUndo() (undoEntry, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.undo) == 0 {
		return undoEntry{}, false
	}
	e := a.undo[len(a.undo)-1]
	a.undo = a.undo[:len(a.undo)-1]
	return e, true
}

func (a *Agent) setAwaiting(id int64) {
	a.mu.Lock()
	a.awaiting = id
	a.mu.Unlock()
}

func (a *Agent) clearAwaiting(id int64) {
	a.mu.Lock()
	if a.awaiting == id {
		a.awaiting = 0
	}
	a.mu.Unlock()
}

// Notify sends a service message (scheduler alerts).
func (a *Agent) Notify(ctx context.Context, text string) {
	if _, err := a.msg.Send(ctx, Message{Text: text}); err != nil {
		a.log.Error("agent: notify", "err", err)
	}
}

// describe: "4,500 ₸ · Magnum Cash&Carry · Kaspi Gold · 14:32".
func (a *Agent) describe(tx store.Tx) string {
	parts := []string{a.amount(tx), merchant.Display(tx.MerchantRaw)}
	if tx.Card != "" {
		parts = append(parts, tx.Card)
	}
	t := tx.OccurredAt.In(a.opts.Location)
	if now := a.now().In(a.opts.Location); t.YearDay() == now.YearDay() && t.Year() == now.Year() {
		parts = append(parts, t.Format("15:04"))
	} else {
		parts = append(parts, t.Format("2 Jan 15:04"))
	}
	return strings.Join(parts, " · ")
}

// short: "4,500 ₸ Magnum" — for buttons and /undo.
func (a *Agent) short(tx store.Tx) string {
	m := merchant.Display(tx.MerchantRaw)
	if r := []rune(m); len(r) > 20 {
		m = string(r[:20]) + "…"
	}
	return a.amount(tx) + " " + m
}

func (a *Agent) amount(tx store.Tx) string {
	if tx.AmountMinor == 0 {
		return "«" + tx.AmountRaw + "»"
	}
	return money.Format(tx.AmountMinor, tx.Currency)
}

// doneText: "✓ 4,500 ₸ · Magnum → Groceries 2,500 ₸, Gifts 2,000 ₸ (for mom)".
func (a *Agent) doneText(ctx context.Context, tx store.Tx, splits []store.Split) (string, error) {
	head := "✓ " + a.amount(tx) + " · " + merchant.Display(tx.MerchantRaw) + " → "
	var parts []string
	for _, s := range splits {
		c, err := a.st.Category(ctx, s.CategoryID)
		if err != nil {
			return "", err
		}
		p := c.Name
		if len(splits) > 1 {
			p += " " + money.Format(s.AmountMinor, tx.Currency)
		}
		if s.Note != "" {
			p += " (" + s.Note + ")"
		}
		parts = append(parts, p)
	}
	return head + strings.Join(parts, ", "), nil
}

func fixButtons(txID int64) [][]Button {
	return [][]Button{{{Text: "Fix", Data: "f:" + itoa(txID)}}}
}

func categoryNames(cats []store.Category) string {
	names := make([]string, len(cats))
	for i, c := range cats {
		names[i] = c.Name
	}
	return strings.Join(names, ", ")
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
