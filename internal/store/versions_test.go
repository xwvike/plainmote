package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func save(t *testing.T, db *Store, user User, resource Resource, body string) SaveResult {
	t.Helper()
	result, err := db.SaveResource(context.Background(), user.ID, resource.ID, ResourceEdit{
		Name: resource.Name, Filename: resource.Filename, Content: []byte(body),
	})
	if err != nil {
		t.Fatalf("save %q: %v", body, err)
	}
	return result
}

func readCurrent(t *testing.T, db *Store, user User, id string) (Resource, string) {
	t.Helper()
	resource, err := db.ResourceForOwner(context.Background(), user.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	body, err := db.ReadContent(context.Background(), resource)
	if err != nil {
		t.Fatal(err)
	}
	return resource, string(body)
}

// TestSaveFilesTheReplacedContent is the whole feature in one: a save that
// changes the content keeps what it replaced, readable, under the old number.
func TestSaveFilesTheReplacedContent(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()

	if result := save(t, db, user, resource, "answer=43\n"); result.Version != 2 {
		t.Fatalf("the first change makes version 2, got %d", result.Version)
	}
	current, body := readCurrent(t, db, user, resource.ID)
	if current.Version != 2 || body != "answer=43\n" {
		t.Fatalf("current is v%d %q", current.Version, body)
	}
	versions, err := db.ListVersions(ctx, user.ID, resource.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 || versions[0].Number != 1 {
		t.Fatalf("history should hold v1 alone, got %+v", versions)
	}
	old, err := db.ReadVersion(ctx, versions[0])
	if err != nil || string(old) != "answer=42\n" {
		t.Fatalf("v1 should still read as the original, got %q, %v", old, err)
	}
	if !versions[0].SavedAt.Equal(resource.VersionAt) {
		t.Fatalf("v1 keeps when it was saved: %v, want %v", versions[0].SavedAt, resource.VersionAt)
	}
}

// TestSameContentIsNotAVersion covers the editor, which posts its text with
// every save: a rename, or saving twice, must not file copies.
func TestSameContentIsNotAVersion(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()

	if result := save(t, db, user, resource, "answer=42\n"); result.Version != 1 {
		t.Fatalf("the same bytes are the same version, got v%d", result.Version)
	}
	if _, err := db.SaveResource(ctx, user.ID, resource.ID, ResourceEdit{
		Name: "Renamed", Filename: "renamed.conf", Content: []byte("answer=42\n"),
	}); err != nil {
		t.Fatal(err)
	}
	current, _ := readCurrent(t, db, user, resource.ID)
	if current.Version != 1 || current.Name != "Renamed" {
		t.Fatalf("a rename keeps the version: v%d %q", current.Version, current.Name)
	}
	if count, _ := db.HistoryCount(ctx, user.ID, resource.ID); count != 0 {
		t.Fatalf("nothing was replaced, yet %d versions were filed", count)
	}
}

// TestUnhashedRowsAreComparedOnce: rows from before the hash was kept have
// none. The first save reads the object to compare, and stores the hash.
func TestUnhashedRowsAreComparedOnce(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	if _, err := db.db.Exec(ctx, `UPDATE resources SET content_sha256 = '' WHERE id = $1`, resource.ID); err != nil {
		t.Fatal(err)
	}
	if result := save(t, db, user, resource, "answer=42\n"); result.Version != 1 {
		t.Fatalf("unchanged content of an old row is still the same version, got v%d", result.Version)
	}
	current, _ := readCurrent(t, db, user, resource.ID)
	if current.ContentSHA256 != contentSHA256([]byte("answer=42\n")) {
		t.Fatalf("the save should have stored the hash, got %q", current.ContentSHA256)
	}
}

func TestHistoryKeepsTheLatestTen(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	blobs := db.blobs.(*memoryBlobs)

	for i := range HistoryKeep + 3 {
		save(t, db, user, resource, fmt.Sprintf("answer=%d\n", 100+i))
	}
	versions, err := db.ListVersions(ctx, user.ID, resource.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != HistoryKeep {
		t.Fatalf("history holds %d, want %d", len(versions), HistoryKeep)
	}
	// v14 is current; v4..v13 are kept, newest first.
	if versions[0].Number != 13 || versions[len(versions)-1].Number != 4 {
		t.Fatalf("kept the wrong ones: v%d..v%d", versions[0].Number, versions[len(versions)-1].Number)
	}
	if got := blobs.count(); got != HistoryKeep+1 {
		t.Fatalf("dropped versions must take their objects: %d objects for %d versions and the current one", got, HistoryKeep)
	}
}

// TestHistoryGivesWayToContent is the storage rule: history lives in the room
// current content leaves, and a save that needs it takes it back - oldest
// first - rather than being refused.
func TestHistoryGivesWayToContent(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	// The first resource is 10 bytes; the limit leaves room for a little history.
	setPlanLimits(t, db, 10, 100)

	for i := range 4 {
		save(t, db, user, resource, fmt.Sprintf("answer=%02d\n", i)) // 10 bytes each
	}
	usage, err := db.UsageForUser(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if usage.StorageBytes != 10 || usage.HistoryBytes != 40 {
		t.Fatalf("usage should split current and history: %+v", usage)
	}

	// 70 bytes of new content leaves 20 for history: the two newest survive.
	if _, err := db.CreateResource(ctx, user.ID, "big", "big.txt", bytes.Repeat([]byte("x"), 70), "", ""); err != nil {
		t.Fatalf("history must never be why a save is refused: %v", err)
	}
	versions, err := db.ListVersions(ctx, user.ID, resource.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 2 || versions[0].Number != 4 || versions[1].Number != 3 {
		t.Fatalf("the oldest should have gone first, kept %+v", versions)
	}

	// A save that grows content trims too, and says how many it took.
	result := save(t, db, user, resource, "answer=1234567890\n") // 18 bytes, retires 10
	if result.Trimmed == 0 {
		t.Fatalf("growing content should have taken history's room, result %+v", result)
	}
	usage, _ = db.UsageForUser(ctx, user.ID)
	if usage.StorageBytes+usage.HistoryBytes > 100 {
		t.Fatalf("history overflowed the limit: %+v", usage)
	}

	// Content itself is still held to the limit.
	_, err = db.CreateResource(ctx, user.ID, "too big", "too.txt", bytes.Repeat([]byte("x"), 20), "", "")
	if !errors.Is(err, ErrQuotaExceeded) {
		t.Fatalf("content past the limit is refused as before, got %v", err)
	}
}

func TestStaleSaveIsAConflict(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	save(t, db, user, resource, "answer=43\n") // v2, made elsewhere

	_, err := db.SaveResource(ctx, user.ID, resource.ID, ResourceEdit{
		Name: resource.Name, Filename: resource.Filename, Content: []byte("answer=44\n"), BaseVersion: 1,
	})
	var conflict *VersionConflict
	if !errors.As(err, &conflict) || !errors.Is(err, ErrVersionConflict) || conflict.Current != 2 {
		t.Fatalf("a save against v1 over v2 must conflict, got %v", err)
	}
	if _, body := readCurrent(t, db, user, resource.ID); body != "answer=43\n" {
		t.Fatalf("a conflict writes nothing, current is %q", body)
	}

	// Posting back what is current loses nothing, however stale the base.
	if _, err := db.SaveResource(ctx, user.ID, resource.ID, ResourceEdit{
		Name: "Renamed", Filename: resource.Filename, Content: []byte("answer=43\n"), BaseVersion: 1,
	}); err != nil {
		t.Fatalf("the current content with a stale base is not a conflict: %v", err)
	}
	// And the base the conflict reported is what a second attempt carries.
	result, err := db.SaveResource(ctx, user.ID, resource.ID, ResourceEdit{
		Name: resource.Name, Filename: resource.Filename, Content: []byte("answer=44\n"), BaseVersion: conflict.Current,
	})
	if err != nil || result.Version != 3 {
		t.Fatalf("saving over the reported version should succeed as v3, got %+v, %v", result, err)
	}
}

func TestRestoreIsANewVersion(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	save(t, db, user, resource, "answer=43\n") // v2
	save(t, db, user, resource, "answer=44\n") // v3

	result, err := db.RestoreVersion(ctx, user.ID, resource.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if result.Version != 4 {
		t.Fatalf("restoring makes v4, got v%d", result.Version)
	}
	current, body := readCurrent(t, db, user, resource.ID)
	if body != "answer=42\n" || current.RestoredFrom != 1 {
		t.Fatalf("v4 should hold v1's content and say so: %q from v%d", body, current.RestoredFrom)
	}
	versions, _ := db.ListVersions(ctx, user.ID, resource.ID)
	if len(versions) != 3 || versions[0].Number != 3 || versions[2].Number != 1 {
		t.Fatalf("v1 stays and v3 joins the history: %+v", versions)
	}

	// Undo is restoring again; the restore marker goes with a plain save.
	if _, err := db.RestoreVersion(ctx, user.ID, resource.ID, 3); err != nil {
		t.Fatal(err)
	}
	save(t, db, user, resource, "answer=45\n")
	current, _ = readCurrent(t, db, user, resource.ID)
	if current.Version != 6 || current.RestoredFrom != 0 {
		t.Fatalf("a plain save is v6 and restored from nothing: v%d from v%d", current.Version, current.RestoredFrom)
	}
	versions, _ = db.ListVersions(ctx, user.ID, resource.ID)
	if versions[0].Number != 5 || versions[0].RestoredFrom != 3 {
		t.Fatalf("v5 keeps its marker in the history: %+v", versions[0])
	}

	if _, err := db.RestoreVersion(ctx, user.ID, resource.ID, 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an unknown version cannot be restored, got %v", err)
	}
}

func TestCopyVersionStartsAResource(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	save(t, db, user, resource, "answer=43\n")

	copied, err := db.CopyVersion(ctx, user.ID, resource.ID, 1, "Example (v1)")
	if err != nil {
		t.Fatal(err)
	}
	current, body := readCurrent(t, db, user, copied.ID)
	if body != "answer=42\n" || current.Version != 1 || current.Filename != resource.Filename || current.Name != "Example (v1)" {
		t.Fatalf("the copy is a new resource at v1 with the old content: %+v %q", current, body)
	}
}

func TestDeletingVersionsTakesTheirObjects(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	blobs := db.blobs.(*memoryBlobs)
	save(t, db, user, resource, "answer=43\n")
	save(t, db, user, resource, "answer=44\n")
	if got := blobs.count(); got != 3 {
		t.Fatalf("three versions, three objects; got %d", got)
	}

	if err := db.DeleteVersion(ctx, user.ID, resource.ID, 1); err != nil {
		t.Fatal(err)
	}
	if got := blobs.count(); got != 2 {
		t.Fatalf("deleting a version deletes its object; %d left", got)
	}
	if err := db.DeleteVersion(ctx, user.ID, resource.ID, 3); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the current content is not a version to delete, got %v", err)
	}
	other, err := db.UpsertUser(ctx, "200", "bob", "Bob", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteVersion(ctx, other.ID, resource.ID, 2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("someone else's version is not found, got %v", err)
	}
	if _, err := db.VersionForOwner(ctx, other.ID, resource.ID, 2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("someone else's version cannot be read, got %v", err)
	}

	if err := db.DeleteResource(ctx, user.ID, resource.ID); err != nil {
		t.Fatal(err)
	}
	if got := blobs.count(); got != 0 {
		t.Fatalf("deleting the resource deletes every version's object; %d left", got)
	}
}

func TestDeletingAccountTakesHistory(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	blobs := db.blobs.(*memoryBlobs)
	save(t, db, user, resource, "answer=43\n")
	if err := db.DeleteAccount(ctx, user.ID); err != nil {
		t.Fatal(err)
	}
	if got := blobs.count(); got != 0 {
		t.Fatalf("an account's history goes with it; %d objects left", got)
	}
}

func TestSwitchingToRemoteDropsHistory(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	blobs := db.blobs.(*memoryBlobs)
	save(t, db, user, resource, "answer=43\n")

	if _, err := db.SaveResource(ctx, user.ID, resource.ID, ResourceEdit{
		Name: resource.Name, Filename: resource.Filename, OriginURL: "https://example.com/app.conf",
	}); err != nil {
		t.Fatal(err)
	}
	if count, _ := db.HistoryCount(ctx, user.ID, resource.ID); count != 0 {
		t.Fatalf("a reference keeps no versions, found %d", count)
	}
	if got := blobs.count(); got != 0 {
		t.Fatalf("nothing stored is left for a reference; %d objects", got)
	}
}

func TestOldVersionsArePruned(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	blobs := db.blobs.(*memoryBlobs)
	save(t, db, user, resource, "answer=43\n")
	save(t, db, user, resource, "answer=44\n")
	if _, err := db.db.Exec(ctx, `UPDATE resource_versions SET replaced_at = $1 WHERE version = 1`,
		time.Now().Add(-HistoryRetention-time.Hour)); err != nil {
		t.Fatal(err)
	}
	result, err := db.Prune(ctx, 0, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if result.Versions != 1 {
		t.Fatalf("only the version replaced over the retention ago goes, removed %d", result.Versions)
	}
	versions, _ := db.ListVersions(ctx, user.ID, resource.ID)
	if len(versions) != 1 || versions[0].Number != 2 {
		t.Fatalf("v2 stays: %+v", versions)
	}
	if got := blobs.count(); got != 2 {
		t.Fatalf("the pruned version's object goes too; %d left", got)
	}
}

// TestDeliveriesRecordTheirVersion follows a version from delivery to the
// access history and the per-link summary.
func TestDeliveriesRecordTheirVersion(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	link, err := db.CreateShare(ctx, user.ID, resource.ID, "web-01", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	deliver := func() {
		t.Helper()
		result, err := db.ConsumeToken(ctx, link.Token, RequestMeta{RemoteIP: "10.0.0.1", Method: "GET", Path: "/d/x"}, time.Now().UTC())
		if err != nil || !result.Allowed {
			t.Fatalf("delivery refused: %+v %v", result, err)
		}
	}
	deliver()
	save(t, db, user, resource, "answer=43\n")

	logs, err := db.ListAccess(ctx, user.ID, resource.ID, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].Version != 1 || logs[0].CurrentVersion != 2 || !logs[0].VersionAvailable {
		t.Fatalf("the delivery carried v1, now history: %+v", logs)
	}
	last, err := db.LastDelivered(ctx, user.ID, resource.ID, time.Now().UTC())
	if err != nil || len(last) != 1 || last[0].Version != 1 || last[0].LinkName != "web-01" {
		t.Fatalf("web-01 last had v1: %+v %v", last, err)
	}

	deliver()
	last, _ = db.LastDelivered(ctx, user.ID, resource.ID, time.Now().UTC())
	if len(last) != 1 || last[0].Version != 2 {
		t.Fatalf("web-01 now has v2: %+v", last)
	}
	if err := db.DeleteVersion(ctx, user.ID, resource.ID, 1); err != nil {
		t.Fatal(err)
	}
	logs, _ = db.ListAccess(ctx, user.ID, resource.ID, "", 10)
	if logs[1].Version != 1 || logs[1].VersionAvailable {
		t.Fatalf("a deleted version is still named but no longer opens: %+v", logs[1])
	}
}
