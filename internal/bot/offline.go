package bot

import (
	"context"
	"log/slog"
	"time"

	"spendbot/internal/classify"
	"spendbot/internal/store"
)

// Offline handles Wallet payments without Telegram: a known merchant gets its category from
// memory, the rest wait for a question in a batch on the web page, like statement purchases.
type Offline struct {
	st  *store.Store
	cls *classify.Classifier
	log *slog.Logger
}

func NewOffline(st *store.Store, cls *classify.Classifier, log *slog.Logger) *Offline {
	return &Offline{st: st, cls: cls, log: log}
}

func (o *Offline) NewTx(id int64) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := o.route(ctx, id); err != nil {
			o.log.Error("offline: route tx", "tx_id", id, "err", err)
		}
	}()
}

func (o *Offline) route(ctx context.Context, id int64) error {
	tx, err := o.st.GetTx(ctx, id)
	if err != nil || tx.Status != store.StatusPending {
		return err
	}
	if tx.AmountMinor != 0 && tx.MerchantNorm != "" {
		if r, ok, err := o.st.Rule(ctx, tx.MerchantNorm); err != nil {
			return err
		} else if ok {
			_, err := o.st.CloseTx(ctx, id, []store.Split{{CategoryID: r.CategoryID, AmountMinor: tx.AmountMinor}}, false, time.Now())
			return err
		}
	}
	return o.st.SetStatus(ctx, id, store.StatusReview)
}
