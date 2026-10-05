// Package llm is the optional language model. One client for OpenAI-compatible APIs
// covers Ollama, llama.cpp server and cloud providers.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"
)

// Provider is everything classify needs from a model.
type Provider interface {
	// Available reports whether the model answered the last check. While it is down,
	// classify goes straight to buttons and rules instead of waiting for a timeout.
	Available() bool
	// JSON sends a system and a user prompt and returns the model's JSON answer.
	JSON(ctx context.Context, system, user string) (string, error)
}

var ErrUnavailable = errors.New("llm unavailable")

type Client struct {
	baseURL, apiKey string
	model           atomic.Value // string; changed on the settings page without a restart
	http            *http.Client
	log             *slog.Logger
	up              atomic.Bool
}

// Model is the model requests go to.
func (c *Client) Model() string { return c.model.Load().(string) }

// SetModel switches the model on the fly.
func (c *Client) SetModel(m string) { c.model.Store(m) }

func NewClient(baseURL, model, apiKey string, log *slog.Logger) *Client {
	c := &Client{baseURL: baseURL, apiKey: apiKey, log: log, http: &http.Client{Timeout: 60 * time.Second}}
	c.model.Store(model)
	return c
}

func (c *Client) Available() bool { return c.up.Load() }

// Watch checks the model every minute until ctx is cancelled.
func (c *Client) Watch(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		c.check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (c *Client) check(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/models", nil)
	if err != nil {
		return
	}
	c.auth(req)
	resp, err := c.http.Do(req)
	ok := err == nil && resp.StatusCode == http.StatusOK
	if resp != nil {
		resp.Body.Close()
	}
	if was := c.up.Swap(ok); was != ok {
		c.log.Info("llm availability changed", "available", ok)
	}
}

func (c *Client) auth(req *http.Request) {
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
}

type chatRequest struct {
	Model          string            `json:"model"`
	Messages       []chatMessage     `json:"messages"`
	Temperature    float64           `json:"temperature"`
	ResponseFormat map[string]string `json:"response_format"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Choices []struct {
		Message chatMessage `json:"message"`
	} `json:"choices"`
}

func (c *Client) JSON(ctx context.Context, system, user string) (string, error) {
	if !c.Available() {
		return "", ErrUnavailable
	}
	body, _ := json.Marshal(chatRequest{
		Model:          c.Model(),
		Messages:       []chatMessage{{"system", system}, {"user", user}},
		ResponseFormat: map[string]string{"type": "json_object"},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	c.auth(req)
	resp, err := c.http.Do(req)
	if err != nil {
		c.up.Store(false)
		return "", fmt.Errorf("llm: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("llm: %s", resp.Status)
	}
	var cr chatResponse
	if err := json.Unmarshal(raw, &cr); err != nil || len(cr.Choices) == 0 {
		return "", errors.New("llm: empty or invalid answer")
	}
	return cr.Choices[0].Message.Content, nil
}
