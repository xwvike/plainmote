package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestFoldersLifecycleNestingAndTrash(t *testing.T) {
	db, user, rootRes := testDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC()

	other, err := db.UpsertUser(ctx, "202", "bob", "Bob", "")
	if err != nil {
		t.Fatal(err)
	}

	// Create multi-level nested folders.
	infra, err := db.CreateFolder(ctx, user.ID, "", "Infra")
	if err != nil {
		t.Fatalf("create root folder: %v", err)
	}
	// Duplicate sibling (case-insensitive) is refused.
	if _, err := db.CreateFolder(ctx, user.ID, "", "infra"); err == nil || !IsRefusal(err) {
		t.Fatalf("duplicate folder name: %v", err)
	}
	// Invalid names refused.
	for _, bad := range []string{"", "   ", ".", "..", "a/b", "a\\b"} {
		if _, err := db.CreateFolder(ctx, user.ID, "", bad); err == nil || !IsRefusal(err) {
			t.Fatalf("bad folder name %q: %v", bad, err)
		}
	}

	nginx, err := db.CreateFolder(ctx, user.ID, infra.ID, "nginx")
	if err != nil {
		t.Fatalf("create nested folder: %v", err)
	}
	prod, err := db.CreateFolder(ctx, user.ID, nginx.ID, "prod")
	if err != nil {
		t.Fatalf("create third-level folder: %v", err)
	}

	// Breadcrumbs order from top-level ancestor down to current folder.
	crumbs, err := db.FolderBreadcrumbs(ctx, user.ID, prod.ID)
	if err != nil || len(crumbs) != 3 || crumbs[0].ID != infra.ID || crumbs[1].ID != nginx.ID || crumbs[2].ID != prod.ID {
		t.Fatalf("breadcrumbs: %+v %v", crumbs, err)
	}
	if _, err := db.FolderBreadcrumbs(ctx, other.ID, prod.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("other user breadcrumbs: %v", err)
	}

	// Move a resource into prod and verify folder-scoped vs global listing.
	nestedRes, err := db.CreateResource(ctx, user.ID, "Site", "site.conf", []byte("server {}\n"), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.MoveResource(ctx, user.ID, nestedRes.ID, prod.ID); err != nil {
		t.Fatalf("move resource into prod: %v", err)
	}
	if err := db.MoveResource(ctx, other.ID, nestedRes.ID, prod.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-account move resource: %v", err)
	}

	rootList, rootTotal, err := db.ListResourcesBy(ctx, user.ID, ResourceFilter{UseFolder: true, FolderID: ""}, 20, 0)
	if err != nil || rootTotal != 1 || len(rootList) != 1 || rootList[0].ID != rootRes.ID {
		t.Fatalf("root listing: total=%d items=%+v err=%v", rootTotal, rootList, err)
	}
	prodList, prodTotal, err := db.ListResourcesBy(ctx, user.ID, ResourceFilter{UseFolder: true, FolderID: prod.ID}, 20, 0)
	if err != nil || prodTotal != 1 || len(prodList) != 1 || prodList[0].ID != nestedRes.ID {
		t.Fatalf("prod listing: total=%d items=%+v err=%v", prodTotal, prodList, err)
	}
	allList, allTotal, err := db.ListResourcesBy(ctx, user.ID, ResourceFilter{}, 20, 0)
	if err != nil || allTotal != 2 || len(allList) != 2 {
		t.Fatalf("global listing: total=%d len=%d err=%v", allTotal, len(allList), err)
	}

	// Cycle protection on MoveFolder.
	if err := db.MoveFolder(ctx, user.ID, infra.ID, prod.ID); err == nil || !IsRefusal(err) {
		t.Fatalf("move folder into descendant: %v", err)
	}
	if err := db.MoveFolder(ctx, user.ID, infra.ID, infra.ID); err == nil || !IsRefusal(err) {
		t.Fatalf("move folder into self: %v", err)
	}

	// Rename folder.
	renamed, err := db.RenameFolder(ctx, user.ID, nginx.ID, "caddy")
	if err != nil || renamed.Name != "caddy" {
		t.Fatalf("rename folder: %+v %v", renamed, err)
	}

	// Share the nested resource, then delete the top-level folder (OS Finder behavior):
	// nested folders are deleted, nested resource is trashed, its share link is revoked.
	share, err := db.CreateShare(ctx, user.ID, nestedRes.ID, "live", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	trashed, revoked, err := db.TrashFolder(ctx, user.ID, infra.ID, now)
	if err != nil || trashed != 1 || revoked != 1 {
		t.Fatalf("trash folder: trashed=%d revoked=%d err=%v", trashed, revoked, err)
	}
	if _, err := db.FolderForOwner(ctx, user.ID, prod.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("descendant folder survived TrashFolder: %v", err)
	}
	if res, err := db.ConsumeToken(ctx, share.Token, RequestMeta{}, now); err != nil || res.Allowed {
		t.Fatalf("share link survived TrashFolder: %+v %v", res, err)
	}
	// Restoring the trashed resource returns it cleanly to root (folder_id == "").
	restored, err := db.RestoreResource(ctx, user.ID, nestedRes.ID, now)
	if err != nil || restored.FolderID != "" {
		t.Fatalf("restored resource: folder=%q err=%v", restored.FolderID, err)
	}
}
