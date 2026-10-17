package api

import (
	"net"
	"sync"
	"time"
)

// Failed logins are limited per (username, source IP) in process memory, which
// is enough because the Control Plane is one process (ADR-0020, defaults.md).
const (
	loginFailureLimit  = 10
	loginFailureWindow = 15 * time.Minute
)

type loginLimiter struct {
	mu      sync.Mutex
	entries map[string]*loginFailures
}

type loginFailures struct {
	count int
	reset time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{entries: make(map[string]*loginFailures)}
}

// retryAfter reports how long the key stays blocked. Ten failures are allowed;
// the next attempt is rejected before the password is checked.
func (l *loginLimiter) retryAfter(key string, now time.Time) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if !ok {
		return 0, false
	}
	if now.After(e.reset) {
		delete(l.entries, key)
		return 0, false
	}
	if e.count < loginFailureLimit {
		return 0, false
	}
	return e.reset.Sub(now), true
}

func (l *loginLimiter) fail(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if !ok || now.After(e.reset) {
		e = &loginFailures{reset: now.Add(loginFailureWindow)}
		l.entries[key] = e
	}
	e.count++
	for k, v := range l.entries {
		if now.After(v.reset) {
			delete(l.entries, k)
		}
	}
}

func (l *loginLimiter) succeed(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

// splitClientIP strips the port from a RemoteAddr. A request with no port (a
// test or a malformed address) is used whole.
//
// ponytail: trusts RemoteAddr only, so behind a TLS proxy every client shares
// one bucket. Add a trusted-proxy setting before honouring X-Forwarded-For.
func splitClientIP(remote string) string {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		return remote
	}
	return host
}
