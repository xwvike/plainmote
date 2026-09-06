package store

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// A new resource is reachable by nobody until something is shared.
func TestNewResourceHasNoLinks(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	shares, err := db.ListShares(ctx, user.ID, resource.ID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(shares) != 0 {
		t.Fatalf("a new resource must have no shares, got %d", len(shares))
	}
}

// Expiry is the end of a share: it is refused, and no successor appears.
func TestShareExpiresWithoutSuccessor(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	share, err := db.CreateShare(ctx, user.ID, resource.ID, "临时", 30*time.Minute, 0)
	if err != nil {
		t.Fatal(err)
	}
	later := time.Now().UTC().Add(time.Hour)
	result, err := db.ConsumeToken(ctx, share.Token, RequestMeta{Method: "GET"}, later)
	if err != nil {
		t.Fatal(err)
	}
	if result.Allowed || result.Reason != "expired" {
		t.Fatalf("expected an expired refusal, got allowed=%v reason=%q", result.Allowed, result.Reason)
	}
	shares, err := db.ListShares(ctx, user.ID, resource.ID, later)
	if err != nil {
		t.Fatal(err)
	}
	if len(shares) != 0 {
		t.Fatalf("an expired share must not be replaced, found %d live shares", len(shares))
	}
}

// A single-use share opens once and is then dead - again with no successor.
func TestSingleUseShareDiesAfterOneUse(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	share, err := db.CreateShare(ctx, user.ID, resource.ID, "一次性", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	meta := RequestMeta{Method: "GET"}
	first, err := db.ConsumeToken(ctx, share.Token, meta, now)
	if err != nil || !first.Allowed {
		t.Fatalf("first use must be allowed: %v %v", first.Allowed, err)
	}
	second, err := db.ConsumeToken(ctx, share.Token, meta, now)
	if err != nil {
		t.Fatal(err)
	}
	if second.Allowed || second.Reason != "exhausted" {
		t.Fatalf("second use must be refused as exhausted, got allowed=%v reason=%q", second.Allowed, second.Reason)
	}
	shares, err := db.ListShares(ctx, user.ID, resource.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(shares) != 0 {
		t.Fatalf("a used-up share must drop off the list, found %d", len(shares))
	}
}

// Changing the terms of a share must not change its address: whoever already
// has the link keeps working.
func TestUpdatingShareKeepsItsAddress(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	share, err := db.CreateShare(ctx, user.ID, resource.ID, "", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateShare(ctx, user.ID, resource.ID, share.ID, "同事 A", 7*24*time.Hour, 1); err != nil {
		t.Fatal(err)
	}
	shares, err := db.ListShares(ctx, user.ID, resource.ID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(shares) != 1 {
		t.Fatalf("expected one share, got %d", len(shares))
	}
	if shares[0].Token != share.Token {
		t.Fatal("updating a share must not mint a new token")
	}
	if shares[0].Name != "同事 A" || shares[0].MaxUses != 1 {
		t.Fatalf("terms were not applied: %+v", shares[0])
	}
}

// A share with no expiry and no use limit is permanent until it is revoked.
func TestPermanentShareLivesUntilRevoked(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	share, err := db.CreateShare(ctx, user.ID, resource.ID, "长期", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if share.ExpiresAt != nil || !share.Never() {
		t.Fatalf("a zero ttl must mean no deadline: %+v", share.ExpiresAt)
	}
	meta := RequestMeta{Method: "GET"}
	farFuture := time.Now().UTC().Add(5000 * time.Hour)
	result, err := db.ConsumeToken(ctx, share.Token, meta, farFuture)
	if err != nil || !result.Allowed {
		t.Fatalf("a permanent share must still open years later: %v %v", result.Reason, err)
	}
	if err := db.RevokeLink(ctx, user.ID, resource.ID, share.ID); err != nil {
		t.Fatal(err)
	}
	after, err := db.ConsumeToken(ctx, share.Token, meta, farFuture)
	if err != nil {
		t.Fatal(err)
	}
	if after.Allowed || after.Reason != "revoked" {
		t.Fatalf("a revoked share must stop working, got allowed=%v reason=%q", after.Allowed, after.Reason)
	}
}

// A resource may hold several permanent shares.
func TestSeveralPermanentSharesCoexist(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	for _, name := range []string{"CI", "备份机"} {
		if _, err := db.CreateShare(ctx, user.ID, resource.ID, name, 0, 0); err != nil {
			t.Fatal(err)
		}
	}
	shares, err := db.ListShares(ctx, user.ID, resource.ID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(shares) != 2 {
		t.Fatalf("expected two permanent shares, got %d", len(shares))
	}
}

func TestRevokeAllSharesClearsEverything(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	permanent, err := db.CreateShare(ctx, user.ID, resource.ID, "长期", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		if _, err := db.CreateShare(ctx, user.ID, resource.ID, name, time.Hour, 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.RevokeShares(ctx, user.ID, resource.ID); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	shares, err := db.ListShares(ctx, user.ID, resource.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(shares) != 0 {
		t.Fatalf("expected every share revoked, %d left", len(shares))
	}
	result, err := db.ConsumeToken(ctx, permanent.Token, RequestMeta{Method: "GET"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if result.Allowed {
		t.Fatal("a bulk revoke must take the permanent share with it")
	}
}

// The list's status column is derived, so it has to count only live links.
func TestListResourcesSummarisesLiveLinks(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	assert := func(want int) {
		t.Helper()
		list, _, err := db.ListResources(ctx, user.ID, "", 10, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 {
			t.Fatalf("expected one resource, got %d", len(list))
		}
		if list[0].LiveShares != want {
			t.Fatalf("summary mismatch: shares=%d, want %d", list[0].LiveShares, want)
		}
	}
	assert(0)
	if _, err := db.CreateShare(ctx, user.ID, resource.ID, "长期", 0, 0); err != nil {
		t.Fatal(err)
	}
	assert(1)
	share, err := db.CreateShare(ctx, user.ID, resource.ID, "x", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	assert(2)
	if _, err := db.ConsumeToken(ctx, share.Token, RequestMeta{Method: "GET"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	assert(1) // used up, so no longer live
}

func TestTokenCipherDoesNotStorePlaintext(t *testing.T) {
	db, user, resource := testDatabase(t)
	share, err := db.CreateShare(context.Background(), user.ID, resource.ID, "secret", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	var ciphertext []byte
	if err := db.db.QueryRow(context.Background(), `SELECT token_ciphertext FROM links WHERE id = $1`, share.ID).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(ciphertext, []byte(share.Token)) {
		t.Fatal("the stored value must not contain the plaintext token")
	}
	plaintext, err := db.cipher.open(ciphertext)
	if err != nil || plaintext != share.Token {
		t.Fatalf("round trip failed: %q %v", plaintext, err)
	}
}

// A filename is only the tail of the address, so it needs hygiene rather than
// uniqueness: nothing that could climb out of its URL segment.
func TestFilenameValidation(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	for _, bad := range []string{"../etc/passwd", "a/b.yaml", "with space.yaml", "-leading.yaml", ".hidden", "a?b", "a#b", "a\b"} {
		if _, err := db.CreateResource(ctx, user.ID, "bad", bad, []byte("x"), ""); err == nil {
			t.Errorf("filename %q should have been rejected", bad)
		}
	}
	for _, good := range []string{"clash.yaml", "sing-box.json", "hosts.txt", "wg0.conf", "a.b.c", "A1_-+@.txt", ""} {
		if _, err := db.CreateResource(ctx, user.ID, "ok", good, []byte("x"), ""); err != nil {
			t.Errorf("filename %q should have been accepted: %v", good, err)
		}
	}
}

// Two people may use the same filename: it is decoration, not an address.
func TestFilenamesMayCollideAcrossOwners(t *testing.T) {
	db, alice, _ := testDatabase(t)
	ctx := context.Background()
	bob, err := db.UpsertUser(ctx, "999", "bob", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateResource(ctx, alice.ID, "clash", "shared.yaml", []byte("a: 1\n"), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateResource(ctx, bob.ID, "clash", "shared.yaml", []byte("b: 2\n"), ""); err != nil {
		t.Fatalf("a second owner must be able to reuse a filename: %v", err)
	}
}
