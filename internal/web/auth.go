package web

import (
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// Guessing passwords is slowed down per client address: after maxFails wrong passwords
// sign-in waits, doubling up to maxLock.
const (
	sessionCookie = "spendbot_session"
	sessionTTL    = 30 * 24 * time.Hour
	maxFails      = 5
	firstLock     = time.Minute
	maxLock       = 15 * time.Minute
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

func roundWait(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d seconds", int(d.Seconds()+0.5))
	}
	m := int(d.Minutes() + 0.5)
	return fmt.Sprintf("%d %s", m, plural(m, "minute", "minutes"))
}

func clearSession(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}
