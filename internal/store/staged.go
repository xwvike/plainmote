package store

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"plainmote/internal/blob"
)

const (
	// SealedHeaderBytes is the fixed encryption overhead added to sealed blobs
	// (4-byte magic + 12-byte IV + 16-byte GCM tag).
	SealedHeaderBytes = sealedHeaderBytes

	sniffPrefixMaxBytes      = 64 << 10
	committedStagedRetention = time.Hour
)

// UploadTicket binds a browser-uploaded staging object to the exact account,
// resource, purpose, size, R2-enforced SHA-256 and expiry authorised when the
// presigned URL was issued. It is encrypted and authenticated with tokenCipher.
type UploadTicket struct {
	fallbackContent []byte // validated server-uploaded bytes, never serialized
	UploadID        string `json:"u"`
	Purpose         string `json:"p"`
	OwnerID         string `json:"o"`
	ResourceID      string `json:"r"`
	StagingKey      string `json:"k"`
	Size            int64  `json:"s"`
	SHA256          string `json:"h"`
	ExpiresAt       int64  `json:"e"`
}

func (d *Store) directStore() (blob.DirectStore, bool) {
	ds, ok := d.blobs.(blob.DirectStore)
	return ds, ok
}

// CanPresign reports whether the underlying blob store supports direct browser
// presigned PUT uploads, HEAD verification, and server-side Copy.
func (d *Store) CanPresign() bool {
	_, ok := d.directStore()
	return ok
}

// DropBlob removes an unreferenced object if a direct upload cannot be committed.
func (d *Store) DropBlob(ctx context.Context, key string) {
	if strings.TrimSpace(key) != "" {
		d.dropObjects(ctx, []string{key})
	}
}

func validSHA256Hex(value string) (string, string, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != 64 {
		return "", "", false
	}
	raw, err := hex.DecodeString(value)
	if err != nil || len(raw) != 32 {
		return "", "", false
	}
	return value, base64.StdEncoding.EncodeToString(raw), true
}

// BeginStagedUpload atomically checks and reserves quota in staged_uploads
// under a dedicated staging/<id> key, then issues a presigned PUT URL that
// enforces both Content-Length and ChecksumSHA256 in object storage.
func (d *Store) BeginStagedUpload(ctx context.Context, ownerID, purpose, resourceID, excludeResourceID string, size int64, addResources int, sha256Hex string, urlTTL, ticketTTL time.Duration, now time.Time) (blob.PresignedPut, string, UploadTicket, error) {
	ds, ok := d.directStore()
	if !ok {
		return blob.PresignedPut{}, "", UploadTicket{}, errors.New("direct upload unsupported")
	}
	if !validUUIDs(ownerID, resourceID) || size <= 0 {
		return blob.PresignedPut{}, "", UploadTicket{}, refusal("上传参数无效")
	}
	hexDigest, b64Digest, ok := validSHA256Hex(sha256Hex)
	if !ok {
		return blob.PresignedPut{}, "", UploadTicket{}, refusal("缺少有效的内容校验摘要")
	}
	uploadID := uuid.NewString()
	stagingKey := "staging/" + uploadID
	expiresAt := now.Add(ticketTTL)

	err := d.withTx(ctx, func(tx pgx.Tx) error {
		limit, usage, err := quotaGate(ctx, tx, ownerID, excludeResourceID, now)
		if err != nil {
			return err
		}
		if addResources > 0 && usage.Resources+int64(addResources) > limit.Resources {
			return resourceQuotaError(limit.Resources, usage.Resources)
		}
		if usage.StorageBytes+size > limit.StorageBytes {
			return storageQuotaError(limit.StorageBytes, usage.StorageBytes, size)
		}
		_, err = tx.Exec(ctx, `
INSERT INTO staged_uploads(id, owner_id, purpose, resource_id, staging_key, size, sha256, created_at, expires_at)
VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9)
`, uploadID, ownerID, purpose, resourceID, stagingKey, size, hexDigest, now, expiresAt)
		if err != nil {
			return fmt.Errorf("reserve staged upload: %w: %w", ErrInternal, err)
		}
		return nil
	})
	if err != nil {
		return blob.PresignedPut{}, "", UploadTicket{}, err
	}

	presigned, err := ds.PresignPut(ctx, stagingKey, size, b64Digest, urlTTL)
	if err != nil {
		_, _ = d.db.Exec(ctx, `DELETE FROM staged_uploads WHERE id = $1`, uploadID)
		return blob.PresignedPut{}, "", UploadTicket{}, fmt.Errorf("presign upload: %w", err)
	}
	ticket := UploadTicket{
		UploadID:   uploadID,
		Purpose:    purpose,
		OwnerID:    ownerID,
		ResourceID: resourceID,
		StagingKey: stagingKey,
		Size:       size,
		SHA256:     hexDigest,
		ExpiresAt:  expiresAt.Unix(),
	}
	raw, err := json.Marshal(ticket)
	if err != nil {
		_, _ = d.db.Exec(ctx, `DELETE FROM staged_uploads WHERE id = $1`, uploadID)
		return blob.PresignedPut{}, "", UploadTicket{}, fmt.Errorf("encode upload ticket: %w", err)
	}
	sealed, err := d.cipher.seal(string(raw))
	if err != nil {
		_, _ = d.db.Exec(ctx, `DELETE FROM staged_uploads WHERE id = $1`, uploadID)
		return blob.PresignedPut{}, "", UploadTicket{}, fmt.Errorf("seal upload ticket: %w", err)
	}
	return presigned, base64.RawURLEncoding.EncodeToString(sealed), ticket, nil
}

