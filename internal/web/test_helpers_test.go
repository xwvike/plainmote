package web

import (
	"bytes"
	"context"
	"testing"

	"plainmote/internal/auth"
	"plainmote/internal/store"
	"plainmote/internal/testsupport"
	"plainmote/internal/upstream"
)

type upstreamFunc func(context.Context, string) ([]byte, string, error)

func (f upstreamFunc) Fetch(ctx context.Context, rawURL string) ([]byte, string, error) {
	return f(ctx, rawURL)
}

func testDatabase(t *testing.T) (*store.Store, User, Resource) {
	t.Helper()
	ctx := context.Background()
	db, err := store.Open(ctx, testDatabaseURL(t), bytes.Repeat([]byte{7}, 32), newMemoryBlobs())
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

func newTestApp(db *store.Store) *App {
	app := &App{db: db, upstream: upstream.New(4 << 20), cfg: Config{
		MaxContent: 4 << 20, PublicURL: "https://cfg.test",
		RegistrationMode: auth.RegistrationClosed, AnonymousEnabled: true,
	}}
	app.templates = app.templateSet()
	app.handler = app.routes()
	return app
}

type memoryBlobs = testsupport.MemoryBlobs

func newMemoryBlobs() *memoryBlobs { return testsupport.NewMemoryBlobs() }
