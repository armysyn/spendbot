package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestDeleteOperations(t *testing.T) {
	h, st, k := setup(t)
	ctx := context.Background()
	purchase, cash := seedOps(t, st)

	// one from its ✎ menu: gone, with Undo in the message
	w := post(h, "/ui/ops/delete", url.Values{"id": {itoa(purchase)}, "back": {"/ui/operations?period=all&undo=9"}})
	loc := w.Header().Get("Location")
	if w.Code != http.StatusSeeOther || !strings.Contains(loc, "msg=1+operation+deleted") || !strings.Contains(loc, "undo=") || k.n != 1 {
		t.Fatalf("delete: %d %s", w.Code, loc)
	}
	if _, err := st.GetTx(ctx, purchase); err == nil {
		t.Fatal("not deleted")
	}
	page := do(h, "GET", loc, nil, "", true).Body.String()
	if !strings.Contains(page, ">Undo</button>") || !strings.Contains(page, "1 deleted operation") {
		t.Errorf("no undo or trash link:\n%s", tail(page))
	}
	u, _ := url.Parse(loc)
	post(h, "/ui/trash/restore", url.Values{"id": {u.Query().Get("undo")}})
	if n, _ := st.TrashCount(ctx); n != 0 {
		t.Fatalf("undo left %d in the trash", n)
	}

	// several picked at once; the select mode renders checkboxes for the bulk form
	if body := do(h, "GET", "/ui/operations?period=all&type=all&select=1", nil, "", true).Body.String(); !strings.Contains(body, `form="bulk"`) ||
		!strings.Contains(body, `id="bulk"`) || !strings.HasSuffix(strings.TrimSpace(body), "</html>") {
		t.Fatal("select mode has no checkboxes")
	}
	rows, _ := st.Ledger(ctx, almaty)
	w = post(h, "/ui/ops/delete", url.Values{"id": {itoa(cash), itoa(rows[0].TxID)}, "back": {"https://evil.example/"}})
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "/ui/operations?") || !strings.Contains(loc, "2+operations+deleted") {
		t.Fatalf("bulk: %s", loc)
	}
	if body := do(h, "GET", "/ui/trash", nil, "", true).Body.String(); !strings.Contains(body, "Restore selected") ||
		!strings.Contains(body, "Cash withdrawal") || !strings.HasSuffix(strings.TrimSpace(body), "</html>") {
		t.Errorf("trash page:\n%s", tail(body))
	}
	if w := post(h, "/ui/ops/delete", url.Values{}); !strings.Contains(w.Header().Get("Location"), "Pick+the+operations") {
		t.Errorf("nothing picked: %s", w.Header().Get("Location"))
	}
}
