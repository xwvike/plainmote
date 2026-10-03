package web

import (
	"strconv"
	"testing"
	"time"
)

func TestWindowLimiter(t *testing.T) {
	now := time.Now()
	limiter := newWindowLimiter(3, time.Minute)
	for i := 0; i < 3; i++ {
		if !limiter.allow("a", now) {
			t.Fatalf("attempt %d refused", i)
		}
	}
	if limiter.allow("a", now) || !limiter.blocked("a", now) {
		t.Fatal("the fourth attempt in a window is over the limit")
	}
	if limiter.blocked("b", now) || !limiter.allow("b", now) {
		t.Fatal("keys are counted apart")
	}
	if limiter.blocked("a", now.Add(time.Minute)) || !limiter.allow("a", now.Add(time.Minute)) {
		t.Fatal("a new window starts the count over")
	}

	// A flood of distinct keys stays bounded.
	limiter = newWindowLimiter(1, time.Minute)
	limiter.maxKeys = 100
	for i := 0; i < 1000; i++ {
		limiter.allow(strconv.Itoa(i), now)
	}
	if len(limiter.hits) > 100 {
		t.Fatalf("the limiter holds %d keys", len(limiter.hits))
	}
}
