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
