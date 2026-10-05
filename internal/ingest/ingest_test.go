package ingest

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"spendbot/internal/store"
)

const token = "0123456789abcdef0123456789abcdef"

type recorder struct{ ids []int64 }

func (r *recorder) NewTx(id int64) { r.ids = append(r.ids, id) }

func setup(t *testing.T) (*Server, *store.Store, *recorder) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	rec := &recorder{}
	s := New(token, st, rec, slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.now = func() time.Time { return time.Date(2026, 10, 5, 9, 40, 0, 0, time.UTC) }
	return s, st, rec
}

func post(s *Server, auth, body string) (*httptest.ResponseRecorder, map[string]any) {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/tx", strings.NewReader(body))
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w, out
}

const body = `{"amount":"4 500,00 ₸","merchant":"MAGNUM CASH&CARRY","card":"Kaspi Gold","at":"2026-10-05T14:32:00+05:00"}`

func TestAcceptAndDedup(t *testing.T) {
	s, st, rec := setup(t)
	w, out := post(s, token, body)
	if w.Code != http.StatusAccepted || out["status"] != "pending" {
		t.Fatalf("got %d %v", w.Code, out)
	}
	id := int64(out["id"].(float64))
	tx, err := st.GetTx(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if tx.AmountMinor != 450000 || tx.MerchantNorm != "magnum cash carry" || !tx.OccurredAt.Equal(time.Date(2026, 10, 5, 9, 32, 0, 0, time.UTC)) {
		t.Fatalf("saved tx: %+v", tx)
	}
	if len(rec.ids) != 1 || rec.ids[0] != id {
		t.Fatalf("notified %v", rec.ids)
	}

	// same payment with different seconds — a duplicate
	w, out = post(s, token, strings.Replace(body, "14:32:00", "14:32:41", 1))
	if w.Code != http.StatusOK || out["duplicate"] != true || int64(out["id"].(float64)) != id {
		t.Fatalf("dup: %d %v", w.Code, out)
	}
	if len(rec.ids) != 1 {
		t.Fatal("duplicate must not trigger classification")
	}
}

func TestAuth(t *testing.T) {
	s, _, _ := setup(t)
	for _, auth := range []string{"", "wrong", token + "x"} {
		if w, _ := post(s, auth, body); w.Code != http.StatusUnauthorized {
			t.Errorf("auth %q: got %d", auth, w.Code)
		}
	}
}

func TestUnparsedAmountIsKept(t *testing.T) {
	s, st, rec := setup(t)
	w, out := post(s, token, `{"amount":"four thousand","merchant":"X"}`)
	if w.Code != http.StatusBadRequest || out["id"] == nil {
		t.Fatalf("got %d %v", w.Code, out)
	}
	tx, _ := st.GetTx(context.Background(), int64(out["id"].(float64)))
	if tx.AmountMinor != 0 || tx.AmountRaw != "four thousand" || len(rec.ids) != 1 {
		t.Fatalf("tx %+v", tx)
	}
}

func TestBadRequests(t *testing.T) {
	s, _, _ := setup(t)
	for _, b := range []string{`not json`, `{"merchant":"x"}`, `{"amount":"1` + strings.Repeat(" ", 5000) + `"}`} {
		if w, _ := post(s, token, b); w.Code != http.StatusBadRequest {
			t.Errorf("body %.20q: got %d", b, w.Code)
		}
	}
}

func TestNumericAmount(t *testing.T) {
	s, _, _ := setup(t)
	if w, out := post(s, token, `{"amount":4500.5,"merchant":"X"}`); w.Code != http.StatusAccepted {
		t.Fatalf("got %d %v", w.Code, out)
	}
}

func TestRateLimit(t *testing.T) {
	s, _, _ := setup(t)
	for i := range ratePerMinute {
		post(s, token, `{"amount":"`+strings.Repeat("1", 1+i%5)+`","merchant":"m`+string(rune('a'+i))+`"}`)
	}
	if w, _ := post(s, token, body); w.Code != http.StatusTooManyRequests {
		t.Fatalf("got %d", w.Code)
	}
}

func TestHealth(t *testing.T) {
	s, _, _ := setup(t)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusOK || w.Body.String() != "ok" {
		t.Fatalf("got %d %q", w.Code, w.Body.String())
	}
}
