package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"plainmote/internal/blob"
)

type presignTestBlobs struct {
	*memoryBlobs
}

func (p *presignTestBlobs) PresignPut(_ context.Context, key string, _ int64, _ string, _ time.Duration) (blob.PresignedPut, error) {
	return blob.PresignedPut{URL: "https://r2.test/" + key}, nil
}

func TestDropPublishedOnErrorDistinguishesRollbackFromUncertainCommit(t *testing.T) {
	db, _, resource := testDatabase(t)
	ctx := context.Background()
	blobs := db.blobs.(*memoryBlobs)

	// 1. Uncertain commit (!rolledBack) when the published object IS referenced
	// in resources (e.g., PostgreSQL committed before the connection dropped):
	// the live blob must remain intact and must not be queued as garbage.
	db.dropPublishedOnError(ctx, resource.ContentKey, false)
	if rc, _, err := blobs.Open(ctx, resource.ContentKey); err != nil {
		t.Fatalf("live object referenced by resources was deleted on uncertain commit: %v", err)
	} else {
		_ = rc.Close()
	}
	var queued int
	if err := db.db.QueryRow(ctx, `SELECT COUNT(*) FROM blob_garbage WHERE key = $1`, resource.ContentKey).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 0 {
		t.Fatalf("live referenced key should not be queued in blob_garbage, got %d", queued)
	}

	// 2. Uncertain commit (!rolledBack) when the object is NOT referenced:
	// do not delete synchronously (commit might still be finishing); queue in
	// blob_garbage so Prune verifies references before deleting.
	orphanKey := contentKey(uuid.NewString())
	if err := blobs.Put(ctx, orphanKey, bytes.NewReader([]byte("orphan")), 6); err != nil {
		t.Fatal(err)
	}
	db.dropPublishedOnError(ctx, orphanKey, false)
	if rc, _, err := blobs.Open(ctx, orphanKey); err != nil {
		t.Fatalf("uncertain unreferenced object should be deferred to blob_garbage, not deleted immediately: %v", err)
	} else {
		_ = rc.Close()
	}
	if _, err := db.Prune(ctx, 0, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if rc, _, err := blobs.Open(ctx, orphanKey); err == nil {
		_ = rc.Close()
		t.Fatalf("unreferenced uncertain object %q should be deleted by Prune", orphanKey)
	}

	// 3. Definite rollback (rolledBack == true): delete immediately.
	rolledKey := contentKey(uuid.NewString())
	if err := blobs.Put(ctx, rolledKey, bytes.NewReader([]byte("rolled")), 6); err != nil {
		t.Fatal(err)
	}
	db.dropPublishedOnError(ctx, rolledKey, true)
	if rc, _, err := blobs.Open(ctx, rolledKey); err == nil {
		_ = rc.Close()
		t.Fatalf("explicitly rolled-back object %q should be deleted immediately", rolledKey)
	}
}

func TestDeleteAccountCleansStagedUploadsThroughPresignExpiry(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	base := db.blobs.(*memoryBlobs)
	db.blobs = &presignTestBlobs{memoryBlobs: base}

	now := time.Now().UTC()
	payload := bytes.Repeat([]byte("s"), 128)
	_, _, ticket, err := db.BeginStagedUpload(
		ctx, user.ID, "resource_create", uuid.NewString(), "",
		int64(len(payload)), 1, contentSHA256(payload),
		10*time.Minute, 15*time.Minute, now,
	)
	if err != nil {
		t.Fatalf("BeginStagedUpload: %v", err)
	}
	// Browser uploads to staging key, then account is deleted before commit.
	if err := base.Put(ctx, ticket.StagingKey, bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteAccount(ctx, user.ID); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	if _, err := base.Head(ctx, ticket.StagingKey); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("expected staging object deleted immediately on DeleteAccount, got err=%v", err)
	}

	// Simulate an in-flight or replayed presigned PUT landing before expires_at.
	if err := base.Put(ctx, ticket.StagingKey, bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatal(err)
	}
	// Prune before expires_at must keep the deferred garbage record waiting.
	if _, err := db.Prune(ctx, 0, now.Add(5*time.Minute)); err != nil {
		t.Fatalf("early Prune: %v", err)
	}
	if _, err := base.Head(ctx, ticket.StagingKey); err != nil {
		t.Fatalf("deferred cleanup should wait until presigned URL expiry, got err=%v", err)
	}
	// Prune after expires_at must sweep the late staging object and clear blob_garbage.
	if _, err := db.Prune(ctx, 0, now.Add(20*time.Minute)); err != nil {
		t.Fatalf("post-expiry Prune: %v", err)
	}
	if _, err := base.Head(ctx, ticket.StagingKey); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("expected late staging object deleted after presign expiry, got err=%v", err)
	}
	var remaining int
	if err := db.db.QueryRow(ctx, `SELECT COUNT(*) FROM blob_garbage WHERE key = $1`, ticket.StagingKey).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("expected blob_garbage entry cleared after sweep, got %d", remaining)
	}
}
