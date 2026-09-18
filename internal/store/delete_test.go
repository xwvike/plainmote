package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

// TestDeleteFreesTheQuotaAndKeepsTheHistory pins both halves of deletion: the
// account gets its room back, and the record of what was served does not go
// with the thing that was served.
func TestDeleteFreesTheQuotaAndKeepsTheHistory(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	blobs := db.blobs.(*memoryBlobs)

	share, err := db.CreateShare(ctx, user.ID, resource.ID, "临时", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	meta := RequestMeta{Method: "GET", RemoteIP: "203.0.113.9"}
	if _, err := db.ConsumeToken(ctx, share.Token, meta, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordAccess(ctx, AccessEvent{
		OwnerID: user.ID, ResourceID: resource.ID,
		ResourceName: resource.Name, ResourceFile: resource.Filename,
		LinkID: share.ID, LinkName: share.Name,
		Outcome: OutcomeSuccess, Status: 200, Detail: "link accepted",
	}, meta); err != nil {
		t.Fatal(err)
	}

	before, err := db.UsageForUser(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	objectsBefore := blobs.count()

	if err := db.DeleteResource(ctx, user.ID, resource.ID); err != nil {
		t.Fatal(err)
	}

	after, err := db.UsageForUser(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Resources != before.Resources-1 || after.StorageBytes != before.StorageBytes-resource.ContentSize {
		t.Fatalf("deleting must give the room back: %+v -> %+v", before, after)
	}
	if got := blobs.count(); got != objectsBefore-1 {
		t.Fatalf("the body must go with the resource, %d objects -> %d", objectsBefore, got)
	}

	// The address stops resolving, because the link went with the resource.
	result, err := db.ConsumeToken(ctx, share.Token, meta, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if result.Allowed {
		t.Fatal("a link to a deleted resource must not resolve")
	}

	logs, err := db.ListAccess(ctx, user.ID, "", OutcomeSuccess, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) == 0 {
		t.Fatal("the access history must outlive the resource")
	}
	if logs[0].ResourceName != resource.Name || logs[0].ResourceFile != resource.Filename {
		t.Fatalf("the log must keep the name it was reached under, got %q/%q", logs[0].ResourceName, logs[0].ResourceFile)
	}
}

// TestDeleteIsScopedToTheOwner keeps deletion inside the same boundary every
// other resource operation uses.
func TestDeleteIsScopedToTheOwner(t *testing.T) {
	db, _, resource := testDatabase(t)
	ctx := context.Background()
	other, err := db.UpsertUser(ctx, "200", "bob", "Bob", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteResource(ctx, other.ID, resource.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another account must not delete it, got %v", err)
	}
	if err := db.DeleteResource(ctx, other.ID, "not-a-uuid"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a malformed id must not reach the database, got %v", err)
	}
	if _, err := db.ReadContent(ctx, resource); err != nil {
		t.Fatalf("the resource must be untouched, got %v", err)
	}
}

// TestDeleteMakesRoomForANewResource is the reason deletion exists: a plan that
// can only be spent locks the account out for good once it fills.
func TestDeleteMakesRoomForANewResource(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	setPlanLimits(t, db, 1, 1<<20)

	body := bytes.Repeat([]byte("a"), 64)
	if _, err := db.CreateResource(ctx, user.ID, "第二个", "second.yaml", body, "", ""); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("expected the plan to be full, got %v", err)
	}
	if err := db.DeleteResource(ctx, user.ID, resource.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateResource(ctx, user.ID, "第二个", "second.yaml", body, "", ""); err != nil {
		t.Fatalf("the freed slot must be usable, got %v", err)
	}
}
