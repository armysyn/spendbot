// Package clickhouse is a minimal client for the ClickHouse HTTP interface: no driver,
// rows as JSONEachRow.
package clickhouse

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	base string // http://localhost:8123
	db   string
	http *http.Client
}

func New(baseURL, db string) *Client {
	return &Client{base: strings.TrimRight(baseURL, "/"), db: db, http: &http.Client{Timeout: 60 * time.Second}}
}

func (c *Client) DB() string { return c.db }

func (c *Client) do(ctx context.Context, query string, body io.Reader) (io.ReadCloser, error) {
	params := url.Values{}
	params.Set("query", query)
	if c.db != "" {
		params.Set("database", c.db)
	}
	params.Set("output_format_json_quote_64bit_integers", "0")
	params.Set("date_time_input_format", "best_effort")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/?"+params.Encode(), body)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		return nil, fmt.Errorf("clickhouse: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return resp.Body, nil
}

// Exec runs a query without a result (DDL, mutations).
func (c *Client) Exec(ctx context.Context, query string) error {
	body, err := c.do(ctx, query, nil)
	if err != nil {
		return err
	}
	defer body.Close()
	_, err = io.Copy(io.Discard, body)
	return err
}

// ExecRaw runs a query without a database (CREATE DATABASE before it exists).
func (c *Client) ExecRaw(ctx context.Context, query string) error {
	c2 := *c
	c2.db = ""
	return c2.Exec(ctx, query)
}

func (c *Client) Ping(ctx context.Context) error { return c.Exec(ctx, "SELECT 1") }

// Insert writes rows into a table in one request.
func Insert[T any](ctx context.Context, c *Client, table string, rows []T) error {
	if len(rows) == 0 {
		return nil
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, r := range rows {
		if err := enc.Encode(r); err != nil {
			return err
		}
	}
	body, err := c.do(ctx, "INSERT INTO "+table+" FORMAT JSONEachRow", &buf)
	if err != nil {
		return err
	}
	defer body.Close()
	_, err = io.Copy(io.Discard, body)
	return err
}

// Select runs a query and decodes rows into T by json tags.
func Select[T any](ctx context.Context, c *Client, query string) ([]T, error) {
	body, err := c.do(ctx, query+" FORMAT JSONEachRow", nil)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	var out []T
	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		var v T
		if err := json.Unmarshal(sc.Bytes(), &v); err != nil {
			return nil, fmt.Errorf("clickhouse: decode row: %w", err)
		}
		out = append(out, v)
	}
	return out, sc.Err()
}
