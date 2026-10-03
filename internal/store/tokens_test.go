package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestUserCodesAreTypedLoosely(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"KQWD-7XHM", "KQWD7XHM", true},
		{"kqwd7xhm", "KQWD7XHM", true},
		{" kqwd 7xhm ", "KQWD7XHM", true},
		{"KQWD-7XH", "", false},
		{"KQWD-7XHMM", "", false},
		{"KQWD-7XH0", "", false}, // 0 is not in the alphabet
		{"KQWD-7XHI", "", false}, // nor is I
		{"KQWD-7XHＭ", "", false},
	} {
		got, ok := NormalizeUserCode(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("NormalizeUserCode(%q) = %q, %v", tc.in, got, ok)
		}
	}
	for i := 0; i < 200; i++ {
		code, err := newUserCode()
		if err != nil {
			t.Fatal(err)
		}
		if normalized, ok := NormalizeUserCode(FormatUserCode(code)); !ok || normalized != code {
			t.Fatalf("a drawn code does not read back: %q", code)
		}
	}
	if FormatUserCode("KQWD7XHM") != "KQWD-7XHM" {
		t.Fatal("codes are shown in two groups of four")
	}
}

// TestDeviceSignIn walks the whole exchange: a code asked for, polled too
// fast, found in the browser, approved, exchanged once for a token that
// works, and nothing left to exchange again.
func TestDeviceSignIn(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if _, _, _, err := db.CreateDeviceGrant(ctx, DeviceRequest{Scope: "admin"}, now); !errors.Is(err, ErrBadScope) {
		t.Fatalf("an unknown scope was accepted: %v", err)
	}
	deviceCode, userCode, grant, err := db.CreateDeviceGrant(ctx, DeviceRequest{
		Scope: TokenScopeWrite, DeviceName: "mira-mbp\x00\n" + strings.Repeat("x", 200), DeviceOS: "darwin/arm64",
		ClientVersion: "05718f86b988", RequestIP: "203.0.113.24",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(grant.DeviceName, "\x00\n") || len([]rune(grant.DeviceName)) > deviceTextMaxRunes {
		t.Fatalf("the device name is not tidied: %q", grant.DeviceName)
	}

	first, err := db.ExchangeDeviceCode(ctx, deviceCode, now)
	if err != nil || first.State != DevicePending || first.Interval != DevicePollInterval {
		t.Fatalf("first poll: %+v %v", first, err)
	}
	fast, err := db.ExchangeDeviceCode(ctx, deviceCode, now.Add(time.Second))
	if err != nil || fast.State != DeviceSlowDown || fast.Interval != DevicePollInterval+DevicePollBackoff {
		t.Fatalf("a fast poll is told to slow down: %+v %v", fast, err)
	}

	found, err := db.PendingDeviceGrant(ctx, userCode, now)
	if err != nil || found.ID != grant.ID || found.RequestIP != "203.0.113.24" {
		t.Fatalf("the code finds its grant: %+v %v", found, err)
	}
	if _, err := db.PendingDeviceGrant(ctx, "AAAAAAAA", now); !errors.Is(err, ErrNotFound) {
		t.Fatal("an unknown code finds nothing")
	}
	// The id alone is not enough: the code has to come with it.
	if err := db.DecideDeviceGrant(ctx, user.ID, grant.ID, "AAAAAAAA", true, now); !errors.Is(err, ErrNotFound) {
		t.Fatal("a grant was approved without its code")
	}
	if err := db.DecideDeviceGrant(ctx, user.ID, grant.ID, userCode, true, now); err != nil {
		t.Fatal(err)
	}
	if err := db.DecideDeviceGrant(ctx, user.ID, grant.ID, userCode, false, now); !errors.Is(err, ErrNotFound) {
		t.Fatal("a decided grant was decided again")
	}

	later := now.Add(30 * time.Second)
	approved, err := db.ExchangeDeviceCode(ctx, deviceCode, later)
	if err != nil || approved.State != DeviceApproved || !strings.HasPrefix(approved.Token, TokenPrefix) || approved.User.ID != user.ID {
		t.Fatalf("approval yields a token: %+v %v", approved, err)
	}
	if again, err := db.ExchangeDeviceCode(ctx, deviceCode, later.Add(time.Minute)); err != nil || again.State != DeviceExpired {
		t.Fatalf("a device code yields one token: %+v %v", again, err)
	}

	who, token, err := db.TokenUser(ctx, approved.Token, "203.0.113.24", later)
	if err != nil || who.ID != user.ID || !token.CanWrite() || token.DeviceOS != "darwin/arm64" || token.LastUsedAt == nil {
		t.Fatalf("the token signs in: %+v %+v %v", who, token, err)
	}
	if !token.ExpiresAt.Equal(approved.APIToken.ExpiresAt) || token.ExpiresAt.Sub(later) > TokenLifetime+time.Second {
		t.Fatal("the token lives a fixed lifetime from sign-in")
	}
	if _, _, err := db.TokenUser(ctx, approved.Token, "", later.Add(TokenLifetime)); !errors.Is(err, ErrNotFound) {
		t.Fatal("an expired token still works")
	}
	if _, _, err := db.TokenUser(ctx, strings.TrimPrefix(approved.Token, TokenPrefix), "", later); !errors.Is(err, ErrNotFound) {
		t.Fatal("a token without its prefix works")
	}

	tokens, err := db.ListAPITokens(ctx, user.ID, later)
	if err != nil || len(tokens) != 1 || tokens[0].LastUsedIP != "203.0.113.24" {
		t.Fatalf("the account lists its token: %+v %v", tokens, err)
	}
	if err := db.RevokeAPIToken(ctx, user.ID, tokens[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.TokenUser(ctx, approved.Token, "", later); !errors.Is(err, ErrNotFound) {
		t.Fatal("a revoked token still works")
	}
}

func TestDeviceSignInRefusals(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Denied: the command line hears it once, then the code is gone.
	deviceCode, userCode, grant, err := db.CreateDeviceGrant(ctx, DeviceRequest{Scope: TokenScopeRead}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DecideDeviceGrant(ctx, user.ID, grant.ID, userCode, false, now); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.ExchangeDeviceCode(ctx, deviceCode, now); got.State != DeviceDenied {
		t.Fatalf("a denial reaches the command line: %+v", got)
	}
	if got, _ := db.ExchangeDeviceCode(ctx, deviceCode, now.Add(10*time.Second)); got.State != DeviceExpired {
		t.Fatalf("a denied code is gone: %+v", got)
	}

	// Expired: neither the browser nor the command line can use it.
	deviceCode, userCode, grant, err = db.CreateDeviceGrant(ctx, DeviceRequest{Scope: TokenScopeRead}, now)
	if err != nil {
		t.Fatal(err)
	}
	late := now.Add(DeviceGrantLifetime)
	if _, err := db.PendingDeviceGrant(ctx, userCode, late); !errors.Is(err, ErrNotFound) {
		t.Fatal("an expired code is still found")
	}
	if err := db.DecideDeviceGrant(ctx, user.ID, grant.ID, userCode, true, late); !errors.Is(err, ErrNotFound) {
		t.Fatal("an expired code was approved")
	}
	if got, _ := db.ExchangeDeviceCode(ctx, deviceCode, late); got.State != DeviceExpired {
		t.Fatalf("an expired code is reported expired: %+v", got)
	}
	if got, _ := db.ExchangeDeviceCode(ctx, "nonsense", now); got.State != DeviceExpired {
		t.Fatalf("an unknown device code reads as expired: %+v", got)
	}

	// Suspended between approval and the next poll: no token, and the
	// suspension ends tokens already issued.
	deviceCode, userCode, grant, err = db.CreateDeviceGrant(ctx, DeviceRequest{Scope: TokenScopeWrite}, now)
	if err != nil {
		t.Fatal(err)
	}
	issuedCode, issuedUser, issuedGrant, err := db.CreateDeviceGrant(ctx, DeviceRequest{Scope: TokenScopeRead}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DecideDeviceGrant(ctx, user.ID, issuedGrant.ID, issuedUser, true, now); err != nil {
		t.Fatal(err)
	}
	issued, err := db.ExchangeDeviceCode(ctx, issuedCode, now)
	if err != nil || issued.State != DeviceApproved {
		t.Fatalf("issue: %+v %v", issued, err)
	}
	if err := db.DecideDeviceGrant(ctx, user.ID, grant.ID, userCode, true, now); err != nil {
		t.Fatal(err)
	}
	if err := db.AdminSuspendUser(ctx, AdminActor{KeyID: "test"}, user.ID, "test", now); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.ExchangeDeviceCode(ctx, deviceCode, now.Add(10*time.Second)); got.State == DeviceApproved {
		t.Fatal("a suspended account got a token")
	}
	if _, _, err := db.TokenUser(ctx, issued.Token, "", now); !errors.Is(err, ErrNotFound) {
		t.Fatal("a suspended account's token still works")
	}
	if err := db.AdminUnsuspendUser(ctx, AdminActor{KeyID: "test"}, user.ID, "test", now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.TokenUser(ctx, issued.Token, "", now); !errors.Is(err, ErrNotFound) {
		t.Fatal("lifting a suspension woke an old token")
	}
	// And a suspended account cannot approve anything in the meantime.
	if err := db.AdminSuspendUser(ctx, AdminActor{KeyID: "test"}, user.ID, "again", now); err != nil {
		t.Fatal(err)
	}
	_, code, pending, err := db.CreateDeviceGrant(ctx, DeviceRequest{Scope: TokenScopeRead}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DecideDeviceGrant(ctx, user.ID, pending.ID, code, true, now); !errors.Is(err, ErrNotFound) {
		t.Fatal("a suspended account approved a sign-in")
	}
}

func TestResolveResources(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	other, err := db.CreateResource(ctx, user.ID, "nginx", "nginx.conf", []byte("listen 80;\n"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	twin, err := db.CreateResource(ctx, user.ID, "example.conf", "twin.conf", []byte("x\n"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	stranger, err := db.UpsertUser(ctx, "999", "stranger", "", "")
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := db.CreateResource(ctx, stranger.ID, "nginx", "nginx.conf", []byte("theirs\n"), "", "")
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
	listed, err := db.APIResources(ctx, user.ID, "nginx")
	if err != nil || len(listed) != 1 || listed[0].ContentSize == 0 || listed[0].Version != 1 {
		t.Fatalf("the listing carries size and version: %+v %v", listed, err)
	}
}
