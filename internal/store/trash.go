package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TrashRetention is how long a trashed resource stays recoverable before the
// periodic prune permanently removes it and its stored objects.
const TrashRetention = 30 * 24 * time.Hour

// TrashResource moves an active resource into the trash and revokes all of its
// active share links immediately. It returns how many active links were
// revoked. Trashed resources remain counted toward the account's quota until
// they are permanently deleted or age out.
func (d *Store) TrashResource(ctx context.Context, ownerID, id string, now time.Time) (int, error) {
	if !validUUIDs(ownerID, id) {
		return 0, ErrNotFound
	}
	var revoked int
	err := d.withTx(ctx, func(tx pgx.Tx) error {
		var locked string
		if err := tx.QueryRow(ctx, `
SELECT id FROM resources
WHERE id = $1 AND owner_id = $2 AND deleted_at IS NULL AND (expires_at IS NULL OR expires_at > $3)
FOR UPDATE
`, id, ownerID, now).Scan(&locked); err != nil {
			return translateNotFound(err)
		}
		// Count only currently live links as revoked, while marking every
		// unrevoked link row revoked so none can revive if the resource is
		// restored later.
		if err := tx.QueryRow(ctx, `
SELECT COUNT(*) FROM links
WHERE resource_id = $1 AND revoked_at IS NULL
  AND (expires_at IS NULL OR expires_at > $2)
  AND (max_uses = 0 OR used_count < max_uses)
`, id, now).Scan(&revoked); err != nil {
			return fmt.Errorf("count live shares: %w: %w", ErrInternal, err)
		}
		if _, err := tx.Exec(ctx, `
UPDATE links SET revoked_at = $1
WHERE resource_id = $2 AND revoked_at IS NULL
`, now, id); err != nil {
			return fmt.Errorf("revoke shares: %w: %w", ErrInternal, err)
		}
		if _, err := tx.Exec(ctx, `
UPDATE resources SET deleted_at = $1
WHERE id = $2 AND owner_id = $3
`, now, id, ownerID); err != nil {
			return fmt.Errorf("trash resource: %w: %w", ErrInternal, err)
		}
		return nil
	})
	return revoked, err
}

// RestoreResource brings a trashed resource back to the active resource list.
// Links that were revoked when it was trashed stay revoked.
func (d *Store) RestoreResource(ctx context.Context, ownerID, id string, now time.Time) (Resource, error) {
	if !validUUIDs(ownerID, id) {
		return Resource{}, ErrNotFound
	}
	resource, err := scanResource(d.db.QueryRow(ctx, `
UPDATE resources r SET deleted_at = NULL
WHERE r.id = $1 AND r.owner_id = $2 AND r.deleted_at IS NOT NULL
  AND (r.expires_at IS NULL OR r.expires_at > $3)
RETURNING `+resourceColumns, id, ownerID, now))
	if err != nil {
		return Resource{}, translateNotFound(err)
	}
	return resource, nil
}

// ListTrash returns the account's trashed resources, most recently trashed
// first, along with the total count.
func (d *Store) ListTrash(ctx context.Context, ownerID string, limit, offset int) ([]Resource, int, error) {
	if !validUUIDs(ownerID) {
		return nil, 0, ErrNotFound
	}
	var total int
	if err := d.db.QueryRow(ctx, `
SELECT COUNT(*) FROM resources r WHERE r.owner_id = $1 AND `+trashedResource, ownerID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count trash: %w: %w", ErrInternal, err)
	}
	if limit <= 0 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := d.db.Query(ctx, `
SELECT `+resourceColumns+`
FROM resources r
WHERE r.owner_id = $1 AND `+trashedResource+`
ORDER BY r.deleted_at DESC, r.id
LIMIT $2 OFFSET $3
`, ownerID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list trash: %w: %w", ErrInternal, err)
	}
	defer rows.Close()
	resources := make([]Resource, 0, limit)
	for rows.Next() {
		resource, err := scanResource(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("list trash: %w: %w", ErrInternal, err)
		}
		resources = append(resources, resource)
	}
	return resources, total, rows.Err()
}

// TrashCount reports how many unexpired resources are currently in the owner's
// trash.
func (d *Store) TrashCount(ctx context.Context, ownerID string) (int, error) {
	if !validUUIDs(ownerID) {
		return 0, ErrNotFound
	}
	var total int
	if err := d.db.QueryRow(ctx, `
SELECT COUNT(*) FROM resources r WHERE r.owner_id = $1 AND `+trashedResource, ownerID).Scan(&total); err != nil {
		return 0, fmt.Errorf("count trash: %w: %w", ErrInternal, err)
	}
	return total, nil
}

// TrashedResourceForOwner reads one trashed resource belonging to ownerID.
func (d *Store) TrashedResourceForOwner(ctx context.Context, ownerID, id string) (Resource, error) {
	if !validUUIDs(ownerID, id) {
		return Resource{}, ErrNotFound
	}
	resource, err := scanResource(d.db.QueryRow(ctx, `
SELECT `+resourceColumns+`
FROM resources r
WHERE r.id = $1 AND r.owner_id = $2 AND `+trashedResource, id, ownerID))
	if err != nil {
		return Resource{}, translateNotFound(err)
	}
	return resource, nil
}

