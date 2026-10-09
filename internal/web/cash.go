package web

import (
	"fmt"
	"net/http"
	"time"

	"spendbot/internal/analytics"
	"spendbot/internal/store"
)

// Cash that only passes through the card: the operations page, filtered to cash, shows what
// was left out and lets the person turn the rule off or skip a period's withdrawals by hand.

type cashBox struct {
	On       bool  // pass-through cash is left out
	Passed   int64 // left out in the period
	PassedN  int
	Counted  int // withdrawals without a category that are not skipped, passed through or not
	Skipped  int
	From, To string // the period, inclusive, YYYY-MM-DD
}

func (s *Server) cashBox(r *http.Request, rows []store.LedgerRow, p analytics.Period) (*cashBox, error) {
	on, err := s.st.PassThroughCash(r.Context())
	if err != nil {
		return nil, err
	}
	b := &cashBox{On: on, From: p.From.Format("2006-01-02"), To: p.To.AddDate(0, 0, -1).Format("2006-01-02")}
	b.Passed, b.PassedN = analytics.PassedThrough(rows, p)
	b.Counted, b.Skipped = analytics.Withdrawals(rows, p)
	return b, nil
}

// cashAction turns the pass-through rule on or off (action=passthrough, on=1|0) or skips the
// cash withdrawals of a period or counts them again (action=skip|count, from, to inclusive).
func (s *Server) cashAction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	back := safeBack(r.PostFormValue("back"), "/ui/operations")
	var msg string
	switch r.PostFormValue("action") {
	case "passthrough":
		on := r.PostFormValue("on") == "1"
		if err := s.st.SetPassThroughCash(ctx, on); err != nil {
			s.fail(w, err)
			return
		}
		msg = "Cash that only passed through the card is counted as spending again."
		if on {
			msg = "Cash that only passed through the card is not counted as spending."
		}
	case "skip", "count":
		from, err1 := time.ParseInLocation("2006-01-02", r.PostFormValue("from"), s.loc)
		to, err2 := time.ParseInLocation("2006-01-02", r.PostFormValue("to"), s.loc)
		if err1 != nil || err2 != nil || to.Before(from) {
			http.Error(w, "invalid period", http.StatusBadRequest)
			return
		}
		to = to.AddDate(0, 0, 1)
		var n int
		var err error
		if r.PostFormValue("action") == "skip" {
			n, err = s.st.SkipWithdrawals(ctx, from, to)
			msg = fmt.Sprintf("%d cash %s skipped: not spending.", n, plural(n, "withdrawal", "withdrawals"))
		} else {
			n, err = s.st.CountWithdrawals(ctx, from, to)
			msg = fmt.Sprintf("%d cash %s counted as spending again.", n, plural(n, "withdrawal", "withdrawals"))
		}
		if err != nil {
			s.fail(w, err)
			return
		}
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	if s.kick != nil {
		s.kick.Kick()
	}
	http.Redirect(w, r, withMsg(back, msg), http.StatusSeeOther)
}
