// Package ingest receives payments from the "Transaction" automation in iPhone Shortcuts.
package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"spendbot/internal/merchant"
	"spendbot/internal/money"
	"spendbot/internal/store"
)

const (
	maxBody       = 4 << 10
	ratePerMinute = 30
)

type Store interface {
	InsertTx(ctx context.Context, t store.Tx) (id int64, dup bool, err error)
	Ping(ctx context.Context) error
}

// Notifier receives the id of a new transaction; classification and the question run in
// the background so the Shortcut does not wait for the LLM.
type Notifier interface {
	NewTx(id int64)
}

type Server struct {
	token  []byte
	store  Store
	notify Notifier
	log    *slog.Logger
	now    func() time.Time
	limit  *limiter
}

func New(token string, st Store, n Notifier, log *slog.Logger) *Server {
	return &Server{token: []byte(token), store: st, notify: n, log: log, now: time.Now,
		limit: &limiter{max: ratePerMinute, window: time.Minute}}
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	s.Register(mux)
	return mux
}

// Register mounts the ingest endpoints on a shared mux (next to the web page).
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v1/tx", s.handleTx)
	mux.HandleFunc("GET /healthz", s.handleHealth)
}

type txRequest struct {
	Amount   flexString `json:"amount"`
	Merchant string     `json:"merchant"`
	Card     string     `json:"card"`
	At       string     `json:"at"`
}

// flexString accepts both a string and a number: Shortcuts may send the amount as a Number.
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	if bytes.HasPrefix(b, []byte(`"`)) {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*f = flexString(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*f = flexString(n.String())
	return nil
}

func (s *Server) handleTx(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		return
	}
	if !s.limit.allow(s.now()) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "rate limit"})
		return
	}

	var req txRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	amountRaw := strings.TrimSpace(string(req.Amount))
	if amountRaw == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "amount is required"})
		return
	}

	now := s.now()
	occurred := parseAt(req.At, now)
	tx := store.Tx{
		ExternalKey:  externalKey(amountRaw, req.Merchant, req.Card, occurred),
		OccurredAt:   occurred,
		AmountRaw:    amountRaw,
		MerchantRaw:  strings.TrimSpace(req.Merchant),
		MerchantNorm: merchant.Normalize(req.Merchant),
		Card:         strings.TrimSpace(req.Card),
		Source:       store.SourceWallet,
		Status:       store.StatusPending,
		CreatedAt:    now,
	}
	minor, currency, parseErr := money.Parse(amountRaw)
	if parseErr == nil {
		tx.AmountMinor, tx.Currency = minor, currency
	} else {
		// Do not lose the payment: keep it with amount_raw, the amount will be asked for.
		tx.Currency = money.DefaultCurrency
	}

	id, dup, err := s.store.InsertTx(r.Context(), tx)
	if err != nil {
		s.log.Error("ingest: save tx", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
		return
	}
	if dup {
		s.log.Info("ingest: duplicate", "tx_id", id)
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "duplicate": true})
		return
	}
	s.notify.NewTx(id)
	if parseErr != nil {
		s.log.Warn("ingest: amount not parsed", "tx_id", id)
		writeJSON(w, http.StatusBadRequest, map[string]any{"id": id, "status": store.StatusPending,
			"error": "amount not parsed, saved for review"})
		return
	}
	s.log.Info("ingest: accepted", "tx_id", id)
	writeJSON(w, http.StatusAccepted, map[string]any{"id": id, "status": store.StatusPending})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		http.Error(w, "db unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte("ok"))
}

func (s *Server) authorized(r *http.Request) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), s.token) == 1
}

// externalKey is sha256 of amount, merchant, card and the time truncated to the minute.
func externalKey(amount, merchant, card string, at time.Time) string {
	h := sha256.New()
	for _, part := range []string{amount, merchant, card, at.UTC().Truncate(time.Minute).Format(time.RFC3339)} {
		h.Write([]byte(strings.TrimSpace(part)))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

var atLayouts = []string{time.RFC3339, "2006-01-02T15:04:05Z0700", "2006-01-02T15:04Z07:00", "2006-01-02 15:04:05Z07:00"}

// parseAt parses the date from Shortcuts; if it is missing or unknown, the receive time is used.
func parseAt(s string, now time.Time) time.Time {
	s = strings.TrimSpace(s)
	for _, l := range atLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t
		}
	}
	return now
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// limiter is a sliding window for the single ingest token.
type limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	hits   []time.Time
}

func (l *limiter) allow(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now.Add(-l.window)
	kept := l.hits[:0]
	for _, t := range l.hits {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	l.hits = kept
	if len(l.hits) >= l.max {
		return false
	}
	l.hits = append(l.hits, now)
	return true
}

// Healthcheck is the client for `spendbot healthcheck`: distroless has no curl.
func Healthcheck(ctx context.Context, addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, port)+"/healthz", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return errors.New("healthz: " + resp.Status)
	}
	return nil
}
