package store

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// TestChangedTokenKeyDegradesInsteadOfFailing is what happens when
// PLAINMOTE_TOKEN_KEY is changed or restored wrongly. It used to take down every
// resource page that had a share on it, because one row that would not decrypt
// failed the whole list. The links themselves never stopped working: delivery
// finds them by hash and does not decrypt. So they stay listed, without an
// address, and stay revocable.
func TestChangedTokenKeyDegradesInsteadOfFailing(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC()

	share, err := db.CreateShare(ctx, user.ID, resource.ID, "旧密钥", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}

	other, err := newTokenCipher(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	db.cipher = other

	shares, err := db.ListShares(ctx, user.ID, resource.ID, now)
	if err != nil {
		t.Fatalf("a share the key cannot open must not fail the list: %v", err)
	}
	if len(shares) != 1 || !shares[0].Unreadable || shares[0].Token != "" {
		t.Fatalf("the share must be listed as unreadable with no address, got %+v", shares)
	}

	// Whoever holds the address can still use it.
	result, err := db.ConsumeToken(ctx, share.Token, RequestMeta{Method: "GET"}, now)
	if err != nil || !result.Allowed {
		t.Fatalf("delivery never decrypts, so the link must still work: allowed=%v err=%v", result.Allowed, err)
	}

	// And its owner can still take it back.
	if err := db.RevokeLink(ctx, user.ID, resource.ID, shares[0].ID); err != nil {
		t.Fatalf("an unreadable share must still be revocable: %v", err)
	}
	if left, err := db.ListShares(ctx, user.ID, resource.ID, now); err != nil || len(left) != 0 {
		t.Fatalf("revoked share must be gone: %d %v", len(left), err)
	}
}

// TestChangedTokenKeyRetiresQuickShares covers the pages that exist only to
// show an address. Without one there is nothing to show, and a quick share
// lives for minutes, so it reads as gone rather than as a server error.
func TestChangedTokenKeyRetiresQuickShares(t *testing.T) {
	db, _, _ := testDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC()

	resource, _, err := db.CreateAnonymousPaste(ctx, "", []byte("x\n"), 5*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	other, err := newTokenCipher(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	db.cipher = other

	if _, _, err := db.AnonymousPasteResult(ctx, resource.ID, now); err != ErrNotFound {
		t.Fatalf("an unreadable quick share must read as gone, got %v", err)
	}
}
