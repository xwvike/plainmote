package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func setPlanLimits(t *testing.T, db *Store, resources, storage int64) {
	t.Helper()
	if _, err := db.db.Exec(context.Background(),
		`UPDATE plans SET max_resources = $1, max_storage = $2 WHERE is_default`, resources, storage); err != nil {
		t.Fatal(err)
	}
}

// TestQuotaRefusalNamesTheLimit keeps the refusal useful: it has to be
// distinguishable from a service failure, and it has to say which ceiling was
// hit so the user knows what to free.
func TestQuotaRefusalNamesTheLimit(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	setPlanLimits(t, db, 2, 1<<20)

	// testDatabase already created one; one more fills the plan.
	if _, err := db.CreateResource(ctx, user.ID, "第二个", "second.yaml", []byte("a: 1\n"), "", ""); err != nil {
		t.Fatal(err)
	}
	_, err := db.CreateResource(ctx, user.ID, "第三个", "third.yaml", []byte("a: 1\n"), "", "")
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("a full account must refuse with ErrQuotaExceeded, got %v", err)
	}
	var quotaErr *QuotaError
	if !errors.As(err, &quotaErr) || quotaErr.Storage {
		t.Fatalf("the refusal must name the resource count, got %#v", err)
	}
	if !strings.Contains(err.Error(), "2") {
		t.Fatalf("the refusal must carry the limit, got %q", err)
	}

	setPlanLimits(t, db, 10, 64)
	_, err = db.CreateResource(ctx, user.ID, "大文件", "big.yaml", bytes.Repeat([]byte("a"), 128), "", "")
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("an oversized body must refuse with ErrQuotaExceeded, got %v", err)
	}
	if !errors.As(err, &quotaErr) || !quotaErr.Storage {
		t.Fatalf("the refusal must name storage, got %#v", err)
	}
}

// TestRefusedUploadLeavesNoObject pins the cleanup the new write order depends
// on: the body reaches object storage before the quota is checked, so a refusal
// has to take it back out.
func TestRefusedUploadLeavesNoObject(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	blobs := db.blobs.(*memoryBlobs)
	setPlanLimits(t, db, 10, 64)

	before := blobs.count()
	if _, err := db.CreateResource(ctx, user.ID, "大文件", "big.yaml", bytes.Repeat([]byte("a"), 128), "", ""); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("expected a quota refusal, got %v", err)
	}
	if after := blobs.count(); after != before {
		t.Fatalf("a refused upload must not leave an object behind, %d -> %d", before, after)
	}
}

// TestQuotaHoldsUnderConcurrentUploads is why the check runs under a row lock:
// without it every caller reads a usage that still leaves room.
func TestQuotaHoldsUnderConcurrentUploads(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	const limit = 5
	setPlanLimits(t, db, limit, 1<<20)

	var wg sync.WaitGroup
	created := make([]bool, 20)
	for i := range created {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := db.CreateResource(ctx, user.ID, fmt.Sprintf("并发 %d", i), fmt.Sprintf("c-%d.yaml", i), []byte("a: 1\n"), "", "")
			created[i] = err == nil
		}()
	}
	wg.Wait()

	usage, err := db.UsageForUser(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if usage.Resources != limit {
		t.Fatalf("the account must stop at %d resources, found %d", limit, usage.Resources)
	}
}

// TestUpdateIsMeasuredAgainstWhatItReplaces keeps an edit from being counted at
// both sizes: an account with no headroom can still rewrite what it already has.
func TestUpdateIsMeasuredAgainstWhatItReplaces(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	body := bytes.Repeat([]byte("a"), 100)
	resource, err := db.CreateResource(ctx, user.ID, "刚好", "exact.yaml", body, "", "")
	if err != nil {
		t.Fatal(err)
	}
	usage, err := db.UsageForUser(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Not one byte of headroom left.
	setPlanLimits(t, db, 10, usage.StorageBytes)

	if err := db.UpdateResource(ctx, user.ID, resource.ID, "刚好", "exact.yaml", bytes.Repeat([]byte("b"), 100), "", ""); err != nil {
		t.Fatalf("a same-size rewrite must fit, got %v", err)
	}
	if err := db.UpdateResource(ctx, user.ID, resource.ID, "更大", "exact.yaml", bytes.Repeat([]byte("b"), 101), "", ""); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("one byte over must be refused, got %v", err)
	}
}
