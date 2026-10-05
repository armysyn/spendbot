package analysis

import (
	"context"

	"spendbot/internal/store"
)

// PrepareQuestions opens questions about merchants with uncategorized purchases. The local
// model guesses the category up front, so the batch has it preselected and only needs a
// confirmation. If a rule for the merchant appeared meanwhile, purchases are categorized without asking.
func (a *Analyzer) PrepareQuestions(ctx context.Context) (int, error) {
	merchants, err := a.st.ReviewMerchants(ctx)
	if err != nil {
		return 0, err
	}
	cats, err := a.st.Categories(ctx)
	if err != nil {
		return 0, err
	}
	created := 0
	for _, m := range merchants {
		if ctx.Err() != nil {
			return created, ctx.Err()
		}
		// The average purchase is a typical example for the model.
		sample := store.Tx{
			MerchantRaw: m.Display, MerchantNorm: m.Norm, Currency: m.Currency,
			AmountMinor: m.TotalMinor / int64(max(m.Count, 1)), OccurredAt: m.Last,
		}
		d, err := a.cls.Decide(ctx, sample, cats)
		if err != nil {
			return created, err
		}
		if d.Auto != 0 || d.By == "rule" {
			cat := d.Auto
			if cat == 0 {
				cat = d.Guess
			}
			n, err := a.st.CloseReviewMerchant(ctx, m.Norm, cat)
			if err != nil {
				return created, err
			}
			a.log.Info("analysis: merchant closed by rule", "txs", n)
			continue
		}
		if err := a.st.AddMerchantQuestion(ctx, m.Norm, m.Display, d.Guess, a.now()); err != nil {
			return created, err
		}
		created++
	}
	return created, nil
}

// MakeBatches groups open questions into batches: a full batch right away, a partial one when
// the oldest question has waited longer than BatchMaxAge. Unanswered batches pile up.
func (a *Analyzer) MakeBatches(ctx context.Context) ([]int64, error) {
	var ids []int64
	for {
		n, oldest, err := a.st.UnbatchedCount(ctx)
		if err != nil || n == 0 {
			return ids, err
		}
		if n < a.opts.BatchSize && a.now().Sub(oldest) < a.opts.BatchMaxAge {
			return ids, nil
		}
		id, err := a.st.CreateBatch(ctx, a.opts.BatchSize, a.now())
		if err != nil || id == 0 {
			return ids, err
		}
		ids = append(ids, id)
	}
}
