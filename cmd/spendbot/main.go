// spendbot tracks spending from Kaspi statements and Apple Wallet payments, asks what the
// money was for, and keeps everything in SQLite.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // time zone database inside the binary: Windows does not have one

	"spendbot/internal/analysis"
	"spendbot/internal/bot"
	"spendbot/internal/classify"
	"spendbot/internal/clickhouse"
	"spendbot/internal/config"
	"spendbot/internal/ingest"
	"spendbot/internal/llm"
	"spendbot/internal/scheduler"
	"spendbot/internal/store"
	"spendbot/internal/web"
)

// version is set at build time: -ldflags "-X main.version=…"
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version)
		return
	}
	// distroless has no curl, so the Docker healthcheck calls the binary itself.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		addr := os.Getenv("LISTEN_ADDR")
		if addr == "" {
			addr = ":8080"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := ingest.Healthcheck(ctx, addr); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	cfg, cfgErr := config.Load()
	var handler slog.Handler = slog.NewJSONHandler(os.Stdout, nil)
	if cfg.LogFormat == "text" {
		handler = slog.NewTextHandler(os.Stdout, nil)
	}
	log := slog.New(handler)
	if cfgErr != nil {
		log.Error("fatal", "err", fmt.Errorf("settings: %w", cfgErr))
		waitOnWindows()
		os.Exit(1)
	}
	// Forgot the page password: run "spendbot reset-password" on the computer itself.
	if len(os.Args) > 1 && os.Args[1] == "reset-password" {
		if err := resetPassword(cfg); err != nil {
			fmt.Fprintln(os.Stderr, "reset-password:", err)
			os.Exit(1)
		}
		fmt.Println("The page password is removed and every browser is signed out.")
		if cfg.WebPassword != "" {
			fmt.Println("WEB_PASSWORD in the settings file still applies; remove it there to open the page.")
		} else {
			fmt.Println("Open the page and set a new password on the Security page.")
		}
		return
	}
	if err := run(cfg, log); err != nil {
		log.Error("fatal", "err", err)
		waitOnWindows()
		os.Exit(1)
	}
}

func resetPassword(cfg config.Config) error {
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()
	return st.ResetPassword(ctx)
}

// waitOnWindows keeps the console window open on an error: otherwise double-clicking the
// exe would show the error for a split second.
func waitOnWindows() {
	if runtime.GOOS == "windows" {
		fmt.Fprintln(os.Stderr, "\nPress Enter to close this window.")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	}
}

