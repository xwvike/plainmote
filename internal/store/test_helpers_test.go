package store

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"plainmote/internal/blob"
)

func testDatabase(t *testing.T) (*Store, User, Resource) {
	t.Helper()
	ctx := context.Background()
	db, err := Open(ctx, testDatabaseURL(t), bytes.Repeat([]byte{7}, 32), newMemoryBlobs(), false)
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

func testDatabaseURL(t *testing.T) string {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv("PLAINMOTE_TEST_DATABASE_URL"))
	if raw == "" {
		t.Skip("PLAINMOTE_TEST_DATABASE_URL is not set")
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" {
		t.Fatalf("invalid PLAINMOTE_TEST_DATABASE_URL: %v", err)
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := admin.Ping(ctx); err != nil {
		admin.Close()
		t.Fatalf("connect test PostgreSQL: %v", err)
	}
	schemaName := "test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{schemaName}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE")
		admin.Close()
	})
	query := parsed.Query()
	query.Set("search_path", schemaName)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

type memoryBlobs struct {
	mu      sync.RWMutex
	objects map[string][]byte
}

func newMemoryBlobs() *memoryBlobs {
	return &memoryBlobs{objects: make(map[string][]byte)}
}

func (m *memoryBlobs) Put(_ context.Context, key string, reader io.Reader, size int64) error {
	data, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	if int64(len(data)) != size {
		return fmt.Errorf("blob is %d bytes, expected %d", len(data), size)
	}
	m.mu.Lock()
	m.objects[key] = bytes.Clone(data)
	m.mu.Unlock()
	return nil
}

func (m *memoryBlobs) Open(_ context.Context, key string) (io.ReadCloser, int64, error) {
	m.mu.RLock()
	data, ok := m.objects[key]
	m.mu.RUnlock()
	if !ok {
		return nil, 0, blob.ErrNotFound
	}
	data = bytes.Clone(data)
	return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
}

func (m *memoryBlobs) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	delete(m.objects, key)
	m.mu.Unlock()
	return nil
}
