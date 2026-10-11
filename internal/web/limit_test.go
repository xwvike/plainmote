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

	// A flood of distinct keys stays bounded, and does not buy a blocked key
	// a fresh start.
	limiter = newWindowLimiter(2, time.Minute)
	limiter.maxKeys = 100
	limiter.allow("blocked", now)
	limiter.allow("blocked", now)
	for i := 0; i < 1000; i++ {
		limiter.allow(strconv.Itoa(i), now)
	}
	if len(limiter.hits) > 100 {
		t.Fatalf("the limiter holds %d keys", len(limiter.hits))
	}
	if !limiter.blocked("blocked", now) || limiter.allow("blocked", now) {
		t.Fatal("a flood of new keys freed a blocked one")
	}
	// With every slot blocked, a new key goes uncounted rather than freeing one.
	limiter = newWindowLimiter(1, time.Minute)
	limiter.maxKeys = 3
	for _, key := range []string{"a", "b", "c"} {
		limiter.allow(key, now)
	}
	if !limiter.allow("d", now) || len(limiter.hits) != 3 || !limiter.blocked("a", now) {
		t.Fatalf("a full limiter of blocked keys: %d keys", len(limiter.hits))
	}
	// Finished windows make room first.
	if !limiter.allow("d", now.Add(time.Minute)) || limiter.hits["d"] == nil {
		t.Fatal("a finished window was not made room of")
	}
}

func TestAddressKey(t *testing.T) {
	for ip, want := range map[string]string{
		"203.0.113.7":               "203.0.113.7",
		"::ffff:203.0.113.7":        "::ffff:203.0.113.7",
		"2001:db8:1:2:aaaa::1":      "2001:db8:1:2::/64",
		"2001:db8:1:2:ffff:ffff::9": "2001:db8:1:2::/64",
		"2001:db8:1:3::1":           "2001:db8:1:3::/64",
		"fe80::1%eth0":              "fe80::/64",
		"not an address":            "not an address",
	} {
		if got := addressKey(ip); got != want {
			t.Errorf("addressKey(%q) = %q, want %q", ip, got, want)
		}
	}
}
