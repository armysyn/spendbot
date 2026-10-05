// Package config reads settings from spendbot.env and the environment and validates them at startup.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Location *time.Location
	DataDir  string // database, backups and generated secrets; defaults to data/ next to the binary
	DBPath   string
	Addr     string

	OpenBrowser bool   // open the page in a browser after start (handy on Windows)
	LogFormat   string // json | text
	File        string // settings file that was read (spendbot.env); empty — environment only

	IngestToken string // empty — generated once and stored in the database

	TelegramToken  string // empty — no Telegram: questions and insights are on the web page only
	AllowedChatID  int64
	TelegramAPIURL string // for tests and a self-hosted Bot API; empty = api.telegram.org

	LLM         string // off | claude | openai
	ClaudeBin   string // path to the Claude Code CLI
	ClaudeModel string
	LLMBaseURL  string // for openai: any OpenAI-compatible API (Ollama, llama.cpp, cloud)
	LLMModel    string
	LLMAPIKey   string

	QuietHours    QuietHours
	WeeklyReport  WeeklyAt
	EveningReport Clock // daily evening summary

	// Statement analysis. ClickHouse is optional: without it everything is computed from SQLite.
	ClickHouseURL    string
	ClickHouseDB     string
	AnalysisLLMURL   string // OpenAI-compatible API of the local model; empty — no model
	AnalysisLLMModel string
	BatchSize        int
	BatchMaxAge      time.Duration

	WebPassword string // empty — the page has no password
	PublicURL   string // page address used in Telegram links

	BackupDir      string
	BackupKeep     int
	NoTxAlertDays  int // alert when no Wallet payments arrived for this many days
	AutoMinHits    int // confirmations after which a merchant's category is applied automatically
	ReminderAfter  time.Duration
	BatchWindow    time.Duration
	BatchThreshold int
}

// Load builds the config from spendbot.env (next to the binary or in the current directory,
// or the path in SPENDBOT_CONFIG), with environment variables taking precedence. Errors for
// all fields are returned at once so the file need not be fixed one line at a time.
func Load() (Config, error) {
	exeDir := "."
	if exe, err := os.Executable(); err == nil {
		exeDir = filepath.Dir(exe)
	}
	file := os.Getenv("SPENDBOT_CONFIG")
	if file == "" {
		for _, p := range []string{filepath.Join(exeDir, "spendbot.env"), "spendbot.env"} {
			if _, err := os.Stat(p); err == nil {
				file = p
				break
			}
		}
	}
	values := map[string]string{}
	if file != "" {
		var err error
		if values, err = ReadEnvFile(file); err != nil {
			return Config{}, err
		}
	}
	c, err := load(func(k string) string {
		if v, ok := os.LookupEnv(k); ok {
			return v
		}
		return values[k]
	}, exeDir)
	c.File = file
	return c, err
}

// ReadEnvFile reads a KEY=VALUE file: # starts a comment, values may be quoted.
func ReadEnvFile(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("settings %s: %w", path, err)
	}
	out := map[string]string{}
	text := strings.TrimPrefix(string(b), "\uFEFF") // Notepad saves UTF-8 with a BOM
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected KEY=value", path, i+1)
		}
		val = strings.TrimSpace(val)
		if q := strings.IndexAny(val, `"'`); q == 0 {
			if end := strings.IndexByte(val[1:], val[0]); end >= 0 {
				val = val[1 : end+1]
			}
		} else if hash := strings.Index(val, " #"); hash >= 0 {
			val = strings.TrimSpace(val[:hash])
		}
		out[strings.TrimSpace(key)] = val
	}
	return out, nil
}