// VerifyUploadTicket authenticates and decodes a ticket returned by BeginStagedUpload.
func (d *Store) VerifyUploadTicket(token string, now time.Time) (UploadTicket, error) {
	var ticket UploadTicket
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(token))
	if err != nil {
		return ticket, refusal("上传凭证无效")
	}
	plain, err := d.cipher.open(raw)
	if err != nil {
		return ticket, refusal("上传凭证无效")
	}
	if err := json.Unmarshal([]byte(plain), &ticket); err != nil {
		return ticket, refusal("上传凭证无效")
	}
	if !validUUIDs(ticket.UploadID, ticket.OwnerID, ticket.ResourceID) || ticket.Size <= 0 || !strings.HasPrefix(ticket.StagingKey, "staging/") {
		return UploadTicket{}, refusal("上传凭证无效")
	}
	// Note: allow already-committed tickets slightly past ExpiresAt to resolve
	// idempotently if their row is still retained; uncommitted expiry is checked
	// against staged_uploads.expires_at inside commit.
	return ticket, nil
}

// pruneStagedUploads transfers expired keys to the durable deletion queue in
// the same statement that retires their reservation. Recently committed rows
// remain available for retry after their staging key has been queued.
func (d *Store) pruneStagedUploads(ctx context.Context, conn *pgxpool.Conn, now time.Time) (int64, error) {
	var total int64
	for {
		var n int64
		err := conn.QueryRow(ctx, `
WITH due AS MATERIALIZED (
  SELECT id, committed_at IS NULL OR committed_at < $2 AS retire
  FROM staged_uploads
  WHERE expires_at <= $1
    AND (staging_cleaned_at IS NULL OR committed_at IS NULL OR committed_at < $2)
  ORDER BY expires_at, id LIMIT $3 FOR UPDATE
), retired AS (
  DELETE FROM staged_uploads s USING due
  WHERE s.id = due.id AND due.retire RETURNING s.staging_key
), swept AS (
  UPDATE staged_uploads s SET staging_cleaned_at = $1 FROM due
  WHERE s.id = due.id AND NOT due.retire RETURNING s.staging_key
), queued AS (
  INSERT INTO blob_garbage(key, since)
  SELECT staging_key, $1 FROM retired UNION ALL SELECT staging_key, $1 FROM swept
  ON CONFLICT (key) DO NOTHING
)
SELECT count(*) FROM due
`, now, now.Add(-committedStagedRetention), pruneBatch).Scan(&n)
		if err != nil {
			return total, err
		}
		total += n
		if n < pruneBatch {
			return total, nil
		}
	}
}

type stagedState struct {
	Committed  bool
	ResourceID string
	Version    int
	NewVersion bool
	Trimmed    int
	StagingKey string
	Size       int64
	SHA256     string
	ExpiresAt  time.Time
}

func readStagedTx(ctx context.Context, q storeQuerier, ticket UploadTicket, lock bool) (stagedState, error) {
	suffix := ""
	if lock {
		suffix = " FOR UPDATE"
	}
	var st stagedState
	var committedAt *time.Time
	var committedResID *string
	err := q.QueryRow(ctx, `
SELECT staging_key, size, sha256, expires_at, committed_at, committed_resource_id::text,
       committed_version, committed_new_version, committed_trimmed
FROM staged_uploads
WHERE id = $1 AND owner_id = $2 AND purpose = $3 AND resource_id = $4`+suffix,
		ticket.UploadID, ticket.OwnerID, ticket.Purpose, ticket.ResourceID).Scan(
		&st.StagingKey, &st.Size, &st.SHA256, &st.ExpiresAt, &committedAt, &committedResID,
		&st.Version, &st.NewVersion, &st.Trimmed)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return stagedState{}, refusal("上传凭证无效或已过期，请重试")
		}
		return stagedState{}, fmt.Errorf("read staged upload: %w: %w", ErrInternal, err)
	}
	if committedAt != nil {
		st.Committed = true
		if committedResID != nil {
			st.ResourceID = *committedResID
		}
	}
	return st, nil
}

func markStagedCommittedTx(ctx context.Context, tx pgx.Tx, uploadID, resourceID string, version int, newVersion bool, trimmed int, now time.Time) error {
	_, err := tx.Exec(ctx, `
UPDATE staged_uploads
SET committed_at = $2, committed_resource_id = $3, committed_version = $4,
    committed_new_version = $5, committed_trimmed = $6
WHERE id = $1
`, uploadID, now, resourceID, version, newVersion, trimmed)
	if err != nil {
		return fmt.Errorf("mark staged upload committed: %w: %w", ErrInternal, err)
	}
	return nil
}

// fallbackTicket resolves the original reservation for a server-side retry. The
// body must be exactly the bytes authorized for the direct PUT, including on
// replay; both transports then share one publication transaction and result.
func (d *Store) fallbackTicket(ctx context.Context, id, ownerID, purpose, target string, content []byte) (UploadTicket, error) {
	ticket := UploadTicket{UploadID: id, OwnerID: ownerID, Purpose: purpose}
	if !validUUIDs(id, ownerID) {
		return ticket, refusal("上传凭证无效")
	}
	err := d.db.QueryRow(ctx, `
SELECT resource_id, size, sha256 FROM staged_uploads
WHERE id = $1 AND owner_id = $2 AND purpose = $3
`, id, ownerID, purpose).Scan(&ticket.ResourceID, &ticket.Size, &ticket.SHA256)
	if errors.Is(err, pgx.ErrNoRows) {
		return ticket, refusal("上传凭证无效或已过期，请重试")
	}
	if err != nil {
		return ticket, err
	}
	if target != "" && target != ticket.ResourceID {
		return ticket, refusal("上传凭证无效")
	}
	if int64(len(content)) != ticket.Size || contentSHA256(content) != ticket.SHA256 {
		return ticket, refusal("上传内容与凭证不匹配")
	}
	ticket.fallbackContent = content
	return ticket, nil
}

