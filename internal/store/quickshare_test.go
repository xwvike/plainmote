package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Signed-in quick shares made before they were their creator's own are
// moved over when the schema is applied: the live ones with the visits
// already recorded, and nothing else - not an anonymous creator's, not one
// already ended.
func TestSchemaMovesLegacySignedInQuickShares(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC()

	live, liveLink, err := db.CreateAnonymousPasteFor(ctx, user.ID, "live.txt", []byte("live\n"), time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ConsumeToken(ctx, liveLink.Token, RequestMeta{Method: "GET", RemoteIP: "203.0.113.9"}, now); err != nil {
		t.Fatal(err)
	}
	ended, _, err := db.CreateAnonymousPasteFor(ctx, user.ID, "ended.txt", []byte("ended\n"), time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(ctx, `UPDATE links SET expires_at = $2 WHERE resource_id = $1`, ended.ID, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	stranger, _, err := db.CreateAnonymousPaste(ctx, "anon.txt", []byte("anon\n"), time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}

	if err := db.initializeSchema(ctx); err != nil {
		t.Fatal(err)
	}

	moved, err := db.ResourceForOwner(ctx, user.ID, live.ID)
	if err != nil || !moved.QuickShare() || !moved.ExpiresAt.Equal(*liveLink.ExpiresAt) {
		t.Fatalf("the live quick share: %+v %v", moved, err)
	}
	if logs, err := db.ListAccess(ctx, user.ID, live.ID, "", 10); err != nil || len(logs) != 1 {
		t.Fatalf("its visit: %d %v", len(logs), err)
	}
	var claims int
	if err := db.db.QueryRow(ctx, `SELECT COUNT(*) FROM paste_claims WHERE resource_id = $1`, live.ID).Scan(&claims); err != nil || claims != 0 {
		t.Fatalf("its claim is left: %d %v", claims, err)
	}
	for _, id := range []string{ended.ID, stranger.ID} {
		if _, err := db.ResourceForOwner(ctx, user.ID, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s was moved: %v", id, err)
		}
	}
	// Run again, it finds nothing to do.
	if err := db.initializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if again, err := db.ResourceForOwner(ctx, user.ID, live.ID); err != nil || !again.ExpiresAt.Equal(*moved.ExpiresAt) {
		t.Fatalf("a second run changed it: %+v %v", again, err)
	}
}

// A quick share whose time is up is gone for its owner before the sweep
// reaches it: not listed, not counted, not opened.
func TestEndedQuickShareIsHiddenBeforeTheSweep(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC()
	resource, _, err := db.CreateQuickShare(ctx, user.ID, "a.txt", []byte("a\n"), time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(ctx, `UPDATE resources SET expires_at = $2 WHERE id = $1`, resource.ID, now.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ResourceForOwner(ctx, user.ID, resource.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("opened: %v", err)
	}
	if list, total, err := db.ListResources(ctx, user.ID, "", 10, 0); err != nil || total != 1 || len(list) != 1 {
		t.Fatalf("listed: %d %v", total, err)
	}
	if found, err := db.ResolveResources(ctx, user.ID, resource.ID); err != nil || len(found) != 0 {
		t.Fatalf("resolved: %+v %v", found, err)
	}
	quota, err := db.QuotaForUser(ctx, user.ID, now)
	if err != nil || quota.Usage.Resources != 1 {
		t.Fatalf("counted: %+v %v", quota.Usage, err)
	}
	if err := db.KeepQuickShare(ctx, user.ID, resource.ID, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("kept after its end: %v", err)
	}
}

// A quick share takes its creator's quota like any resource does.
func TestQuickShareCountsTowardTheQuota(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC()
	quota, err := db.QuotaForUser(ctx, user.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(ctx, `UPDATE plans SET max_storage = $1`, quota.Usage.StorageBytes+4); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.CreateQuickShare(ctx, user.ID, "big.txt", []byte("too large\n"), time.Hour, now); err == nil || !IsRefusal(err) {
		t.Fatalf("a quick share past the quota: %v", err)
	}
	if _, _, err := db.CreateQuickShare(ctx, AnonymousUserID, "x.txt", []byte("x"), time.Hour, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the anonymous account cannot own a quick share: %v", err)
	}
}
