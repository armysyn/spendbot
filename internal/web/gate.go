package web

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"spendbot/internal/accounts"
	"spendbot/internal/auth"
)

// Gate stands in front of the pages: it signs browsers in, finds whose account a request is
// for and hands it to that account's Server. Every account has a database of its own; the
// sign-in page asks only for a password, and the password tells whose account it is, so two
// accounts never share one.
//
// With a single account and no password the pages are open — and say so on every page.

// Builder makes the Server of an account: its database, background analysis and settings.
type Builder func(ctx context.Context, a accounts.Account) (*Server, error)

type Gate struct {
	reg     *accounts.Registry
	build   Builder
	env     []byte // WEB_PASSWORD: the first account's password while none is set on the page
	uploads string // statements held while the person decides where they go
	log     *slog.Logger
	now     func() time.Time
	guard   guard

	mu      sync.Mutex
	servers map[int64]*Server
}

func NewGate(reg *accounts.Registry, build Builder, envPassword, uploads string, log *slog.Logger) *Gate {
	return &Gate{reg: reg, build: build, env: []byte(envPassword), uploads: uploads, log: log, now: time.Now,
		servers: map[int64]*Server{}}
}

// viewer is who a request comes from, put in the request context for the pages.
type viewer struct {
	Account   accounts.Account
	Accounts  int    // how many accounts there are
	Open      bool   // no password at all: anyone on the network gets in
	SessionID string // empty — no session (open pages or Basic auth)
}

type viewerKey struct{}

func viewerFrom(ctx context.Context) viewer {
	v, _ := ctx.Value(viewerKey{}).(viewer)
	return v
}

// server returns the account's Server, building it on first use.
func (g *Gate) server(ctx context.Context, a accounts.Account) (*Server, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if s, ok := g.servers[a.ID]; ok {
		return s, nil
	}
	s, err := g.build(ctx, a)
	if err != nil {
		return nil, err
	}
	s.gate = g
	g.servers[a.ID] = s
	return s, nil
}

// Start builds the Servers of every account, so each one's background analysis runs from the start.
func (g *Gate) Start(ctx context.Context) error {
	list, err := g.reg.List(ctx)
	if err != nil {
		return err
	}
	for _, a := range list {
		if _, err := g.server(ctx, a); err != nil {
			return fmt.Errorf("account %d: %w", a.ID, err)
		}
	}
	return nil
}

// HasPassword reports whether the pages ask for a password.
func (g *Gate) HasPassword(ctx context.Context) bool {
	list, err := g.reg.List(ctx)
	if err != nil {
		return true
	}
	return !g.open(list)
}

func (g *Gate) open(list []accounts.Account) bool {
	return len(list) == 1 && list[0].Hash == "" && len(g.env) == 0
}

// match finds the account a password belongs to.
func (g *Gate) match(list []accounts.Account, pw string) (accounts.Account, bool) {
	for _, a := range list {
		switch {
		case a.Hash != "" && auth.Verify(pw, a.Hash):
			return a, true
		case a.Hash == "" && a.Primary() && len(g.env) > 0 && subtle.ConstantTimeCompare([]byte(pw), g.env) == 1:
			return a, true
		}
	}
	return accounts.Account{}, false
}

// Register mounts the pages: everything under /ui, and / sends there.
func (g *Gate) Register(mux *http.ServeMux) {
	protect := http.NewCrossOriginProtection()
	protect.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.log.Warn("web: cross-origin request denied", "method", r.Method, "path", r.URL.Path,
			"origin", r.Header.Get("Origin"), "sec_fetch_site", r.Header.Get("Sec-Fetch-Site"))
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
	}))
	h := protect.Handler(http.HandlerFunc(g.serve))
	mux.Handle("/ui", h)
	mux.Handle("/ui/", h)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui", http.StatusFound)
	})
}

func (g *Gate) serve(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	list, err := g.reg.List(ctx)
	if err != nil || len(list) == 0 {
		g.log.Error("web: accounts", "err", err)
		http.Error(w, "internal error, see the logs", http.StatusInternalServerError)
		return
	}
	if r.URL.Path == "/ui/login" {
		g.login(w, r, list)
		return
	}
	v := viewer{Accounts: len(list)}
	if g.open(list) {
		v.Account, v.Open = list[0], true
	} else if a, id, ok := g.signedIn(r, list); ok {
		v.Account, v.SessionID = a, id
	} else {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			http.Redirect(w, r, "/ui/login?next="+urlq(r.URL.RequestURI()), http.StatusSeeOther)
			return
		}
		http.Error(w, "sign in first", http.StatusUnauthorized)
		return
	}
	s, err := g.server(ctx, v.Account)
	if err != nil {
		g.log.Error("web: account server", "account", v.Account.ID, "err", err)
		http.Error(w, "internal error, see the logs", http.StatusInternalServerError)
		return
	}
	r = r.WithContext(context.WithValue(ctx, viewerKey{}, v))
	switch {
	case r.URL.Path == "/ui/logout" && r.Method == http.MethodPost:
		g.logout(w, r, v)
	case r.URL.Path == "/ui/security" && r.Method == http.MethodGet:
		g.security(w, r, s, v, list)
	case r.URL.Path == "/ui/security" && r.Method == http.MethodPost:
		g.securityAction(w, r, v, list)
	case r.URL.Path == "/ui/import/new-account" && r.Method == http.MethodPost:
		g.importNewAccount(w, r, v, list)
	default:
		s.Handler().ServeHTTP(w, r)
	}
}

