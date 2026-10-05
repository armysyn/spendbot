package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadDefaults(t *testing.T) {
	c, err := load(env(map[string]string{
		"INGEST_TOKEN":             strings.Repeat("a", 32),
		"TELEGRAM_TOKEN":           "123:abc",
		"TELEGRAM_ALLOWED_CHAT_ID": "42",
	}), "/opt/spendbot")
	if err != nil {
		t.Fatal(err)
	}
	if c.AllowedChatID != 42 || c.Location.String() != "Asia/Almaty" || c.AutoMinHits != 3 || c.LLM != "off" {
		t.Fatalf("unexpected config: %+v", c)
	}
	if c.QuietHours != (QuietHours{Start: 23 * 60, End: 8 * 60}) {
		t.Fatalf("quiet hours: %+v", c.QuietHours)
	}
	if c.WeeklyReport != (WeeklyAt{Day: time.Monday, At: 10 * 60}) {
		t.Fatalf("weekly: %+v", c.WeeklyReport)
	}
}

func TestLoadReportsAllErrors(t *testing.T) {
	_, err := load(env(map[string]string{"LLM": "openai", "QUIET_HOURS": "bad", "INGEST_TOKEN": "short", "TELEGRAM_TOKEN": "1:x"}), ".")
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"INGEST_TOKEN", "TELEGRAM_ALLOWED_CHAT_ID", "LLM=openai", "QUIET_HOURS"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

// Starts without Telegram and a token: that is how the Windows build runs.
func TestMinimalConfig(t *testing.T) {
	c, err := load(env(map[string]string{}), "/opt/spendbot")
	if err != nil {
		t.Fatal(err)
	}
	if c.TelegramToken != "" || c.DBPath != "/opt/spendbot/data/spend.db" || c.BackupDir != "/opt/spendbot/data/backups" {
		t.Fatalf("%+v", c)
	}
}

func TestReadEnvFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "spendbot.env")
	os.WriteFile(p, []byte("\uFEFF# comment\r\nA=1\r\nB = two words  # note\nC=\"MON 10:00\"\nD=\n"), 0o600)
	got, err := ReadEnvFile(p)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"A": "1", "B": "two words", "C": "MON 10:00", "D": ""}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	os.WriteFile(p, []byte("bad line\n"), 0o600)
	if _, err := ReadEnvFile(p); err == nil {
		t.Error("bad line must fail")
	}
}

func TestQuietHours(t *testing.T) {
	q, _ := ParseQuietHours("23:00-08:00")
	at := func(h, m int) time.Time { return time.Date(2026, 10, 5, h, m, 0, 0, time.UTC) }
	cases := []struct {
		t    time.Time
		want bool
	}{
		{at(22, 59), false}, {at(23, 0), true}, {at(3, 0), true}, {at(7, 59), true}, {at(8, 0), false}, {at(12, 0), false},
	}
	for _, c := range cases {
		if got := q.Contains(c.t); got != c.want {
			t.Errorf("%s: got %v want %v", c.t.Format("15:04"), got, c.want)
		}
	}
	day, _ := ParseQuietHours("13:00-14:00")
	if !day.Contains(at(13, 30)) || day.Contains(at(14, 0)) {
		t.Error("daytime interval")
	}
	off, _ := ParseQuietHours("off")
	if off.Contains(at(3, 0)) {
		t.Error("off must never be quiet")
	}
}
