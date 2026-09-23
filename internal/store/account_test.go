package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDeleteAccountRemovesEverythingItOwns(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	blobs := db.blobs.(*memoryBlobs)
	now := time.Now().UTC()

	other, err := db.UpsertUser(ctx, "200", "bob", "Bob", "")
	if err != nil {
		t.Fatal(err)
	}
	kept, err := db.CreateResource(ctx, other.ID, "Kept", "kept.txt", []byte("bob's"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateResource(ctx, user.ID, "Second", "second.txt", []byte("more"), "", ""); err != nil {
		t.Fatal(err)
	}
	share, err := db.CreateShare(ctx, user.ID, resource.ID, "", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordAccess(ctx, AccessEvent{
		OwnerID: user.ID, ResourceID: resource.ID, LinkID: share.ID,
		Outcome: OutcomeSuccess, Status: 200,
	}, RequestMeta{Method: "GET", RemoteIP: "203.0.113.9"}); err != nil {
		t.Fatal(err)
	}
	session, _, _, err := db.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.CreateAnonymousPasteFor(ctx, user.ID, "", []byte("pending claim"), time.Minute, now); err != nil {
		t.Fatal(err)
	}
	objects := blobs.count()

	if err := db.DeleteAccount(ctx, user.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Account(ctx, user.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the account must be gone, got %v", err)
	}
	if got := blobs.count(); got != objects-2 {
		t.Fatalf("both bodies must be deleted: %d objects -> %d", objects, got)
	}
	if _, _, err := db.SessionUser(ctx, session); err == nil {
		t.Fatal("a session of a deleted account must not sign anyone in")
	}
	if result, err := db.ConsumeToken(ctx, share.Token, RequestMeta{Method: "GET"}, now); err != nil || result.Allowed {
		t.Fatalf("a link of a deleted account must not resolve: %+v %v", result, err)
	}
	for table, query := range map[string]string{
		"resources":    `SELECT COUNT(*) FROM resources WHERE owner_id = $1`,
		"access_logs":  `SELECT COUNT(*) FROM access_logs WHERE owner_id = $1`,
		"paste_claims": `SELECT COUNT(*) FROM paste_claims WHERE user_id = $1`,
		"user_plans":   `SELECT COUNT(*) FROM user_plans WHERE user_id = $1`,
	} {
		var count int
		if err := db.db.QueryRow(ctx, query, user.ID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Errorf("%s still holds %d rows of the deleted account", table, count)
		}
	}

	// The quick share itself was never the account's: it stays anonymous and
	// expires on its own schedule.
	var anonymous int
	if err := db.db.QueryRow(ctx, `SELECT COUNT(*) FROM resources WHERE owner_id = $1`, AnonymousUserID).Scan(&anonymous); err != nil {
		t.Fatal(err)
	}
	if anonymous != 1 {
		t.Fatalf("the pending quick share must stay anonymous, found %d", anonymous)
	}
	if _, err := db.ResourceForOwner(ctx, other.ID, kept.ID); err != nil {
		t.Fatalf("another account's resource was touched: %v", err)
	}
}

// A write that arrives after the account is gone must not leave its body in
// the bucket with nothing pointing at it.
func TestWriteAfterDeleteLeavesNoObject(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	blobs := db.blobs.(*memoryBlobs)
	if err := db.DeleteAccount(ctx, user.ID); err != nil {
		t.Fatal(err)
	}
	objects := blobs.count()
	if _, err := db.CreateResource(ctx, user.ID, "Late", "late.txt", []byte("late"), "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("creating under a deleted account: %v", err)
	}
	if got := blobs.count(); got != objects {
		t.Fatalf("the refused write left an object behind: %d -> %d", objects, got)
	}
}

func TestDeleteAccountRefusesTheAnonymousAccount(t *testing.T) {
	db, _, _ := testDatabase(t)
	if err := db.DeleteAccount(context.Background(), AnonymousUserID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the anonymous account must not be deletable, got %v", err)
	}
}

func TestExportIsTheAccountsWholeHistory(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	other, err := db.UpsertUser(ctx, "200", "bob", "Bob", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateResource(ctx, other.ID, "Not mine", "no.txt", []byte("x"), "", ""); err != nil {
		t.Fatal(err)
	}
	live, err := db.CreateShare(ctx, user.ID, resource.ID, "live", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	revoked, err := db.CreateShare(ctx, user.ID, resource.ID, "revoked", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RevokeLink(ctx, user.ID, resource.ID, revoked.ID); err != nil {
		t.Fatal(err)
	}
	for _, owner := range []string{user.ID, other.ID} {
		if err := db.RecordAccess(ctx, AccessEvent{OwnerID: owner, Outcome: OutcomeSuccess, Status: 200},
			RequestMeta{Method: "GET", RemoteIP: "203.0.113.9"}); err != nil {
			t.Fatal(err)
		}
	}

	resources, links, err := db.ExportResources(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(resources) != 1 || resources[0].ID != resource.ID {
		t.Fatalf("export must hold exactly the account's resources: %+v", resources)
	}
	if len(links) != 2 || links[0].ID != live.ID || links[1].ID != revoked.ID || links[1].RevokedAt == nil {
		t.Fatalf("export must hold every link, revoked ones included: %+v", links)
	}

	var logs int
	if err := db.EachAccessLog(ctx, user.ID, func(AccessLog) error { logs++; return nil }); err != nil {
		t.Fatal(err)
	}
	if logs != 1 {
		t.Fatalf("export must hold exactly the account's access log, got %d rows", logs)
	}
}

func TestExportsAreRationedPerWindow(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	other, err := db.UpsertUser(ctx, "200", "bob", "Bob", "")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now().UTC().Truncate(time.Second)

	for i := 0; i < ExportLimit; i++ {
		if _, err := db.ReserveExport(ctx, user.ID, start.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatalf("export %d within the limit: %v", i+1, err)
		}
	}
	allowance, err := db.ReserveExport(ctx, user.ID, start.Add(3*time.Hour))
	if !errors.Is(err, ErrExportLimit) {
		t.Fatalf("an export over the limit must be refused, got %v", err)
	}
	if !allowance.NextAt.Equal(start.Add(ExportWindow)) {
		t.Fatalf("the next export opens when the oldest leaves the window: got %s, want %s", allowance.NextAt, start.Add(ExportWindow))
	}
	if _, err := db.ReserveExport(ctx, other.ID, start.Add(3*time.Hour)); err != nil {
		t.Fatalf("the limit is per account, another account was refused: %v", err)
	}
	if _, err := db.ReserveExport(ctx, user.ID, start.Add(ExportWindow+time.Second)); err != nil {
		t.Fatalf("an export after the oldest left the window must be allowed: %v", err)
	}

	if _, err := db.Prune(ctx, 0, start.Add(ExportWindow+2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	var kept int
	if err := db.db.QueryRow(ctx, `SELECT COUNT(*) FROM account_exports WHERE user_id = $1`, user.ID).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept != 1 {
		t.Fatalf("pruning must keep only exports still inside the window, kept %d", kept)
	}
}

// A missing name is stored as missing. The placeholder shown in its place is
// in the reader's language, which the store does not know.
func TestUnnamedThingsAreStoredWithoutAPlaceholder(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC()
	resource, err := db.CreateResource(ctx, user.ID, "", "", []byte("no name"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if resource.Name != "" {
		t.Fatalf("an unnamed resource was stored as %q", resource.Name)
	}
	paste, _, err := db.CreateAnonymousPasteFor(ctx, "", "", []byte("quick"), time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if paste.Name != "" {
		t.Fatalf("an unnamed quick share was stored as %q", paste.Name)
	}
	share, err := db.CreateShare(ctx, user.ID, resource.ID, "", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	result, err := db.ConsumeToken(ctx, share.Token, RequestMeta{Method: "GET"}, now)
	if err != nil || !result.Allowed {
		t.Fatalf("consume: %+v %v", result, err)
	}
	if result.LinkName != "" {
		t.Fatalf("an unnamed link reaches the access log as %q", result.LinkName)
	}
}
