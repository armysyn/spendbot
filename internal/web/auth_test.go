package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"spendbot/internal/store"
)

// browser keeps cookies between requests, like a real one.
type browser struct {
	h       http.Handler
	cookies []*http.Cookie
	ip      string
}

func (b *browser) req(method, path string, form url.Values) *httptest.ResponseRecorder {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, path, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if b.ip != "" {
		r.RemoteAddr = b.ip + ":5555"
	}
	for _, c := range b.cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	b.h.ServeHTTP(w, r)
	for _, c := range w.Result().Cookies() {
		kept := b.cookies[:0]
		for _, o := range b.cookies {
			if o.Name != c.Name {
				kept = append(kept, o)
			}
		}
		b.cookies = kept
		if c.MaxAge >= 0 && c.Value != "" {
			b.cookies = append(b.cookies, c)
		}
	}
	return w
}

func openServer(t *testing.T) (http.Handler, *Gate) {
	t.Helper()
	st, err := store.Open(context.Background(), t.TempDir()+"/t.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	mux := http.NewServeMux()
	g := gateFor(t, mux, New(st, nil, 3, time.Minute, almaty, slog.New(slog.NewTextHandler(io.Discard, nil))), "")
	return mux, g
}

func firstHash(t *testing.T, g *Gate) string {
	t.Helper()
	a, err := g.reg.Get(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	return a.Hash
}

func TestPasswordFlow(t *testing.T) {
	h, g := openServer(t)
	me := &browser{h: h, ip: "192.0.2.10"}
	other := &browser{h: h, ip: "192.0.2.20"}

	// no password: open, with a warning on every page
	if w := other.req("GET", "/ui", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "No password.") {
		t.Fatalf("open page: %d", w.Code)
	}
	// set one: too short, different, then fine — this browser stays signed in
	if loc := me.req("POST", "/ui/security", url.Values{"action": {"set"}, "password": {"short"}, "repeat": {"short"}}).Header().Get("Location"); !strings.Contains(loc, "8") {
		t.Errorf("short password: %s", loc)
	}
	me.req("POST", "/ui/security", url.Values{"action": {"set"}, "password": {"blue lamp 42"}, "repeat": {"blue lamp 41"}})
	if v := firstHash(t, g); v != "" {
		t.Fatal("different passwords must not be saved")
	}
	me.req("POST", "/ui/security", url.Values{"action": {"set"}, "password": {"blue lamp 42"}, "repeat": {"blue lamp 42"}})
	if v := firstHash(t, g); !strings.HasPrefix(v, "pbkdf2-sha256$") || strings.Contains(v, "blue lamp") {
		t.Fatalf("stored: %q", v)
	}
	if w := me.req("GET", "/ui", nil); w.Code != 200 || strings.Contains(w.Body.String(), "No password.") || !strings.Contains(w.Body.String(), "Sign out") {
		t.Fatalf("the browser that set it stays in: %d", w.Code)
	}
	// someone else is sent to sign in and cannot post
	if w := other.req("GET", "/ui/operations", nil); w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "/ui/login") {
		t.Fatalf("other browser: %d", w.Code)
	}
	if w := other.req("POST", "/ui/security", url.Values{"action": {"remove"}}); w.Code != http.StatusUnauthorized {
		t.Fatalf("other browser removing the password: %d", w.Code)
	}
	// wrong password, then right — back to where they wanted to go
	if w := other.req("POST", "/ui/login", url.Values{"password": {"nope nope"}, "next": {"/ui/operations"}}); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "Wrong password") {
		t.Fatalf("wrong password: %d", w.Code)
	}
	if w := other.req("POST", "/ui/login", url.Values{"password": {"blue lamp 42"}, "next": {"/ui/operations"}}); w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/ui/operations" {
		t.Fatalf("sign in: %d %s", w.Code, w.Header().Get("Location"))
	}
	if w := other.req("GET", "/ui/operations", nil); w.Code != 200 {
		t.Fatalf("signed in: %d", w.Code)
	}
	// "next" never leads off the site
	if w := (&browser{h: h}).req("POST", "/ui/login", url.Values{"password": {"blue lamp 42"}, "next": {"https://evil.example/"}}); w.Header().Get("Location") != "/ui" {
		t.Errorf("next off the site: %s", w.Header().Get("Location"))
	}
	// sign out every other browser
	me.req("POST", "/ui/security", url.Values{"action": {"signout_others"}})
	if w := other.req("GET", "/ui", nil); w.Code != http.StatusSeeOther {
		t.Errorf("other browser must be signed out: %d", w.Code)
	}
	// changing needs the current password
	me.req("POST", "/ui/security", url.Values{"action": {"set"}, "current": {"wrong one"}, "password": {"green lamp 7"}, "repeat": {"green lamp 7"}})
	if w := (&browser{h: h}).req("POST", "/ui/login", url.Values{"password": {"blue lamp 42"}}); w.Code != http.StatusSeeOther {
		t.Error("a wrong current password must not change it")
	}
	// sign out
	me.req("POST", "/ui/logout", url.Values{})
	if w := me.req("GET", "/ui", nil); w.Code != http.StatusSeeOther {
		t.Errorf("signed out: %d", w.Code)
	}
	// remove it again: open
	me.req("POST", "/ui/login", url.Values{"password": {"blue lamp 42"}})
	me.req("POST", "/ui/security", url.Values{"action": {"remove"}, "current": {"blue lamp 42"}})
	if w := other.req("GET", "/ui", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "No password.") {
		t.Errorf("removed: %d", w.Code)
	}
}

func TestGuessingIsSlowedDown(t *testing.T) {
	h, _ := openServer(t)
	me := &browser{h: h, ip: "192.0.2.1"}
	me.req("POST", "/ui/security", url.Values{"action": {"set"}, "password": {"blue lamp 42"}, "repeat": {"blue lamp 42"}})
	thief := &browser{h: h, ip: "192.0.2.66"}
	for range maxFails {
		thief.req("POST", "/ui/login", url.Values{"password": {"guess guess"}})
	}
	// locked now: even the right password waits
	if w := thief.req("POST", "/ui/login", url.Values{"password": {"blue lamp 42"}}); w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "Too many wrong passwords") {
		t.Fatalf("locked: %d", w.Code)
	}
	// another address is not affected
	if w := (&browser{h: h, ip: "192.0.2.2"}).req("POST", "/ui/login", url.Values{"password": {"blue lamp 42"}}); w.Code != http.StatusSeeOther {
		t.Fatalf("other address: %d", w.Code)
	}
	if w := thief.req("GET", "/ui/login", nil); !strings.HasSuffix(strings.TrimSpace(w.Body.String()), "</html>") {
		t.Error("login page cut short")
	}
}

func TestGuardLocks(t *testing.T) {
	var g guard
	now := time.Now()
	for range maxFails {
		g.fail("a", now)
	}
	if w := g.locked("a", now); w != firstLock {
		t.Errorf("first lock %s", w)
	}
	for range maxFails {
		g.fail("a", now)
	}
	if w := g.locked("a", now); w != 2*firstLock {
		t.Errorf("second lock %s", w)
	}
	g.ok("a")
	if g.locked("a", now) != 0 {
		t.Error("success clears")
	}
}
