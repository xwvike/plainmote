package blob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// RunStoreContract is the behaviour the S3 implementation owes its callers.
func RunStoreContract(t *testing.T, store Store) {
	t.Helper()
	ctx := context.Background()

	t.Run("round trip preserves bytes", func(t *testing.T) {
		payload := []byte{0x89, 'P', 'N', 'G', 0x00, 0x00, 0xff, 0xfe, '\n'}
		key := "contract/round-trip"
		if err := store.Put(ctx, key, bytes.NewReader(payload), int64(len(payload))); err != nil {
			t.Fatal(err)
		}
		reader, size, err := store.Open(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		if size != int64(len(payload)) {
			t.Fatalf("size %d, want %d", size, len(payload))
		}
		got, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("bytes changed: % x", got)
		}
	})

	t.Run("empty object is allowed", func(t *testing.T) {
		key := "contract/empty"
		if err := store.Put(ctx, key, strings.NewReader(""), 0); err != nil {
			t.Fatal(err)
		}
		reader, size, err := store.Open(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		if size != 0 {
			t.Fatalf("size %d, want 0", size)
		}
	})

	t.Run("missing key reports ErrNotFound", func(t *testing.T) {
		if _, _, err := store.Open(ctx, "contract/never-written"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})

	t.Run("put replaces what was there", func(t *testing.T) {
		key := "contract/replaced"
		if err := store.Put(ctx, key, strings.NewReader("first"), 5); err != nil {
			t.Fatal(err)
		}
		if err := store.Put(ctx, key, strings.NewReader("second"), 6); err != nil {
			t.Fatal(err)
		}
		reader, _, err := store.Open(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		got, _ := io.ReadAll(reader)
		if string(got) != "second" {
			t.Fatalf("got %q, want the second write", got)
		}
	})

	t.Run("delete is idempotent", func(t *testing.T) {
		key := "contract/deleted"
		if err := store.Put(ctx, key, strings.NewReader("x"), 1); err != nil {
			t.Fatal(err)
		}
		if err := store.Delete(ctx, key); err != nil {
			t.Fatal(err)
		}
		// Cleanup paths delete without checking, so a second one must be quiet.
		if err := store.Delete(ctx, key); err != nil {
			t.Fatalf("deleting an absent key should be fine: %v", err)
		}
		if _, _, err := store.Open(ctx, key); !errors.Is(err, ErrNotFound) {
			t.Fatalf("deleted key still readable: %v", err)
		}
	})

	t.Run("keys with slashes nest", func(t *testing.T) {
		key := "contract/a/b/c/deep"
		if err := store.Put(ctx, key, strings.NewReader("deep"), 4); err != nil {
			t.Fatal(err)
		}
		reader, _, err := store.Open(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		defer reader.Close()
		got, _ := io.ReadAll(reader)
		if string(got) != "deep" {
			t.Fatalf("got %q", got)
		}
	})
}
