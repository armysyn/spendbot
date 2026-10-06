package web

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"spendbot/internal/store"
)

func setPassword(b *browser, pw string) *http.Response {
	return b.req("POST", "/ui/security", url.Values{"action": {"set"}, "password": {pw}, "repeat": {pw}}).Result()
}

// Two people, two accounts: each sees only their own money, the password picks the account.
func TestAccounts(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	seedOps(t, st) // the first account's money: Magnum, Coffee Boom…
	mux := http.NewServeMux()
	g := gateFor(t, mux, New(st, nil, 3, time.Minute, almaty, slogDiscard()), "")
	me := &browser{h: mux, ip: "192.0.2.1"}

	// a second account needs a password on this one first
	me.req("POST", "/ui/security", url.Values{"action": {"create"}, "name": {"Anna"}, "password": {"anna pass 1"}, "repeat": {"anna pass 1"}})
	if list, _ := g.reg.List(ctx); len(list) != 1 {
		t.Fatal("an account without a password must not create another")
	}
	setPassword(me, "my pass 123")
	// the same password as mine is refused
	if loc := me.req("POST", "/ui/security", url.Values{"action": {"create"}, "name": {"Anna"}, "password": {"my pass 123"}, "repeat": {"my pass 123"}}).Header().Get("Location"); !strings.Contains(loc, "Another+account") {
		t.Errorf("same password: %s", loc)
	}
	me.req("POST", "/ui/security", url.Values{"action": {"create"}, "name": {"Anna"}, "password": {"anna pass 1"}, "repeat": {"anna pass 1"}})
	if list, _ := g.reg.List(ctx); len(list) != 2 || list[1].Name != "Anna" {
		t.Fatalf("accounts: %+v", list)
	}
	// the password picks the account: Anna sees an empty account, not my money
	anna := &browser{h: mux, ip: "192.0.2.2"}
	anna.req("POST", "/ui/login", url.Values{"password": {"anna pass 1"}})
	page := anna.req("GET", "/ui/operations?period=all", nil).Body.String()
	if strings.Contains(page, "Magnum") || !strings.Contains(page, "Anna") {
		t.Fatalf("Anna sees someone else's money or not her name:\n%s", tail(page))
	}
	if !strings.Contains(me.req("GET", "/ui/operations?period=all", nil).Body.String(), "Magnum") {
		t.Fatal("my money is gone from my account")
	}
	// every page renders to the end for a signed-in browser with several accounts
	for _, p := range []string{"/ui/security", "/ui", "/ui/income", "/ui/issues"} {
		if b := anna.req("GET", p, nil).Body.String(); !strings.HasSuffix(strings.TrimSpace(b), "</html>") {
			t.Errorf("%s cut short:\n%s", p, tail(b))
		}
	}
	// what Anna writes stays in her account
	anna.req("POST", "/ui/issues", url.Values{"title": {"Anna's wish"}})
	if strings.Contains(me.req("GET", "/ui/issues?status=all", nil).Body.String(), "Anna&#39;s wish") {
		t.Error("issues leak between accounts")
	}
	// with two accounts a password cannot be removed
	if loc := me.req("POST", "/ui/security", url.Values{"action": {"remove"}, "current": {"my pass 123"}}).Header().Get("Location"); !strings.Contains(loc, "every+one+needs+a+password") {
		t.Errorf("remove with two accounts: %s", loc)
	}
	// the first account cannot be deleted; Anna's can, with her password
	if loc := me.req("POST", "/ui/security", url.Values{"action": {"delete"}, "current": {"my pass 123"}}).Header().Get("Location"); !strings.Contains(loc, "cannot+be+deleted") {
		t.Errorf("delete first: %s", loc)
	}
	anna.req("POST", "/ui/security", url.Values{"action": {"delete"}, "current": {"anna pass 1"}})
	if list, _ := g.reg.List(ctx); len(list) != 1 {
		t.Fatalf("after delete: %+v", list)
	}
	if w := (&browser{h: mux}).req("POST", "/ui/login", url.Values{"password": {"anna pass 1"}}); w.Code != http.StatusUnauthorized {
		t.Errorf("a deleted account's password: %d", w.Code)
	}
}

// A statement of another person is held, not imported: import it here anyway, or make an
// account for that person. Needs a real statement in statements/.
func TestForeignStatement(t *testing.T) {
	pdf, err := os.ReadFile("../../statements/gold_statement.pdf")
	if err != nil {
		t.Skip("no statement in statements/")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir()+"/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	mux := http.NewServeMux()
	g := gateFor(t, mux, New(st, nil, 3, time.Minute, almaty, slogDiscard()), "")
	me := &browser{h: mux, ip: "192.0.2.1"}
	setPassword(me, "my pass 123")
	g.reg.SetHolder(ctx, 1, "Someone Else Entirely")

	upload := func() string {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		fw, _ := mw.CreateFormFile("pdf", "statement.pdf")
		fw.Write(pdf)
		mw.Close()
		r, _ := http.NewRequest("POST", "/ui/import", io.NopCloser(&body))
		r.Header.Set("Content-Type", mw.FormDataContentType())
		for _, c := range me.cookies {
			r.AddCookie(c)
		}
		w := newRecorder()
		mux.ServeHTTP(w, r)
		return w.Body.String()
	}
	page := upload()
	if !strings.Contains(page, "another person") || !strings.Contains(page, `name="upload"`) {
		t.Fatalf("no warning:\n%s", tail(page))
	}
	if ops, _ := st.Ledger(ctx, almaty); len(ops) != 0 {
		t.Fatal("a held statement must not be imported")
	}
	id := between(page, `name="upload" value="`, `"`)
	// a new account for that person: imported there, this browser signed in to it
	w := me.req("POST", "/ui/import/new-account", url.Values{"upload": {id}, "name": {"Statement owner"}, "password": {"owner pass 9"}, "repeat": {"owner pass 9"}})
	if w.Code != http.StatusSeeOther || !strings.Contains(w.Header().Get("Location"), "created") {
		t.Fatalf("new account: %d %s", w.Code, w.Header().Get("Location"))
	}
	list, _ := g.reg.List(ctx)
	if len(list) != 2 || list[1].Holder == "" || list[1].Holder == "Someone Else Entirely" {
		t.Fatalf("new account: %+v", list)
	}
	if !strings.Contains(me.req("GET", "/ui/operations?period=all&type=all", nil).Body.String(), "Statement owner") {
		t.Error("the browser must be in the new account")
	}
	if ops, _ := st.Ledger(ctx, almaty); len(ops) != 0 {
		t.Error("the first account must stay empty")
	}
	// back in my account, "import here anyway" puts it into mine
	me.req("POST", "/ui/logout", url.Values{})
	me.req("POST", "/ui/login", url.Values{"password": {"my pass 123"}})
	page = upload()
	id = between(page, `name="upload" value="`, `"`)
	me.req("POST", "/ui/import", url.Values{"upload": {id}, "confirm": {"1"}})
	if ops, _ := st.Ledger(ctx, almaty); len(ops) == 0 {
		t.Error("import anyway")
	}
	if a, _ := g.reg.Get(ctx, 1); a.Holder != "Someone Else Entirely" {
		t.Error("importing anyway keeps the account's holder")
	}
}

func between(s, a, b string) string {
	i := strings.Index(s, a)
	if i < 0 {
		return ""
	}
	s = s[i+len(a):]
	if j := strings.Index(s, b); j >= 0 {
		return s[:j]
	}
	return s
}
