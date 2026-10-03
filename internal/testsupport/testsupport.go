// Package testsupport is what the tests of several packages share: a
// PostgreSQL schema of their own and object storage kept in memory. Only
// tests import it, so none of it reaches a build of the service.
package testsupport

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

// DatabaseURL is PLAINMOTE_TEST_DATABASE_URL pointed at a fresh schema that
// is dropped when the test ends, so tests never see each other's rows. A
// test is skipped when the variable is not set.
func DatabaseURL(t testing.TB) string {
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
	schema := "test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+identifier); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP SCHEMA "+identifier+" CASCADE")
		admin.Close()
	})
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

// MemoryBlobs is a blob.Store held in memory.
type MemoryBlobs struct {
	mu      sync.RWMutex
	objects map[string][]byte
}

var _ blob.Store = (*MemoryBlobs)(nil)

func NewMemoryBlobs() *MemoryBlobs {
	return &MemoryBlobs{objects: make(map[string][]byte)}
}

// Count is how many objects are stored.
func (m *MemoryBlobs) Count() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.objects)
}

func (m *MemoryBlobs) Put(_ context.Context, key string, reader io.Reader, size int64) error {
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

func (m *MemoryBlobs) Open(_ context.Context, key string) (io.ReadCloser, int64, error) {
	m.mu.RLock()
	data, ok := m.objects[key]
	m.mu.RUnlock()
	if !ok {
		return nil, 0, blob.ErrNotFound
	}
	data = bytes.Clone(data)
	return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
}

func (m *MemoryBlobs) OpenRange(_ context.Context, key string, start, end int64) (io.ReadCloser, int64, error) {
	m.mu.RLock()
	data, ok := m.objects[key]
	m.mu.RUnlock()
	if !ok {
		return nil, 0, blob.ErrNotFound
	}
	part := bytes.Clone(data[start : end+1])
	return io.NopCloser(bytes.NewReader(part)), int64(len(part)), nil
}

func (m *MemoryBlobs) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	delete(m.objects, key)
	m.mu.Unlock()
	return nil
}
