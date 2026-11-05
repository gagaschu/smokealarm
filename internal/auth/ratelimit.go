package auth

import (
	"sync"
	"time"
)

// Login tries allowed per client within the window, before it is blocked.
const (
	maxAttempts   = 5
	attemptWindow = 15 * time.Minute
)

// RateLimiter slows down password guessing: a client gets five tries within 15
// minutes and is then blocked until the oldest try is out of the window. A
// successful login forgets the tries. It lives in memory, so a restart clears
// it.
//
// A try counts as soon as it starts, not when the password turned out wrong.
// The password check takes a moment, and all requests of a burst would get
// through before the first failure is known.
type RateLimiter struct {
	Now func() time.Time

	mu       sync.Mutex
	attempts map[string][]time.Time // client -> times of recent tries
}

// Attempt counts a login try and reports whether the client may make it. A
// refused try does not count, so a blocked client cannot extend its own block.
func (l *RateLimiter) Attempt(client string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	recent := l.recent(client)
	if len(recent) >= maxAttempts {
		return false
	}
	if l.attempts == nil {
		l.attempts = map[string][]time.Time{}
	}
	l.attempts[client] = append(recent, l.Now())
	// Forget clients that never came back, so the map does not grow without bound.
	for other := range l.attempts {
		l.recent(other)
	}
	return true
}

// Succeeded forgets the tries of a client.
func (l *RateLimiter) Succeeded(client string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, client)
}

// recent returns the tries inside the window and drops the older ones. The
// caller must hold the lock.
func (l *RateLimiter) recent(client string) []time.Time {
	cutoff := l.Now().Add(-attemptWindow)
	var kept []time.Time
	for _, t := range l.attempts[client] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.attempts, client)
	} else {
		l.attempts[client] = kept
	}
	return kept
}
