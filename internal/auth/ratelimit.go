package auth

import (
	"sync"
	"time"
)

// LoginLimiter throttles failed login attempts per key (IP and username).
// After MaxFailures within Window, the key is locked for Lockout.
type LoginLimiter struct {
	mu          sync.Mutex
	entries     map[string]*limEntry
	MaxFailures int
	Window      time.Duration
	Lockout     time.Duration
	now         func() time.Time
}

type limEntry struct {
	failures    []time.Time
	lockedUntil time.Time
}

func NewLoginLimiter() *LoginLimiter {
	return &LoginLimiter{entries: map[string]*limEntry{}, MaxFailures: 5, Window: 10 * time.Minute, Lockout: 15 * time.Minute, now: time.Now}
}

// Allowed returns whether an attempt for key is allowed, and if not, the retry-after duration.
func (l *LoginLimiter) Allowed(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.entries[key]
	if e == nil {
		return true, 0
	}
	now := l.now()
	if now.Before(e.lockedUntil) {
		return false, e.lockedUntil.Sub(now)
	}
	return true, 0
}

// Fail records a failed attempt.
func (l *LoginLimiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	e := l.entries[key]
	if e == nil {
		e = &limEntry{}
		l.entries[key] = e
	}
	cut := now.Add(-l.Window)
	kept := e.failures[:0]
	for _, t := range e.failures {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	e.failures = append(kept, now)
	if len(e.failures) >= l.MaxFailures {
		e.lockedUntil = now.Add(l.Lockout)
		e.failures = nil
	}
}

// Succeed clears the key.
func (l *LoginLimiter) Succeed(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.entries, key)
}

// GC removes stale entries.
func (l *LoginLimiter) GC() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for k, e := range l.entries {
		if now.After(e.lockedUntil) && (len(e.failures) == 0 || now.Sub(e.failures[len(e.failures)-1]) > l.Window) {
			delete(l.entries, k)
		}
	}
}
