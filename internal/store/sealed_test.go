package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The service cannot read what it stores here, so these stand in for real
// ciphertext: the right magic and length, and a byte to tell them apart.
func sealedPart(mark byte) SealedPart {
	return SealedPart{
		Content: append([]byte("PMr1"), bytes.Repeat([]byte{mark}, 48)...),
		Meta:    append([]byte("PMm1"), bytes.Repeat([]byte{mark}, 40)...),
	}
}

func wrappedKey(mark byte) []byte { return bytes.Repeat([]byte{mark}, 60) }

func TestSealedResourceLifecycle(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	id := uuid.NewString()

	for name, part := range map[string]SealedPart{
		"plain content": {Content: []byte("port: 7890\n"), Meta: sealedPart(1).Meta},
		"plain meta":    {Content: sealedPart(1).Content, Meta: []byte(`{"n":"x"}`)},
	} {
		if _, err := db.CreateSealedResource(ctx, user.ID, id, part, wrappedKey(1)); err == nil || !IsRefusal(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	resource, err := db.CreateSealedResource(ctx, user.ID, id, sealedPart(1), wrappedKey(1))
	if err != nil || !resource.Sealed() || resource.Name != "" || resource.ContentType != SealedContentType {
		t.Fatalf("created: %+v %v", resource, err)
	}
	if _, err := db.CreateSealedResource(ctx, user.ID, id, sealedPart(2), wrappedKey(2)); err == nil {
		t.Fatal("an id used twice")
	}
	stored, err := db.ResourceForOwner(ctx, user.ID, id)
	if err != nil || !bytes.Equal(stored.SealedKey, wrappedKey(1)) || !bytes.Equal(stored.SealedMeta, sealedPart(1).Meta) {
		t.Fatalf("read back: %+v %v", stored, err)
	}

	// A plain save, a plain link or a plain copy is not how this is changed.
	if _, err := db.SaveResource(ctx, user.ID, id, ResourceEdit{Name: "x", Content: []byte("x")}); !errors.Is(err, errSealedInBrowser) {
		t.Fatalf("a plain save: %v", err)
	}
	if _, err := db.CreateShare(ctx, user.ID, id, "", time.Hour, 0); !errors.Is(err, errSealedNeedsKey) {
		t.Fatalf("a plain link: %v", err)
	}

	// An edit is a new version; the version it replaces keeps its metadata.
	saved, err := db.SaveSealedResource(ctx, user.ID, id, 1, sealedPart(3))
	if err != nil || saved.Version != 2 || !saved.NewVersion {
		t.Fatalf("save: %+v %v", saved, err)
	}
	if _, err := db.SaveSealedResource(ctx, user.ID, id, 1, sealedPart(4)); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("a save against v1: %v", err)
	}
	renamed, err := db.SaveSealedResource(ctx, user.ID, id, 2, SealedPart{Meta: sealedPart(5).Meta})
	if err != nil || renamed.NewVersion || renamed.Version != 2 {
		t.Fatalf("a rename: %+v %v", renamed, err)
	}
	v1, err := db.VersionForOwner(ctx, user.ID, id, 1)
	if err != nil || !bytes.Equal(v1.SealedMeta, sealedPart(1).Meta) {
		t.Fatalf("v1: %+v %v", v1, err)
	}
	if _, err := db.CopyVersion(ctx, user.ID, id, 1, "copy"); !errors.Is(err, errSealedCopy) {
		t.Fatalf("a plain copy: %v", err)
	}
	// A restore brings the ciphertext and its metadata back as they were.
	restored, err := db.RestoreVersion(ctx, user.ID, id, 1)
	if err != nil || restored.Version != 3 {
		t.Fatalf("restore: %+v %v", restored, err)
	}
	now, _ := db.ResourceForOwner(ctx, user.ID, id)
	body, _ := db.ReadContent(ctx, now)
	if !bytes.Equal(body, sealedPart(1).Content) || !bytes.Equal(now.SealedMeta, sealedPart(1).Meta) || now.ContentType != SealedContentType || now.Filename != "" {
		t.Fatalf("restored: %+v", now)
	}

	// A link carries the keys it was made with, and its delivery hands the
	// link's wrapped key over.
	linkID := uuid.NewString()
	link, err := db.CreateSealedShare(ctx, user.ID, id, linkID, "ops", time.Hour, 0, wrappedKey(7), wrappedKey(8))
	if err != nil || link.ID != linkID {
		t.Fatalf("sealed link: %+v %v", link, err)
	}
	if _, err := db.CreateSealedShare(ctx, user.ID, id, linkID, "again", time.Hour, 0, wrappedKey(7), wrappedKey(8)); err == nil {
		t.Fatal("a link id used twice")
	}
	consumed, err := db.ConsumeToken(ctx, link.Token, RequestMeta{Method: "GET"}, time.Now().UTC())
	if err != nil || !consumed.Allowed || !bytes.Equal(consumed.LinkSealedKey, wrappedKey(7)) {
		t.Fatalf("delivery: %+v %v", consumed, err)
	}
	listed, err := db.ListShares(ctx, user.ID, id, time.Now().UTC())
	if err != nil || len(listed) != 1 || !bytes.Equal(listed[0].OwnerKey, wrappedKey(8)) {
		t.Fatalf("listed: %+v %v", listed, err)
	}

	index, err := db.SealedIndex(ctx, user.ID)
	if err != nil || len(index) != 1 || index[0].ID != id {
		t.Fatalf("index: %+v %v", index, err)
	}
	other, _ := db.UpsertUser(ctx, "200", "bob", "Bob", "")
	if theirs, _ := db.SealedIndex(ctx, other.ID); len(theirs) != 0 {
		t.Fatal("another account's index")
	}
	if _, err := db.SaveSealedResource(ctx, other.ID, id, 3, sealedPart(9)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another account's save: %v", err)
	}
}

// Sealing replaces the plaintext - current, history, name - in one go and
// revokes the links whose addresses hold no key; unsealing undoes it.
func TestSealAndUnseal(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	if _, err := db.SaveResource(ctx, user.ID, resource.ID, ResourceEdit{Name: "Example", Filename: "example.conf", Content: []byte("answer=43\n")}); err != nil {
		t.Fatal(err)
	}
	link, err := db.CreateShare(ctx, user.ID, resource.ID, "", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := db.ResourceForOwner(ctx, user.ID, resource.ID)
	v1, _ := db.VersionForOwner(ctx, user.ID, resource.ID, 1)

	// The versions sent must be the versions there are.
	if _, err := db.SealResource(ctx, user.ID, resource.ID, 2, sealedPart(1), map[int]SealedPart{}, wrappedKey(1)); !errors.Is(err, errSealedVersions) {
		t.Fatalf("history left out: %v", err)
	}
	if _, err := db.SealResource(ctx, user.ID, resource.ID, 1, sealedPart(1), map[int]SealedPart{1: sealedPart(2)}, wrappedKey(1)); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("against an old version: %v", err)
	}
	revoked, err := db.SealResource(ctx, user.ID, resource.ID, 2, sealedPart(1), map[int]SealedPart{1: sealedPart(2)}, wrappedKey(1))
	if err != nil || revoked != 1 {
		t.Fatalf("seal: %d %v", revoked, err)
	}
	sealed, _ := db.ResourceForOwner(ctx, user.ID, resource.ID)
	if !sealed.Sealed() || sealed.Name != "" || sealed.Filename != "" || sealed.Version != 2 {
		t.Fatalf("sealed: %+v", sealed)
	}
	sealedV1, _ := db.VersionForOwner(ctx, user.ID, resource.ID, 1)
	if sealedV1.Filename != "" || sealedV1.ContentType != SealedContentType || !bytes.Equal(sealedV1.SealedMeta, sealedPart(2).Meta) {
		t.Fatalf("sealed v1: %+v", sealedV1)
	}
	if body, _ := db.ReadVersion(ctx, sealedV1); !bytes.Equal(body, sealedPart(2).Content) {
		t.Fatal("v1 still plaintext")
	}
	// The plaintext objects are gone, not just unreferenced.
	for _, key := range []string{before.ContentKey, v1.ContentKey} {
		if _, err := db.ReadContent(ctx, Resource{ContentKey: key}); err == nil {
			t.Errorf("plaintext object %s is still stored", key)
		}
	}
	if consumed, _ := db.ConsumeToken(ctx, link.Token, RequestMeta{Method: "GET"}, time.Now().UTC()); consumed.Allowed {
		t.Fatal("a link without a key still delivers")
	}

	sealedLink, err := db.CreateSealedShare(ctx, user.ID, resource.ID, uuid.NewString(), "", time.Hour, 0, wrappedKey(7), wrappedKey(8))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UnsealResource(ctx, user.ID, resource.ID, 1, PlainPart{Content: []byte("x")}, map[int]PlainPart{1: {Content: []byte("y")}}); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("unseal against an old version: %v", err)
	}
	if err := db.UnsealResource(ctx, user.ID, resource.ID, 2, PlainPart{Content: []byte("answer=43\n"), Name: "Example", Filename: "example.conf"},
		map[int]PlainPart{1: {Content: []byte("answer=42\n"), Filename: "example.conf"}}); err != nil {
		t.Fatal(err)
	}
	plain, _ := db.ResourceForOwner(ctx, user.ID, resource.ID)
	if plain.Sealed() || plain.Name != "Example" || plain.Filename != "example.conf" || plain.ContentType == SealedContentType {
		t.Fatalf("unsealed: %+v", plain)
	}
	if body, _ := db.ReadContent(ctx, plain); string(body) != "answer=43\n" {
		t.Fatalf("unsealed body %q", body)
	}
	plainV1, _ := db.VersionForOwner(ctx, user.ID, resource.ID, 1)
	if plainV1.SealedMeta != nil || plainV1.Filename != "example.conf" {
		t.Fatalf("unsealed v1: %+v", plainV1)
	}
	// The link stays and delivers plaintext; its keys are gone.
	links, _ := db.ListShares(ctx, user.ID, resource.ID, time.Now().UTC())
	if len(links) != 1 || links[0].ID != sealedLink.ID || links[0].SealedKey != nil || links[0].OwnerKey != nil {
		t.Fatalf("links after unsealing: %+v", links)
	}
}