// signedIn finds the account of a browser by its session cookie, or of a script by the
// password sent with Basic auth.
func (g *Gate) signedIn(r *http.Request, list []accounts.Account) (accounts.Account, string, bool) {
	ctx := r.Context()
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" && len(c.Value) < 200 {
		id := auth.TokenHash(c.Value)
		accID, ok, err := g.reg.SessionAccount(ctx, id, g.now())
		if err != nil {
			g.log.Error("web: session", "err", err)
		}
		if ok {
			for _, a := range list {
				if a.ID == accID {
					now := g.now()
					if err := g.reg.TouchSession(ctx, id, now, now.Add(sessionTTL)); err != nil {
						g.log.Warn("web: touch session", "err", err)
					}
					return a, id, true
				}
			}
		}
	}
	if _, pass, ok := r.BasicAuth(); ok {
		if a, ok := g.match(list, pass); ok {
			return a, "", true
		}
	}
	return accounts.Account{}, "", false
}

// startSession signs this browser in to an account, replacing any session it had.
func (g *Gate) startSession(w http.ResponseWriter, r *http.Request, accountID int64) error {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		_ = g.reg.DeleteSession(r.Context(), auth.TokenHash(c.Value))
	}
	token, id, err := auth.NewToken()
	if err != nil {
		return err
	}
	now := g.now()
	agent := r.UserAgent()
	if len(agent) > 200 {
		agent = agent[:200]
	}
	if err := g.reg.CreateSession(r.Context(), id, accountID, agent, now, now.Add(sessionTTL)); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: token, Path: "/", MaxAge: int(sessionTTL.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: r.TLS != nil})
	return nil
}

func (g *Gate) fail(w http.ResponseWriter, err error) {
	g.log.Error("web", "err", err)
	http.Error(w, "internal error, see the logs", http.StatusInternalServerError)
}

type loginData struct {
	Page
	Next  string
	Error string
}

