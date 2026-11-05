package auth

import (
	"sync"
	"time"
)

// Failed logins allowed per client within the window, before it is blocked.
const (
	maxFailures   = 5
	failureWindow = 15 * time.Minute
)

// RateLimiter slows down password guessing: after five failed logins within
// 15 minutes a client is blocked until the oldest failure is out of the
// window. It lives in memory, so a restart clears it.
type RateLimiter struct {
	Now func() time.Time

	mu       sync.Mutex
	failures map[string][]time.Time // client -> times of recent failures
}

// Allowed reports whether the client may try to log in.
func (l *RateLimiter) Allowed(client string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.recent(client)) < maxFailures
}

// Failed records a failed login.
func (l *RateLimiter) Failed(client string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.failures == nil {
		l.failures = map[string][]time.Time{}
	}
	l.failures[client] = append(l.recent(client), l.Now())
	// Forget clients that never came back, so the map does not grow without bound.
	for other := range l.failures {
		l.recent(other)
	}
}

// Succeeded forgets the failures of a client.
func (l *RateLimiter) Succeeded(client string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, client)
}

// recent returns the failures inside the window and drops the older ones. The
// caller must hold the lock.
func (l *RateLimiter) recent(client string) []time.Time {
	cutoff := l.Now().Add(-failureWindow)
	var kept []time.Time
	for _, t := range l.failures[client] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.failures, client)
	} else {
		l.failures[client] = kept
	}
	return kept
}
