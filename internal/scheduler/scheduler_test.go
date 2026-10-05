package scheduler

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"spendbot/internal/config"
	"spendbot/internal/store"
)

type fakeAgent struct {
	evening, weekly int
	notes           []string
}

func (f *fakeAgent) Flush(context.Context)              {}
func (f *fakeAgent) Recover(context.Context)            {}
func (f *fakeAgent) Remind(context.Context)             {}
func (f *fakeAgent) EveningReport(context.Context)      { f.evening++ }
func (f *fakeAgent) WeeklyReport(context.Context)       { f.weekly++ }
func (f *fakeAgent) Notify(_ context.Context, t string) { f.notes = append(f.notes, t) }

var almaty, _ = time.LoadLocation("Asia/Almaty")

func setup(t *testing.T) (*Scheduler, *fakeAgent, *store.Store, *time.Time) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	q, _ := config.ParseQuietHours("23:00-08:00")
	w, _ := config.ParseWeeklyAt("MON 10:00")
	ev, _ := config.ParseClock("21:00")
	cfg := config.Config{Location: almaty, QuietHours: q, WeeklyReport: w, EveningReport: ev,
		BackupDir: filepath.Join(dir, "backups"), BackupKeep: 2, NoTxAlertDays: 3}
	fa := &fakeAgent{}
	s := New(cfg, st, fa, slog.New(slog.NewTextHandler(io.Discard, nil)))
	now := time.Date(2026, 10, 5, 9, 0, 0, 0, almaty) // Monday
	s.now = func() time.Time { return now }
	return s, fa, st, &now
}

func TestReportsRunOncePerDay(t *testing.T) {
	s, fa, _, now := setup(t)
	ctx := context.Background()
	s.Tick(ctx)
	if fa.weekly != 0 || fa.evening != 0 {
		t.Fatal("too early")
	}
	*now = now.Add(time.Hour + time.Minute) // 10:01
	s.Tick(ctx)
	s.Tick(ctx)
	if fa.weekly != 1 {
		t.Fatalf("weekly %d", fa.weekly)
	}
	*now = time.Date(2026, 10, 5, 21, 0, 0, 0, almaty)
	s.Tick(ctx)
	s.Tick(ctx)
	if fa.evening != 1 {
		t.Fatalf("evening %d", fa.evening)
	}
	// Tuesday: no weekly summary, the evening one again
	*now = time.Date(2026, 10, 6, 21, 30, 0, 0, almaty)
	s.Tick(ctx)
	if fa.weekly != 1 || fa.evening != 2 {
		t.Fatalf("tuesday weekly=%d evening=%d", fa.weekly, fa.evening)
	}
}

func TestBackupRotation(t *testing.T) {
	s, _, _, now := setup(t)
	ctx := context.Background()
	for d := range 4 {
		*now = time.Date(2026, 10, 5+d, 3, 31, 0, 0, almaty)
		s.Tick(ctx)
	}
	files, _ := filepath.Glob(filepath.Join(s.cfg.BackupDir, "spend-*.db"))
	if len(files) != 2 || filepath.Base(files[1]) != "spend-2026-10-08.db" {
		t.Fatalf("backups: %v", files)
	}
	if fi, err := os.Stat(files[1]); err != nil || fi.Size() == 0 {
		t.Fatalf("backup file: %v", err)
	}
}

func TestNoTxAlert(t *testing.T) {
	s, fa, st, now := setup(t)
	ctx := context.Background()
	st.InsertTx(ctx, store.Tx{ExternalKey: "a", OccurredAt: *now, AmountMinor: 100, Currency: "KZT",
		Source: store.SourceWallet, CreatedAt: now.Add(-4 * 24 * time.Hour)})
	*now = time.Date(2026, 10, 5, 12, 0, 0, 0, almaty)
	s.Tick(ctx)
	s.Tick(ctx)
	if len(fa.notes) != 1 {
		t.Fatalf("notes %v", fa.notes)
	}
}