func (g *Gate) login(w http.ResponseWriter, r *http.Request, list []accounts.Account) {
	d := &loginData{Next: safeBack(r.FormValue("next"), "/ui")}
	if g.open(list) {
		http.Redirect(w, r, d.Next, http.StatusSeeOther)
		return
	}
	if r.Method == http.MethodPost {
		ip, now := clientIP(r), g.now()
		switch wait := g.guard.locked(ip, now); {
		case wait > 0:
			d.Error = fmt.Sprintf("Too many wrong passwords. Try again in %s.", roundWait(wait))
		default:
			a, ok := g.match(list, r.PostFormValue("password"))
			if !ok {
				g.guard.fail(ip, now)
				g.log.Warn("web: wrong password", "ip", ip)
				d.Error = "Wrong password."
				break
			}
			g.guard.ok(ip)
			if err := g.startSession(w, r, a.ID); err != nil {
				g.fail(w, err)
				return
			}
			g.log.Info("web: signed in", "ip", ip, "account", a.ID)
			http.Redirect(w, r, d.Next, http.StatusSeeOther)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}
	s, err := g.server(r.Context(), list[0])
	if err != nil {
		g.fail(w, err)
		return
	}
	d.Bare, d.Title, d.Nav = true, "Sign in", "login"
	s.render(w, "login.html", d)
}

func (g *Gate) logout(w http.ResponseWriter, r *http.Request, v viewer) {
	if v.SessionID != "" {
		if err := g.reg.DeleteSession(r.Context(), v.SessionID); err != nil {
			g.fail(w, err)
			return
		}
	}
	clearSession(w)
	http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
}

type accountRow struct {
	accounts.Account
	Current bool
}

type securityData struct {
	Page
	Flash    string
	Has      bool // this account asks for a password
	FromEnv  bool // only WEB_PASSWORD is set
	OnPage   bool // a password is set on the page
	Name     string
	Holder   string
	Primary  bool
	Sessions []accounts.Session
	Current  string
	Accounts []accountRow
}

func (g *Gate) security(w http.ResponseWriter, r *http.Request, s *Server, v viewer, list []accounts.Account) {
	ctx := r.Context()
	a := v.Account
	d := &securityData{Flash: r.URL.Query().Get("msg"), OnPage: a.Hash != "", Name: a.Name, Holder: a.Holder,
		Primary: a.Primary(), Current: v.SessionID}
	d.Has = d.OnPage || a.Primary() && len(g.env) > 0
	d.FromEnv = d.Has && !d.OnPage
	var err error
	if d.Sessions, err = g.reg.Sessions(ctx, a.ID, g.now()); err != nil {
		g.fail(w, err)
		return
	}
	for _, x := range list {
		d.Accounts = append(d.Accounts, accountRow{Account: x, Current: x.ID == a.ID})
	}
	s.show(w, r, "security.html", "Security", "security", d)
}

// unique reports whether a new password is free: the password tells whose account it is.
func (g *Gate) unique(list []accounts.Account, pw string, self int64) bool {
	for _, a := range list {
		if a.ID != self && a.Hash != "" && auth.Verify(pw, a.Hash) {
			return false
		}
	}
	return true
}

// newPassword checks a new password and its repeat and hashes it; problem is shown to the person.
func (g *Gate) newPassword(r *http.Request, list []accounts.Account, self int64) (hash, problem string) {
	pw := r.PostFormValue("password")
	if err := auth.Check(pw); err != nil {
		return "", strings.ToUpper(err.Error()[:1]) + err.Error()[1:] + "."
	}
	if pw != r.PostFormValue("repeat") {
		return "", "The two passwords differ."
	}
	if !g.unique(list, pw, self) {
		return "", "Another account uses this password — pick a different one: the password tells whose account it is."
	}
	hash, err := auth.Hash(pw)
	if err != nil {
		return "", "Could not save the password, see the logs."
	}
	return hash, ""
}

// securityAction: the password, sessions and accounts of the person signed in.
func (g *Gate) securityAction(w http.ResponseWriter, r *http.Request, v viewer, list []accounts.Account) {
	ctx := r.Context()
	back := func(msg string) { http.Redirect(w, r, "/ui/security?msg="+urlq(msg), http.StatusSeeOther) }
	ip, now := clientIP(r), g.now()
	a := v.Account
	hasPassword := a.Hash != "" || a.Primary() && len(g.env) > 0
	// changing a password, deleting an account need the current password, guarded like sign-in
	current := func() bool {
		if !hasPassword {
			return true
		}
		if wait := g.guard.locked(ip, now); wait > 0 {
			back(fmt.Sprintf("Too many wrong passwords. Try again in %s.", roundWait(wait)))
			return false
		}
		if m, ok := g.match([]accounts.Account{a}, r.PostFormValue("current")); !ok || m.ID != a.ID {
			g.guard.fail(ip, now)
			back("The current password is wrong.")
			return false
		}
		g.guard.ok(ip)
		return true
	}
	switch r.PostFormValue("action") {
	case "set":
		hash, problem := g.newPassword(r, list, a.ID)
		if problem != "" {
			back(problem)
			return
		}
		if !current() {
			return
		}
		if err := g.reg.SetPassword(ctx, a.ID, hash); err != nil {
			g.fail(w, err)
			return
		}
		// other browsers sign in again with the new password; this one stays in
		if err := g.reg.DeleteSessions(ctx, a.ID, v.SessionID); err != nil {
			g.fail(w, err)
			return
		}
		if v.SessionID == "" {
			if err := g.startSession(w, r, a.ID); err != nil {
				g.fail(w, err)
				return
			}
		}
		g.log.Info("web: password set", "ip", ip, "account", a.ID)
		back("Password saved. Other browsers have to sign in again.")
	case "remove":
		if len(list) > 1 {
			back("With several accounts every one needs a password: the password tells whose account it is.")
			return
		}
		if !current() {
			return
		}
		if err := g.reg.SetPassword(ctx, a.ID, ""); err != nil {
			g.fail(w, err)
			return
		}
		g.log.Warn("web: password removed", "ip", ip)
		if len(g.env) > 0 {
			back("The password set on this page is removed; WEB_PASSWORD from the settings file still applies.")
			return
		}
		back("Password removed: anyone on your network can open spendbot now.")
	case "signout_others":
		if err := g.reg.DeleteSessions(ctx, a.ID, v.SessionID); err != nil {
			g.fail(w, err)
			return
		}
		back("Every other browser is signed out.")
	case "rename":
		name := trimTo(r.PostFormValue("name"), 40)
		if name == "" {
			back("Type a name.")
			return
		}
		if err := g.reg.SetName(ctx, a.ID, name); err != nil {
			g.fail(w, err)
			return
		}
		back("The account is now called " + name + ".")
	case "create":
		if !hasPassword {
			back("Set a password for this account first: with several accounts the password tells whose account it is.")
			return
		}
		name := trimTo(r.PostFormValue("name"), 40)
		if name == "" {
			back("Type a name for the new account.")
			return
		}
		hash, problem := g.newPassword(r, list, 0)
		if problem != "" {
			back(problem)
			return
		}
		na, err := g.reg.Create(ctx, name, hash, now)
		if err != nil {
			g.fail(w, err)
			return
		}
		if _, err := g.server(ctx, na); err != nil {
			g.fail(w, err)
			return
		}
		g.log.Info("web: account created", "account", na.ID)
		back("Account " + name + " created. Sign out and sign in with its password to open it.")
	case "delete":
		if a.Primary() {
			back("The first account cannot be deleted.")
			return
		}
		if !current() {
			return
		}
		kept, err := g.reg.Delete(ctx, a.ID, now)
		if err != nil {
			g.fail(w, err)
			return
		}
		g.mu.Lock()
		if s := g.servers[a.ID]; s != nil {
			s.close()
			delete(g.servers, a.ID)
		}
		g.mu.Unlock()
		g.log.Warn("web: account deleted", "account", a.ID, "data", kept)
		clearSession(w)
		http.Redirect(w, r, "/ui/login", http.StatusSeeOther)
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
	}
}

// SetHolder remembers whose statements an account holds.
func (g *Gate) SetHolder(ctx context.Context, id int64, holder string) error {
	return g.reg.SetHolder(ctx, id, holder)
}

// ---- statements held while the person decides ----

// hold keeps an uploaded statement for an hour and returns its id.
func (g *Gate) hold(b []byte) (string, error) {
	if err := os.MkdirAll(g.uploads, 0o700); err != nil {
		return "", err
	}
	// older ones are not needed any more
	if old, _ := filepath.Glob(filepath.Join(g.uploads, "*.pdf")); len(old) > 0 {
		for _, f := range old {
			if st, err := os.Stat(f); err == nil && g.now().Sub(st.ModTime()) > time.Hour {
				if err := os.Remove(f); err != nil {
					g.log.Warn("web: remove an old upload", "err", err)
				}
			}
		}
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	id := hex.EncodeToString(raw)
	return id, os.WriteFile(filepath.Join(g.uploads, id+".pdf"), b, 0o600)
}

// held returns a held statement; the id must be one hold made.
func (g *Gate) held(id string) ([]byte, error) {
	if len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
		return nil, errors.New("no such upload")
	}
	return os.ReadFile(filepath.Join(g.uploads, id+".pdf"))
}

func (g *Gate) drop(id string) {
	if b, err := g.held(id); err == nil && b != nil {
		if err := os.Remove(filepath.Join(g.uploads, id+".pdf")); err != nil {
			g.log.Warn("web: remove an upload", "err", err)
		}
	}
}

// importNewAccount makes an account for the person a held statement belongs to, imports it
// there and signs this browser in to it.
func (g *Gate) importNewAccount(w http.ResponseWriter, r *http.Request, v viewer, list []accounts.Account) {
	ctx := r.Context()
	upload := r.PostFormValue("upload")
	b, err := g.held(upload)
	if err != nil {
		http.Redirect(w, r, "/ui?msg="+urlq("The uploaded statement is gone (they are kept for an hour). Upload it again."), http.StatusSeeOther)
		return
	}
	retry := func(msg string) {
		http.Redirect(w, r, "/ui?msg="+urlq(msg+" The statement was not imported; upload it again."), http.StatusSeeOther)
	}
	hasPassword := v.Account.Hash != "" || v.Account.Primary() && len(g.env) > 0
	if !hasPassword {
		retry("Set a password for your account first (Security): with several accounts the password tells whose account it is.")
		return
	}
	name := trimTo(r.PostFormValue("name"), 40)
	if name == "" {
		retry("Type a name for the new account.")
		return
	}
	hash, problem := g.newPassword(r, list, 0)
	if problem != "" {
		retry(problem)
		return
	}
	na, err := g.reg.Create(ctx, name, hash, g.now())
	if err != nil {
		g.fail(w, err)
		return
	}
	s, err := g.server(ctx, na)
	if err != nil {
		g.fail(w, err)
		return
	}
	res, holder, err := s.importBytes(ctx, b)
	if err != nil {
		g.fail(w, err)
		return
	}
	if holder != "" {
		if err := g.reg.SetHolder(ctx, na.ID, holder); err != nil {
			g.fail(w, err)
			return
		}
	}
	g.drop(upload)
	if err := g.startSession(w, r, na.ID); err != nil {
		g.fail(w, err)
		return
	}
	g.log.Info("web: account created from a statement", "account", na.ID, "ops", res.Total)
	msg := fmt.Sprintf("Account %s created and the statement imported: %d operations. You are signed in to it now; sign out to go back.", name, res.Total)
	http.Redirect(w, r, "/ui?msg="+urlq(msg), http.StatusSeeOther)
}
