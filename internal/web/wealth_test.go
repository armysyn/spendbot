package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"spendbot/internal/store"
)

func TestWealthPage(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	seedOps(t, st)
	srv := New(st, nil, 3, time.Minute, almaty, slogDiscard())
	srv.now = func() time.Time { return time.Date(2026, 10, 8, 12, 0, 0, 0, almaty) }
	mux := http.NewServeMux()
	gateFor(t, mux, srv, "")
	b := &browser{h: mux}
	post := func(kv ...string) string {
		f := url.Values{}
		for i := 0; i+1 < len(kv); i += 2 {
			f.Set(kv[i], kv[i+1])
		}
		return b.req("POST", "/ui/wealth", f).Header().Get("Location")
	}
	if empty := b.req("GET", "/ui/wealth", nil).Body.String(); !strings.Contains(empty, "Nothing is known about your money yet") || strings.Contains(empty, "0001") {
		t.Error("an empty page says what to do, not zeros")
	}
	post("action", "add", "name", "Deposit", "kind", "deposit", "liquid", "1", "amount", "1m", "on", "2026-01-15")
	post("action", "add", "name", "Car loan", "kind", "loan", "liquid", "1", "amount", "400000", "on", "2026-01-15")
	if loc := post("action", "add", "name", "Future", "kind", "deposit", "amount", "1", "on", "2027-01-01"); !strings.Contains(loc, "not+in+the+future") {
		t.Errorf("a future date must be refused: %s", loc)
	}
	hs, _ := st.Holdings(ctx, almaty)
	if len(hs) != 2 || hs[1].Liquid || len(hs[0].Values) != 1 || hs[0].Values[0].Amount != 100_000_000 {
		t.Fatalf("holdings (a debt is never liquid): %+v", hs)
	}
	post("action", "value", "id", itoa(hs[0].ID), "amount", "1.2m", "on", "2026-09-01")
	post("action", "value", "id", itoa(hs[0].ID), "amount", "1.3m", "on", "2026-09-01") // the same day replaces
	if hs, _ = st.Holdings(ctx, almaty); len(hs[0].Values) != 2 || hs[0].Values[1].Amount != 130_000_000 {
		t.Fatalf("values: %+v", hs[0].Values)
	}
	page := b.req("GET", "/ui/wealth", nil).Body.String()
	for _, want := range []string{"Net worth", "900,000", "1,300,000", "Car loan", "Cushion"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if !strings.HasSuffix(strings.TrimSpace(page), "</html>") {
		t.Fatalf("cut short:\n%s", tail(page))
	}
	// a category marked as debt payments
	home, _ := st.FindCategory(ctx, "Home")
	b.req("POST", "/ui/categories", url.Values{"action": {"debt"}, "id": {itoa(home.ID)}})
	if names, _ := st.DebtCategories(ctx); len(names) != 1 || names[0] != "Home" {
		t.Errorf("debt categories: %v", names)
	}
	post("action", "archive", "id", itoa(hs[1].ID))
	if hs, _ = st.Holdings(ctx, almaty); !hs[1].Archived {
		t.Error("archive")
	}
	post("action", "delete", "id", itoa(hs[1].ID))
	if hs, _ = st.Holdings(ctx, almaty); len(hs) != 1 {
		t.Error("delete")
	}
}
