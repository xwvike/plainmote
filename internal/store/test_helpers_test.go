package store

import (
	"bytes"
	"context"
	"testing"

	"plainmote/internal/testsupport"
)

func testDatabase(t *testing.T) (*Store, User, Resource) {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, testDatabaseURL(t), bytes.Repeat([]byte{7}, 32), newMemoryBlobs())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	user, err := db.UpsertUser(ctx, "100", "alice", "Alice", "")
	if err != nil {
		t.Fatal(err)
	}
	resource, err := db.CreateResource(ctx, user.ID, "Example", "example.conf", []byte("answer=42\n"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	return db, user, resource
}

func testDatabaseURL(t *testing.T) string { return testsupport.DatabaseURL(t) }

type memoryBlobs = testsupport.MemoryBlobs

func newMemoryBlobs() *memoryBlobs { return testsupport.NewMemoryBlobs() }