// trimUTF8Tail removes an incomplete UTF-8 rune split at the end of a prefix
// sample so UTF-8 text is not misclassified as binary at a 64 KiB boundary.
func trimUTF8Tail(sample []byte, totalSize int64) []byte {
	if int64(len(sample)) >= totalSize || len(sample) == 0 {
		return sample
	}
	for i := 1; i <= 3 && i <= len(sample); i++ {
		b := sample[len(sample)-i]
		if b&0xC0 != 0x80 {
			if !utf8.FullRune(sample[len(sample)-i:]) {
				return sample[:len(sample)-i]
			}
			break
		}
	}
	return sample
}

// publishStagedObject verifies stagingKey via HEAD, reads up to prefixMax
// bytes for magic/type sniffing, and server-side copies stagingKey into an
// immutable server-owned finalKey conditioned on the verified ETag. Because
// browsers only ever receive presigned URLs for staging/* keys, they can never
// overwrite the published finalKey after commit.
func (d *Store) publishStagedObject(ctx context.Context, fallback []byte, stagingKey, finalKey string, expectedSize, prefixMax int64) ([]byte, error) {
	if fallback != nil {
		if err := d.blobs.Put(ctx, finalKey, bytes.NewReader(fallback), expectedSize); err != nil {
			return nil, fmt.Errorf("store fallback upload: %w: %w", ErrInternal, err)
		}
		return fallback[:min(len(fallback), int(prefixMax))], nil
	}
	ds, ok := d.directStore()
	if !ok {
		return nil, fmt.Errorf("direct upload unsupported: %w", ErrInternal)
	}
	info, err := ds.Head(ctx, stagingKey)
	if errors.Is(err, blob.ErrNotFound) {
		return nil, refusal("上传文件尚未就绪，请重试")
	}
	if err != nil {
		return nil, fmt.Errorf("head staged blob: %w: %w", ErrInternal, err)
	}
	if info.Size != expectedSize || info.Size <= 0 {
		return nil, refusal("上传文件大小不匹配，请重试")
	}
	var prefix []byte
	if prefixMax > 0 {
		end := info.Size - 1
		if end >= prefixMax {
			end = prefixMax - 1
		}
		body, _, err := d.blobs.OpenRange(ctx, stagingKey, 0, end)
		if err != nil {
			return nil, fmt.Errorf("read staged blob prefix: %w: %w", ErrInternal, err)
		}
		prefix, err = io.ReadAll(io.LimitReader(body, prefixMax))
		body.Close()
		if err != nil {
			return nil, fmt.Errorf("read staged blob prefix: %w: %w", ErrInternal, err)
		}
	}
	if err := ds.Copy(ctx, stagingKey, finalKey, info.ETag); err != nil {
		return nil, fmt.Errorf("publish staged blob: %w: %w", ErrInternal, err)
	}
	return prefix, nil
}

