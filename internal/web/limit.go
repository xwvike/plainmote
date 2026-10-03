package web

import (
	"sync"
	"time"
)

// windowLimiter counts attempts per key in fixed windows: at most limit in
// each window. It lives in memory, which is enough for one process; a restart
// forgets the counts, and every limit here is a brake on guessing and on
// floods, not an accounting.
type windowLimiter struct {
	limit  int
	window time.Duration

	mu    sync.Mutex
	hits  map[string]*windowCount
	swept time.Time
}

type windowCount struct {
	count int
	since time.Time
}

func newWindowLimiter(limit int, window time.Duration) *windowLimiter {
	return &windowLimiter{limit: limit, window: window, hits: map[string]*windowCount{}}
}

// allow counts one attempt for key and reports whether it is within the limit.
func (l *windowLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)
	entry := l.hits[key]
	if entry == nil || now.Sub(entry.since) >= l.window {
		entry = &windowCount{since: now}
		l.hits[key] = entry
	}
	entry.count++
	return entry.count <= l.limit
}

// blocked reports whether key has used up its window, without counting.
func (l *windowLimiter) blocked(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.hits[key]
	return entry != nil && now.Sub(entry.since) < l.window && entry.count >= l.limit
}

// sweep drops finished windows now and then, so keys nobody repeats do not
// pile up.
func (l *windowLimiter) sweep(now time.Time) {
	if now.Sub(l.swept) < l.window {
		return
	}
	l.swept = now
	for key, entry := range l.hits {
		if now.Sub(entry.since) >= l.window {
			delete(l.hits, key)
		}
	}
}
