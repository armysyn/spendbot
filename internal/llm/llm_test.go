package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClient(t *testing.T) {
	var got chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			w.Write([]byte(`{"data":[]}`))
		case "/v1/chat/completions":
			json.NewDecoder(r.Body).Decode(&got)
			w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"category\":\"Groceries\"}"}}]}`))
		}
	}))
	defer srv.Close()

	c := NewClient(srv.URL+"/v1", "qwen", "key", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := c.JSON(context.Background(), "s", "u"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("before health check: %v", err)
	}
	c.check(context.Background())
	if !c.Available() {
		t.Fatal("must be available")
	}
	out, err := c.JSON(context.Background(), "sys", "usr")
	if err != nil || out != `{"category":"Groceries"}` {
		t.Fatalf("out=%q err=%v", out, err)
	}
	if got.Model != "qwen" || len(got.Messages) != 2 || got.ResponseFormat["type"] != "json_object" {
		t.Fatalf("request: %+v", got)
	}

	srv.Close()
	if _, err := c.JSON(context.Background(), "s", "u"); err == nil || c.Available() {
		t.Fatal("network error must mark llm unavailable")
	}
}

func TestOllamaPull(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/version":
			w.Write([]byte(`{"version":"0.35.1"}`))
		case "/api/tags":
			w.Write([]byte(`{"models":[{"name":"qwen2.5:3b"}]}`))
		case "/api/pull":
			w.Write([]byte("{\"status\":\"pulling manifest\"}\n{\"status\":\"downloading\",\"completed\":50,\"total\":100}\n{\"status\":\"success\"}\n"))
		}
	}))
	defer srv.Close()
	o := NewOllama(srv.URL + "/v1")
	ctx := context.Background()
	if !o.Running(ctx) {
		t.Fatal("must be running")
	}
	if ms, err := o.Models(ctx); err != nil || len(ms) != 1 || ms[0] != "qwen2.5:3b" {
		t.Fatalf("models %v %v", ms, err)
	}
	var last int64
	if err := o.Pull(ctx, "qwen2.5:7b", func(_ string, done, _ int64) { last = max(last, done) }); err != nil || last != 50 {
		t.Fatalf("pull: %v last=%d", err, last)
	}
	if NewOllama("http://127.0.0.1:1").Running(ctx) {
		t.Fatal("nothing listens on port 1")
	}
}
