package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AnonymousUserID owns every paste made without signing in. It is a seeded
// users row rather than a null owner, so owner_id stays NOT NULL across
// resources and access_logs, and the quota, the log and the delete path keep
// working with no special case. Nobody can sign in as it.
const AnonymousUserID = "00000000-0000-0000-0000-000000000001"

const (
	// AnonymousDefaultTTL is what the page offers first, and AnonymousMaxTTL is
	// the ceiling a crafted request cannot get past. Both are minutes rather
	// than days on purpose: this is a handoff, not storage. Nothing posted
	// through the open endpoint can be reached for long, which is most of what
	// keeps it from being worth abusing and all of what bounds its footprint.
	AnonymousDefaultTTL = time.Minute
	AnonymousMaxTTL     = 30 * time.Minute

	// AnonymousMaxBytes is far below the signed-in limit: enough for a config
	// or a snippet, not enough to be a file host.
	AnonymousMaxBytes = 128 << 10

	// anonymousContentType is what an anonymous paste is always served as. The
	// body comes from a textarea, so it is text by construction, and refusing
	// to serve it as anything else - html most of all - takes "host a page on
	// someone else's domain" off the table entirely. The filename still decides
	// what a download is called.
	//
	// The charset is part of the type and not an afterthought: without it a
	// browser guesses, and a paste in Chinese comes back as mojibake.
	anonymousContentType = "text/plain; charset=utf-8"
	anonymousEncoding    = "utf-8"
)

// Minutes, not a Go duration: %s renders AnonymousMaxTTL as "30m0s".
var errAnonymousTTL = fmt.Errorf("有效期最短 %d 分钟，最长 %d 分钟",
	int(AnonymousDefaultTTL/time.Minute), int(AnonymousMaxTTL/time.Minute))

// CreateAnonymousPaste writes the body and the one time-limited link that
// reaches it in a single transaction. A paste with no link is unreachable
// litter, and a link with no paste is a dead address; neither is worth having
// on its own.
func (d *Store) CreateAnonymousPaste(ctx context.Context, filename string, content []byte, ttl time.Duration, now time.Time) (Resource, Link, error) {
	return d.CreateAnonymousPasteFor(ctx, "", filename, content, ttl, now)
}

