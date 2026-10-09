package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"spendbot/internal/kaspi"
	"spendbot/internal/store"
)

func TestPassThroughCash(t *testing.T) {
	h, st, k := setup(t)
	ctx := context.Background()
	_, cash := seedOps(t, st)
	// own money from another bank the day before the ATM withdrawal of 20,000 ₸
	at := time.Date(2026, 8, 3, 12, 0, 0, 0, almaty)
	if _, _, err := st.InsertTx(ctx, store.Tx{ExternalKey: "own", OccurredAt: at, AmountMinor: -2000000, Currency: "KZT",
		MerchantRaw: "С карты другого банка", Kind: kaspi.TopUp, Source: store.SourceImport, Status: store.StatusInfo, CreatedAt: at}); err != nil {
		t.Fatal(err)
	}
	has := func(path, want string) bool {
		t.Helper()
		w := do(h, "GET", path, nil, "", true)
		if w.Code != http.StatusOK || !strings.HasSuffix(strings.TrimSpace(w.Body.String()), "</html>") {
			t.Fatalf("%s: %d\n%s", path, w.Code, tail(w.Body.String()))
		}
		return strings.Contains(w.Body.String(), want)
	}
	for path, want := range map[string]string{
		"/ui/analytics?period=all":            "Passed through",
		"/ui/income?period=all":               "only passed through the card",
		"/ui/operations?m=%23cash&period=all": "is not counted as spending",
		"/ui/operations?type=all&period=all":  "passed through",
	} {
		if !has(path, want) {
			t.Errorf("%s lacks %q", path, want)
		}
	}

	// turned off, every withdrawal is spending again
	w := post(h, "/ui/cash", url.Values{"action": {"passthrough"}, "on": {"0"}, "back": {"/ui/operations?m=%23cash"}})
	if w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/ui/operations?") || k.n != 1 {
		t.Fatalf("switch: %d %s", w.Code, w.Header().Get("Location"))
	}
	if on, _ := st.PassThroughCash(ctx); on {
		t.Fatal("still on")
	}
	if has("/ui/analytics?period=all", "Passed through") || !has("/ui/operations?m=%23cash&period=all", "Every cash withdrawal counts") {
		t.Error("off: cash still left out")
	}

	// skipped by hand for a period, and counted again
	period := url.Values{"from": {"2026-08-01"}, "to": {"2026-08-31"}, "back": {"https://evil.example/"}}
	period.Set("action", "skip")
	if w := post(h, "/ui/cash", period); !strings.HasPrefix(w.Header().Get("Location"), "/ui/operations?msg=1+cash") {
		t.Errorf("skip: %s", w.Header().Get("Location"))
	}
	if tx, _ := st.GetTx(ctx, cash); tx.Status != store.StatusIgnored {
		t.Errorf("skipped: %s", tx.Status)
	}
	period.Set("action", "count")
	post(h, "/ui/cash", period)
	if tx, _ := st.GetTx(ctx, cash); tx.Status != store.StatusReview {
		t.Errorf("counted again: %s", tx.Status)
	}
	period.Set("to", "2026-07-01")
	if w := post(h, "/ui/cash", period); w.Code != http.StatusBadRequest {
		t.Errorf("backwards period: %d", w.Code)
	}
}
