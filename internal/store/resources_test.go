package store

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// TestOneWayToReadAResource keeps resource rows read in one place: a query
// that scans its own subset of columns is how a Resource ends up missing a
// field somewhere - whether it is taken down, say - and reading as if it
// were not.
func TestOneWayToReadAResource(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	scan := regexp.MustCompile(`&\w+\.OwnerID,\s*&\w+\.Name`)
	found := 0
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range scan.FindAllIndex(source, -1) {
			found++
			if name != "resources.go" || !strings.Contains(string(source[max(0, match[0]-400):match[0]]), "func scanResource") {
				t.Errorf("%s scans a resource row itself; use resourceColumns and scanResource", name)
			}
		}
	}
	if found != 1 {
		t.Fatalf("found %d resource scanners, want scanResource alone", found)
	}
}

// TestEveryReaderSeesTheWholeResource reads one taken-down resource with
// several versions through every reader and checks each sees all of it.
func TestEveryReaderSeesTheWholeResource(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	if err := db.UpdateResource(ctx, user.ID, resource.ID, "Example", "example.conf", []byte("answer=43\n"), "utf-8", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AdminTakedown(ctx, AdminActor{KeyID: "test"}, resource.ID, "test", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	check := func(label string, r Resource) {
		t.Helper()
		if r.ID != resource.ID || !r.TakenDown || r.TakedownReason == "" || r.Version != 2 || r.ContentSHA256 == "" ||
			r.ContentKey == "" || r.ContentSize == 0 || r.Filename != "example.conf" || r.CreatedAt.IsZero() {
			t.Errorf("%s: %+v", label, r)
		}
	}
	one, err := db.ResourceForOwner(ctx, user.ID, resource.ID)
	if err != nil {
		t.Fatal(err)
	}
	check("ResourceForOwner", one)
	listed, _, err := db.ListResources(ctx, user.ID, "", 10, 0)
	if err != nil || len(listed) != 1 {
		t.Fatalf("ListResources: %v %v", listed, err)
	}
	check("ListResources", listed[0])
	api, err := db.APIResources(ctx, user.ID, "")
	if err != nil || len(api) != 1 {
		t.Fatalf("APIResources: %v %v", api, err)
	}
	check("APIResources", api[0])
	resolved, err := db.ResolveResources(ctx, user.ID, resource.ID)
	if err != nil || len(resolved) != 1 {
		t.Fatalf("ResolveResources: %v %v", resolved, err)
	}
	check("ResolveResources", resolved[0])
	exported, err := db.ExportResources(ctx, user.ID)
	if err != nil || len(exported) != 1 {
		t.Fatalf("ExportResources: %v %v", exported, err)
	}
	check("ExportResources", exported[0])
}
