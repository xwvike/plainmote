// Package blob stores resource bodies in S3-compatible object storage.
package blob

import (
	"context"
	"errors"
	"io"
)

// ErrNotFound is returned when a key has no object behind it.
var ErrNotFound = errors.New("blob not found")

// Store is the object-storage boundary used by the service. Tests provide an
// in-memory implementation; production always uses S3.
type Store interface {
	// Put writes size bytes from r under key, replacing anything already
	// there.
	Put(ctx context.Context, key string, r io.Reader, size int64) error
	// Open returns the object's bytes and its length. The caller closes it.
	Open(ctx context.Context, key string) (io.ReadCloser, int64, error)
	// OpenRange returns the inclusive byte interval without reading the rest of
	// the object through the application. The caller validates the interval
	// against the size stored with the resource and closes the returned body.
	OpenRange(ctx context.Context, key string, start, end int64) (io.ReadCloser, int64, error)
	// Delete removes an object. Removing something absent is not an error,
	// so cleaning up twice is safe.
	Delete(ctx context.Context, key string) error
}