// CreateAnonymousPasteFromUpload commits an anonymous quick share whose body
// was uploaded to stagingKey. Retrying the same ticket is idempotent and writes
// paste_claims in the same transaction so the result page works immediately.
func (d *Store) CreateAnonymousPasteFromUpload(ctx context.Context, ticket UploadTicket, filename string, ttl time.Duration, now time.Time) (Resource, Link, error) {
	if ttl <= 0 {
		ttl = AnonymousDefaultTTL
	}
	if ttl < AnonymousMinTTL || ttl > AnonymousMaxTTL {
		return Resource{}, Link{}, errAnonymousTTL
	}
	if ticket.Size <= 0 {
		return Resource{}, Link{}, refusal("内容不能为空")
	}
	if ticket.Size > AnonymousMaxBytes {
		return Resource{}, Link{}, refusalf("内容最大 %s", BytesText(AnonymousMaxBytes))
	}
	filename = strings.TrimSpace(filename)
	if err := validateFilename(filename); err != nil {
		return Resource{}, Link{}, err
	}
	st, err := readStagedTx(ctx, d.db, ticket, false)
	if err != nil {
		return Resource{}, Link{}, err
	}
	if st.Committed {
		return d.AnonymousPasteResult(ctx, st.ResourceID, now)
	}
	if now.After(st.ExpiresAt) {
		return Resource{}, Link{}, refusal("上传凭证已过期，请重试")
	}
	finalKey := contentKey(ticket.ResourceID)
	prefix, err := d.publishStagedObject(ctx, ticket.fallbackContent, st.StagingKey, finalKey, st.Size, sniffPrefixMaxBytes)
	if err != nil {
		return Resource{}, Link{}, err
	}
	contentType, encoding := anonymousType(trimUTF8Tail(prefix, st.Size))
	resource := Resource{
		ID: ticket.ResourceID, OwnerID: AnonymousUserID, Name: filename, Filename: filename,
		ContentKey: finalKey, ContentSize: st.Size, ContentType: contentType, ContentEncoding: encoding,
		ContentSHA256: st.SHA256, CreatedAt: now, UpdatedAt: now, Version: 1, VersionAt: now,
	}
	link := Link{ResourceID: resource.ID, ExpiresAt: shareExpiry(now, ttl)}
	var alreadyCommitted bool
	var committedID string
	rolledBack, err := d.withTxStatus(ctx, func(tx pgx.Tx) error {
		limit, usage, err := quotaGateForUpload(ctx, tx, AnonymousUserID, "", ticket.UploadID, now)
		if err != nil {
			return err
		}
		lockedStaged, err := readStagedTx(ctx, tx, ticket, true)
		if err != nil {
			return err
		}
		if lockedStaged.Committed {
			alreadyCommitted = true
			committedID = lockedStaged.ResourceID
			return nil
		}
		if now.After(lockedStaged.ExpiresAt) {
			return refusal("上传凭证已过期，请重试")
		}
		if usage.Resources+1 > limit.Resources {
			return resourceQuotaError(limit.Resources, usage.Resources)
		}
		if usage.StorageBytes+resource.ContentSize > limit.StorageBytes {
			return storageQuotaError(limit.StorageBytes, usage.StorageBytes, resource.ContentSize)
		}
		tag, err := tx.Exec(ctx, `
INSERT INTO resources(id, owner_id, name, filename, content_key, content_size, content_type, content_encoding,
                      created_at, updated_at, version, version_at, content_sha256)
VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9, $9, 1, $9, $10)
ON CONFLICT (id) DO NOTHING
`, resource.ID, AnonymousUserID, resource.Name, resource.Filename, resource.ContentKey,
			resource.ContentSize, resource.ContentType, resource.ContentEncoding, now, resource.ContentSHA256)
		if err != nil {
			return fmt.Errorf("create anonymous paste: %w: %w", ErrInternal, err)
		}
		if tag.RowsAffected() == 0 {
			return refusal("上传凭证已使用")
		}
		if err := d.insertLink(ctx, tx, &link, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO paste_claims(resource_id, user_id, created_at)
VALUES($1, $2, $3)
`, resource.ID, AnonymousUserID, now); err != nil {
			return fmt.Errorf("create paste claim: %w: %w", ErrInternal, err)
		}
		return markStagedCommittedTx(ctx, tx, ticket.UploadID, resource.ID, 1, true, 0, now)
	})
	if err != nil {
		d.dropPublishedOnError(ctx, AnonymousUserID, rolledBack, finalKey)
		return Resource{}, Link{}, err
	}
	if alreadyCommitted {
		d.DropBlob(ctx, finalKey)
		return d.AnonymousPasteResult(ctx, committedID, now)
	}
	d.DropBlob(ctx, st.StagingKey)
	return resource, link, nil
}

// CreateQuickShareFromUpload commits a signed-in quick share whose body was
// uploaded to stagingKey.
func (d *Store) CreateQuickShareFromUpload(ctx context.Context, ownerID string, ticket UploadTicket, filename string, ttl time.Duration, now time.Time) (Resource, Link, error) {
	if !validUUIDs(ownerID) || ownerID == AnonymousUserID {
		return Resource{}, Link{}, ErrNotFound
	}
	ttl, err := quickShareTTL(ttl)
	if err != nil {
		return Resource{}, Link{}, err
	}
	if ticket.Size <= 0 {
		return Resource{}, Link{}, refusal("内容不能为空")
	}
	if ticket.Size > AnonymousMaxBytes {
		return Resource{}, Link{}, refusalf("内容最大 %s", BytesText(AnonymousMaxBytes))
	}
	filename = strings.TrimSpace(filename)
	if err := validateFilename(filename); err != nil {
		return Resource{}, Link{}, err
	}
	st, err := readStagedTx(ctx, d.db, ticket, false)
	if err != nil {
		return Resource{}, Link{}, err
	}
	if st.Committed {
		return d.QuickShareLink(ctx, ownerID, st.ResourceID, now)
	}
	if now.After(st.ExpiresAt) {
		return Resource{}, Link{}, refusal("上传凭证已过期，请重试")
	}
	finalKey := contentKey(ticket.ResourceID)
	prefix, err := d.publishStagedObject(ctx, ticket.fallbackContent, st.StagingKey, finalKey, st.Size, sniffPrefixMaxBytes)
	if err != nil {
		return Resource{}, Link{}, err
	}
	contentType, encoding := anonymousType(trimUTF8Tail(prefix, st.Size))
	resource := Resource{
		ID: ticket.ResourceID, Name: filename, Filename: filename, ContentKey: finalKey, ContentSize: st.Size,
		ContentType: contentType, ContentEncoding: encoding, ContentSHA256: st.SHA256,
	}
	return d.insertQuickShareFromUpload(ctx, ownerID, ticket, st.StagingKey, resource, Link{}, ttl, now)
}

// CreateSealedQuickShareFromUpload commits an encrypted quick share whose
// ciphertext was uploaded to stagingKey.
func (d *Store) CreateSealedQuickShareFromUpload(ctx context.Context, ownerID string, ticket UploadTicket, linkID string, meta, sealedKey, linkKey, ownerKey []byte, ttl time.Duration, now time.Time) (Resource, Link, error) {
	if !validUUIDs(ownerID) || ownerID == AnonymousUserID {
		return Resource{}, Link{}, ErrNotFound
	}
	if !validUUIDs(linkID) || !validSealedMeta(meta) || len(sealedKey) != sealedKeyBytes || !validLinkKeys(linkKey, ownerKey) {
		return Resource{}, Link{}, errSealedShape
	}
	ttl, err := quickShareTTL(ttl)
	if err != nil {
		return Resource{}, Link{}, err
	}
	if ticket.Size <= sealedHeaderBytes || ticket.Size > AnonymousMaxBytes+sealedHeaderBytes {
		return Resource{}, Link{}, refusalf("内容最大 %s", BytesText(AnonymousMaxBytes))
	}
	st, err := readStagedTx(ctx, d.db, ticket, false)
	if err != nil {
		return Resource{}, Link{}, err
	}
	if st.Committed {
		return d.QuickShareLink(ctx, ownerID, st.ResourceID, now)
	}
	if now.After(st.ExpiresAt) {
		return Resource{}, Link{}, refusal("上传凭证已过期，请重试")
	}
	finalKey := contentKey(ticket.ResourceID)
	prefix, err := d.publishStagedObject(ctx, ticket.fallbackContent, st.StagingKey, finalKey, st.Size, int64(len(sealedContentMagic)))
	if err != nil {
		return Resource{}, Link{}, err
	}
	if !bytes.HasPrefix(prefix, sealedContentMagic) {
		d.DropBlob(ctx, finalKey)
		return Resource{}, Link{}, errSealedShape
	}
	resource := Resource{
		ID: ticket.ResourceID, ContentKey: finalKey, ContentSize: st.Size, ContentType: SealedContentType,
		ContentSHA256: st.SHA256, SealedKey: sealedKey, SealedMeta: meta,
	}
	return d.insertQuickShareFromUpload(ctx, ownerID, ticket, st.StagingKey, resource, Link{ID: linkID, SealedKey: linkKey, OwnerKey: ownerKey}, ttl, now)
}

func (d *Store) insertQuickShareFromUpload(ctx context.Context, ownerID string, ticket UploadTicket, stagingKey string, resource Resource, link Link, ttl time.Duration, now time.Time) (Resource, Link, error) {
	link.ExpiresAt = shareExpiry(now, ttl)
	resource.OwnerID, resource.ExpiresAt = ownerID, link.ExpiresAt
	resource.CreatedAt, resource.UpdatedAt, resource.Version, resource.VersionAt = now, now, 1, now
	link.ResourceID = resource.ID

	var dropped []string
	var alreadyCommitted bool
	var committedID string
	rolledBack, err := d.withTxStatus(ctx, func(tx pgx.Tx) error {
		limit, usage, err := quotaGateForUpload(ctx, tx, ownerID, "", ticket.UploadID, now)
		if err != nil {
			return err
		}
		lockedStaged, err := readStagedTx(ctx, tx, ticket, true)
		if err != nil {
			return err
		}
		if lockedStaged.Committed {
			alreadyCommitted = true
			committedID = lockedStaged.ResourceID
			return nil
		}
		if now.After(lockedStaged.ExpiresAt) {
			return refusal("上传凭证已过期，请重试")
		}
		if usage.Resources+1 > limit.Resources {
			return resourceQuotaError(limit.Resources, usage.Resources)
		}
		if usage.StorageBytes+resource.ContentSize > limit.StorageBytes {
			return storageQuotaError(limit.StorageBytes, usage.StorageBytes, resource.ContentSize)
		}
		if eff := effectiveResourceFilename(resource.Name, resource.Filename); eff != "" {
			next, err := uniqueSiblingFilenameTx(ctx, tx, ownerID, "", "", eff)
			if err != nil {
				return err
			}
			if next != eff {
				if resource.Filename != "" {
					if resource.Name == resource.Filename {
						resource.Name = next
					}
					resource.Filename = next
				} else {
					resource.Name = next
				}
			}
		}
		tag, err := tx.Exec(ctx, `
INSERT INTO resources(id, owner_id, name, filename, content_key, content_size, content_type, content_encoding,
                      created_at, updated_at, version, version_at, content_sha256, expires_at, sealed_key, sealed_meta)
VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9, $9, 1, $9, $10, $11, $12, $13)
ON CONFLICT (id) DO NOTHING
`, resource.ID, ownerID, resource.Name, resource.Filename, resource.ContentKey,
			resource.ContentSize, resource.ContentType, resource.ContentEncoding, now, resource.ContentSHA256, resource.ExpiresAt,
			resource.SealedKey, resource.SealedMeta)
		if err != nil {
			return fmt.Errorf("create quick share: %w: %w", ErrInternal, err)
		}
		if tag.RowsAffected() == 0 {
			return errSealedShape
		}
		if err := d.insertLink(ctx, tx, &link, now); err != nil {
			if isUniqueViolation(err) {
				return errSealedShape
			}
			return err
		}
		if err := markStagedCommittedTx(ctx, tx, ticket.UploadID, resource.ID, 1, true, 0, now); err != nil {
			return err
		}
		dropped, err = trimHistoryTx(ctx, tx, ownerID, "", limit.StorageBytes)
		return err
	})
	if err != nil {
		d.dropPublishedOnError(ctx, ownerID, rolledBack, resource.ContentKey)
		return Resource{}, Link{}, err
	}
	if alreadyCommitted {
		d.DropBlob(ctx, resource.ContentKey)
		return d.QuickShareLink(ctx, ownerID, committedID, now)
	}
	d.DropBlob(ctx, stagingKey)
	d.dropObjects(ctx, dropped)
	return resource, link, nil
}

// CreateResourceFromUpload commits a new plain resource in the root folder.
func (d *Store) CreateResourceFromUpload(ctx context.Context, ownerID string, ticket UploadTicket, name, filename, uploadedAs, encodingHint string) (Resource, error) {
	return d.CreateResourceFromUploadInFolder(ctx, ownerID, "", ticket, name, filename, uploadedAs, encodingHint)
}

// CreateResourceFromUploadInFolder commits a new plain resource whose body was
// uploaded to stagingKey directly into folderID ("" = root).
func (d *Store) CreateResourceFromUploadInFolder(ctx context.Context, ownerID, folderID string, ticket UploadTicket, name, filename, uploadedAs, encodingHint string) (Resource, error) {
	if !validUUIDs(ownerID) || ownerID == AnonymousUserID {
		return Resource{}, ErrNotFound
	}
	if folderID != "" && !validUUIDs(folderID) {
		return Resource{}, ErrNotFound
	}
	now := time.Now().UTC()
	st, err := readStagedTx(ctx, d.db, ticket, false)
	if err != nil {
		return Resource{}, err
	}
	if st.Committed {
		return d.ResourceForOwner(ctx, ownerID, st.ResourceID)
	}
	if now.After(st.ExpiresAt) {
		return Resource{}, refusal("上传凭证已过期，请重试")
	}
	finalKey := contentKey(ticket.ResourceID)
	prefix, err := d.publishStagedObject(ctx, ticket.fallbackContent, st.StagingKey, finalKey, st.Size, sniffPrefixMaxBytes)
	if err != nil {
		return Resource{}, err
	}
	sample := trimUTF8Tail(prefix, st.Size)
	filename = RefitFilename(strings.TrimSpace(filename), uploadedAs, sample)
	if err := validateFilename(filename); err != nil {
		d.DropBlob(ctx, finalKey)
		return Resource{}, err
	}
	contentType, encoding := DetectContent(filename, sample, encodingHint)
	resource := Resource{
		ID: ticket.ResourceID, OwnerID: ownerID, Name: resourceName(name, filename), Filename: filename,
		ContentKey: finalKey, ContentSize: st.Size, ContentType: contentType, ContentEncoding: encoding,
		ContentSHA256: st.SHA256, CreatedAt: now, UpdatedAt: now, Version: 1, VersionAt: now,
	}
	var dropped []string
	var alreadyCommitted bool
	var committedID string
	rolledBack, err := d.withTxStatus(ctx, func(tx pgx.Tx) error {
		limit, usage, err := quotaGateForUpload(ctx, tx, ownerID, "", ticket.UploadID, now)
		if err != nil {
			return err
		}
		lockedStaged, err := readStagedTx(ctx, tx, ticket, true)
		if err != nil {
			return err
		}
		if lockedStaged.Committed {
			alreadyCommitted = true
			committedID = lockedStaged.ResourceID
			return nil
		}
		if now.After(lockedStaged.ExpiresAt) {
			return refusal("上传凭证已过期，请重试")
		}
		if usage.Resources+1 > limit.Resources {
			return resourceQuotaError(limit.Resources, usage.Resources)
		}
		if usage.StorageBytes+resource.ContentSize > limit.StorageBytes {
			return storageQuotaError(limit.StorageBytes, usage.StorageBytes, resource.ContentSize)
		}
		if folderID != "" {
			var ok string
			if err := tx.QueryRow(ctx, `SELECT id FROM folders WHERE id = $1 AND owner_id = $2`, folderID, ownerID).Scan(&ok); err != nil {
				return translateNotFound(err)
			}
		}
		if eff := effectiveResourceFilename(resource.Name, resource.Filename); eff != "" {
			next, err := uniqueSiblingFilenameTx(ctx, tx, ownerID, folderID, "", eff)
			if err != nil {
				return err
			}
			if next != eff {
				if resource.Filename != "" {
					if resource.Name == resource.Filename {
						resource.Name = next
					}
					resource.Filename = next
				} else {
					resource.Name = next
				}
			}
		}
		resource.FolderID = folderID
		tag, err := tx.Exec(ctx, `
INSERT INTO resources(id, owner_id, name, filename, content_key, content_size, content_type, content_encoding,
                      created_at, updated_at, version, version_at, content_sha256, folder_id)
VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9, $9, 1, $9, $10, NULLIF($11, '')::uuid)
ON CONFLICT (id) DO NOTHING
`, resource.ID, ownerID, resource.Name, resource.Filename, resource.ContentKey,
			resource.ContentSize, resource.ContentType, resource.ContentEncoding, now, resource.ContentSHA256, folderID)
		if err != nil {
			return fmt.Errorf("create resource: %w: %w", ErrInternal, err)
		}
		if tag.RowsAffected() == 0 {
			return refusal("上传凭证已使用")
		}
		if err := markStagedCommittedTx(ctx, tx, ticket.UploadID, resource.ID, 1, true, 0, now); err != nil {
			return err
		}
		dropped, err = trimHistoryTx(ctx, tx, ownerID, "", limit.StorageBytes)
		return err
	})
	if err != nil {
		d.dropPublishedOnError(ctx, ownerID, rolledBack, finalKey)
		return Resource{}, err
	}
	if alreadyCommitted {
		d.DropBlob(ctx, finalKey)
		return d.ResourceForOwner(ctx, ownerID, committedID)
	}
	d.DropBlob(ctx, st.StagingKey)
	d.dropObjects(ctx, dropped)
	return resource, nil
}

// SaveResourceFromUpload updates an existing plain resource with a new version
// whose body was uploaded to stagingKey.
func (d *Store) SaveResourceFromUpload(ctx context.Context, ownerID, resourceID string, ticket UploadTicket, name, filename, uploadedAs, encodingHint string, baseVersion int) (SaveResult, error) {
	now := time.Now().UTC()
	st, err := readStagedTx(ctx, d.db, ticket, false)
	if err != nil {
		return SaveResult{}, err
	}
	if st.Committed {
		return SaveResult{Version: st.Version, NewVersion: st.NewVersion, Trimmed: st.Trimmed}, nil
	}
	if now.After(st.ExpiresAt) {
		return SaveResult{}, refusal("上传凭证已过期，请重试")
	}
	current, err := d.ResourceForOwner(ctx, ownerID, resourceID)
	if err != nil {
		return SaveResult{}, err
	}
	if err := refuseQuickShare(current); err != nil {
		return SaveResult{}, err
	}
	if current.Sealed() {
		return SaveResult{}, errSealedInBrowser
	}
	finalKey := contentKey(resourceID)
	prefix, err := d.publishStagedObject(ctx, ticket.fallbackContent, st.StagingKey, finalKey, st.Size, sniffPrefixMaxBytes)
	if err != nil {
		return SaveResult{}, err
	}
	sample := trimUTF8Tail(prefix, st.Size)
	filename = RefitFilename(strings.TrimSpace(filename), uploadedAs, sample)
	if err := validateFilename(filename); err != nil {
		d.DropBlob(ctx, finalKey)
		return SaveResult{}, err
	}
	contentType, encoding := DetectContent(filename, sample, encodingHint)
	nextName := resourceName(name, filename)
	nextSHA := st.SHA256

	var result SaveResult
	var dropped []string
	var alreadyCommitted bool
	var replaceContent bool
	rolledBack, err := d.withTxStatus(ctx, func(tx pgx.Tx) error {
		limit, usage, err := quotaGateForUpload(ctx, tx, ownerID, resourceID, ticket.UploadID, now)
		if err != nil {
			return err
		}
		lockedStaged, err := readStagedTx(ctx, tx, ticket, true)
		if err != nil {
			return err
		}
		if lockedStaged.Committed {
			alreadyCommitted = true
			result = SaveResult{Version: lockedStaged.Version, NewVersion: lockedStaged.NewVersion, Trimmed: lockedStaged.Trimmed}
			return nil
		}
		if now.After(lockedStaged.ExpiresAt) {
			return refusal("上传凭证已过期，请重试")
		}
		locked, err := lockContentTx(ctx, tx, ownerID, resourceID)
		if err != nil {
			return err
		}
		// Re-check inside the resource row lock so a concurrent SealResource or
		// Keep/QuickShare transition cannot allow plaintext to overwrite a sealed resource.
		if locked.QuickShare {
			return errQuickShareReadOnly
		}
		if locked.Sealed {
			return errSealedInBrowser
		}
		oldEff := effectiveResourceFilename(locked.Name, locked.Filename)
		newEff := effectiveResourceFilename(nextName, filename)
		if newEff != "" && !strings.EqualFold(newEff, oldEff) {
			exists, err := hasSiblingFilenameTx(ctx, tx, ownerID, locked.FolderID, resourceID, newEff)
			if err != nil {
				return err
			}
			if exists {
				return refusal("当前文件夹已存在同名文件")
			}
		}
		if baseVersion > 0 && locked.Version != baseVersion && (nextSHA == "" || nextSHA != locked.SHA256) {
			return &VersionConflict{Current: locked.Version, At: locked.VersionAt}
		}
		replaceContent = !(nextSHA != "" && locked.SHA256 != "" && nextSHA == locked.SHA256 && encoding == locked.Encoding)
		next := locked
		next.Type, next.Encoding = contentType, encoding
		if replaceContent {
			if err := retireTx(ctx, tx, resourceID, locked, now); err != nil {
				return err
			}
			next.Version = locked.Version + 1
			next.Key, next.Size, next.SHA256 = finalKey, st.Size, nextSHA
			next.VersionAt, next.RestoredFrom = now, 0
			result.NewVersion = true
		}
		if usage.StorageBytes+next.Size > limit.StorageBytes {
			return storageQuotaError(limit.StorageBytes, usage.StorageBytes, next.Size)
		}
		tag, err := tx.Exec(ctx, `
UPDATE resources
SET name = $1, filename = $2, content_key = $3, content_size = $4, content_type = $5, content_encoding = $6,
    updated_at = $7, version = $8, version_at = $9, restored_from = NULLIF($10, 0), content_sha256 = $11
WHERE id = $12 AND owner_id = $13
`, nextName, filename, next.Key, next.Size, next.Type, next.Encoding,
			now, next.Version, next.VersionAt, next.RestoredFrom, next.SHA256, resourceID, ownerID)
		if err != nil {
			return fmt.Errorf("update resource: %w: %w", ErrInternal, err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		result.Version = next.Version
		trimmed, err := trimHistoryTx(ctx, tx, ownerID, resourceID, limit.StorageBytes)
		if err != nil {
			return err
		}
		result.Trimmed = len(trimmed)
		dropped = trimmed
		return markStagedCommittedTx(ctx, tx, ticket.UploadID, resourceID, result.Version, result.NewVersion, result.Trimmed, now)
	})
	if err != nil {
		d.dropPublishedOnError(ctx, ownerID, rolledBack, finalKey)
		return SaveResult{}, err
	}
	if alreadyCommitted {
		d.DropBlob(ctx, finalKey)
		return result, nil
	}
	if !replaceContent {
		d.DropBlob(ctx, finalKey)
	}
	d.DropBlob(ctx, st.StagingKey)
	d.dropObjects(ctx, dropped)
	return result, nil
}

// CreateSealedResourceFromUpload commits a new encrypted resource whose
// ciphertext was uploaded to stagingKey.
func (d *Store) CreateSealedResourceFromUpload(ctx context.Context, ownerID string, ticket UploadTicket, meta, sealedKey []byte) (Resource, error) {
	if !validUUIDs(ownerID) || ownerID == AnonymousUserID {
		return Resource{}, ErrNotFound
	}
	if !validSealedMeta(meta) || len(sealedKey) != sealedKeyBytes || ticket.Size <= sealedHeaderBytes {
		return Resource{}, errSealedShape
	}
	now := time.Now().UTC()
	st, err := readStagedTx(ctx, d.db, ticket, false)
	if err != nil {
		return Resource{}, err
	}
	if st.Committed {
		return d.ResourceForOwner(ctx, ownerID, st.ResourceID)
	}
	if now.After(st.ExpiresAt) {
		return Resource{}, refusal("上传凭证已过期，请重试")
	}
	finalKey := contentKey(ticket.ResourceID)
	prefix, err := d.publishStagedObject(ctx, ticket.fallbackContent, st.StagingKey, finalKey, st.Size, int64(len(sealedContentMagic)))
	if err != nil {
		return Resource{}, err
	}
	if !bytes.HasPrefix(prefix, sealedContentMagic) {
		d.DropBlob(ctx, finalKey)
		return Resource{}, errSealedShape
	}
	resource := Resource{
		ID: ticket.ResourceID, OwnerID: ownerID, ContentType: SealedContentType, ContentKey: finalKey,
		ContentSize: st.Size, CreatedAt: now, UpdatedAt: now, Version: 1, VersionAt: now,
		ContentSHA256: st.SHA256, SealedKey: sealedKey, SealedMeta: meta,
	}
	var dropped []string
	var alreadyCommitted bool
	var committedID string
	rolledBack, err := d.withTxStatus(ctx, func(tx pgx.Tx) error {
		limit, usage, err := quotaGateForUpload(ctx, tx, ownerID, "", ticket.UploadID, now)
		if err != nil {
			return err
		}
		lockedStaged, err := readStagedTx(ctx, tx, ticket, true)
		if err != nil {
			return err
		}
		if lockedStaged.Committed {
			alreadyCommitted = true
			committedID = lockedStaged.ResourceID
			return nil
		}
		if now.After(lockedStaged.ExpiresAt) {
			return refusal("上传凭证已过期，请重试")
		}
		if usage.Resources+1 > limit.Resources {
			return resourceQuotaError(limit.Resources, usage.Resources)
		}
		if usage.StorageBytes+resource.ContentSize > limit.StorageBytes {
			return storageQuotaError(limit.StorageBytes, usage.StorageBytes, resource.ContentSize)
		}
		tag, err := tx.Exec(ctx, `
INSERT INTO resources(id, owner_id, name, filename, content_key, content_size, content_type, content_encoding,
                      created_at, updated_at, version, version_at, content_sha256, sealed_key, sealed_meta)
VALUES($1, $2, '', '', $3, $4, $5, '', $6, $6, 1, $6, $7, $8, $9)
ON CONFLICT (id) DO NOTHING
`, ticket.ResourceID, ownerID, resource.ContentKey, resource.ContentSize, SealedContentType, now, resource.ContentSHA256, sealedKey, meta)
		if err != nil {
			return fmt.Errorf("create sealed resource: %w: %w", ErrInternal, err)
		}
		if tag.RowsAffected() == 0 {
			return errSealedShape
		}
		if err := markStagedCommittedTx(ctx, tx, ticket.UploadID, resource.ID, 1, true, 0, now); err != nil {
			return err
		}
		dropped, err = trimHistoryTx(ctx, tx, ownerID, "", limit.StorageBytes)
		return err
	})
	if err != nil {
		d.dropPublishedOnError(ctx, ownerID, rolledBack, finalKey)
		return Resource{}, err
	}
	if alreadyCommitted {
		d.DropBlob(ctx, finalKey)
		return d.ResourceForOwner(ctx, ownerID, committedID)
	}
	d.DropBlob(ctx, st.StagingKey)
	d.dropObjects(ctx, dropped)
	return resource, nil
}

// SaveSealedResourceFromUpload commits a new version of an encrypted resource
// whose ciphertext was uploaded to stagingKey.
func (d *Store) SaveSealedResourceFromUpload(ctx context.Context, ownerID, resourceID string, baseVersion int, ticket UploadTicket, meta []byte) (SaveResult, error) {
	if !validSealedMeta(meta) || ticket.Size <= sealedHeaderBytes {
		return SaveResult{}, errSealedShape
	}
	now := time.Now().UTC()
	st, err := readStagedTx(ctx, d.db, ticket, false)
	if err != nil {
		return SaveResult{}, err
	}
	if st.Committed {
		return SaveResult{Version: st.Version, NewVersion: st.NewVersion, Trimmed: st.Trimmed}, nil
	}
	if now.After(st.ExpiresAt) {
		return SaveResult{}, refusal("上传凭证已过期，请重试")
	}
	current, err := d.ResourceForOwner(ctx, ownerID, resourceID)
	if err != nil {
		return SaveResult{}, err
	}
	if !current.Sealed() {
		return SaveResult{}, errSealedShape
	}
	if err := refuseQuickShare(current); err != nil {
		return SaveResult{}, err
	}
	finalKey := contentKey(resourceID)
	prefix, err := d.publishStagedObject(ctx, ticket.fallbackContent, st.StagingKey, finalKey, st.Size, int64(len(sealedContentMagic)))
	if err != nil {
		return SaveResult{}, err
	}
	if !bytes.HasPrefix(prefix, sealedContentMagic) {
		d.DropBlob(ctx, finalKey)
		return SaveResult{}, errSealedShape
	}
	var result SaveResult
	var dropped []string
	var alreadyCommitted bool
	rolledBack, err := d.withTxStatus(ctx, func(tx pgx.Tx) error {
		limit, usage, err := quotaGateForUpload(ctx, tx, ownerID, resourceID, ticket.UploadID, now)
		if err != nil {
			return err
		}
		lockedStaged, err := readStagedTx(ctx, tx, ticket, true)
		if err != nil {
			return err
		}
		if lockedStaged.Committed {
			alreadyCommitted = true
			result = SaveResult{Version: lockedStaged.Version, NewVersion: lockedStaged.NewVersion, Trimmed: lockedStaged.Trimmed}
			return nil
		}
		if now.After(lockedStaged.ExpiresAt) {
			return refusal("上传凭证已过期，请重试")
		}
		locked, err := lockContentTx(ctx, tx, ownerID, resourceID)
		if err != nil {
			return err
		}
		if !locked.Sealed {
			return errSealedShape
		}
		if locked.QuickShare {
			return errQuickShareReadOnly
		}
		if baseVersion > 0 && locked.Version != baseVersion {
			return &VersionConflict{Current: locked.Version, At: locked.VersionAt}
		}
		if err := retireTx(ctx, tx, resourceID, locked, now); err != nil {
			return err
		}
		next := locked
		next.Version, next.VersionAt, next.RestoredFrom = locked.Version+1, now, 0
		next.Key, next.Size, next.SHA256 = finalKey, st.Size, st.SHA256
		result.NewVersion = true
		if usage.StorageBytes+next.Size > limit.StorageBytes {
			return storageQuotaError(limit.StorageBytes, usage.StorageBytes, next.Size)
		}
		if _, err := tx.Exec(ctx, `
UPDATE resources
SET content_key = $1, content_size = $2, content_sha256 = $3, version = $4, version_at = $5,
    restored_from = NULLIF($6, 0), sealed_meta = $7, updated_at = $8
WHERE id = $9 AND owner_id = $10
`, next.Key, next.Size, next.SHA256, next.Version, next.VersionAt, next.RestoredFrom, meta, now, resourceID, ownerID); err != nil {
			return fmt.Errorf("save sealed resource: %w: %w", ErrInternal, err)
		}
		result.Version = next.Version
		trimmed, err := trimHistoryTx(ctx, tx, ownerID, resourceID, limit.StorageBytes)
		if err != nil {
			return err
		}
		result.Trimmed = len(trimmed)
		dropped = trimmed
		return markStagedCommittedTx(ctx, tx, ticket.UploadID, resourceID, result.Version, result.NewVersion, result.Trimmed, now)
	})
	if err != nil {
		d.dropPublishedOnError(ctx, ownerID, rolledBack, finalKey)
		return SaveResult{}, err
	}
	if alreadyCommitted {
		d.DropBlob(ctx, finalKey)
		return result, nil
	}
	d.DropBlob(ctx, st.StagingKey)
	d.dropObjects(ctx, dropped)
	return result, nil
}
