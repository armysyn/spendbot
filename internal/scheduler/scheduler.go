// Package scheduler is an in-process ticker for questions, reminders, summaries and backups,
// with no cron or extra services. Last run times are stored in the database, so a restart
// does not repeat daily jobs.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"spendbot/internal/config"
	"spendbot/internal/store"
)

type Agent interface {
	Flush(ctx context.Context)
	Recover(ctx context.Context)
	Remind(ctx context.Context)
	EveningReport(ctx context.Context)
	WeeklyReport(ctx context.Context)
	Notify(ctx context.Context, text string)
}

type Scheduler struct {
	cfg   config.Config
	st    *store.Store
	agent Agent
	log   *slog.Logger
	now   func() time.Time
}

const (
	tick        = 30 * time.Second
	backupAt    = config.Clock(3*60 + 30) // 03:30
	alertCheck  = config.Clock(12 * 60)   // 12:00
	backupGlob  = "spend-*.db"
	backupStamp = "2006-01-02"
)

func New(cfg config.Config, st *store.Store, agent Agent, log *slog.Logger) *Scheduler {
	return &Scheduler{cfg: cfg, st: st, agent: agent, log: log, now: time.Now}
}

func (s *Scheduler) Run(ctx context.Context) {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.Tick(ctx)
		}
	}
}

func (s *Scheduler) Tick(ctx context.Context) {
	now := s.now().In(s.cfg.Location)
	// Without Telegram there is no agent: only backups remain.
	if s.agent != nil {
		s.agent.Recover(ctx)
		s.agent.Flush(ctx)
		s.agent.Remind(ctx)
	}

	quiet := s.cfg.QuietHours.Contains(now)
	if !quiet && s.agent != nil {
		s.daily(ctx, "evening_report", now, s.cfg.EveningReport, s.agent.EveningReport)
		if now.Weekday() == s.cfg.WeeklyReport.Day {
			s.daily(ctx, "weekly_report", now, s.cfg.WeeklyReport.At, s.agent.WeeklyReport)
		}
		s.daily(ctx, "no_tx_alert", now, alertCheck, s.noTxAlert)
	}
	s.daily(ctx, "backup", now, backupAt, func(ctx context.Context) {
		if err := s.backup(ctx, now); err != nil {
			s.log.Error("scheduler: backup", "err", err)
			if s.agent != nil {
				s.agent.Notify(ctx, "Database backup failed, see the logs.")
			}
		}
	})
}

// daily runs fn once a day starting at time at. If the process was down at that time,
// the job runs on the first tick after it starts on the same day.
func (s *Scheduler) daily(ctx context.Context, key string, now time.Time, at config.Clock, fn func(context.Context)) {
	if config.ClockOf(now) < at {
		return
	}
	today := now.Format(backupStamp)
	last, err := s.st.GetKV(ctx, "last_run:"+key)
	if err != nil {
		s.log.Error("scheduler: read kv", "key", key, "err", err)
		return
	}
	if last == today {
		return
	}
	// Mark before running: better to miss a summary than to send it every 30 seconds.
	if err := s.st.SetKV(ctx, "last_run:"+key, today); err != nil {
		s.log.Error("scheduler: write kv", "key", key, "err", err)
		return
	}
	s.log.Info("scheduler: run", "job", key)
	fn(ctx)
}

// noTxAlert warns when nothing has come from Wallet for a while: the Shortcuts
// automation may be broken.
func (s *Scheduler) noTxAlert(ctx context.Context) {
	if s.cfg.NoTxAlertDays == 0 {
		return
	}
	last, ok, err := s.st.LastTxAt(ctx, store.SourceWallet)
	if err != nil || !ok {
		return
	}
	days := int(s.now().Sub(last).Hours() / 24)
	if days >= s.cfg.NoTxAlertDays {
		s.agent.Notify(ctx, fmt.Sprintf("No Apple Wallet payments for %d days. Check the Transaction automation in Shortcuts and access to the server.", days))
	}
}

// backup runs VACUUM INTO BACKUP_DIR and keeps the last BACKUP_KEEP copies.
func (s *Scheduler) backup(ctx context.Context, now time.Time) error {
	if s.cfg.BackupKeep == 0 {
		return nil
	}
	if err := os.MkdirAll(s.cfg.BackupDir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(s.cfg.BackupDir, "spend-"+now.Format(backupStamp)+".db")
	// VACUUM INTO does not overwrite an existing file.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := s.st.Backup(ctx, path); err != nil {
		return err
	}
	files, err := filepath.Glob(filepath.Join(s.cfg.BackupDir, backupGlob))
	if err != nil {
		return err
	}
	sort.Strings(files) // the date is in the name, so this sorts by time
	for len(files) > s.cfg.BackupKeep {
		if err := os.Remove(files[0]); err != nil {
			return err
		}
		files = files[1:]
	}
	s.log.Info("scheduler: backup done", "kept", len(files))
	return nil
}