// CreateAnonymousPasteFor keeps the paste anonymous while remembering which
// signed-in account may explicitly adopt it. An empty creator is the public
// flow and leaves no claim behind.
func (d *Store) CreateAnonymousPasteFor(ctx context.Context, creatorID, filename string, content []byte, ttl time.Duration, now time.Time) (Resource, Link, error) {
	if creatorID != "" && (!validUUIDs(creatorID) || creatorID == AnonymousUserID) {
		return Resource{}, Link{}, ErrNotFound
	}
	if ttl <= 0 {
		ttl = AnonymousDefaultTTL
	}
	if ttl < AnonymousDefaultTTL || ttl > AnonymousMaxTTL {
		return Resource{}, Link{}, errAnonymousTTL
	}
	filename = strings.TrimSpace(filename)
	if err := validateFilename(filename); err != nil {
		return Resource{}, Link{}, err
	}
	if len(content) == 0 {
		return Resource{}, Link{}, errors.New("内容不能为空")
	}
	if len(content) > AnonymousMaxBytes {
		return Resource{}, Link{}, fmt.Errorf("内容最大 %s", BytesText(AnonymousMaxBytes))
	}
	if !utf8.Valid(content) {
		return Resource{}, Link{}, errors.New("内容必须是有效的 UTF-8 文本")
	}
	// A textarea submits CRLF whatever the user typed, so the stored bytes are
	// normalised once here rather than being whatever the browser felt like.
	content = []byte(ApplyEOL(string(content), "lf"))

	name := filename
	if name == "" {
		name = "匿名内容"
	}
	resource := Resource{
		ID: uuid.NewString(), OwnerID: AnonymousUserID, Name: name, Filename: filename,
		ContentType: anonymousContentType, ContentEncoding: anonymousEncoding,
		ContentSize: int64(len(content)), CreatedAt: now, UpdatedAt: now,
	}
	resource.ContentKey = contentKey(resource.ID)
	link := Link{ResourceID: resource.ID, ExpiresAt: shareExpiry(now, ttl)}

	// Same ordering as every other write here: the object first, outside the
	// transaction, and taken back out if the transaction does not commit.
	if err := d.blobs.Put(ctx, resource.ContentKey, bytes.NewReader(content), resource.ContentSize); err != nil {
		return Resource{}, Link{}, fmt.Errorf("store paste body: %w: %w", ErrInternal, err)
	}

	err := d.withTx(ctx, func(tx pgx.Tx) error {
		limit, usage, err := quotaGate(ctx, tx, AnonymousUserID, "", now)
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
INSERT INTO resources(id, owner_id, name, filename, content_key, content_size, content_type, content_encoding, origin_url, created_at, updated_at)
VALUES($1, $2, $3, $4, $5, $6, $7, $8, '', $9, $9)
`, resource.ID, resource.OwnerID, resource.Name, resource.Filename, resource.ContentKey,
			resource.ContentSize, resource.ContentType, resource.ContentEncoding, now); err != nil {
			return fmt.Errorf("create paste: %w: %w", ErrInternal, err)
		}
		if err := d.insertLink(ctx, tx, &link, now); err != nil {
			return err
		}
		if creatorID != "" {
			if _, err := tx.Exec(ctx, `
INSERT INTO paste_claims(resource_id, user_id, created_at)
VALUES($1, $2, $3)
`, resource.ID, creatorID, now); err != nil {
				return fmt.Errorf("create paste claim: %w: %w", ErrInternal, err)
			}
		}
		return nil
	})
	if err != nil {
		_ = d.blobs.Delete(ctx, resource.ContentKey)
		return Resource{}, Link{}, err
	}
	return resource, link, nil
}

func (d *Store) ClaimableAnonymousPaste(ctx context.Context, userID, resourceID string, now time.Time) (Resource, Link, error) {
	if !validUUIDs(userID, resourceID) {
		return Resource{}, Link{}, ErrNotFound
	}
	return d.claimableAnonymousPaste(ctx, d.db, userID, resourceID, now, false)
}

func (d *Store) claimableAnonymousPaste(ctx context.Context, q storeQuerier, userID, resourceID string, now time.Time, lock bool) (Resource, Link, error) {
	resourceQuery := `
SELECT r.id, r.owner_id, r.name, r.filename, r.content_key, r.content_size,
       r.content_type, r.content_encoding, r.origin_url, r.created_at, r.updated_at
FROM paste_claims pc
JOIN resources r ON r.id = pc.resource_id
WHERE pc.resource_id = $1 AND pc.user_id = $2 AND r.owner_id = $3
  AND EXISTS (
    SELECT 1 FROM links l
    WHERE l.resource_id = r.id AND l.revoked_at IS NULL
      AND (l.expires_at IS NULL OR l.expires_at > $4)
      AND (l.max_uses = 0 OR l.used_count < l.max_uses)
  )`
	if lock {
		resourceQuery += ` FOR UPDATE OF r, pc`
	}
	var resource Resource
	err := q.QueryRow(ctx, resourceQuery, resourceID, userID, AnonymousUserID, now).Scan(
		&resource.ID, &resource.OwnerID, &resource.Name, &resource.Filename,
		&resource.ContentKey, &resource.ContentSize, &resource.ContentType, &resource.ContentEncoding,
		&resource.OriginURL, &resource.CreatedAt, &resource.UpdatedAt,
	)
	if err != nil {
		return Resource{}, Link{}, translateNotFound(err)
	}
	linkQuery := `SELECT ` + linkColumns + ` FROM links
WHERE resource_id = $1 AND revoked_at IS NULL
  AND (expires_at IS NULL OR expires_at > $2)
  AND (max_uses = 0 OR used_count < max_uses)
ORDER BY created_at DESC, id DESC LIMIT 1`
	if lock {
		linkQuery += ` FOR UPDATE`
	}
	link, err := d.scanLink(q.QueryRow(ctx, linkQuery, resourceID, now))
	if err != nil {
		return Resource{}, Link{}, translateNotFound(err)
	}
	return resource, link, nil
}

// ClaimAnonymousPaste turns the existing anonymous resource into an owned one.
// Its link and object are left in place, so the address, remaining lifetime,
// use count and previously recorded visits all follow it into the account.
func (d *Store) ClaimAnonymousPaste(ctx context.Context, userID, resourceID string, now time.Time) (Resource, error) {
	if !validUUIDs(userID, resourceID) {
		return Resource{}, ErrNotFound
	}
	var resource Resource
	err := d.withTx(ctx, func(tx pgx.Tx) error {
		limit, usage, err := quotaGate(ctx, tx, userID, "", now)
		if err != nil {
			return err
		}
		resource, _, err = d.claimableAnonymousPaste(ctx, tx, userID, resourceID, now, true)
		if err != nil {
			return err
		}
		if usage.Resources+1 > limit.Resources {
			return resourceQuotaError(limit.Resources, usage.Resources)
		}
		if usage.StorageBytes+resource.ContentSize > limit.StorageBytes {
			return storageQuotaError(limit.StorageBytes, usage.StorageBytes, resource.ContentSize)
		}
		if _, err := tx.Exec(ctx, `UPDATE resources SET owner_id = $1, updated_at = $2 WHERE id = $3`, userID, now, resourceID); err != nil {
			return fmt.Errorf("claim paste resource: %w: %w", ErrInternal, err)
		}
		if _, err := tx.Exec(ctx, `UPDATE access_logs SET owner_id = $1 WHERE resource_id = $2`, userID, resourceID); err != nil {
			return fmt.Errorf("claim paste logs: %w: %w", ErrInternal, err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM paste_claims WHERE resource_id = $1 AND user_id = $2`, resourceID, userID); err != nil {
			return fmt.Errorf("delete paste claim: %w: %w", ErrInternal, err)
		}
		resource.OwnerID = userID
		resource.UpdatedAt = now
		return nil
	})
	if err != nil {
		return Resource{}, err
	}
	return resource, nil
}

