package llm

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"
	"time"
)

func fakeCLI(out string, err error) (*ClaudeCLI, *[]string, *time.Time) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	var gotArgs []string
	c := &ClaudeCLI{model: "haiku", log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		timeout: time.Second, backoff: 5 * time.Minute, now: func() time.Time { return now }}
	c.run = func(_ context.Context, stdin string, args ...string) ([]byte, error) {
		gotArgs = append(args, "STDIN="+stdin)
		return []byte(out), err
	}
	return c, &gotArgs, &now
}

func TestClaudeCLI(t *testing.T) {
	c, args, _ := fakeCLI(`{"type":"result","subtype":"success","is_error":false,"result":"`+
		"```json\\n{\\\"category\\\": \\\"Groceries\\\"}\\n```"+`"}`, nil)
	out, err := c.JSON(context.Background(), "sys", "Merchant: MAGNUM")
	if err != nil || out != `{"category": "Groceries"}` {
		t.Fatalf("out=%q err=%v", out, err)
	}
	for _, want := range []string{"-p", "haiku", "--no-session-persistence", "--strict-mcp-config", "STDIN=Merchant: MAGNUM"} {
		if !slices.Contains(*args, want) {
			t.Errorf("args missing %q: %v", want, *args)
		}
	}
	// tools are disabled with an empty list
	if i := slices.Index(*args, "--tools"); i < 0 || (*args)[i+1] != "" {
		t.Errorf("tools not disabled: %v", *args)
	}
}

func TestClaudeCLIBackoff(t *testing.T) {
	c, _, now := fakeCLI("", errors.New("not logged in"))
	if _, err := c.JSON(context.Background(), "s", "u"); err == nil {
		t.Fatal("want error")
	}
	if c.Available() {
		t.Fatal("must be down after failure")
	}
	if _, err := c.JSON(context.Background(), "s", "u"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
	*now = now.Add(5 * time.Minute)
	if !c.Available() {
		t.Fatal("must retry after backoff")
	}
}

func TestClaudeCLIErrorResult(t *testing.T) {
	c, _, _ := fakeCLI(`{"type":"result","subtype":"error_during_execution","is_error":true,"result":"rate limited"}`, nil)
	if _, err := c.JSON(context.Background(), "s", "u"); err == nil || c.Available() {
		t.Fatalf("error result must fail and mark down: %v", err)
	}
}
