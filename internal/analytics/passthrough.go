package analytics

import (
	"sort"
	"time"

	"spendbot/internal/kaspi"
	"spendbot/internal/store"
)

// PassWindow is how long money put on the card may stay there and still count as passing
// through when it leaves as cash.
const PassWindow = 7 * 24 * time.Hour

// MarkPassThrough marks cash that only passed through the card: money put on it that is not
// income — the person's own money from another bank, a cash deposit, money from a person —
// and taken out as cash within PassWindow. Such a withdrawal is not spending: whoever cashes
// out millions for someone else or moves money between banks through an ATM did not spend it,
// and counting it would inflate every number built on spending. The top-ups that paid for it
// carry the part of them that left, so it is not counted as money from people either.
//
// A withdrawal counts only when the money put on the card before it covers it whole; one with
// a category the person gave stays spending. sources are top-up senders counted as income:
// their money is not passing through. Rows are marked in place and returned.
func MarkPassThrough(rows []store.LedgerRow, sources map[string]bool) []store.LedgerRow {
	order := make([]int, 0, len(rows))
	for i, r := range rows {
		rows[i].PassThrough = 0
		if r.Currency == "KZT" && r.Status != store.StatusIgnored && (isFunding(r, sources) || isPassCash(r)) {
			order = append(order, i)
		}
	}
	sort.SliceStable(order, func(a, b int) bool {
		ra, rb := rows[order[a]], rows[order[b]]
		if !ra.At.Equal(rb.At) {
			return ra.At.Before(rb.At)
		}
		// statements give dates without times: money in on a day comes before cash out of it
		if (ra.Amount < 0) != (rb.Amount < 0) {
			return ra.Amount < 0
		}
		return ra.TxID < rb.TxID
	})
	type fund struct {
		i    int
		left int64
	}
	var pool []fund // oldest first
	for _, i := range order {
		r := rows[i]
		if r.Amount < 0 {
			pool = append(pool, fund{i: i, left: -r.Amount})
			continue
		}
		since := r.At.Add(-PassWindow)
		for len(pool) > 0 && rows[pool[0].i].At.Before(since) {
			pool = pool[1:]
		}
		var have int64
		for _, f := range pool {
			have += f.left
		}
		if have < r.Amount {
			continue
		}
		rows[i].PassThrough = r.Amount
		need := r.Amount
		for need > 0 {
			f := &pool[0]
			take := min(f.left, need)
			f.left -= take
			rows[f.i].PassThrough += take
			need -= take
			if f.left == 0 {
				pool = pool[1:]
			}
		}
	}
	return rows
}

// isFunding reports money put on the card that is not income and may leave again as cash.
func isFunding(r store.LedgerRow, sources map[string]bool) bool {
	return r.Kind == kaspi.TopUp && r.Amount < 0 && !IsSalary(r) && !sources[r.Merchant] &&
		(IsOwnMoney(r.Merchant) || IsPerson(r.Merchant))
}

// isPassCash reports a cash withdrawal that may have only passed through.
func isPassCash(r store.LedgerRow) bool {
	return r.Kind == cash && r.Amount > 0 && len(r.Categories) == 0
}

// PassedThrough sums the cash withdrawals in p that passed through the card.
func PassedThrough(rows []store.LedgerRow, p Period) (sum int64, n int) {
	for _, r := range rows {
		if r.PassThrough > 0 && r.Kind == cash && p.contains(r.At) {
			sum += r.Amount
			n++
		}
	}
	return sum, n
}

// Withdrawals counts the cash withdrawals in p without a category: those that count and
// those the person skipped — what store.SkipWithdrawals and CountWithdrawals would change.
func Withdrawals(rows []store.LedgerRow, p Period) (counted, skipped int) {
	for _, r := range rows {
		if r.Kind != cash || r.Amount <= 0 || len(r.Categories) > 0 || !p.contains(r.At) {
			continue
		}
		if r.Status == store.StatusIgnored {
			skipped++
		} else {
			counted++
		}
	}
	return counted, skipped
}
