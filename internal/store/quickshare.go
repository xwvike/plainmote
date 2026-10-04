package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// A quick share made while signed in is its creator's own from the start: a
// resource with one link, listed with the rest, whose access log its creator
// reads. It is read only and ends with its link - expires_at is the link's
// expiry - unless it is kept as a resource first. Made without signing in, a
// quick share is anonymous as before (anonymous.go).

// errQuickShareReadOnly refuses an edit to a quick share not yet kept.
var errQuickShareReadOnly = refusal("快速分享为只读，转为资源后才能修改")

// refuseQuickShare is the guard every change other than keeping, deleting
// or revoking goes through.
func refuseQuickShare(resource Resource) error {
	if resource.QuickShare() {
		return errQuickShareReadOnly
	}
	return nil
}

// CreateQuickShare stores a signed-in quick share. What it holds is typed as
// an anonymous paste's is - text as plain text, images, audio and video as
// what their bytes prove, anything else a download - and it counts toward the
// account's storage like any resource.
func (d *Store) CreateQuickShare(ctx context.Context, ownerID, filename string, content []byte, ttl time.Duration, now time.Time) (Resource, Link, error) {
	if !validUUIDs(ownerID) || ownerID == AnonymousUserID {
		return Resource{}, Link{}, ErrNotFound
	}
	ttl, err := quickShareTTL(ttl)
	if err != nil {
		return Resource{}, Link{}, err
	}
	if len(content) == 0 {
		return Resource{}, Link{}, refusal("内容不能为空")
	}
	if len(content) > AnonymousMaxBytes {
		return Resource{}, Link{}, refusalf("内容最大 %s", BytesText(AnonymousMaxBytes))
	}
	if err := validateFilename(filename); err != nil {
		return Resource{}, Link{}, err
	}
	contentType, encoding := anonymousType(content)
	return d.insertQuickShare(ctx, ownerID, filename, content, contentType, encoding, ttl, now)
}

func quickShareTTL(ttl time.Duration) (time.Duration, error) {
	if ttl <= 0 {
		ttl = AnonymousDefaultTTL
	}
	if ttl < AnonymousMinTTL || ttl > AnonymousMaxTTL {
		return 0, errAnonymousTTL
	}
	return ttl, nil
}

