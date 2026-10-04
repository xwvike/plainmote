package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A resource keeps the versions its content replaced, whole, for a while:
// at most HistoryKeep of them, each for HistoryRetention after it was
// replaced. They are counted as the account's storage but only ever occupy
// what its current content leaves free - a save that needs the room takes it
// back, oldest version first - so history can never be the reason a save is
// refused.
const (
	HistoryKeep      = 10
	HistoryRetention = 30 * 24 * time.Hour
)

// ErrVersionConflict marks a save made against a version that is no longer
// current: the content was saved from somewhere else in the meantime.
var ErrVersionConflict = errors.New("store: version conflict")

// VersionConflict says what the content moved on to. Nothing was written.
type VersionConflict struct {
	Current int
	At      time.Time
}

func (e *VersionConflict) Is(target error) bool { return target == ErrVersionConflict }

func (e *VersionConflict) Error() string {
	return fmt.Sprintf("此资源已在别处更新为 v%d", e.Current)
}

// SaveResult is what a save did besides saving: the version now current,
// whether this save is what made it, and how many earlier versions gave up
// their room to it.
type SaveResult struct {
	Version    int
	NewVersion bool
	Trimmed    int
}

func contentSHA256(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// currentContent is a resource's content as it stands under its row lock.
type currentContent struct {
	Key          string
	Size         int64
	Type         string
	Encoding     string
	SHA256       string
	Version      int
	VersionAt    time.Time
	RestoredFrom int
	Remote       bool
	Filename     string
	SealedMeta   []byte
}

// lockContentTx locks the resource row and reads its content. Everything that
// replaces content goes through here after quotaGate, so the version a save
// is checked against, the one it retires and the one it writes are the same.
func lockContentTx(ctx context.Context, tx pgx.Tx, ownerID, id string) (currentContent, error) {
	var current currentContent
	err := tx.QueryRow(ctx, `
SELECT content_key, content_size, content_type, content_encoding, content_sha256,
       version, COALESCE(version_at, updated_at), COALESCE(restored_from, 0), origin_url <> '', filename, sealed_meta
FROM resources WHERE id = $1 AND owner_id = $2
FOR UPDATE
`, id, ownerID).Scan(&current.Key, &current.Size, &current.Type, &current.Encoding, &current.SHA256,
		&current.Version, &current.VersionAt, &current.RestoredFrom, &current.Remote, &current.Filename, &current.SealedMeta)
	if err != nil {
		return currentContent{}, translateNotFound(err)
	}
	return current, nil
}

// retireTx files the current content as a version of its own. Its object is
// not copied: the key simply moves from the resource row to the history row,
// which is why a save no longer deletes the object it replaces.
func retireTx(ctx context.Context, tx pgx.Tx, id string, current currentContent, now time.Time) error {
	if current.Key == "" || current.Remote {
		return nil
	}
	_, err := tx.Exec(ctx, `
INSERT INTO resource_versions(
  resource_id, version, content_key, content_size, content_type, content_encoding,
  content_sha256, restored_from, saved_at, replaced_at, filename, sealed_meta
)
VALUES($1, $2, $3, $4, $5, $6, $7, NULLIF($8, 0), $9, $10, $11, $12)
`, id, current.Version, current.Key, current.Size, current.Type, current.Encoding,
		current.SHA256, current.RestoredFrom, current.VersionAt, now, current.Filename, current.SealedMeta)
	if err != nil {
		return fmt.Errorf("keep version %d: %w: %w", current.Version, ErrInternal, err)
	}
	return nil
}

// trimHistoryTx holds the history to its limits after a write and returns the
// objects of the versions it dropped, for the caller to delete once the
// transaction has committed. resourceID, when given, is held to HistoryKeep;
// the account as a whole is held to the room its current content leaves under
// limit, dropping the versions replaced longest ago first.
func trimHistoryTx(ctx context.Context, tx pgx.Tx, ownerID, resourceID string, limit int64) ([]string, error) {
	var keys []string
	collect := func(rows pgx.Rows, err error) error {
		if err != nil {
			return err
		}
		dropped, err := pgx.CollectRows(rows, pgx.RowTo[string])
		keys = append(keys, dropped...)
		return err
	}
	if resourceID != "" {
		if err := collect(tx.Query(ctx, `
DELETE FROM resource_versions
WHERE resource_id = $1 AND version NOT IN (
  SELECT version FROM resource_versions WHERE resource_id = $1 ORDER BY version DESC LIMIT $2
)
RETURNING content_key
`, resourceID, HistoryKeep)); err != nil {
			return nil, fmt.Errorf("trim history: %w: %w", ErrInternal, err)
		}
	}
	// Newest first, a running total: every version whose total no longer fits
	// in the room goes. The order is total, so no two rows tie.
	if err := collect(tx.Query(ctx, `
WITH room AS (
  SELECT $2::bigint - COALESCE(SUM(content_size), 0) AS bytes FROM resources WHERE owner_id = $1
), history AS (
  SELECT v.resource_id, v.version,
         SUM(v.content_size) OVER (ORDER BY v.replaced_at DESC, v.resource_id, v.version DESC) AS kept
  FROM resource_versions v JOIN resources r ON r.id = v.resource_id
  WHERE r.owner_id = $1
)
DELETE FROM resource_versions v
USING history h, room
WHERE v.resource_id = h.resource_id AND v.version = h.version AND h.kept > room.bytes
RETURNING v.content_key
`, ownerID, limit)); err != nil {
		return nil, fmt.Errorf("fit history: %w: %w", ErrInternal, err)
	}
	return keys, nil
}

// dropObjects deletes objects no row points at any more. It runs after the
// commit that let go of them, detached from the request: once the rows are
// gone a client hanging up must not leave the objects behind. A failure costs
// storage, not correctness, and is only logged.
func (d *Store) dropObjects(ctx context.Context, keys []string) {
	if len(keys) == 0 {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	for _, key := range keys {
		if key == "" {
			continue
		}
		if err := d.blobs.Delete(cleanupCtx, key); err != nil {
			fmt.Fprintf(os.Stderr, "delete object %s: %v\n", key, err)
		}
	}
}

const versionColumns = `v.resource_id, v.version, v.content_key, v.content_size, v.content_type, v.content_encoding,
       v.content_sha256, COALESCE(v.restored_from, 0), v.saved_at, v.replaced_at, v.filename, v.sealed_meta`

func scanVersion(row rowScanner) (Version, error) {
	var v Version
	err := row.Scan(&v.ResourceID, &v.Number, &v.ContentKey, &v.ContentSize, &v.ContentType, &v.ContentEncoding,
		&v.ContentSHA256, &v.RestoredFrom, &v.SavedAt, &v.ReplacedAt, &v.Filename, &v.SealedMeta)
	return v, err
}

// ListVersions returns a resource's earlier versions, newest first. The
// current content is not among them; it is the resource itself.
func (d *Store) ListVersions(ctx context.Context, ownerID, resourceID string) ([]Version, error) {
	if !validUUIDs(ownerID, resourceID) {
		return nil, ErrNotFound
	}
	rows, err := d.db.Query(ctx, `
SELECT `+versionColumns+`
FROM resource_versions v JOIN resources r ON r.id = v.resource_id
WHERE v.resource_id = $1 AND r.owner_id = $2
ORDER BY v.version DESC
`, resourceID, ownerID)
	if err != nil {
		return nil, fmt.Errorf("list versions: %w: %w", ErrInternal, err)
	}
	versions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Version, error) { return scanVersion(row) })
	if err != nil {
		return nil, fmt.Errorf("list versions: %w: %w", ErrInternal, err)
	}
	return versions, nil
}

