package store

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestSingleUseLinkSpendsExactlyOnce pins the invariant the whole use-count
// feature rests on: however many requests arrive at once, a one-use link opens
// for exactly one of them. The spend is a single conditional UPDATE, so this
// holds without serializing requests in the application.
func TestSingleUseLinkSpendsExactlyOnce(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	share, err := db.CreateShare(ctx, user.ID, resource.ID, "once", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}

	const racers = 12
	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	results := make([]ConsumeResult, racers)
	errs := make([]error, racers)
	for i := 0; i < racers; i++ {
		done.Add(1)
		go func(slot int) {
			defer done.Done()
			start.Wait()
			results[slot], errs[slot] = db.ConsumeToken(ctx, share.Token, RequestMeta{Method: "GET"}, time.Now().UTC())
		}(i)
	}
	start.Done()
	done.Wait()

	allowed := 0
	for i, result := range results {
		if errs[i] != nil {
			t.Fatalf("racer %d: %v", i, errs[i])
		}
		if result.Allowed {
			allowed++
		} else if result.Reason != "exhausted" {
			t.Errorf("racer %d refused as %q, want exhausted", i, result.Reason)
		}
	}
	if allowed != 1 {
		t.Fatalf("a single-use link opened %d times", allowed)
	}

	var used int
	if err := db.db.QueryRow(ctx, `SELECT used_count FROM links WHERE id = $1`, share.ID).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if used != 1 {
		t.Fatalf("use count ended at %d, want 1", used)
	}
}

// The same guarantee for a link with a small allowance.
func TestLimitedLinkNeverOverspends(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	share, err := db.CreateShare(ctx, user.ID, resource.ID, "two", time.Hour, 2)
	if err != nil {
		t.Fatal(err)
	}
	var done sync.WaitGroup
	allowed := make(chan struct{}, 20)
	for i := 0; i < 10; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			result, err := db.ConsumeToken(ctx, share.Token, RequestMeta{Method: "GET"}, time.Now().UTC())
			if err == nil && result.Allowed {
				allowed <- struct{}{}
			}
		}()
	}
	done.Wait()
	close(allowed)
	if got := len(allowed); got != 2 {
		t.Fatalf("a two-use link opened %d times", got)
	}
}