func load(getenv func(string) string, exeDir string) (Config, error) {
	var errs []error
	get := func(key, def string) string {
		if v := strings.TrimSpace(getenv(key)); v != "" {
			return v
		}
		return def
	}

	dataDir := get("DATA_DIR", filepath.Join(exeDir, "data"))
	c := Config{
		DataDir:        dataDir,
		DBPath:         get("DB_PATH", filepath.Join(dataDir, "spend.db")),
		OpenBrowser:    get("OPEN_BROWSER", "0") == "1",
		LogFormat:      get("LOG_FORMAT", "json"),
		Addr:           get("LISTEN_ADDR", ":8080"),
		IngestToken:    get("INGEST_TOKEN", ""),
		TelegramToken:  get("TELEGRAM_TOKEN", ""),
		TelegramAPIURL: get("TELEGRAM_API_URL", ""),
		ClaudeBin:      get("CLAUDE_BIN", "claude"),
		ClaudeModel:    get("CLAUDE_MODEL", "haiku"),
		LLMBaseURL:     strings.TrimRight(get("LLM_BASE_URL", ""), "/"),
		LLMModel:       get("LLM_MODEL", ""),
		LLMAPIKey:      get("LLM_API_KEY", ""),
		ReminderAfter:  3 * time.Hour,
		BatchWindow:    10 * time.Minute,
		BatchThreshold: 3,
	}

	loc, err := time.LoadLocation(get("TZ", "Asia/Almaty"))
	if err != nil {
		errs = append(errs, fmt.Errorf("TZ: %w", err))
		loc = time.UTC
	}
	c.Location = loc

	if c.IngestToken != "" && len(c.IngestToken) < 16 {
		errs = append(errs, errors.New("INGEST_TOKEN: must be at least 16 characters, or empty to generate one"))
	}
	// Telegram is optional; with a token a chat is required, otherwise the bot would answer anyone.
	if c.TelegramToken != "" {
		if v := get("TELEGRAM_ALLOWED_CHAT_ID", ""); v == "" {
			errs = append(errs, errors.New("TELEGRAM_ALLOWED_CHAT_ID: required when TELEGRAM_TOKEN is set"))
		} else if c.AllowedChatID, err = strconv.ParseInt(v, 10, 64); err != nil {
			errs = append(errs, fmt.Errorf("TELEGRAM_ALLOWED_CHAT_ID: %w", err))
		}
	}
	// Without LLM=… the provider is inferred from LLM_BASE_URL.
	defLLM := "off"
	if c.LLMBaseURL != "" {
		defLLM = "openai"
	}
	switch c.LLM = strings.ToLower(get("LLM", defLLM)); c.LLM {
	case "off", "claude":
	case "openai":
		if c.LLMBaseURL == "" || c.LLMModel == "" {
			errs = append(errs, errors.New("LLM=openai: LLM_BASE_URL and LLM_MODEL are required"))
		}
	default:
		errs = append(errs, fmt.Errorf("LLM: expected off, claude or openai, got %q", c.LLM))
	}

	if c.QuietHours, err = ParseQuietHours(get("QUIET_HOURS", "23:00-08:00")); err != nil {
		errs = append(errs, fmt.Errorf("QUIET_HOURS: %w", err))
	}
	if c.WeeklyReport, err = ParseWeeklyAt(get("WEEKLY_REPORT", "MON 10:00")); err != nil {
		errs = append(errs, fmt.Errorf("WEEKLY_REPORT: %w", err))
	}
	if c.EveningReport, err = ParseClock(get("EVENING_REPORT", "21:00")); err != nil {
		errs = append(errs, fmt.Errorf("EVENING_REPORT: %w", err))
	}

	c.ClickHouseURL = strings.TrimRight(get("CLICKHOUSE_URL", ""), "/")
	c.ClickHouseDB = get("CLICKHOUSE_DB", "spend")
	if get("ANALYSIS_LLM", "on") != "off" {
		c.AnalysisLLMURL = strings.TrimRight(get("ANALYSIS_LLM_URL", "http://localhost:11434/v1"), "/")
	}
	c.AnalysisLLMModel = get("ANALYSIS_LLM_MODEL", "qwen2.5:7b")
	if c.BatchMaxAge, err = time.ParseDuration(get("BATCH_MAX_AGE", "30m")); err != nil {
		errs = append(errs, fmt.Errorf("BATCH_MAX_AGE: %w", err))
	}
	c.WebPassword = get("WEB_PASSWORD", "")
	if c.WebPassword != "" && len(c.WebPassword) < 8 {
		errs = append(errs, errors.New("WEB_PASSWORD: at least 8 characters"))
	}
	c.PublicURL = strings.TrimRight(get("PUBLIC_URL", "http://localhost:8080"), "/")

	c.BackupDir = get("BACKUP_DIR", filepath.Join(dataDir, "backups"))
	intVar := func(key string, def int, dst *int) {
		v := get(key, strconv.Itoa(def))
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			errs = append(errs, fmt.Errorf("%s: expected a non-negative number, got %q", key, v))
		}
		*dst = n
	}
	intVar("BACKUP_KEEP", 14, &c.BackupKeep)
	intVar("NO_TX_ALERT_DAYS", 3, &c.NoTxAlertDays)
	intVar("AUTO_MIN_HITS", 3, &c.AutoMinHits)
	intVar("BATCH_SIZE", 20, &c.BatchSize)
	if c.BatchSize == 0 {
		c.BatchSize = 20
	}

	return c, errors.Join(errs...)
}

// Clock is a time of day in minutes since midnight.
type Clock int

func ParseClock(s string) (Clock, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("expected HH:MM, got %q", s)
	}
	return Clock(t.Hour()*60 + t.Minute()), nil
}

func ClockOf(t time.Time) Clock { return Clock(t.Hour()*60 + t.Minute()) }

func (c Clock) String() string { return fmt.Sprintf("%02d:%02d", int(c)/60, int(c)%60) }

// QuietHours is when the bot asks no questions. It may span midnight.
type QuietHours struct{ Start, End Clock }

func ParseQuietHours(s string) (QuietHours, error) {
	if strings.EqualFold(s, "off") {
		return QuietHours{}, nil
	}
	from, to, ok := strings.Cut(s, "-")
	if !ok {
		return QuietHours{}, fmt.Errorf("expected HH:MM-HH:MM or off, got %q", s)
	}
	var q QuietHours
	var err error
	if q.Start, err = ParseClock(from); err != nil {
		return q, err
	}
	if q.End, err = ParseClock(to); err != nil {
		return q, err
	}
	return q, nil
}

// Contains reports whether t (in its own time zone) falls into quiet hours.
func (q QuietHours) Contains(t time.Time) bool {
	if q.Start == q.End {
		return false
	}
	c := ClockOf(t)
	if q.Start < q.End {
		return c >= q.Start && c < q.End
	}
	return c >= q.Start || c < q.End
}

// WeeklyAt is a weekday and time, such as MON 10:00.
type WeeklyAt struct {
	Day time.Weekday
	At  Clock
}

var weekdays = map[string]time.Weekday{
	"SUN": time.Sunday, "MON": time.Monday, "TUE": time.Tuesday, "WED": time.Wednesday,
	"THU": time.Thursday, "FRI": time.Friday, "SAT": time.Saturday,
}

func ParseWeeklyAt(s string) (WeeklyAt, error) {
	day, at, ok := strings.Cut(strings.TrimSpace(s), " ")
	wd, known := weekdays[strings.ToUpper(day)]
	if !ok || !known {
		return WeeklyAt{}, fmt.Errorf("expected something like MON 10:00, got %q", s)
	}
	c, err := ParseClock(at)
	if err != nil {
		return WeeklyAt{}, err
	}
	return WeeklyAt{Day: wd, At: c}, nil
}
