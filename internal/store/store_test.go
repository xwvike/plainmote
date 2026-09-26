package store

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestGetUserByGitHubIDReturnsTheCompleteUser(t *testing.T) {
	db, user, _ := testDatabase(t)
	got, err := db.GetUser(context.Background(), user.GitHubID)
	if err != nil {
		t.Fatal(err)
	}
	if got != user {
		t.Fatalf("GetUser() = %+v, want %+v", got, user)
	}
	if _, err := db.GetUser(context.Background(), "999999"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing user error = %v, want ErrNotFound", err)
	}
}

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
	// The new expiry counts from the change, and the page's gauge counts
	// with it: the terms began now, not when the link was made.
	if !shares[0].TermsAt.After(shares[0].CreatedAt) || shares[0].ExpiresAt.Sub(shares[0].TermsAt) != 7*24*time.Hour {
		t.Fatalf("terms did not restart: created %v, terms %v, expires %v", shares[0].CreatedAt, shares[0].TermsAt, shares[0].ExpiresAt)
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
	// This case is about names, not about how many a plan allows.
	setPlanLimits(t, db, 500, 1<<20)
	// Only what is actually hazardous: a path separator, a control character,
	// a bidi control that makes a name render as something else, and the two
	// directory entries.
	for _, bad := range []string{
		"a/b.yaml", "a\\b.yaml", "../etc/passwd", ".", "..",
		"line\nbreak.yaml", "bell\a.yaml", "null\x00.yaml",
		"photo\u202Egnp.exe", "mark\u200Fname.yaml", "isolate\u2066name.yaml",
		strings.Repeat("名", 129) + ".yaml",
	} {
		if _, err := db.CreateResource(ctx, user.ID, "bad", bad, []byte("x"), "", ""); err == nil {
			t.Errorf("filename %q should have been rejected", bad)
		}
	}
	// Everything else is the user's own file, named however they name it - in
	// whatever script they write in.
	for _, good := range []string{
		"clash.yaml", "sing-box.json", "hosts.txt", "wg0.conf", "a.b.c", "A1_-+@.txt", "",
		"机场配置.yaml",             // Chinese
		"設定ファイル.yaml",           // Japanese
		"إعدادات.yaml",          // Arabic
		"پیکربندی\u200Cها.yaml", // Persian, held together by a zero-width non-joiner
		"настройки.yaml",        // Cyrillic
		"설정.yaml",               // Korean
		"Ünïcode.txt", "עברית.yaml", "ไทย.yaml",
		"my config.yaml", "config(1).yaml", "a?b", "a#b", "100%.yaml",
		"quo\"te.yaml",  // legal on Linux; the delivery header escapes it
		"-leading.yaml", // legal; what a shell does with it is the shell's business
		".env", "backup..2024.yaml", "trailing.",
		"报告 📊.yaml",
	} {
		if _, err := db.CreateResource(ctx, user.ID, "ok", good, []byte("x"), "", ""); err != nil {
			t.Errorf("filename %q should have been accepted: %v", good, err)
		}
	}
	// Surrounding whitespace is trimmed rather than refused - it is almost
	// always a paste artefact, and there is nothing to warn anyone about.
	trimmed, err := db.CreateResource(ctx, user.ID, "ok", "  spaced.yaml  ", []byte("x"), "", "")
	if err != nil || trimmed.Filename != "spaced.yaml" {
		t.Errorf("surrounding whitespace should be trimmed, got %q: %v", trimmed.Filename, err)
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
	if _, err := db.CreateResource(ctx, alice.ID, "clash", "shared.yaml", []byte("a: 1\n"), "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateResource(ctx, bob.ID, "clash", "shared.yaml", []byte("b: 2\n"), "", ""); err != nil {
		t.Fatalf("a second owner must be able to reuse a filename: %v", err)
	}
}

// A link that stops working leaves the live list and shows up among the ended
// ones, with why and when; the window and the limit bound that list.
func TestEndedSharesSayWhyAndWhen(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	live, err := db.CreateShare(ctx, user.ID, resource.ID, "live", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	revoked, err := db.CreateShare(ctx, user.ID, resource.ID, "revoked", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	once, err := db.CreateShare(ctx, user.ID, resource.ID, "once", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RevokeLink(ctx, user.ID, resource.ID, revoked.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ConsumeToken(ctx, once.Token, RequestMeta{Method: "GET", RemoteIP: "198.51.100.7"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Add(time.Second)
	ended, err := db.ListEndedShares(ctx, user.ID, resource.ID, now, now.Add(-time.Hour), 5)
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]string{}
	for _, link := range ended {
		reason, at := link.Ending(now)
		if at.IsZero() {
			t.Errorf("%s ended with no time", link.Name)
		}
		reasons[link.Name] = reason
	}
	if len(ended) != 2 || reasons["revoked"] != "revoked" || reasons["once"] != "exhausted" {
		t.Fatalf("ended links: %v", reasons)
	}
	if _, found := reasons[live.Name]; found {
		t.Fatal("a live link is not ended")
	}
	if ended, _ := db.ListEndedShares(ctx, user.ID, resource.ID, now, now.Add(-time.Hour), 1); len(ended) != 1 {
		t.Fatalf("the limit holds: %d", len(ended))
	}
	if ended, _ := db.ListEndedShares(ctx, user.ID, resource.ID, now, now.Add(time.Minute), 5); len(ended) != 0 {
		t.Fatalf("the window holds: %d", len(ended))
	}
}

// Only a link that no longer works can be deleted, and its access history
// outlives it.
func TestOnlyEndedLinksCanBeDeleted(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	live, err := db.CreateShare(ctx, user.ID, resource.ID, "live", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	ended, err := db.CreateShare(ctx, user.ID, resource.ID, "ended", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RevokeLink(ctx, user.ID, resource.ID, ended.ID); err != nil {
		t.Fatal(err)
	}
	// A refused visit after the revocation is on the record.
	if _, err := db.ConsumeToken(ctx, ended.Token, RequestMeta{Method: "GET", RemoteIP: "198.51.100.7"}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := db.DeleteEndedLink(ctx, user.ID, resource.ID, live.ID, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a live link must not be deleted: %v", err)
	}
	if err := db.DeleteEndedLink(ctx, user.ID, resource.ID, ended.ID, now); err != nil {
		t.Fatal(err)
	}
	if left, _ := db.ListEndedShares(ctx, user.ID, resource.ID, now.Add(time.Second), now.Add(-time.Hour), 5); len(left) != 0 {
		t.Fatalf("the deleted link is still listed: %d", len(left))
	}
	rows, err := db.ListAccess(ctx, user.ID, "", "", 100)
	if err != nil || len(rows) != 1 || rows[0].LinkName != "ended" {
		t.Fatalf("the access history must keep the deleted link's visit: %v %+v", err, rows)
	}
}