// HistoryCount is how many earlier versions a resource has.
func (d *Store) HistoryCount(ctx context.Context, ownerID, resourceID string) (int, error) {
	if !validUUIDs(ownerID, resourceID) {
		return 0, ErrNotFound
	}
	var count int
	err := d.db.QueryRow(ctx, `
SELECT COUNT(*) FROM resource_versions v JOIN resources r ON r.id = v.resource_id
WHERE v.resource_id = $1 AND r.owner_id = $2
`, resourceID, ownerID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count versions: %w: %w", ErrInternal, err)
	}
	return count, nil
}

// VersionForOwner reads one earlier version of a resource the owner holds.
func (d *Store) VersionForOwner(ctx context.Context, ownerID, resourceID string, number int) (Version, error) {
	if !validUUIDs(ownerID, resourceID) || number <= 0 {
		return Version{}, ErrNotFound
	}
	v, err := scanVersion(d.db.QueryRow(ctx, `
SELECT `+versionColumns+`
FROM resource_versions v JOIN resources r ON r.id = v.resource_id
WHERE v.resource_id = $1 AND r.owner_id = $2 AND v.version = $3
`, resourceID, ownerID, number))
	if err != nil {
		return Version{}, translateNotFound(err)
	}
	return v, nil
}

func (d *Store) OpenVersion(ctx context.Context, v Version) (io.ReadCloser, int64, error) {
	return d.blobs.Open(ctx, v.ContentKey)
}

func (d *Store) OpenVersionRange(ctx context.Context, v Version, start, end int64) (io.ReadCloser, int64, error) {
	return d.blobs.OpenRange(ctx, v.ContentKey, start, end)
}

