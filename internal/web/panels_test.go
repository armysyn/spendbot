package web

import (
	"net/http"
	"strings"
	"testing"
)

func TestPanels(t *testing.T) {
	h, st, _ := setup(t)
	seedOps(t, st)
	get := func(p string) (int, string) {
		w := do(h, "GET", p, nil, "", true)
		return w.Code, w.Body.String()
	}
	// Groceries: only Coffee Boom, 1,500 — the panel says so and links to the filtered operations
	code, b := get("/ui/panel/category?name=Groceries&period=all")
	if code != 200 || !strings.Contains(b, "1,500") || !strings.Contains(b, `data-title="Groceries"`) ||
		!strings.Contains(b, "/ui/operations?cat=Groceries&amp;period=all") || strings.Contains(b, "<html") {
		t.Fatalf("category panel %d:\n%s", code, b)
	}
	// a merchant over a picked range
	if _, b = get("/ui/panel/merchant?m=magnum&from=2026-08-01&to=2026-08-31"); !strings.Contains(b, "Magnum") || !strings.Contains(b, "5,700") {
		t.Errorf("merchant panel:\n%s", b)
	}
	// a day: everything on it, spending summed
	if _, b = get("/ui/panel/day?d=2026-08-04"); !strings.Contains(b, "Cash withdrawal") || !strings.Contains(b, "/ui/operations?from=2026-08-04") {
		t.Errorf("day panel:\n%s", b)
	}
	if code, _ = get("/ui/panel/nope"); code != http.StatusNotFound {
		t.Errorf("unknown panel: %d", code)
	}
	if code, _ = get("/ui/panel/day?d=junk"); code != http.StatusNotFound {
		t.Errorf("bad day: %d", code)
	}
	// the charts open panels
	if _, b = get("/ui/trends"); !strings.Contains(b, `data-panel="/ui/panel/category?`) || !strings.Contains(b, `class="year-heat"`) || !strings.Contains(b, `data-panel="/ui/panel/day?`) {
		t.Error("trends structure must open panels")
	}
	if _, b = get("/ui/analytics?period=all"); !strings.Contains(b, `data-panel="/ui/panel/merchant?`) || !strings.Contains(b, `data-panel="/ui/panel/day?`) {
		t.Error("analytics must open panels")
	}
	if _, b = get("/ui"); !strings.Contains(b, `id="drawer"`) || !strings.Contains(b, ".drawer[hidden]") {
		t.Error("the drawer is on every page, hidden until opened")
	}
}
