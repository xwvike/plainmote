package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"plainmote/internal/blob"
)

var ErrNotFound = errors.New("store: not found")

type Store struct {
	db     *pgxpool.Pool
	blobs  blob.Store
	cipher *tokenCipher

	// The last prune that finished, for the operator's overview.
	pruneMu   sync.Mutex
	pruneAt   time.Time
	pruneLast PruneResult
}

func Open(ctx context.Context, databaseURL string, key []byte, blobs blob.Store) (*Store, error) {
	if strings.TrimSpace(databaseURL) == "" {
		return nil, errors.New("database URL must not be empty")
	}
	if blobs == nil {
		return nil, errors.New("a blob store is required")
	}
	cipher, err := newTokenCipher(key)
	if err != nil {
		return nil, err
	}
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect PostgreSQL: %w", err)
	}
	store := &Store{db: pool, blobs: blobs, cipher: cipher}
	if err := store.initializeSchema(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return store, nil
}

func (d *Store) Close() { d.db.Close() }

func (d *Store) withTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, d.db, fn)
}

// withTxStatus runs fn inside a transaction and distinguishes a definite
// rollback (Begin or fn returned an error, so COMMIT was never sent) from an
// uncertain commit failure (fn returned nil and tx.Commit failed, where
// PostgreSQL may still have committed the transaction before the connection
// broke).
func (d *Store) withTxStatus(ctx context.Context, fn func(pgx.Tx) error) (rolledBack bool, err error) {
	tx, err := d.db.Begin(ctx)
	if err != nil {
		return true, err
	}
	committed := false
	defer func() {
		if !committed {
			rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			_ = tx.Rollback(rollbackCtx)
		}
	}()
	if err := fn(tx); err != nil {
		return true, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, err
	}
	committed = true
	return false, nil
}

func translateNotFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func validUUIDs(values ...string) bool {
	for _, value := range values {
		if uuid.Validate(value) != nil {
			return false
		}
	}
	return true
}