func (d *Store) ReadVersion(ctx context.Context, v Version) ([]byte, error) {
	reader, _, err := d.OpenVersion(ctx, v)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

// RestoreVersion makes an earlier version's content current again, as a new
// version on top of the one it replaces. Nothing is rewritten: the version
// restored stays where it was, and the content it replaces joins the history,
// so a restore is undone by restoring again.
func (d *Store) RestoreVersion(ctx context.Context, ownerID, resourceID string, number int) (SaveResult, error) {
	source, err := d.VersionForOwner(ctx, ownerID, resourceID, number)
	if err != nil {
		return SaveResult{}, err
	}
	body, err := d.ReadVersion(ctx, source)
	if err != nil {
		return SaveResult{}, fmt.Errorf("read version %d: %w: %w", number, ErrInternal, err)
	}
	resource, err := d.ResourceForOwner(ctx, ownerID, resourceID)
	if err != nil {
		return SaveResult{}, err
	}
	if err := refuseQuickShare(resource); err != nil {
		return SaveResult{}, err
	}
	// The name stays unless it would misname what comes back, and the type is
	// read from the bytes under that name, the way a save reads it - not taken
	// from the row, which may predate what detection knows now.
	filename := RefitFilename(resource.Filename, source.Filename, body)
	contentType, encoding := DetectContent(filename, body, source.ContentEncoding)
	// An encrypted version comes back as it is, with the name it had: the
	// service cannot read either, and needs to read neither.
	if resource.Sealed() {
		filename, contentType, encoding = "", SealedContentType, ""
	}
	// A copy, not the same key: the history row keeps its own object, and one
	// object shared by two rows would need counting before either could go.
	key := contentKey(resourceID)
	if err := d.blobs.Put(ctx, key, bytes.NewReader(body), int64(len(body))); err != nil {
		return SaveResult{}, fmt.Errorf("store restored body: %w: %w", ErrInternal, err)
	}
	sha := source.ContentSHA256
	if sha == "" {
		sha = contentSHA256(body)
	}

	now := time.Now().UTC()
	var result SaveResult
	var dropped []string
	err = d.withTx(ctx, func(tx pgx.Tx) error {
		limit, usage, err := quotaGate(ctx, tx, ownerID, resourceID, now)
		if err != nil {
			return err
		}
		current, err := lockContentTx(ctx, tx, ownerID, resourceID)
		if err != nil {
			return err
		}
		if current.Remote {
			return ErrNotFound
		}
		// Gone between the read above and the lock: deleted, or trimmed by a
		// save that needed the room.
		var still int
		if err := tx.QueryRow(ctx, `SELECT version FROM resource_versions WHERE resource_id = $1 AND version = $2 FOR UPDATE`,
			resourceID, number).Scan(&still); err != nil {
			return translateNotFound(err)
		}
		if usage.StorageBytes+source.ContentSize > limit.StorageBytes {
			return storageQuotaError(limit.StorageBytes, usage.StorageBytes, source.ContentSize)
		}
		if err := retireTx(ctx, tx, resourceID, current, now); err != nil {
			return err
		}
		result.Version = current.Version + 1
		if _, err := tx.Exec(ctx, `
UPDATE resources
SET content_key = $1, content_size = $2, content_type = $3, content_encoding = $4, content_sha256 = $5,
    version = $6, version_at = $7, restored_from = $8, updated_at = $7, filename = $9,
    sealed_meta = COALESCE($12, sealed_meta)
WHERE id = $10 AND owner_id = $11
`, key, source.ContentSize, contentType, encoding, sha,
			result.Version, now, number, filename, resourceID, ownerID, source.SealedMeta); err != nil {
			return fmt.Errorf("restore version: %w: %w", ErrInternal, err)
		}
		dropped, err = trimHistoryTx(ctx, tx, ownerID, resourceID, limit.StorageBytes)
		return err
	})
	if err != nil {
		_ = d.blobs.Delete(ctx, key)
		return SaveResult{}, err
	}
	d.dropObjects(ctx, dropped)
	result.Trimmed = len(dropped)
	return result, nil
}

// CopyVersion starts a new resource from an earlier version: its own content,
// its own links, its own history. It is how two variants that have to live
// side by side are kept, rather than as branches of one resource.
func (d *Store) CopyVersion(ctx context.Context, ownerID, resourceID string, number int, name string) (Resource, error) {
	source, err := d.VersionForOwner(ctx, ownerID, resourceID, number)
	if err != nil {
		return Resource{}, err
	}
	resource, err := d.ResourceForOwner(ctx, ownerID, resourceID)
	if err != nil {
		return Resource{}, err
	}
	// A copy would be a new resource, free of the takedown: one click around
	// it. The owner can still edit or delete what was taken down.
	if resource.TakenDown {
		return Resource{}, ErrCopyTakenDown
	}
	// Its encryption is bound to this resource; a copy is a new resource,
	// encrypted anew in the browser.
	if resource.Sealed() {
		return Resource{}, errSealedCopy
	}
	body, err := d.ReadVersion(ctx, source)
	if err != nil {
		return Resource{}, fmt.Errorf("read version %d: %w: %w", number, ErrInternal, err)
	}
	return d.CreateResource(ctx, ownerID, name, RefitFilename(resource.Filename, source.Filename, body), body, source.ContentEncoding, "")
}

// DeleteVersion removes one earlier version and its object for good - for a
// secret that was saved by mistake, the one way to be rid of it short of
// deleting the resource. The current content is not a version and cannot be
// removed here.
func (d *Store) DeleteVersion(ctx context.Context, ownerID, resourceID string, number int) error {
	if !validUUIDs(ownerID, resourceID) || number <= 0 {
		return ErrNotFound
	}
	var key string
	err := d.db.QueryRow(ctx, `
DELETE FROM resource_versions v
USING resources r
WHERE v.resource_id = r.id AND v.resource_id = $1 AND r.owner_id = $2 AND v.version = $3
RETURNING v.content_key
`, resourceID, ownerID, number).Scan(&key)
	if err != nil {
		return translateNotFound(err)
	}
	if err := d.blobs.Delete(ctx, key); err != nil {
		return fmt.Errorf("delete version body: %w: %w", ErrInternal, err)
	}
	return nil
}

// ErrCopyTakenDown refuses a new resource made from one the operator took down.
var ErrCopyTakenDown = refusal("此资源已被下架，不能另存为新资源")

// LinkVersion is the version a live link last delivered.
type LinkVersion struct {
	LinkID   string
	LinkName string
	Version  int
}

// LastDelivered reports, for each live link of a resource, which version its
// most recent delivery carried. A link that has delivered nothing yet, or only
// before versions were recorded, is left out.
func (d *Store) LastDelivered(ctx context.Context, ownerID, resourceID string, now time.Time) ([]LinkVersion, error) {
	if !validUUIDs(ownerID, resourceID) {
		return nil, ErrNotFound
	}
	// One index probe per live link rather than a sort of the resource's
	// whole log: a machine polling every minute leaves a lot of rows.
	rows, err := d.db.Query(ctx, `
SELECT l.id::text, l.name, last.resource_version
FROM links l
JOIN resources r ON r.id = l.resource_id
CROSS JOIN LATERAL (
  SELECT a.resource_version FROM access_logs a
  WHERE a.link_id = l.id AND a.resource_version IS NOT NULL AND a.outcome = $4
  ORDER BY a.occurred_at DESC
  LIMIT 1
) last
WHERE l.resource_id = $1 AND r.owner_id = $2
  AND l.revoked_at IS NULL AND (l.expires_at IS NULL OR l.expires_at > $3)
  AND (l.max_uses = 0 OR l.used_count < l.max_uses)
ORDER BY l.created_at, l.id
`, resourceID, ownerID, now, OutcomeSuccess)
	if err != nil {
		return nil, fmt.Errorf("last delivered: %w: %w", ErrInternal, err)
	}
	links, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (LinkVersion, error) {
		var link LinkVersion
		err := row.Scan(&link.LinkID, &link.LinkName, &link.Version)
		return link, err
	})
	if err != nil {
		return nil, fmt.Errorf("last delivered: %w: %w", ErrInternal, err)
	}
	return links, nil
}

// pruneHistory drops versions replaced longer ago than HistoryRetention, rows
// first and objects after, in batches.
func (d *Store) pruneHistory(ctx context.Context, conn *pgxpool.Conn, now time.Time) (int64, error) {
	var removed int64
	for {
		if err := ctx.Err(); err != nil {
			return removed, nil
		}
		rows, err := conn.Query(ctx, `
DELETE FROM resource_versions
WHERE (resource_id, version) IN (
  SELECT resource_id, version FROM resource_versions
  WHERE replaced_at < $1 ORDER BY replaced_at LIMIT $2
)
RETURNING content_key
`, now.Add(-HistoryRetention), pruneBatch)
		if err != nil {
			return removed, err
		}
		keys, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return removed, err
		}
		for _, key := range keys {
			_ = d.blobs.Delete(ctx, key)
		}
		removed += int64(len(keys))
		if len(keys) < pruneBatch {
			return removed, nil
		}
	}
}
