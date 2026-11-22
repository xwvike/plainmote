package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCreateAndUseAPITokens(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if _, _, err := db.CreateAPIToken(ctx, user.ID, "ci", "admin", TokenLifetime, now); !errors.Is(err, ErrBadScope) {
		t.Fatalf("an unknown scope was accepted: %v", err)
	}
	if _, _, err := db.CreateAPIToken(ctx, user.ID, "   \n", TokenScopeRead, TokenLifetime, now); !errors.Is(err, ErrTokenNameRequired) {
		t.Fatalf("a blank name was accepted: %v", err)
	}
	if _, _, err := db.CreateAPIToken(ctx, user.ID, "ci", TokenScopeRead, -time.Second, now); !errors.Is(err, ErrBadTokenTTL) {
		t.Fatalf("a negative ttl was accepted: %v", err)
	}
	if _, _, err := db.CreateAPIToken(ctx, user.ID, "ci", TokenScopeRead, MaxTokenTTL+time.Hour, now); !errors.Is(err, ErrBadTokenTTL) {
		t.Fatalf("an overlong ttl was accepted: %v", err)
	}

	// Token with finite expiry.
	secret, created, err := db.CreateAPIToken(ctx, user.ID, "deploy-bot\x00\n"+strings.Repeat("x", 200), TokenScopeWrite, TokenLifetime, now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, TokenPrefix) {
		t.Fatalf("secret missing prefix: %q", secret)
	}
	if strings.ContainsAny(created.DeviceName, "\x00\n") || len([]rune(created.DeviceName)) > deviceTextMaxRunes {
		t.Fatalf("name was not tidied: %q", created.DeviceName)
	}
	if created.ExpiresAt == nil || created.ExpiresAt.Sub(now) != TokenLifetime {
		t.Fatalf("unexpected expiry: %+v", created.ExpiresAt)
	}

	who, token, err := db.TokenUser(ctx, secret, "203.0.113.24", now)
	if err != nil || who.ID != user.ID || !token.CanWrite() || token.LastUsedAt == nil || token.LastUsedIP != "203.0.113.24" {
		t.Fatalf("token lookup: %+v %+v %v", who, token, err)
	}
	if _, _, err := db.TokenUser(ctx, secret, "", now.Add(TokenLifetime)); !errors.Is(err, ErrNotFound) {
		t.Fatal("an expired token still works")
	}
	if _, _, err := db.TokenUser(ctx, strings.TrimPrefix(secret, TokenPrefix), "", now); !errors.Is(err, ErrNotFound) {
		t.Fatal("a token without its prefix works")
	}

	// Token that never expires (ttl == 0).
	foreverSecret, forever, err := db.CreateAPIToken(ctx, user.ID, "backup", TokenScopeRead, 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if forever.ExpiresAt != nil {
		t.Fatalf("never-expiring token got expiry: %+v", forever.ExpiresAt)
	}
	if _, got, err := db.TokenUser(ctx, foreverSecret, "", now.Add(10*365*24*time.Hour)); err != nil || got.CanWrite() || got.ExpiresAt != nil {
		t.Fatalf("never-expiring token failed far in the future: %+v %v", got, err)
	}

	tokens, err := db.ListAPITokens(ctx, user.ID, now)
	if err != nil || len(tokens) != 2 {
		t.Fatalf("listed tokens: %+v %v", tokens, err)
	}
	if err := db.RevokeAPIToken(ctx, user.ID, created.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.TokenUser(ctx, secret, "", now); !errors.Is(err, ErrNotFound) {
		t.Fatal("a revoked token still works")
	}

	// Suspension ends existing tokens and refuses creating new ones.
	if err := db.AdminSuspendUser(ctx, AdminActor{KeyID: "test"}, user.ID, "test", now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.TokenUser(ctx, foreverSecret, "", now); !errors.Is(err, ErrNotFound) {
		t.Fatal("a suspended account's token still works")
	}
	if _, _, err := db.CreateAPIToken(ctx, user.ID, "after-suspend", TokenScopeRead, 0, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a suspended account created a token: %v", err)
	}
	if err := db.AdminUnsuspendUser(ctx, AdminActor{KeyID: "test"}, user.ID, "test", now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.TokenUser(ctx, foreverSecret, "", now); !errors.Is(err, ErrNotFound) {
		t.Fatal("lifting a suspension woke an old token")
	}
}

func TestResolveResources(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	other, err := db.CreateResource(ctx, user.ID, "nginx", "nginx.conf", []byte("listen 80;\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	twin, err := db.CreateResource(ctx, user.ID, "example.conf", "twin.conf", []byte("x\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := db.UpsertUser(ctx, "999", "stranger", "", "")
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := db.CreateResource(ctx, stranger.ID, "nginx", "nginx.conf", []byte("theirs\n"), "")
	if err != nil {
		t.Fatal(err)
	}

	ids := func(ref string) []string {
		t.Helper()
		found, err := db.ResolveResources(ctx, user.ID, ref)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, r := range found {
			out = append(out, r.ID)
		}
		return out
	}
	if got := ids(other.ID); len(got) != 1 || got[0] != other.ID {
		t.Fatalf("by id: %v", got)
	}
	if got := ids(strings.ToUpper(other.ID[:8])); len(got) != 1 || got[0] != other.ID {
		t.Fatalf("by prefix: %v", got)
	}
	if got := ids(other.ID[:5]); len(got) != 0 {
		t.Fatalf("a prefix under six characters matches nothing: %v", got)
	}
	if got := ids("nginx.conf"); len(got) != 1 || got[0] != other.ID {
		t.Fatalf("by filename, own resources only: %v", got)
	}
	if got := ids("example.conf"); len(got) != 2 {
		t.Fatalf("a name and a filename alike are both reported: %v (%s %s)", got, resource.ID, twin.ID)
	}
	if got := ids(foreign.ID); len(got) != 0 {
		t.Fatal("another account's resource resolved")
	}
	if got := ids("%"); len(got) != 0 {
		t.Fatalf("a wildcard is not a reference: %v", got)
	}
	listed, err := db.APIResources(ctx, user.ID, "nginx", "")
	if err != nil || len(listed) != 1 || listed[0].ContentSize == 0 || listed[0].Version != 1 {
		t.Fatalf("the listing carries size and version: %+v %v", listed, err)
	}
}
