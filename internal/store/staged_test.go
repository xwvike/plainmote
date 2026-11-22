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
	// the live blob must survive reconciliation.
	db.dropPublishedOnError(ctx, resource.OwnerID, false, resource.ContentKey)
	if rc, _, err := blobs.Open(ctx, resource.ContentKey); err != nil {
		t.Fatalf("live object referenced by resources was deleted on uncertain commit: %v", err)
	} else {
		_ = rc.Close()
	}

	// 2. Uncertain commit (!rolledBack) when the object is NOT referenced:
	// do not delete synchronously (commit might still be finishing); queue in
	// blob_garbage so Prune verifies references before deleting.
	orphanKey := contentKey(uuid.NewString())
	if err := blobs.Put(ctx, orphanKey, bytes.NewReader([]byte("orphan")), 6); err != nil {
		t.Fatal(err)
	}
	db.dropPublishedOnError(ctx, resource.OwnerID, false, orphanKey)
	if rc, _, err := blobs.Open(ctx, orphanKey); err != nil {
		t.Fatalf("uncertain unreferenced object should be deferred to blob_garbage, not deleted immediately: %v", err)
	} else {
		_ = rc.Close()
	}
	if _, err := db.Prune(ctx, 0, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := blobs.Head(ctx, resource.ContentKey); err != nil {
		t.Fatalf("collector deleted live key: %v", err)
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
	db.dropPublishedOnError(ctx, resource.OwnerID, true, rolledKey)
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

func TestCollectorWaitsForPublication(t *testing.T) {
	db, owner, res := testDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	collector, err := db.db.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer collector.Release()
	writer, err := db.db.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback(context.Background())

	key := contentKey(res.ID)
	payload := []byte("new version")
	if err := db.blobs.Put(ctx, key, bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Exec(ctx, `UPDATE resources SET content_key=$1 WHERE id=$2`, key, res.ID); err != nil {
		t.Fatal(err)
	}
	db.dropPublishedOnError(ctx, owner.ID, false, key)

	done := make(chan error, 1)
	go func() {
		_, err := db.retryGarbage(ctx, collector, time.Now().UTC())
		done <- err
	}()
	// Observe the actual lock wait rather than assuming the goroutine has run.
	// Always unblock and join it before releasing its pooled connection.
	joined := false
	defer func() {
		_ = writer.Rollback(context.Background())
		if !joined {
			<-done
		}
	}()
	for {
		var waiting bool
		if err := db.db.QueryRow(ctx, `SELECT $1::integer = ANY(pg_blocking_pids($2))`,
			writer.Conn().PgConn().PID(), collector.Conn().PgConn().PID()).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			joined = true
			t.Fatalf("collector finished before the writer: %v", err)
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err := writer.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	err = <-done
	joined = true
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.blobs.(*memoryBlobs).Head(ctx, key); err != nil {
		t.Fatalf("published object missing: %v", err)
	}
}

func TestPrunedUploadSurvivesCollectorRestart(t *testing.T) {
	db, owner, _ := testDatabase(t)
	ctx := context.Background()
	blobs := &presignTestBlobs{db.blobs.(*memoryBlobs)}
	db.blobs = blobs
	now := time.Now().UTC()
	payload := []byte("sample")
	_, _, ticket, err := db.BeginStagedUpload(ctx, owner.ID, "resource_create", uuid.NewString(), "",
		int64(len(payload)), 1, contentSHA256(payload), time.Minute, 2*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := blobs.Put(ctx, ticket.StagingKey, bytes.NewReader(payload), ticket.Size); err != nil {
		t.Fatal(err)
	}
	later := now.Add(3 * time.Minute)
	conn, err := db.db.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.pruneStagedUploads(ctx, conn, later)
	conn.Release()
	if err != nil {
		t.Fatal(err)
	}
	var queued bool
	if err := db.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM blob_garbage WHERE key=$1)`, ticket.StagingKey).Scan(&queued); err != nil || !queued {
		t.Fatalf("retired reservation has no durable cleanup job: %v", err)
	}
	next, err := db.db.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Release()
	if _, err := db.retryGarbage(ctx, next, later); err != nil {
		t.Fatal(err)
	}
	if _, err := blobs.Head(ctx, ticket.StagingKey); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("staging object after restart: %v", err)
	}
}

func TestSealedSaveFallbackAndDirectRetryShareVersion(t *testing.T) {
	db, owner, _ := testDatabase(t)
	ctx := context.Background()
	db.blobs = &presignTestBlobs{db.blobs.(*memoryBlobs)}
	id := uuid.NewString()
	resource, err := db.CreateSealedResource(ctx, owner.ID, id, sealedPart(1), wrappedKey(1))
	if err != nil {
		t.Fatal(err)
	}
	edit := sealedPart(2)
	_, _, ticket, err := db.BeginStagedUpload(ctx, owner.ID, "sealed_save", id, id,
		int64(len(edit.Content)), 0, contentSHA256(edit.Content), time.Minute, 2*time.Minute, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		result SaveResult
		err    error
	}
	out := make(chan outcome, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			result, err := db.SaveSealedResourceWithFallbackUpload(ctx, owner.ID, id, ticket.UploadID, resource.Version, edit)
			out <- outcome{result, err}
		}()
	}
	close(start)
	first, second := <-out, <-out
	if first.err != nil || second.err != nil || first.result != second.result || first.result.Version != resource.Version+1 {
		t.Fatalf("concurrent fallback results: %+v, %+v", first, second)
	}
	replayed, err := db.SaveSealedResourceFromUpload(ctx, owner.ID, id, resource.Version, ticket, edit.Meta)
	if err != nil || replayed != first.result {
		t.Fatalf("direct retry: %+v, %v", replayed, err)
	}
	count, err := db.HistoryCount(ctx, owner.ID, id)
	if err != nil || count != 1 {
		t.Fatalf("history count after retries: %d, %v", count, err)
	}
}
