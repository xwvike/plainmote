package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTrashLifecycleQuotaLinksAndPrune(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	blobs := db.blobs.(*memoryBlobs)
	now := time.Now().UTC()

	// Add a second version and a tag so we verify version history, tags and
	// share links behave properly across trash -> restore -> purge.
	if err := db.UpdateResource(ctx, user.ID, resource.ID, resource.Name, resource.Filename, []byte("answer=43\n"), "utf-8"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SetResourceTags(ctx, user.ID, resource.ID, []string{"tmp"}); err != nil {
		t.Fatal(err)
	}
	share, err := db.CreateShare(ctx, user.ID, resource.ID, "for bob", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}

	usageBefore, err := db.UsageForUser(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	objectsBefore := blobs.Count()

	revoked, err := db.TrashResource(ctx, user.ID, resource.ID, now)
	if err != nil || revoked != 1 {
		t.Fatalf("TrashResource: revoked=%d err=%v", revoked, err)
	}

	// Invisible to live reads, listings, tags, resolve and share delivery.
	if _, err := db.ResourceForOwner(ctx, user.ID, resource.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("trashed resource must not be found as live: %v", err)
	}
	if list, total, err := db.ListResources(ctx, user.ID, "", 10, 0); err != nil || total != 0 || len(list) != 0 {
		t.Fatalf("trashed resource appeared in ListResources: %d %v", total, err)
	}
	if matches, err := db.ResolveResources(ctx, user.ID, resource.ID); err != nil || len(matches) != 0 {
		t.Fatalf("trashed resource appeared in ResolveResources: %+v %v", matches, err)
	}
	if tags, err := db.AccountTags(ctx, user.ID); err != nil || len(tags) != 0 {
		t.Fatalf("trashed resource tag leaked to AccountTags: %+v %v", tags, err)
	}
	if _, err := db.ShareLinkResource(ctx, user.ID, share.Token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("trashed share link resolved resource: %v", err)
	}
	meta := RequestMeta{Method: "GET", RemoteIP: "203.0.113.9"}
	if consumed, err := db.ConsumeToken(ctx, share.Token, meta, now); err != nil || consumed.Allowed {
		t.Fatalf("share link to trashed resource must not deliver: %+v %v", consumed, err)
	}

	// Quota stays charged while in trash (objects are still stored).
	usageTrashed, err := db.UsageForUser(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if usageTrashed.Resources != usageBefore.Resources || usageTrashed.StorageBytes != usageBefore.StorageBytes || usageTrashed.HistoryBytes != usageBefore.HistoryBytes {
		t.Fatalf("trashed resource must still count toward quota: %+v -> %+v", usageBefore, usageTrashed)
	}
	if blobs.Count() != objectsBefore {
		t.Fatalf("trashing must keep stored objects: %d -> %d", objectsBefore, blobs.Count())
	}

	// Visible in trash listing and count.
	trash, total, err := db.ListTrash(ctx, user.ID, 10, 0)
	if err != nil || total != 1 || len(trash) != 1 || trash[0].ID != resource.ID || !trash[0].Trashed() {
		t.Fatalf("ListTrash: total=%d items=%+v err=%v", total, trash, err)
	}
	if count, err := db.TrashCount(ctx, user.ID); err != nil || count != 1 {
		t.Fatalf("TrashCount: %d %v", count, err)
	}

	// Another user cannot restore or purge it.
	other, err := db.UpsertUser(ctx, "201", "bob", "Bob", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.RestoreResource(ctx, other.ID, resource.ID, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other user restored resource: %v", err)
	}
	if err := db.PurgeResource(ctx, other.ID, resource.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other user purged resource: %v", err)
	}

	// Restoring brings back the resource and its history, while keeping
	// previously revoked share links revoked.
	restored, err := db.RestoreResource(ctx, user.ID, resource.ID, now)
	if err != nil || restored.ID != resource.ID || restored.Trashed() {
		t.Fatalf("RestoreResource: %+v %v", restored, err)
	}
	if consumed, err := db.ConsumeToken(ctx, share.Token, meta, now); err != nil || consumed.Allowed || consumed.Reason != OutcomeRevoked {
		t.Fatalf("revoked link must stay revoked after restore: %+v %v", consumed, err)
	}
	if versions, err := db.ListVersions(ctx, user.ID, resource.ID); err != nil || len(versions) != 1 {
		t.Fatalf("version history must survive trash and restore: %+v %v", versions, err)
	}

	// PurgeResource refuses active resources and permanently deletes trashed
	// ones, freeing quota and S3 objects.
	if err := db.PurgeResource(ctx, user.ID, resource.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PurgeResource must refuse an active resource: %v", err)
	}
	if _, err := db.TrashResource(ctx, user.ID, resource.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := db.PurgeResource(ctx, user.ID, resource.ID); err != nil {
		t.Fatalf("PurgeResource: %v", err)
	}
	usageAfterPurge, err := db.UsageForUser(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if usageAfterPurge.Resources != 0 || usageAfterPurge.StorageBytes != 0 || usageAfterPurge.HistoryBytes != 0 {
		t.Fatalf("purging must free quota: %+v", usageAfterPurge)
	}
	if blobs.Count() != 0 {
		t.Fatalf("purging must delete all stored objects, left %d", blobs.Count())
	}
}

func TestEmptyTrashAndPruneExpiredTrash(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	blobs := db.blobs.(*memoryBlobs)
	now := time.Now().UTC()

	keep, err := db.CreateResource(ctx, user.ID, "Keep", "keep.txt", []byte("keep\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	recent, err := db.CreateResource(ctx, user.ID, "Recent", "recent.txt", []byte("recent\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	old, err := db.CreateResource(ctx, user.ID, "Old", "old.txt", []byte("old\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.TrashResource(ctx, user.ID, recent.ID, now.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.TrashResource(ctx, user.ID, old.ID, now.Add(-TrashRetention-time.Hour)); err != nil {
		t.Fatal(err)
	}

	result, err := db.Prune(ctx, 0, now)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if result.Trashed != 1 {
		t.Fatalf("Prune Trashed = %d, want 1", result.Trashed)
	}
	if _, err := db.TrashedResourceForOwner(ctx, user.ID, old.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired trashed resource must be gone: %v", err)
	}
	if _, err := db.TrashedResourceForOwner(ctx, user.ID, recent.ID); err != nil {
		t.Fatalf("recent trashed resource must remain: %v", err)
	}

	// EmptyTrash deletes remaining trashed items without touching live ones.
	emptied, err := db.EmptyTrash(ctx, user.ID)
	if err != nil || emptied != 1 {
		t.Fatalf("EmptyTrash: %d %v", emptied, err)
	}
	if _, err := db.ResourceForOwner(ctx, user.ID, keep.ID); err != nil {
		t.Fatalf("live resource must survive EmptyTrash: %v", err)
	}
	if blobs.Count() != 2 { // initial example resource + keep
		t.Fatalf("unexpected blob count after EmptyTrash: %d", blobs.Count())
	}
}