func run(cfg config.Config, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o700); err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	var wg sync.WaitGroup
	goRun := func(fn func()) {
		wg.Add(1)
		go func() { defer wg.Done(); fn() }()
	}

	// The ingest token comes from the settings or is generated on first start.
	if cfg.IngestToken == "" {
		if cfg.IngestToken, err = generatedToken(ctx, st); err != nil {
			return err
		}
	}

	var provider llm.Provider
	switch cfg.LLM {
	case "claude":
		c, err := llm.NewClaudeCLI(cfg.ClaudeBin, cfg.ClaudeModel, log)
		if err != nil {
			return err
		}
		provider = c
		log.Info("llm enabled: claude code cli", "model", cfg.ClaudeModel)
	case "openai":
		c := llm.NewClient(cfg.LLMBaseURL, cfg.LLMModel, cfg.LLMAPIKey, log)
		goRun(func() { c.Watch(ctx) })
		provider = c
		log.Info("llm enabled: openai-compatible", "model", cfg.LLMModel)
	default:
		log.Info("bot llm disabled: buttons, merchant memory and rules only")
	}
	cls := classify.New(st, provider, cfg.AutoMinHits, cfg.Location, log)

	mux := http.NewServeMux()

	// Telegram is optional. Without it Wallet payments are categorized from merchant memory
	// and the rest wait for a question in a batch on the web page.
	var agent *bot.Agent
	var notifier analysis.Notifier
	var jobs scheduler.Agent
	if cfg.TelegramToken != "" {
		tg, err := bot.NewTelegram(cfg.TelegramToken, cfg.TelegramAPIURL, cfg.AllowedChatID, log)
		if err != nil {
			return fmt.Errorf("telegram: %w", err)
		}
		agent = bot.New(st, cls, tg, bot.Options{
			Location:       cfg.Location,
			Quiet:          cfg.QuietHours,
			BatchWindow:    cfg.BatchWindow,
			BatchThreshold: cfg.BatchThreshold,
			ReminderAfter:  cfg.ReminderAfter,
		}, log)
		notifier, jobs = agent, agent
		ingest.New(cfg.IngestToken, st, agent, log).Register(mux)
		goRun(func() { agent.Run(ctx) })
		goRun(func() { tg.Start(ctx, agent) })
	} else {
		ingest.New(cfg.IngestToken, st, bot.NewOffline(st, cls, log), log).Register(mux)
		log.Info("telegram disabled: questions and insights are on the web page")
	}

	// Analysis: batched questions and insights. ClickHouse is optional — without it everything uses SQLite.
	var ch *clickhouse.Client
	if cfg.ClickHouseURL != "" {
		ch = clickhouse.New(cfg.ClickHouseURL, cfg.ClickHouseDB)
	}
	var local *llm.Client
	var localProvider llm.Provider
	if cfg.AnalysisLLMURL != "" {
		model := cfg.AnalysisLLMModel
		// The model chosen on the settings page wins over the default.
		if saved, err := st.GetKV(ctx, "analysis_model"); err == nil && saved != "" {
			model = saved
		}
		local = llm.NewClient(cfg.AnalysisLLMURL, model, "", log)
		localProvider = local
		goRun(func() { local.Watch(ctx) })
	}
	an := analysis.New(st, ch, classify.New(st, localProvider, cfg.AutoMinHits, cfg.Location, log), localProvider,
		notifier, analysis.Options{
			Location:    cfg.Location,
			Quiet:       cfg.QuietHours,
			PublicURL:   cfg.PublicURL,
			BatchSize:   cfg.BatchSize,
			BatchMaxAge: cfg.BatchMaxAge,
			Interval:    2 * time.Minute,
		}, log)
	goRun(func() { an.Run(ctx) })
	log.Info("analysis enabled", "clickhouse", cfg.ClickHouseURL != "", "local_llm", cfg.AnalysisLLMURL != "")

	w := web.New(st, an, cfg.WebPassword, cfg.AutoMinHits, cfg.BatchMaxAge, cfg.Location, log)
	if ch != nil {
		w.WithAnalytics(ch)
	}
	settings := web.Settings{ConfigFile: cfg.File, DataDir: filepath.Dir(cfg.DBPath),
		IngestURL: cfg.PublicURL + "/api/v1/tx", IngestToken: cfg.IngestToken,
		Telegram: cfg.TelegramToken != "", ClickHouse: ch != nil}
	if local != nil {
		settings.Model, settings.Ollama = local, llm.NewOllama(cfg.AnalysisLLMURL)
	}
	// Requests in plain words on the operations page: the local model keeps data at home;
	// without it the bot's model is used. Only the request and category names are sent.
	ai := localProvider
	if ai == nil {
		ai = provider
	}
	w.WithAI(ai).WithSettings(settings).Register(mux)
	if !w.HasPassword(ctx) && !isLoopback(cfg.Addr) {
		log.Warn("web ui WITHOUT password is open to the network", "addr", cfg.Addr)
	}

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second, // importing a large statement
	}
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return fmt.Errorf("port %s is busy or unavailable (is the program already running?): %w", cfg.Addr, err)
	}
	srvErr := make(chan error, 1)
	goRun(func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			srvErr <- err
		}
	})
	goRun(func() { scheduler.New(cfg, st, jobs, log).Run(ctx) })

	page := cfg.PublicURL + "/ui"
	log.Info("http listening", "addr", cfg.Addr, "url", page)
	if cfg.OpenBrowser {
		fmt.Fprintf(os.Stderr, "\nspendbot %s is running: %s\nKeep this window open while you use the program.\n\n", version, page)
		openBrowser(page, log)
	}

	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err = <-srvErr:
		log.Error("http server", "err", err)
		stop()
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if serr := srv.Shutdown(shutdownCtx); serr != nil {
		log.Warn("http shutdown", "err", serr)
	}
	wg.Wait()
	return err
}

// generatedToken is the ingest token generated once and stored in the database.
func generatedToken(ctx context.Context, st *store.Store) (string, error) {
	if t, err := st.GetKV(ctx, "ingest_token"); err != nil || t != "" {
		return t, err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	t := hex.EncodeToString(b)
	return t, st.SetKV(ctx, "ingest_token", t)
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

// openBrowser opens the page in the default browser.
func openBrowser(url string, log *slog.Logger) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Warn("open browser", "err", err)
		return
	}
	go func() { _ = cmd.Wait() }()
}