func (d *Store) insertQuickShare(ctx context.Context, ownerID, filename string, content []byte, contentType, encoding string, ttl time.Duration, now time.Time) (Resource, Link, error) {
	link := Link{ExpiresAt: shareExpiry(now, ttl)}
	resource := Resource{
		ID: uuid.NewString(), OwnerID: ownerID, Name: filename, Filename: filename,
		ContentType: contentType, ContentEncoding: encoding,
		ContentSize: int64(len(content)), CreatedAt: now, UpdatedAt: now,
		Version: 1, VersionAt: now, ContentSHA256: contentSHA256(content), ExpiresAt: link.ExpiresAt,
	}
	resource.ContentKey = contentKey(resource.ID)
	link.ResourceID = resource.ID

	if err := d.blobs.Put(ctx, resource.ContentKey, bytes.NewReader(content), resource.ContentSize); err != nil {
		return Resource{}, Link{}, fmt.Errorf("store quick share body: %w: %w", ErrInternal, err)
	}
	var dropped []string
	err := d.withTx(ctx, func(tx pgx.Tx) error {
		limit, usage, err := quotaGate(ctx, tx, ownerID, "", now)
		if err != nil {
			return err
		}
		if usage.Resources+1 > limit.Resources {
			return resourceQuotaError(limit.Resources, usage.Resources)
		}
		if usage.StorageBytes+resource.ContentSize > limit.StorageBytes {
			return storageQuotaError(limit.StorageBytes, usage.StorageBytes, resource.ContentSize)
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO resources(id, owner_id, name, filename, content_key, content_size, content_type, content_encoding, origin_url,
                      created_at, updated_at, version, version_at, content_sha256, expires_at)
VALUES($1, $2, $3, $4, $5, $6, $7, $8, '', $9, $9, 1, $9, $10, $11)
`, resource.ID, ownerID, resource.Name, resource.Filename, resource.ContentKey,
			resource.ContentSize, resource.ContentType, resource.ContentEncoding, now, resource.ContentSHA256, resource.ExpiresAt); err != nil {
			return fmt.Errorf("create quick share: %w: %w", ErrInternal, err)
		}
		if err := d.insertLink(ctx, tx, &link, now); err != nil {
			return err
		}
		dropped, err = trimHistoryTx(ctx, tx, ownerID, "", limit.StorageBytes)
		return err
	})
	if err != nil {
		_ = d.blobs.Delete(ctx, resource.ContentKey)
		return Resource{}, Link{}, err
	}
	d.dropObjects(ctx, dropped)
	return resource, link, nil
}

// QuickShareLink is the link a quick share was made with, while it works.
func (d *Store) QuickShareLink(ctx context.Context, ownerID, resourceID string, now time.Time) (Resource, Link, error) {
	resource, err := d.ResourceForOwner(ctx, ownerID, resourceID)
	if err != nil {
		return Resource{}, Link{}, err
	}
	if !resource.QuickShare() {
		return Resource{}, Link{}, ErrNotFound
	}
	link, err := d.scanLink(d.db.QueryRow(ctx, `SELECT `+linkColumns+` FROM links
WHERE resource_id = $1 AND revoked_at IS NULL
  AND (expires_at IS NULL OR expires_at > $2)
  AND (max_uses = 0 OR used_count < max_uses)
ORDER BY created_at DESC, id DESC LIMIT 1`, resourceID, now))
	if err != nil {
		return Resource{}, Link{}, pasteLinkError(err)
	}
	return resource, link, nil
}

// KeepQuickShare makes a quick share an ordinary resource: it stops being
// read only and is no longer deleted when its link ends. The link keeps the
// term it was given. Encrypted content cannot be kept yet: a resource is
// edited and served as what it holds, and the service cannot read this.
func (d *Store) KeepQuickShare(ctx context.Context, ownerID, resourceID string, now time.Time) error {
	resource, err := d.ResourceForOwner(ctx, ownerID, resourceID)
	if err != nil {
		return err
	}
	if !resource.QuickShare() {
		return nil
	}
	if resource.Encrypted() {
		return refusal("端到端加密的快速分享暂不能转为资源")
	}
	tag, err := d.db.Exec(ctx, `
UPDATE resources SET expires_at = NULL, updated_at = $3
WHERE id = $1 AND owner_id = $2 AND expires_at > $3
`, resourceID, ownerID, now)
	if err != nil {
		return fmt.Errorf("keep quick share: %w: %w", ErrInternal, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// PruneQuickShares deletes the quick shares whose time is up, each the way
// its owner deleting it would: the content and its links go, the access log
// stays with the owner under the name it was reached by.
func (d *Store) PruneQuickShares(ctx context.Context, now time.Time) (int64, error) {
	var removed int64
	for {
		if err := ctx.Err(); err != nil {
			return removed, nil
		}
		rows, err := d.db.Query(ctx, `
SELECT id, owner_id FROM resources
WHERE expires_at IS NOT NULL AND expires_at <= $1
ORDER BY expires_at
LIMIT $2
`, now, pruneBatch)
		if err != nil {
			return removed, fmt.Errorf("find ended quick shares: %w", err)
		}
		type ended struct{ id, owner string }
		due, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (ended, error) {
			var e ended
			return e, row.Scan(&e.id, &e.owner)
		})
		if err != nil {
			return removed, fmt.Errorf("find ended quick shares: %w", err)
		}
		for _, e := range due {
			// Kept in the meantime, or deleted by its owner: either way not
			// this pass's to delete.
			switch err := d.deleteEndedQuickShare(ctx, e.owner, e.id, now); {
			case err == nil:
				removed++
			case !errors.Is(err, ErrNotFound):
				return removed, err
			}
		}
		if len(due) < pruneBatch {
			return removed, nil
		}
	}
}

// deleteEndedQuickShare is DeleteResource for a quick share still ended
// when the row is locked.
func (d *Store) deleteEndedQuickShare(ctx context.Context, ownerID, id string, now time.Time) error {
	var contentKey string
	err := d.withTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
DELETE FROM resources WHERE id = $1 AND owner_id = $2 AND expires_at IS NOT NULL AND expires_at <= $3
RETURNING content_key
`, id, ownerID, now).Scan(&contentKey)
		return translateNotFound(err)
	})
	if err != nil {
		return err
	}
	if contentKey != "" {
		_ = d.blobs.Delete(ctx, contentKey)
	}
	return nil
}
