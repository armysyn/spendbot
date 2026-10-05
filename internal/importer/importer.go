// Package importer loads statement operations into the database without duplicates on
// re-import and without double-counting purchases that already came from Wallet.
package importer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"spendbot/internal/kaspi"
	"spendbot/internal/merchant"
	"spendbot/internal/statement"
	"spendbot/internal/store"
)

type Result struct {
	Total      int   // operations in the statement
	Duplicates int   // already imported before
	Matched    int   // matched Wallet or manual transactions
	Auto       int   // categorized from merchant memory
	Review     int   // purchases without a category — they go to a question batch
	Info       int   // not spending: incoming money, transfers (refunds count with purchases)
	Verify     error // mismatch with the statement totals, if any
}

func (r Result) String() string {
	s := fmt.Sprintf("%d operations: %d new purchases (%d from merchant memory, %d to questions), %d not spending, "+
		"%d matched Wallet, %d already imported", r.Total, r.Auto+r.Review, r.Auto, r.Review, r.Info, r.Matched, r.Duplicates)
	if r.Verify != nil {
		s += "\n⚠ statement totals do not match: " + r.Verify.Error()
	}
	return s
}

// Import saves the operations. Importing the same statement again changes nothing.
func Import(ctx context.Context, st *store.Store, s statement.Statement, now time.Time) (Result, error) {
	res := Result{Total: len(s.Ops), Verify: s.Verify()}
	if err := st.SaveStatement(ctx, store.StatementInfo{Account: s.Account, From: s.From, To: s.To,
		Summary: s.Summary, Ops: len(s.Ops), ImportedAt: now}); err != nil {
		return res, err
	}
	occ := occurrences(s.Ops)
	// Oldest first: the n-th identical operation of a day is written after the (n-1)-th,
	// so the content-based twin lookup sees exactly the ones already stored.
	for i := len(s.Ops) - 1; i >= 0; i-- {
		op := s.Ops[i]
		key := opKey(s.Account, op, occ[i])
		linked, err := st.OpLinked(ctx, key)
		if err != nil {
			return res, err
		}
		if linked {
			res.Duplicates++
			continue
		}
		// In transactions spending is positive and income negative.
		amount := -op.AmountMinor
		// Statements have no time: noon keeps the day stable across time zone conversions.
		day := time.Date(op.Date.Year(), op.Date.Month(), op.Date.Day(), 12, 0, 0, 0, op.Date.Location())

		// The same operation may have come from another version of the statement under another
		// key: recognize it by content — the n-th identical operation of a day is the n-th stored one.
		twins, err := st.ImportTwins(ctx, day.Add(-12*time.Hour), day.Add(12*time.Hour), amount, op.Details, op.Kind)
		if err != nil {
			return res, err
		}
		if len(twins) >= occ[i] {
			if err := st.LinkOp(ctx, key, twins[occ[i]-1]); err != nil {
				return res, err
			}
			res.Duplicates++
			continue
		}

		if op.IsSpending() && !op.IsRefund() {
			// A Wallet payment has an exact time; in the Kaspi statement it may land on the
			// neighbouring day, hence a ±1 day window.
			from := day.Add(-36 * time.Hour)
			id, ok, err := st.FindUnlinkedTx(ctx, amount, op.Currency, from, day.Add(36*time.Hour))
			if err != nil {
				return res, err
			}
			if ok {
				if err := st.LinkOp(ctx, key, id); err != nil {
					return res, err
				}
				if err := st.SetKind(ctx, id, op.Kind); err != nil {
					return res, err
				}
				res.Matched++
				continue
			}
		}

		tx := store.Tx{
			ExternalKey:  "stmt:" + key,
			OccurredAt:   day,
			AmountMinor:  amount,
			Currency:     op.Currency,
			MerchantRaw:  op.Details,
			MerchantNorm: merchant.Normalize(op.Details),
			Card:         "Kaspi Gold " + s.Account,
			Source:       store.SourceImport,
			Kind:         op.Kind,
			Note:         strings.TrimSpace(strings.TrimLeft(op.Foreign, "+- ")),
			Status:       store.StatusInfo,
			CreatedAt:    now,
		}
		var rule store.Rule
		var hasRule bool
		switch {
		case op.IsSpending():
			tx.Status = store.StatusReview
			if rule, hasRule, err = st.Rule(ctx, tx.MerchantNorm); err != nil {
				return res, err
			}
			if hasRule {
				tx.Status = store.StatusPending // CloseTx moves it to done
			}
		case op.Kind == kaspi.Transfer && op.AmountMinor < 0:
			// A transfer to a person is not spending unless a category was set for them (rent and such).
			if rule, hasRule, err = st.Rule(ctx, tx.MerchantNorm); err != nil {
				return res, err
			}
			if hasRule {
				tx.Status = store.StatusPending
			}
		}
		id, _, err := st.InsertTx(ctx, tx)
		if err != nil {
			return res, err
		}
		if err := st.LinkOp(ctx, key, id); err != nil {
			return res, err
		}
		switch {
		case hasRule:
			// Any rule is a category a person has already confirmed.
			splits := []store.Split{{CategoryID: rule.CategoryID, AmountMinor: amount}}
			if _, err := st.CloseTx(ctx, id, splits, false, now); err != nil {
				return res, err
			}
			res.Auto++
		case tx.Status == store.StatusReview:
			res.Review++
		default:
			res.Info++
		}
	}
	return res, nil
}

// occurrences numbers identical operations of a day (amount, details, kind) starting from
// the oldest. Kaspi lists operations newest first and new ones of a day appear on top, so
// counting from the bottom does not shift when the statement is downloaded again later.
func occurrences(ops []statement.Op) []int {
	out := make([]int, len(ops))
	seen := map[string]int{}
	for i := len(ops) - 1; i >= 0; i-- {
		o := ops[i]
		k := fmt.Sprint(o.Date.Format("2006-01-02"), "|", o.AmountMinor, "|", o.Details, "|", o.Kind)
		seen[k]++
		out[i] = seen[k]
	}
	return out
}

// opKey identifies an operation: card, day, amount, details, kind and the number among
// identical ones — not the position in the statement, which changes between versions.
func opKey(account string, op statement.Op, n int) string {
	h := sha256.New()
	for _, p := range []string{"kaspi:v2", account, op.Date.Format("2006-01-02"), strconv.FormatInt(op.AmountMinor, 10),
		op.Details, op.Kind, strconv.Itoa(n)} {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
