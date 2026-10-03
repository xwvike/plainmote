package store

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestAnonymousPasteIsReachableThenGone is the whole shape of the feature: a
// body goes in without an account, one address reaches it, and the address
// stops working when its minutes are up.
func TestAnonymousPasteIsReachableThenGone(t *testing.T) {
	db, _, _ := testDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC()

	resource, link, err := db.CreateAnonymousPaste(ctx, "notes.txt", []byte("port: 7890\n"), 5*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if resource.OwnerID != AnonymousUserID {
		t.Fatalf("a paste must belong to the anonymous account, got %q", resource.OwnerID)
	}

	meta := RequestMeta{Method: "GET", RemoteIP: "203.0.113.9"}
	result, err := db.ConsumeToken(ctx, link.Token, meta, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Allowed {
		t.Fatalf("the address must work while it is alive, got %q", result.Reason)
	}
	body, err := db.ReadContent(ctx, result.Resource)
	if err != nil || string(body) != "port: 7890\n" {
		t.Fatalf("the paste must deliver what was written, got %q: %v", body, err)
	}

	expired, err := db.ConsumeToken(ctx, link.Token, meta, now.Add(6*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if expired.Allowed || expired.Reason != OutcomeExpired {
		t.Fatalf("the address must die with its ttl, got allowed=%v reason=%q", expired.Allowed, expired.Reason)
	}
}

// TestAnonymousTermsCannotBeStretched keeps the ceiling out of reach of a
// crafted request: the short lifetime is what the open endpoint rests on.
func TestAnonymousTermsCannotBeStretched(t *testing.T) {
	db, _, _ := testDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC()

	for _, tc := range []struct {
		label string
		ttl   time.Duration
		body  []byte
	}{
		{"a year", 365 * 24 * time.Hour, []byte("x")},
		{"just over the ceiling", AnonymousMaxTTL + time.Second, []byte("x")},
		{"under the floor", time.Second, []byte("x")},
		{"too large", time.Minute, bytes.Repeat([]byte("a"), AnonymousMaxBytes+1)},
		{"empty", time.Minute, nil},
	} {
		if _, _, err := db.CreateAnonymousPaste(ctx, "x.txt", tc.body, tc.ttl, now); err == nil {
			t.Errorf("%s: should have been refused", tc.label)
		}
	}

	// Zero means "use the default" rather than "no expiry", so a request that
	// simply omits the field cannot produce a link that never dies.
	_, link, err := db.CreateAnonymousPaste(ctx, "x.txt", []byte("x"), 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if link.ExpiresAt == nil {
		t.Fatal("an anonymous link must always expire")
	}
	if got := link.ExpiresAt.Sub(now); got != AnonymousDefaultTTL {
		t.Fatalf("expected the default ttl, got %s", got)
	}
	// The default is not the floor: a shorter lifetime is still a valid choice.
	if _, _, err := db.CreateAnonymousPaste(ctx, "x.txt", []byte("x"), AnonymousMinTTL, now); err != nil {
		t.Fatalf("the shortest lifetime must be accepted: %v", err)
	}
}

// TestAnonymousPasteIsAlwaysServedAsText is the abuse boundary. Whatever the
// file is called, what comes back is text - so the open endpoint cannot be used
// to put a page on this domain.
func TestAnonymousPasteIsAlwaysServedAsText(t *testing.T) {
	db, _, _ := testDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC()

	resource, _, err := db.CreateAnonymousPaste(ctx, "login.html", []byte("<h1>sign in</h1>\n"), time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if resource.ContentType != "text/plain; charset=utf-8" {
		t.Fatalf("an anonymous paste must be text/plain, got %q", resource.ContentType)
	}
	if served := ContentTypeWithEncoding(resource.ContentType, resource.ContentEncoding); !strings.HasPrefix(served, "text/plain") {
		t.Fatalf("delivery must keep it text, got %q", served)
	}
	// The name still rides along, so a download is called what it was called.
	if resource.Filename != "login.html" {
		t.Fatalf("the filename must survive, got %q", resource.Filename)
	}

	// The charset has to be in there, or a paste in Chinese is handed over for
	// the browser to guess at.
	chinese, _, err := db.CreateAnonymousPaste(ctx, "", []byte("名称: 上海节点\n"), time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := ContentTypeWithEncoding(chinese.ContentType, chinese.ContentEncoding); !strings.Contains(got, "charset=utf-8") {
		t.Fatalf("delivery must name the charset, got %q", got)
	}
}

// TestAnonymousFileIsServedByItsBytes: an image, audio or video whose own
// bytes say so is served as that, to be looked at; anything else, whatever it
// is called, is an opaque download.
func TestAnonymousFileIsServedByItsBytes(t *testing.T) {
	db, _, _ := testDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC()
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0xff, 0x00}, 64)...)
	image, _, err := db.CreateAnonymousPaste(ctx, "photo.png", png, time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if image.ContentType != "image/png" || image.Filename != "photo.png" || image.ContentSize != int64(len(png)) {
		t.Fatalf("a PNG must be served as one, got %q %q %d", image.ContentType, image.Filename, image.ContentSize)
	}
	// A UTF-16 export, as Windows writes XML, is text all the same, and is
	// served with its charset.
	utf16 := []byte{0xff, 0xfe}
	for _, r := range "<?xml version=\"1.0\" encoding=\"UTF-16\"?>\r\n<Task>启动</Task>\r\n" {
		utf16 = append(utf16, byte(r), byte(r>>8))
	}
	xml, _, err := db.CreateAnonymousPaste(ctx, "启动xray.xml", utf16, time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := ContentTypeWithEncoding(xml.ContentType, xml.ContentEncoding); got != "text/plain; charset=utf-16le" {
		t.Fatalf("UTF-16 text must be served as text in its charset, got %q", got)
	}
	named, _, err := db.CreateAnonymousPaste(ctx, "photo.png", bytes.Repeat([]byte{0xff, 0x00, 0x13}, 64), time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if named.ContentType != "application/octet-stream" || named.ContentEncoding != "" {
		t.Fatalf("a name proves nothing: unknown bytes must be opaque, got %q", named.ContentType)
	}
}

// TestAnonymousPasteKeepsItsBytes: the store keeps line endings as given. A
// textarea's CRLF is undone before this, by the handler that knows it came
// from one.
func TestAnonymousPasteKeepsItsBytes(t *testing.T) {
	db, _, _ := testDatabase(t)
	ctx := context.Background()
	resource, _, err := db.CreateAnonymousPaste(ctx, "", []byte("a\r\nb\r\n"), time.Minute, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	body, err := db.ReadContent(ctx, resource)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "a\r\nb\r\n" {
		t.Fatalf("the stored bytes must be the ones given, got %q", body)
	}
}

func TestClaimAnonymousPasteCanRetryAfterQuotaRefusal(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC()
	resource, original, err := db.CreateAnonymousPasteFor(ctx, user.ID, "keep.txt", []byte("keep me\n"), 5*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	// testDatabase already creates one owned resource. A ceiling of one must
	// refuse the transfer without consuming the creator's claim.
	if _, err := db.db.Exec(ctx, `UPDATE plans SET max_resources = 1 WHERE is_default`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimAnonymousPaste(ctx, user.ID, resource.ID, now); !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("claim over quota: %v", err)
	}
	if _, link, err := db.ClaimableAnonymousPaste(ctx, user.ID, resource.ID, now); err != nil || link.Token != original.Token {
		t.Fatalf("quota refusal consumed the claim: link=%+v error=%v", link, err)
	}

	if _, err := db.db.Exec(ctx, `UPDATE plans SET max_resources = 2 WHERE is_default`); err != nil {
		t.Fatal(err)
	}
	claimed, err := db.ClaimAnonymousPaste(ctx, user.ID, resource.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != resource.ID || claimed.OwnerID != user.ID {
		t.Fatalf("claim changed the resource: %+v", claimed)
	}
	shares, err := db.ListShares(ctx, user.ID, resource.ID, now)
	if err != nil || len(shares) != 1 || shares[0].Token != original.Token {
		t.Fatalf("claim changed the share: %+v error=%v", shares, err)
	}
}

func TestAnonymousPasteCanBeClaimedAfterTheVisitorSignsIn(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC()
	resource, original, err := db.CreateAnonymousPaste(ctx, "later.txt", []byte("keep later\n"), 5*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := db.ClaimAnonymousPaste(ctx, user.ID, resource.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != resource.ID || claimed.OwnerID != user.ID {
		t.Fatalf("anonymous result was not transferred: %+v", claimed)
	}
	shares, err := db.ListShares(ctx, user.ID, resource.ID, now)
	if err != nil || len(shares) != 1 || shares[0].Token != original.Token {
		t.Fatalf("claim replaced the public share: %+v error=%v", shares, err)
	}
}

// TestExpiredAnonymousPastesArePruned closes the loop: the address dying is
// what makes the body collectable, and nothing else has to ask for it.
func TestExpiredAnonymousPastesArePruned(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	blobs := db.blobs.(*memoryBlobs)
	now := time.Now().UTC()

	alive, _, err := db.CreateAnonymousPaste(ctx, "alive.txt", []byte("still here\n"), 30*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	dead, _, err := db.CreateAnonymousPaste(ctx, "dead.txt", []byte("not for long\n"), time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	objectsBefore := blobs.Count()

	// Retention zero, so this run is only about the pastes.
	result, err := db.Prune(ctx, 0, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if result.Pastes != 1 {
		t.Fatalf("exactly the expired paste should go, removed %d", result.Pastes)
	}
	if got := blobs.Count(); got != objectsBefore-1 {
		t.Fatalf("the body must go with it, %d objects -> %d", objectsBefore, got)
	}
	if _, err := db.ReadContent(ctx, alive); err != nil {
		t.Fatalf("the live paste must be untouched: %v", err)
	}
	if _, err := db.ReadContent(ctx, dead); err == nil {
		t.Fatal("the expired paste's body must be gone")
	}
	// A signed-in account's resources are not anonymous and are never swept.
	if _, err := db.ReadContent(ctx, resource); err != nil {
		t.Fatalf("an owned resource must never be pruned: %v", err)
	}
	if _, err := db.ResourceForOwner(ctx, user.ID, resource.ID); err != nil {
		t.Fatalf("an owned resource must survive: %v", err)
	}
}

// TestAnonymousPastesStayOutOfEveryAccount keeps the shared owner from leaking:
// a paste belongs to a principal nobody signs in as, so it must never appear in
// a real user's list or log.
func TestAnonymousPastesStayOutOfEveryAccount(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC()

	_, link, err := db.CreateAnonymousPaste(ctx, "secret.txt", []byte("not yours\n"), time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ConsumeToken(ctx, link.Token, RequestMeta{Method: "GET"}, now); err != nil {
		t.Fatal(err)
	}

	resources, total, err := db.ListResources(ctx, user.ID, "", 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range resources {
		if r.Filename == "secret.txt" {
			t.Fatal("an anonymous paste must not appear in an account's resources")
		}
	}
	if total != int(mustUsage(t, db, user.ID).Resources) {
		t.Fatalf("the account's own count must not include pastes: list %d usage %d", total, mustUsage(t, db, user.ID).Resources)
	}

	logs, err := db.ListAccess(ctx, user.ID, "", "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range logs {
		if item.ResourceFile == "secret.txt" {
			t.Fatal("an anonymous delivery must not appear in an account's log")
		}
	}
}

func mustUsage(t *testing.T, db *Store, userID string) QuotaUsage {
	t.Helper()
	usage, err := db.UsageForUser(context.Background(), userID)
	if err != nil {
		t.Fatal(err)
	}
	return usage
}
