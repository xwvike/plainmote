package web

import (
	"sync"
	"time"

	"plainmote/internal/store"
)

// addressKey is what a limit counts a client address as: see store.AddressKey.
func addressKey(ip string) string { return store.AddressKey(ip) }

// windowLimiter counts attempts per key in fixed windows: at most limit in
// each window. It lives in memory, which is enough for one process; a restart
// forgets the counts, and every limit here is a brake on guessing and on
// floods, not an accounting.
type windowLimiter struct {
	limit  int
	window time.Duration
	// maxKeys bounds the memory a flood of distinct keys can take. Past it,
	// finished windows are dropped, then keys still within their limit; a key
	// over its limit is kept until its window ends, so a flood of new keys
	// cannot buy a blocked one a fresh start. With every slot held by a
	// blocked key, a new key goes uncounted: a brake that sometimes forgets
	// beats a process that runs out of memory.
	maxKeys int

	mu    sync.Mutex
	hits  map[string]*windowCount
	swept time.Time
}

type windowCount struct {
	count int
	since time.Time
}

func newWindowLimiter(limit int, window time.Duration) *windowLimiter {
	return &windowLimiter{limit: limit, window: window, maxKeys: 10000, hits: map[string]*windowCount{}}
}

// allow counts one attempt for key and reports whether it is within the limit.
func (l *windowLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)
	entry := l.hits[key]
	switch {
	case entry != nil && now.Sub(entry.since) >= l.window:
		entry.count, entry.since = 0, now
	case entry == nil:
		if len(l.hits) >= l.maxKeys && !l.makeRoom(now) {
			return true
		}
		entry = &windowCount{since: now}
		l.hits[key] = entry
	}
	entry.count++
	return entry.count <= l.limit
}

// makeRoom frees a slot for a new key: a finished window if there is one,
// otherwise a key still within its limit. It reports false when every key
// held is over its limit.
func (l *windowLimiter) makeRoom(now time.Time) bool {
	l.swept = time.Time{}
	l.sweep(now)
	if len(l.hits) < l.maxKeys {
		return true
	}
	for key, entry := range l.hits {
		if entry.count < l.limit {
			delete(l.hits, key)
			return true
		}
	}
	return false
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