// An encrypted quick share is an encrypted resource that ends with its one
// link: made under the ids its keys are bound to, delivered with that link's
// key, kept as an encrypted resource, and refused in any other shape or
// under an id already taken - by anyone.
func TestSealedQuickShare(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC()
	other, err := db.UpsertUser(ctx, "200", "bob", "Bob", "")
	if err != nil {
		t.Fatal(err)
	}
	id, linkID := uuid.NewString(), uuid.NewString()
	before, _ := db.UsageForUser(ctx, user.ID)
	codeKey, ownerCode := bytes.Repeat([]byte{7}, 76), bytes.Repeat([]byte{8}, 32)

	for name, attempt := range map[string]func() error{
		"plain content": func() error {
			_, _, err := db.CreateSealedQuickShare(ctx, user.ID, id, linkID, SealedPart{Content: []byte("x=1\n"), Meta: sealedPart(1).Meta}, wrappedKey(1), wrappedKey(2), wrappedKey(3), time.Hour, now)
			return err
		},
		"a short resource key": func() error {
			_, _, err := db.CreateSealedQuickShare(ctx, user.ID, id, linkID, sealedPart(1), wrappedKey(1)[:59], wrappedKey(2), wrappedKey(3), time.Hour, now)
			return err
		},
		"a code key, link sized": func() error {
			_, _, err := db.CreateSealedQuickShare(ctx, user.ID, id, linkID, sealedPart(1), wrappedKey(1), codeKey, wrappedKey(3), time.Hour, now)
			return err
		},
		"a malformed link id": func() error {
			_, _, err := db.CreateSealedQuickShare(ctx, user.ID, id, "link", sealedPart(1), wrappedKey(1), wrappedKey(2), wrappedKey(3), time.Hour, now)
			return err
		},
		"too long a term": func() error {
			_, _, err := db.CreateSealedQuickShare(ctx, user.ID, id, linkID, sealedPart(1), wrappedKey(1), wrappedKey(2), wrappedKey(3), 90*24*time.Hour, now)
			return err
		},
	} {
		if err := attempt(); err == nil || !IsRefusal(err) {
			t.Errorf("%s: %v", name, err)
		}
	}

	resource, link, err := db.CreateSealedQuickShare(ctx, user.ID, id, linkID, sealedPart(1), wrappedKey(1), wrappedKey(2), wrappedKey(3), time.Hour, now)
	if err != nil || !resource.Sealed() || !resource.QuickShare() || resource.Name != "" || link.ID != linkID || link.CodeLink() || link.Token == "" {
		t.Fatalf("created: %+v %+v %v", resource, link, err)
	}
	if usage, _ := db.UsageForUser(ctx, user.ID); usage.StorageBytes-before.StorageBytes != int64(len(sealedPart(1).Content)) {
		t.Fatalf("it counts toward the quota: %+v", usage)
	}
	// The same ids again, from its owner or anyone else: refused, and what
	// is there stays as it was.
	for _, owner := range []string{user.ID, other.ID} {
		if _, _, err := db.CreateSealedQuickShare(ctx, owner, id, uuid.NewString(), sealedPart(9), wrappedKey(9), wrappedKey(9), wrappedKey(9), time.Hour, now); err == nil {
			t.Fatal("the same resource id again")
		}
		if _, _, err := db.CreateSealedQuickShare(ctx, owner, uuid.NewString(), linkID, sealedPart(9), wrappedKey(9), wrappedKey(9), wrappedKey(9), time.Hour, now); err == nil {
			t.Fatal("the same link id again")
		}
	}
	if _, theirs, _ := db.ListResources(ctx, other.ID, "", 50, 0); theirs != 0 {
		t.Fatalf("another account was left with %d resources", theirs)
	}
	stored, _ := db.ResourceForOwner(ctx, user.ID, id)
	if body, _ := db.ReadContent(ctx, stored); !bytes.Equal(body, sealedPart(1).Content) || !bytes.Equal(stored.SealedKey, wrappedKey(1)) {
		t.Fatal("what was there changed")
	}

	// Delivered with the link's own key; the quick share's link is the one
	// its page shows.
	result, err := db.ConsumeToken(ctx, link.Token, RequestMeta{Method: "GET", RemoteIP: "198.51.100.7"}, now)
	if err != nil || !bytes.Equal(result.LinkSealedKey, wrappedKey(2)) {
		t.Fatalf("consumed: %v", err)
	}
	if _, shown, err := db.QuickShareLink(ctx, user.ID, id, now); err != nil || shown.ID != linkID || !bytes.Equal(shown.OwnerKey, wrappedKey(3)) {
		t.Fatalf("its link: %+v %v", shown, err)
	}
	if err := db.KeepQuickShare(ctx, user.ID, id, now); err != nil {
		t.Fatal(err)
	}
	if kept, _ := db.ResourceForOwner(ctx, user.ID, id); kept.QuickShare() || !kept.Sealed() {
		t.Fatalf("kept: %+v", kept)
	}

	// One opened by a code.
	coded, codeLink, err := db.CreateSealedQuickShare(ctx, user.ID, uuid.NewString(), uuid.NewString(), sealedPart(2), wrappedKey(1), codeKey, ownerCode, time.Hour, now)
	if err != nil || !codeLink.CodeLink() {
		t.Fatalf("a code link: %+v %v", codeLink, err)
	}
	// And a link opened by a code on an encrypted resource, beside one with
	// its key in the address.
	if l, err := db.CreateSealedShare(ctx, user.ID, coded.ID, uuid.NewString(), "", time.Hour, 0, codeKey, ownerCode); err != nil || !l.CodeLink() {
		t.Fatalf("a code link on a resource: %v", err)
	}
	if _, err := db.CreateSealedShare(ctx, user.ID, coded.ID, uuid.NewString(), "", time.Hour, 0, codeKey, wrappedKey(3)); err == nil {
		t.Fatal("a code key with a link key's owner copy")
	}

	// A link's keys made again: a key to a code and back, by its owner only,
	// in a shape that matches, and while it is live.
	keyed, err := db.CreateSealedShare(ctx, user.ID, coded.ID, uuid.NewString(), "", time.Hour, 0, wrappedKey(2), wrappedKey(3))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RekeySealedShare(ctx, other.ID, coded.ID, keyed.ID, codeKey, ownerCode); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another account's rekey: %v", err)
	}
	if err := db.RekeySealedShare(ctx, user.ID, coded.ID, keyed.ID, codeKey, wrappedKey(3)); err == nil || !IsRefusal(err) {
		t.Fatalf("mismatched keys: %v", err)
	}
	if err := db.RekeySealedShare(ctx, user.ID, id, keyed.ID, codeKey, ownerCode); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a link under another resource: %v", err)
	}
	if err := db.RekeySealedShare(ctx, user.ID, coded.ID, keyed.ID, codeKey, ownerCode); err != nil {
		t.Fatal(err)
	}
	links, _ := db.ListShares(ctx, user.ID, coded.ID, now)
	for _, l := range links {
		if l.ID == keyed.ID && (!l.CodeLink() || !bytes.Equal(l.OwnerKey, ownerCode)) {
			t.Fatalf("rekeyed: %+v", l)
		}
	}
	if err := db.RevokeLink(ctx, user.ID, coded.ID, keyed.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.RekeySealedShare(ctx, user.ID, coded.ID, keyed.ID, wrappedKey(2), wrappedKey(3)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a revoked link: %v", err)
	}
}

// One encrypted the earlier way, with its key only in its link, still
// cannot be kept: nothing opens it.
func TestLegacyEncryptedQuickShareIsNotKept(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	envelope := append([]byte("PMe1\x00"), bytes.Repeat([]byte{1}, 40)...)
	resource, _, err := db.CreateEncryptedPaste(ctx, user.ID, envelope, time.Hour, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.KeepQuickShare(ctx, user.ID, resource.ID, time.Now().UTC()); err == nil || !IsRefusal(err) {
		t.Fatalf("kept: %v", err)
	}
}
