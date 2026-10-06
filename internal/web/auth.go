package web

import (
	"context"
	"crypto/subtle"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"spendbot/internal/auth"
	"spendbot/internal/store"
)

// Signing in: one password for the page, set on the Security page (stored as a PBKDF2 hash)
// or, as before, with WEB_PASSWORD. A signed-in browser holds a random session token in a
// cookie for 30 days, extended while it is used. Without a password the page is open, and
// every page says so.

const (
	sessionCookie = "spendbot_session"
	sessionTTL    = 30 * 24 * time.Hour
	kvPassword    = store.KVWebPassword
	// After maxFails wrong passwords from one address, sign-in waits, doubling up to maxLock.
	maxFails  = 5
	firstLock = time.Minute
	maxLock   = 15 * time.Minute
)

type attempts struct {
	fails int
	until time.Time
	lock  time.Duration
}

// guard limits password guessing per client address.
type guard struct {
	mu sync.Mutex
	m  map[string]*attempts
}

// locked reports how long an address still has to wait.
func (g *guard) locked(ip string, now time.Time) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	if a := g.m[ip]; a != nil && now.Before(a.until) {
		return a.until.Sub(now)
	}
	return 0
}

func (g *guard) fail(ip string, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.m == nil {
		g.m = map[string]*attempts{}
	}
	a := g.m[ip]
	if a == nil {
		a = &attempts{}
		g.m[ip] = a
	}
	a.fails++
	if a.fails >= maxFails {
		a.lock = min(max(a.lock*2, firstLock), maxLock)
		a.until, a.fails = now.Add(a.lock), 0
	}
}

func (g *guard) ok(ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.m, ip)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// passwordHash is the stored hash, cached; empty — none set on the page.
func (s *Server) passwordHash(ctx context.Context) string {
	s.pwMu.Lock()
	defer s.pwMu.Unlock()
	if !s.pwLoaded {
		h, err := s.st.GetKV(ctx, kvPassword)
		if err != nil {
			s.log.Error("web: read password", "err", err)
			return ""
		}
		s.pwHash, s.pwLoaded = h, true
	}
	return s.pwHash
}

func (s *Server) setPasswordHash(ctx context.Context, hash string) error {
	s.pwMu.Lock()
	defer s.pwMu.Unlock()
	var err error
	if hash == "" {
		err = s.st.DeleteKV(ctx, kvPassword)
	} else {
		err = s.st.SetKV(ctx, kvPassword, hash)
	}
	if err == nil {
		s.pwHash, s.pwLoaded = hash, true
	}
	return err
}

// hasPassword reports whether the page asks for a password.
func (s *Server) hasPassword(ctx context.Context) bool {
	return s.passwordHash(ctx) != "" || len(s.password) > 0
}

// HasPassword is for the startup log.
func (s *Server) HasPassword(ctx context.Context) bool { return s.hasPassword(ctx) }

// checkPassword compares with the page password; WEB_PASSWORD counts only while none is set on the page.
func (s *Server) checkPassword(ctx context.Context, pw string) bool {
	if h := s.passwordHash(ctx); h != "" {
		return auth.Verify(pw, h)
	}
	return len(s.password) > 0 && subtle.ConstantTimeCompare([]byte(pw), s.password) == 1
}

// session returns the id of a valid session from the cookie.
func (s *Server) session(r *http.Request) (string, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" || len(c.Value) > 200 {
		return "", false
	}
	id := auth.TokenHash(c.Value)
	ok, err := s.st.SessionValid(r.Context(), id, s.now())
	if err != nil {
		s.log.Error("web: session", "err", err)
	}
	return id, ok
}

// signedIn reports a signed-in browser, or a script sending the password with Basic auth.
func (s *Server) signedIn(r *http.Request) (string, bool) {
	if id, ok := s.session(r); ok {
		return id, true
	}
	if _, pass, ok := r.BasicAuth(); ok && s.checkPassword(r.Context(), pass) {
		return "", true
	}
	return "", false
}

// auth lets a request through when there is no password or the browser is signed in;
// otherwise a page asks to sign in and anything else gets 401.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ui/login" || !s.hasPassword(r.Context()) {
			next.ServeHTTP(w, r)
			return
		}
		id, ok := s.signedIn(r)
		if ok {
			if id != "" {
				// a session in use stays alive: its 30 days start again
				now := s.now()
				if err := s.st.TouchSession(r.Context(), id, now, now.Add(sessionTTL)); err != nil {
					s.log.Warn("web: touch session", "err", err)
				}
			}
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			http.Redirect(w, r, "/ui/login?next="+urlq(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		http.Error(w, "sign in first", http.StatusUnauthorized)
	})
}

func (s *Server) startSession(w http.ResponseWriter, r *http.Request) error {
	token, id, err := auth.NewToken()
	if err != nil {
		return err
	}
	now := s.now()
	agent := r.UserAgent()
	if len(agent) > 200 {
		agent = agent[:200]
	}
	if err := s.st.CreateSession(r.Context(), id, agent, now, now.Add(sessionTTL)); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil})
	return nil
}

func clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

type loginData struct {
	Page
	Next  string
	Error string
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	d := &loginData{Next: safeBack(r.FormValue("next"), "/ui")}
	if !s.hasPassword(r.Context()) {
		http.Redirect(w, r, d.Next, http.StatusSeeOther)
		return
	}
	if r.Method == http.MethodPost {
		ip := clientIP(r)
		now := s.now()
		switch wait := s.guard.locked(ip, now); {
		case wait > 0:
			d.Error = fmt.Sprintf("Too many wrong passwords. Try again in %s.", roundWait(wait))
		case !s.checkPassword(r.Context(), r.PostFormValue("password")):
			s.guard.fail(ip, now)
			s.log.Warn("web: wrong password", "ip", ip)
			d.Error = "Wrong password."
		default:
			s.guard.ok(ip)
			if err := s.startSession(w, r); err != nil {
				s.fail(w, err)
				return
			}
			s.log.Info("web: signed in", "ip", ip)
			http.Redirect(w, r, d.Next, http.StatusSeeOther)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}
	d.Bare = true
	d.Title, d.Nav = "Sign in", "login"
	s.render(w, "login.html", d)
}

func roundWait(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d seconds", int(d.Seconds()+0.5))
	}
	m := int(d.Minutes() + 0.5)
	return fmt.Sprintf("%d %s", m, plural(m, "minute", "minutes"))
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if id, ok := s.session(r); ok {
		if err := s.st.DeleteSession(r.Context(), id); err != nil {
			s.fail(w, err)
			return
		}
	}
	clearSession(w)
	http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
}

type securityData struct {
	Page
	Flash     string
	Has       bool // a password is asked for
	FromEnv   bool // only WEB_PASSWORD is set
	OnPage    bool // a password is set on the page
	Sessions  []store.Session
	CurrentID string
}

func (s *Server) security(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d := &securityData{Flash: r.URL.Query().Get("msg"), Has: s.hasPassword(ctx), OnPage: s.passwordHash(ctx) != ""}
	d.FromEnv = d.Has && !d.OnPage
	d.CurrentID, _ = s.session(r)
	var err error
	if d.Sessions, err = s.st.Sessions(ctx, s.now()); err != nil {
		s.fail(w, err)
		return
	}
	s.show(w, r, "security.html", "Security", "security", d)
}

// securityAction sets, changes and removes the password and signs browsers out.
func (s *Server) securityAction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	back := func(msg string) { http.Redirect(w, r, "/ui/security?msg="+urlq(msg), http.StatusSeeOther) }
	ip, now := clientIP(r), s.now()
	// changing or removing a password needs the current one, guarded like sign-in
	current := func() bool {
		if !s.hasPassword(ctx) {
			return true
		}
		if wait := s.guard.locked(ip, now); wait > 0 {
			back(fmt.Sprintf("Too many wrong passwords. Try again in %s.", roundWait(wait)))
			return false
		}
		if !s.checkPassword(ctx, r.PostFormValue("current")) {
			s.guard.fail(ip, now)
			back("The current password is wrong.")
			return false
		}
		s.guard.ok(ip)
		return true
	}
	keep, _ := s.session(r)
	switch r.PostFormValue("action") {
	case "set":
		pw := r.PostFormValue("password")
		if err := auth.Check(pw); err != nil {
			back(strings.ToUpper(err.Error()[:1]) + err.Error()[1:] + ".")
			return
		}
		if pw != r.PostFormValue("repeat") {
			back("The two passwords differ.")
			return
		}
		if !current() {
			return
		}
		hash, err := auth.Hash(pw)
		if err != nil {
			s.fail(w, err)
			return
		}
		if err := s.setPasswordHash(ctx, hash); err != nil {
			s.fail(w, err)
			return
		}
		// other browsers sign in again with the new password; this one stays in
		if err := s.st.DeleteSessions(ctx, keep); err != nil {
			s.fail(w, err)
			return
		}
		if keep == "" {
			if err := s.startSession(w, r); err != nil {
				s.fail(w, err)
				return
			}
		}
		s.log.Info("web: password set", "ip", ip)
		back("Password saved. Other browsers have to sign in again.")
	case "remove":
		if !current() {
			return
		}
		if err := s.setPasswordHash(ctx, ""); err != nil {
			s.fail(w, err)
			return
		}
		s.log.Warn("web: password removed", "ip", ip)
		if len(s.password) > 0 {
			back("The password set on this page is removed; WEB_PASSWORD from the settings file still applies.")
			return
		}
		back("Password removed: anyone on your network can open spendbot now.")
	case "signout_others":
		if err := s.st.DeleteSessions(ctx, keep); err != nil {
			s.fail(w, err)
			return
		}
		back("Every other browser is signed out.")
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
	}
}
