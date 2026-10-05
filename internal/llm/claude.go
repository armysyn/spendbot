package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ClaudeCLI talks to Claude through an installed Claude Code in headless mode
// (`claude -p`) and uses its login. It works where the binary exists and the user is
// logged in: on a Mac via `make run`, not inside the distroless container.
type ClaudeCLI struct {
	bin, model string
	log        *slog.Logger
	timeout    time.Duration
	backoff    time.Duration
	run        func(ctx context.Context, stdin string, args ...string) ([]byte, error)

	mu        sync.Mutex // one call at a time: the server has a single core
	stateMu   sync.Mutex
	downUntil time.Time
	now       func() time.Time
}

func NewClaudeCLI(bin, model string, log *slog.Logger) (*ClaudeCLI, error) {
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("claude cli %q not found: %w", bin, err)
	}
	c := &ClaudeCLI{bin: path, model: model, log: log, timeout: 60 * time.Second,
		backoff: 5 * time.Minute, now: time.Now}
	c.run = c.exec
	return c, nil
}

// Available: after a failed call the model is considered down for backoff so questions
// do not wait for timeouts. There are no probe calls — they would cost money.
func (c *ClaudeCLI) Available() bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return !c.now().Before(c.downUntil)
}

func (c *ClaudeCLI) markDown() {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if c.now().Before(c.downUntil) {
		return
	}
	c.downUntil = c.now().Add(c.backoff)
	c.log.Warn("claude cli unavailable, falling back to buttons", "retry_in", c.backoff)
}

type cliResult struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
}

func (c *ClaudeCLI) JSON(ctx context.Context, system, user string) (string, error) {
	if !c.Available() {
		return "", ErrUnavailable
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	// No tools, settings, MCP or history: text in, text out.
	out, err := c.run(ctx, user,
		"-p", "--model", c.model, "--output-format", "json",
		"--system-prompt", system+"\nAnswer with a JSON object only, no markdown or explanations.",
		"--tools", "", "--setting-sources", "", "--strict-mcp-config", "--no-session-persistence")
	if err != nil {
		c.markDown()
		return "", fmt.Errorf("claude cli: %w", err)
	}
	var res cliResult
	if err := json.Unmarshal(out, &res); err != nil {
		c.markDown()
		return "", fmt.Errorf("claude cli: invalid output: %w", err)
	}
	if res.IsError || res.Subtype != "success" {
		c.markDown()
		return "", fmt.Errorf("claude cli: %s: %.200s", res.Subtype, res.Result)
	}
	return extractJSON(res.Result)
}

func (c *ClaudeCLI) exec(ctx context.Context, stdin string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.bin, args...)
	cmd.Dir = os.TempDir() // do not pick up CLAUDE.md from the working directory
	cmd.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%w: %.300s", err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// extractJSON pulls the object out of the answer even if the model wrapped it in ```json.
func extractJSON(s string) (string, error) {
	start, end := strings.Index(s, "{"), strings.LastIndex(s, "}")
	if start < 0 || end < start {
		return "", errors.New("no JSON object in the answer")
	}
	return s[start : end+1], nil
}
