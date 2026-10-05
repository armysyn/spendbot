package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"spendbot/internal/store"
)

func TestIssuesFlow(t *testing.T) {
	h, st, _ := setup(t)
	ctx := context.Background()
	w := post(h, "/ui/issues", url.Values{"kind": {"bug"}, "title": {"  Income chart is empty  "}, "body": {"Steps:\n1. open\n2. look"},
		"priority": {"high"}, "page": {"/ui/income?period=all"}})
	if w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/ui/issues/1?") {
		t.Fatalf("create: %d %s", w.Code, w.Header().Get("Location"))
	}
	is, err := st.Issue(ctx, 1)
	if err != nil || is.Title != "Income chart is empty" || is.Kind != "bug" || is.Priority != "high" || is.Page != "/ui/income?period=all" {
		t.Fatalf("issue %+v %v", is, err)
	}
	// a page outside the site is dropped, an empty title refused
	post(h, "/ui/issues", url.Values{"title": {"Idea"}, "page": {"https://evil.example"}})
	if i2, _ := st.Issue(ctx, 2); i2.Page != "" || i2.Kind != "idea" || i2.Priority != "normal" {
		t.Errorf("defaults: %+v", i2)
	}
	if w := post(h, "/ui/issues", url.Values{"title": {"  "}}); !strings.Contains(w.Header().Get("Location"), "new=1") {
		t.Error("an empty title must be refused")
	}

	page := do(h, "GET", "/ui/issues/1", nil, "", true).Body.String()
	if !strings.Contains(page, "Steps:\n1. open") || !strings.HasSuffix(strings.TrimSpace(page), "</html>") {
		t.Fatalf("issue page:\n%s", tail(page))
	}
	post(h, "/ui/issues/1", url.Values{"action": {"comment"}, "body": {"Fixed in the cash flow"}, "close": {"1"}})
	is, _ = st.Issue(ctx, 1)
	if is.Status != store.IssueClosed || is.ClosedAt == nil || is.CommentCount != 1 {
		t.Errorf("comment and close: %+v", is)
	}
	post(h, "/ui/issues/1", url.Values{"action": {"reopen"}})
	if is, _ = st.Issue(ctx, 1); is.Status != store.IssueOpen || is.ClosedAt != nil {
		t.Errorf("reopen: %+v", is)
	}
	post(h, "/ui/issues/1", url.Values{"action": {"edit"}, "kind": {"idea"}, "title": {"New title"}, "priority": {"low"}})
	if is, _ = st.Issue(ctx, 1); is.Title != "New title" || is.Kind != "idea" || is.Priority != "low" {
		t.Errorf("edit: %+v", is)
	}

	for _, p := range []string{"/ui/issues", "/ui/issues?status=all&kind=idea&q=title", "/ui/issues?new=1&page=/ui/income", "/ui/issues?status=closed"} {
		b := do(h, "GET", p, nil, "", true).Body.String()
		if !strings.HasSuffix(strings.TrimSpace(b), "</html>") {
			t.Errorf("%s cut short", p)
		}
	}
	if list := do(h, "GET", "/ui/issues?q=new+title", nil, "", true).Body.String(); !strings.Contains(list, "New title") || strings.Contains(list, `href="/ui/issues/2"`) {
		t.Error("search")
	}
	// every page has the report link with its own address
	if b := do(h, "GET", "/ui/income", nil, "", true).Body.String(); !strings.Contains(b, "/ui/issues?new=1&amp;page=%2Fui%2Fincome") {
		t.Error("report link")
	}
	post(h, "/ui/issues/1", url.Values{"action": {"delete"}})
	if _, err := st.Issue(ctx, 1); err != store.ErrNotFound {
		t.Errorf("delete: %v", err)
	}
	if w := do(h, "GET", "/ui/issues/99", nil, "", true); w.Code != http.StatusNotFound {
		t.Errorf("missing issue: %d", w.Code)
	}
}