// PurgeResource permanently deletes one resource that is already in the trash,
// along with its version history, links and stored objects.
func (d *Store) PurgeResource(ctx context.Context, ownerID, id string) error {
	if !validUUIDs(ownerID, id) {
		return ErrNotFound
	}
	var contentKey string
	var history []string
	err := d.withTx(ctx, func(tx pgx.Tx) error {
		var locked string
		if err := tx.QueryRow(ctx, `
SELECT id FROM resources WHERE id = $1 AND owner_id = $2 AND deleted_at IS NOT NULL FOR UPDATE
`, id, ownerID).Scan(&locked); err != nil {
			return translateNotFound(err)
		}
		rows, err := tx.Query(ctx, `SELECT content_key FROM resource_versions WHERE resource_id = $1`, id)
		if err != nil {
			return fmt.Errorf("read history: %w: %w", ErrInternal, err)
		}
		if history, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return fmt.Errorf("read history: %w: %w", ErrInternal, err)
		}
		err = tx.QueryRow(ctx, `
DELETE FROM resources WHERE id = $1 AND owner_id = $2 AND deleted_at IS NOT NULL RETURNING content_key
`, id, ownerID).Scan(&contentKey)
		return translateNotFound(err)
	})
	if err != nil {
		return err
	}
	d.dropObjects(ctx, append(history, contentKey))
	return nil
}

// EmptyTrash permanently deletes every trashed resource of ownerID, freeing
// its quota and removing its stored objects.
func (d *Store) EmptyTrash(ctx context.Context, ownerID string) (int64, error) {
	if !validUUIDs(ownerID) {
		return 0, ErrNotFound
	}
	var keys []string
	var removed int64
	err := d.withTx(ctx, func(tx pgx.Tx) error {
		var locked string
		if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id = $1 FOR UPDATE`, ownerID).Scan(&locked); err != nil {
			return translateNotFound(err)
		}
		rows, err := tx.Query(ctx, `
DELETE FROM resource_versions
WHERE resource_id IN (SELECT id FROM resources WHERE owner_id = $1 AND deleted_at IS NOT NULL)
RETURNING content_key
`, ownerID)
		if err != nil {
			return fmt.Errorf("empty trash history: %w: %w", ErrInternal, err)
		}
		if keys, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return fmt.Errorf("empty trash history: %w: %w", ErrInternal, err)
		}
		rows, err = tx.Query(ctx, `
DELETE FROM resources WHERE owner_id = $1 AND deleted_at IS NOT NULL RETURNING content_key
`, ownerID)
		if err != nil {
			return fmt.Errorf("empty trash: %w: %w", ErrInternal, err)
		}
		current, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("empty trash: %w: %w", ErrInternal, err)
		}
		removed = int64(len(current))
		keys = append(keys, current...)
		return nil
	})
	if err != nil {
		return 0, err
	}
	d.dropObjects(ctx, keys)
	return removed, nil
}

// pruneTrash permanently removes trashed resources older than TrashRetention,
// in batches, dropping their stored objects after the database rows are gone.
func (d *Store) pruneTrash(ctx context.Context, conn *pgxpool.Conn, now time.Time) (int64, error) {
	cutoff := now.Add(-TrashRetention)
	var removed int64
	for {
		if err := ctx.Err(); err != nil {
			return removed, nil
		}
		rows, err := conn.Query(ctx, `
SELECT id FROM resources
WHERE deleted_at IS NOT NULL AND deleted_at < $1
ORDER BY deleted_at
LIMIT $2
`, cutoff, pruneBatch)
		if err != nil {
			return removed, err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return removed, err
		}
		if len(ids) == 0 {
			return removed, nil
		}
		var keys []string
		var batchRemoved int64
		if err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			// Lock only rows still trashed past the cutoff: an owner restoring
			// a resource concurrently is never raced into deletion.
			rows, err := tx.Query(ctx, `
SELECT id FROM resources
WHERE id = ANY($1::uuid[]) AND deleted_at IS NOT NULL AND deleted_at < $2
FOR UPDATE
`, ids, cutoff)
			if err != nil {
				return err
			}
			locked, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				return err
			}
			if len(locked) == 0 {
				return nil
			}
			rows, err = tx.Query(ctx, `
DELETE FROM resource_versions WHERE resource_id = ANY($1::uuid[]) RETURNING content_key
`, locked)
			if err != nil {
				return err
			}
			history, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				return err
			}
			rows, err = tx.Query(ctx, `
DELETE FROM resources WHERE id = ANY($1::uuid[]) RETURNING content_key
`, locked)
			if err != nil {
				return err
			}
			current, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				return err
			}
			batchRemoved = int64(len(current))
			keys = append(history, current...)
			return nil
		}); err != nil {
			return removed, err
		}
		d.dropObjects(ctx, keys)
		removed += batchRemoved
		if len(ids) < pruneBatch {
			return removed, nil
		}
	}
}
