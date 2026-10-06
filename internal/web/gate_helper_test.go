package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"spendbot/internal/accounts"
	"spendbot/internal/store"
)

// gateFor puts srv behind a Gate as the first account; other accounts get fresh databases.
func gateFor(t *testing.T, mux *http.ServeMux, srv *Server, env string) *Gate {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	reg, err := accounts.Open(ctx, filepath.Join(dir, "accounts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	if _, err := reg.Bootstrap(ctx, filepath.Join(dir, "primary.db"), "Me", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	build := func(ctx context.Context, a accounts.Account) (*Server, error) {
		if a.Primary() {
			return srv, nil
		}
		st, err := store.Open(ctx, a.DBPath)
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { st.Close() })
		return New(st, nil, 3, time.Minute, almaty, slogDiscard()).OnClose(func() { st.Close() }), nil
	}
	g := NewGate(reg, build, env, filepath.Join(dir, "uploads"), slogDiscard())
	g.Register(mux)
	return g
}

func newRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }
