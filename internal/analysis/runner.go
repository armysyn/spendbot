package analysis

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"spendbot/internal/classify"
	"spendbot/internal/clickhouse"
	"spendbot/internal/config"
	"spendbot/internal/llm"
	"spendbot/internal/money"
	"spendbot/internal/store"
)

type Notifier interface {
	Notify(ctx context.Context, text string)
}

type Options struct {
	Location    *time.Location
	Quiet       config.QuietHours
	PublicURL   string        // web page address for links in Telegram
	BatchSize   int           // questions per batch
	BatchMaxAge time.Duration // a partial batch is made when a question waits longer
	Interval    time.Duration // how often to wake up
}

type Analyzer struct {
	st     *store.Store
	ch     *clickhouse.Client   // nil — no ClickHouse: everything is computed from SQLite
	cls    *classify.Classifier // with the local model
	llm    llm.Provider         // may be nil
	notify Notifier             // may be nil (no Telegram)
	opts   Options
	log    *slog.Logger
	now    func() time.Time

	kick     chan struct{}
	syncKick chan struct{}
	syncMu   sync.Mutex // Sync from the sync loop and from Cycle must not run at the same time
	version  string     // DataVersion the insights were last computed for
}

func New(st *store.Store, ch *clickhouse.Client, cls *classify.Classifier, provider llm.Provider,
	n Notifier, opts Options, log *slog.Logger) *Analyzer {
	return &Analyzer{st: st, ch: ch, cls: cls, llm: provider, notify: n, opts: opts, log: log,
		now: time.Now, kick: make(chan struct{}, 1), syncKick: make(chan struct{}, 1)}
}

// ledger returns operations for insights: from ClickHouse when configured and reachable,
// otherwise from SQLite. The computation is the same, the source does not change the numbers.
func (a *Analyzer) ledger(ctx context.Context) ([]store.LedgerRow, error) {
	if a.ch != nil {
		rows, err := LedgerFromClickHouse(ctx, a.ch, a.opts.Location)
		if err == nil {
			return rows, nil
		}
		a.log.Warn("analysis: clickhouse unavailable, using sqlite", "err", err)
	}
	return a.st.Ledger(ctx, a.opts.Location)
}

// Kick runs an analysis cycle now (right after a statement import, for example).
func (a *Analyzer) Kick() {
	for _, ch := range []chan struct{}{a.kick, a.syncKick} {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (a *Analyzer) Run(ctx context.Context) {
	if a.ch != nil {
		if err := Migrate(ctx, a.ch, a.opts.Location.String()); err != nil {
			a.log.Error("analysis: clickhouse migrate", "err", err)
		}
		// Sync runs in its own fast loop: model questions about hundreds of merchants take
		// minutes, and analytics should not wait for them.
		go a.syncLoop(ctx)
	}
	t := time.NewTicker(a.opts.Interval)
	defer t.Stop()
	for {
		a.Cycle(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.kick:
		}
	}
}

const syncEvery = 30 * time.Second

func (a *Analyzer) syncLoop(ctx context.Context) {
	t := time.NewTicker(syncEvery)
	defer t.Stop()
	for {
		a.syncOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-a.syncKick:
			// Sync takes rows older than two seconds — give fresh data time to settle.
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
		}
	}
}

func (a *Analyzer) syncOnce(ctx context.Context) {
	if a.ch == nil {
		return
	}
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	if _, err := Sync(ctx, a.st, a.ch, a.now()); err != nil {
		a.log.Error("analysis: sync", "err", err)
	}
}

// Cycle runs questions, batches, insights and notifications. Insights are recomputed only
// when the data changed since last time. A failing step does not stop the others.
func (a *Analyzer) Cycle(ctx context.Context) {
	// Stopping the program mid-cycle is not an error worth logging.
	if n, err := a.PrepareQuestions(ctx); err != nil && ctx.Err() == nil {
		a.log.Error("analysis: questions", "err", err)
	} else if n > 0 {
		a.log.Info("analysis: new merchant questions", "count", n)
	}
	if ctx.Err() != nil {
		return
	}
	if _, err := a.MakeBatches(ctx); err != nil {
		a.log.Error("analysis: batches", "err", err)
	}
	// Fresh data before insights (answers may have changed categories).
	a.syncOnce(ctx)
	version, err := a.st.DataVersion(ctx)
	if err != nil {
		a.log.Error("analysis: data version", "err", err)
	}
	if err == nil && version != a.version {
		if n, err := a.Insights(ctx); err != nil {
			a.log.Error("analysis: insights", "err", err)
		} else {
			a.version = version
			if n > 0 {
				a.log.Info("analysis: new insights", "count", n)
			}
		}
		if _, err := a.Tips(ctx); err != nil {
			a.log.Error("analysis: tips", "err", err)
		}
	}
	a.notifyNew(ctx)
}

// notifyNew sends Telegram links to new batches and insights; during quiet hours it waits for the morning.
func (a *Analyzer) notifyNew(ctx context.Context) {
	if a.notify == nil || a.opts.Quiet.Contains(a.now().In(a.opts.Location)) {
		return
	}
	batches, err := a.st.Batches(ctx, false, 50)
	if err != nil {
		a.log.Error("analysis: list batches", "err", err)
		return
	}
	var fresh []store.Batch
	for _, b := range batches {
		if b.NotifiedAt == nil {
			fresh = append(fresh, b)
		}
	}
	if len(fresh) > 0 {
		var sb strings.Builder
		fmt.Fprintf(&sb, "❓ Merchant questions: %d new %s, %d open in total.", len(fresh),
			plural(len(fresh), "batch", "batches"), len(batches))
		for i, b := range fresh {
			// Many batches at once (after a yearly statement) — no wall of links.
			if i < 3 {
				fmt.Fprintf(&sb, "\n#%d — %d merchants, %s: %s/ui/batch/%d", b.ID, b.Items,
					money.Format(b.TotalMinor, money.DefaultCurrency), a.opts.PublicURL, b.ID)
			}
			if err := a.st.MarkBatchNotified(ctx, b.ID, a.now()); err != nil {
				a.log.Error("analysis: mark batch", "err", err)
			}
		}
		if len(fresh) > 3 {
			fmt.Fprintf(&sb, "\n…and %d more. All batches: %s/ui", len(fresh)-3, a.opts.PublicURL)
		}
		a.notify.Notify(ctx, sb.String())
	}

	ins, err := a.st.Insights(ctx, true, 20)
	if err != nil || len(ins) == 0 {
		return
	}
	var sb strings.Builder
	sb.WriteString("🔎 New insights:")
	ids := make([]int64, 0, len(ins))
	for i, in := range ins {
		ids = append(ids, in.ID)
		if i < 8 {
			sb.WriteString("\n• " + in.Title)
		}
	}
	if len(ins) > 8 {
		fmt.Fprintf(&sb, "\n…and %d more", len(ins)-8)
	}
	fmt.Fprintf(&sb, "\nDetails: %s/ui", a.opts.PublicURL)
	a.notify.Notify(ctx, sb.String())
	if err := a.st.MarkInsightsNotified(ctx, a.now(), ids...); err != nil {
		a.log.Error("analysis: mark insights", "err", err)
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
