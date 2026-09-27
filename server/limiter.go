package server

import (
	"net/http"
	"sync"
	"time"

	stellarjay "github.com/kyle-visner/stellarjay"
)

type rateWindow struct {
	started time.Time
	count   int
}

type fixedWindowLimiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	now     func() time.Time
	entries map[string]rateWindow
}

func newFixedWindowLimiter(limit int, window time.Duration) *fixedWindowLimiter {
	return &fixedWindowLimiter{
		limit: limit, window: window, now: time.Now,
		entries: make(map[string]rateWindow),
	}
}

func (l *fixedWindowLimiter) Allow(key string) bool {
	if l.limit < 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	entry := l.entries[key]
	if entry.started.IsZero() || now.Sub(entry.started) >= l.window {
		entry = rateWindow{started: now}
	}
	entry.count++
	l.entries[key] = entry
	if len(l.entries) > 4096 {
		for candidate, value := range l.entries {
			if now.Sub(value.started) >= l.window {
				delete(l.entries, candidate)
			}
		}
	}
	return entry.count <= l.limit
}

func writeRateLimit(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "60")
	writeError(w, http.StatusTooManyRequests, stellarjay.ErrRateLimit, "request rate limit exceeded")
}