// pruneAnonymous removes pastes nothing can reach any more. An anonymous link
// always expires, so every paste reaches this state on its own; a paste whose
// link was never written reaches it immediately, which is what cleans up after
// a half-finished create.
//
// The rows go first and the objects after, so a failure here leaves an object
// nothing points at rather than a row pointing at nothing.
func (d *Store) pruneAnonymous(ctx context.Context, conn *pgxpool.Conn, now time.Time) (int64, error) {
	var removed int64
	for {
		if err := ctx.Err(); err != nil {
			return removed, nil
		}
		rows, err := conn.Query(ctx, `
DELETE FROM resources
WHERE id IN (
  SELECT r.id FROM resources r
  WHERE r.owner_id = $1
    AND NOT EXISTS (
      SELECT 1 FROM links l
      WHERE l.resource_id = r.id
        AND l.revoked_at IS NULL
        AND (l.expires_at IS NULL OR l.expires_at > $2)
    )
  ORDER BY r.created_at
  LIMIT $3
)
RETURNING content_key
`, AnonymousUserID, now, pruneBatch)
		if err != nil {
			return removed, err
		}
		var keys []string
		for rows.Next() {
			var key string
			if err := rows.Scan(&key); err != nil {
				rows.Close()
				return removed, err
			}
			keys = append(keys, key)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return removed, err
		}
		for _, key := range keys {
			if key != "" {
				_ = d.blobs.Delete(ctx, key)
			}
		}
		removed += int64(len(keys))
		if len(keys) < pruneBatch {
			return removed, nil
		}
	}
}
