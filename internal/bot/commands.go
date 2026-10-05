package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"spendbot/internal/report"
	"spendbot/internal/store"
)

const helpText = `I record Apple Wallet payments and ask what the money was for.

Answer with a button or reply with text: "groceries" or "groceries, gift for mom 2000".
For spending outside Wallet just send text: taxi 1200

/pending — uncategorized spending
/stat [week|month|last month|2026-09] — summary by category
/cat — categories: /cat add Name, /cat rename Old = New, /cat del Name
/undo — undo the last action
/export [month|2026-09] — CSV file`

func (a *Agent) command(ctx context.Context, text string) (string, error) {
	cmd, arg, _ := strings.Cut(text, " ")
	cmd, _, _ = strings.Cut(strings.ToLower(cmd), "@") // /stat@spendbot in groups
	arg = strings.TrimSpace(arg)

	switch cmd {
	case "/start", "/help":
		return helpText, nil
	case "/pending":
		return a.cmdPending(ctx)
	case "/stat":
		return a.cmdStat(ctx, arg)
	case "/cat":
		return a.cmdCat(ctx, arg)
	case "/undo":
		return a.cmdUndo(ctx)
	case "/export":
		return a.cmdExport(ctx, arg)
	}
	return "Unknown command. /help", nil
}

func (a *Agent) cmdPending(ctx context.Context) (string, error) {
	open, err := a.st.OpenTxs(ctx)
	if err != nil {
		return "", err
	}
	if len(open) == 0 {
		return "All categorized 👌", nil
	}
	// The list does not mark transactions as asked: it is just a view.
	const limit = 20
	var b strings.Builder
	fmt.Fprintf(&b, "Uncategorized: %d", len(open))
	var buttons [][]Button
	for i, tx := range open {
		if i >= limit {
			fmt.Fprintf(&b, "\n…and %d more", len(open)-limit)
			break
		}
		fmt.Fprintf(&b, "\n%d. %s", i+1, a.describe(tx))
		buttons = append(buttons, []Button{{Text: fmt.Sprintf("%d. %s", i+1, a.short(tx)), Data: "a:" + itoa(tx.ID)}})
	}
	_, err = a.msg.Send(ctx, Message{Text: b.String(), Buttons: buttons})
	return "", err
}

func (a *Agent) cmdStat(ctx context.Context, arg string) (string, error) {
	r, err := report.ParseRange(arg, a.now().In(a.opts.Location), report.Month)
	if err != nil {
		return err.Error(), nil
	}
	return a.summary(ctx, r)
}

func (a *Agent) summary(ctx context.Context, r report.Range) (string, error) {
	totals, err := a.st.Totals(ctx, r.From, r.To)
	if err != nil {
		return "", err
	}
	return report.Summary(r, totals), nil
}

func (a *Agent) cmdCat(ctx context.Context, arg string) (string, error) {
	sub, rest, _ := strings.Cut(arg, " ")
	rest = strings.Join(strings.Fields(rest), " ")
	switch strings.ToLower(sub) {
	case "":
		cats, err := a.st.Categories(ctx)
		if err != nil {
			return "", err
		}
		return "Categories: " + categoryNames(cats) +
			"\n\n/cat add Name — add\n/cat rename Old = New — rename\n/cat del Name — remove from buttons", nil

	case "add":
		if rest == "" || len([]rune(rest)) > 40 {
			return "Give a name: /cat add Sports", nil
		}
		if _, err := a.st.FindCategory(ctx, rest); err == nil {
			return "This category already exists.", nil
		}
		id, err := a.st.AddCategory(ctx, rest)
		if err != nil {
			return "", err
		}
		a.pushUndo("adding category "+rest, func(ctx context.Context) error { return a.st.ArchiveCategory(ctx, id) })
		return "Added category \"" + rest + "\".", nil

	case "rename":
		from, to, ok := strings.Cut(rest, "=")
		from, to = strings.TrimSpace(from), strings.TrimSpace(to)
		if !ok || from == "" || to == "" {
			return "Format: /cat rename Old = New", nil
		}
		c, err := a.st.FindCategory(ctx, from)
		if errors.Is(err, store.ErrNotFound) {
			return "No category \"" + from + "\".", nil
		} else if err != nil {
			return "", err
		}
		if other, err := a.st.FindCategory(ctx, to); err == nil && other.ID != c.ID {
			return "Category \"" + to + "\" already exists.", nil
		}
		if err := a.st.RenameCategory(ctx, c.ID, to); err != nil {
			return "", err
		}
		a.pushUndo("renaming "+to, func(ctx context.Context) error { return a.st.RenameCategory(ctx, c.ID, c.Name) })
		return "Renamed \"" + c.Name + "\" to \"" + to + "\".", nil

	case "del", "archive", "rm":
		c, err := a.st.FindCategory(ctx, rest)
		if errors.Is(err, store.ErrNotFound) {
			return "No category \"" + rest + "\".", nil
		} else if err != nil {
			return "", err
		}
		if err := a.st.ArchiveCategory(ctx, c.ID); err != nil {
			return "", err
		}
		// Merchant rules are deleted on archiving and do not come back with /undo.
		a.pushUndo("removing category "+c.Name, func(ctx context.Context) error {
			_, err := a.st.AddCategory(ctx, c.Name)
			return err
		})
		return "Removed \"" + c.Name + "\" from buttons. Old spending stays in it.", nil
	}
	return "Did not get that. /cat shows the list and help.", nil
}

func (a *Agent) cmdUndo(ctx context.Context) (string, error) {
	e, ok := a.popUndo()
	if !ok {
		return "Nothing to undo.", nil
	}
	if err := e.do(ctx); err != nil {
		return "", err
	}
	a.Flush(ctx)
	return "Undone: " + e.desc + ".", nil
}

func (a *Agent) cmdExport(ctx context.Context, arg string) (string, error) {
	r, err := report.ParseRange(arg, a.now().In(a.opts.Location), report.Month)
	if err != nil {
		return err.Error(), nil
	}
	rows, err := a.st.ExportRows(ctx, r.From, r.To)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "No spending for " + r.Title + ".", nil
	}
	name := fmt.Sprintf("spend-%s.csv", r.From.Format("2006-01-02"))
	if r.From.Day() == 1 && r.To.Equal(r.From.AddDate(0, 1, 0)) {
		name = fmt.Sprintf("spend-%s.csv", r.From.Format("2006-01"))
	}
	return "", a.msg.SendFile(ctx, name, report.CSV(rows, a.opts.Location), "Spending: "+r.Title)
}

// EveningReport sends the day's summary, uncategorized spending included.
// Nothing is sent on a day without spending.
func (a *Agent) EveningReport(ctx context.Context) {
	r := report.Day(a.now().In(a.opts.Location))
	totals, err := a.st.Totals(ctx, r.From, r.To)
	if err != nil {
		a.log.Error("agent: summary", "err", err)
		return
	}
	if len(totals) > 0 {
		a.Notify(ctx, report.Summary(r, totals))
	}
}

// WeeklyReport sends last week's summary.
func (a *Agent) WeeklyReport(ctx context.Context) {
	a.sendSummary(ctx, report.LastWeek(a.now().In(a.opts.Location)))
}

func (a *Agent) sendSummary(ctx context.Context, r report.Range) {
	text, err := a.summary(ctx, r)
	if err != nil {
		a.log.Error("agent: summary", "err", err)
		return
	}
	a.Notify(ctx, text)
}
