package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Ollama manages models of a local Ollama: checks that it runs, lists downloaded
// models and pulls new ones with progress. Model requests go through the
// OpenAI-compatible Client.
type Ollama struct {
	base string // http://localhost:11434
	http *http.Client
}

// NewOllama accepts the OpenAI-compatible API address (…/v1) or the Ollama root.
func NewOllama(url string) *Ollama {
	return &Ollama{base: strings.TrimSuffix(strings.TrimRight(url, "/"), "/v1"), http: &http.Client{}}
}

// Running reports whether Ollama answers.
func (o *Ollama) Running(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, o.base+"/api/version", nil)
	resp, err := o.http.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// Models lists downloaded models, such as qwen2.5:7b.
func (o *Ollama) Models(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, o.base+"/api/tags", nil)
	resp, err := o.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return nil, err
	}
	names := make([]string, len(tags.Models))
	for i, m := range tags.Models {
		names[i] = m.Name
	}
	return names, nil
}

// Pull downloads a model and reports progress. It may take a while — gigabytes.
func (o *Ollama) Pull(ctx context.Context, model string, progress func(status string, completed, total int64)) error {
	body, _ := json.Marshal(map[string]any{"model": model, "stream": true})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.base+"/api/pull", bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp, err := o.http.Do(req)
	if err != nil {
		return fmt.Errorf("ollama: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama: %s", resp.Status)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		var ev struct {
			Status    string `json:"status"`
			Completed int64  `json:"completed"`
			Total     int64  `json:"total"`
			Error     string `json:"error"`
		}
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		if ev.Error != "" {
			return errors.New("ollama: " + ev.Error)
		}
		progress(ev.Status, ev.Completed, ev.Total)
		if ev.Status == "success" {
			return nil
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return errors.New("ollama: download interrupted")
}
