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

	"github.com/jackc/pgx/v5"

	"plainmote/internal/blob"
)

const (
	// SealedHeaderBytes is the fixed encryption overhead added to sealed blobs
	// (4-byte magic + 12-byte IV + 16-byte GCM tag).
	SealedHeaderBytes = sealedHeaderBytes

	sniffPrefixMaxBytes = 64 << 10
)

// UploadTicket binds a browser-uploaded object key to the exact account,
// resource, purpose, size and expiry authorised when the presigned URL was
// issued. It is encrypted and authenticated with the store's tokenCipher so
// callers cannot forge or swap keys across accounts.
type UploadTicket struct {
	Purpose    string `json:"p"`
	OwnerID    string `json:"o"`
	ResourceID string `json:"r"`
	Key        string `json:"k"`
	Size       int64  `json:"s"`
	ExpiresAt  int64  `json:"e"`
}

func (d *Store) directStore() (blob.DirectStore, bool) {
	ds, ok := d.blobs.(blob.DirectStore)
	return ds, ok
}

// CanPresign reports whether the underlying blob store supports direct browser
// presigned PUT uploads and HEAD verification.
func (d *Store) CanPresign() bool {
	_, ok := d.directStore()
	return ok
}

// NewUploadKey generates a fresh object storage key under resourceID.
func (d *Store) NewUploadKey(resourceID string) string {
	return contentKey(resourceID)
}

// DropBlob removes an unreferenced object if a direct upload cannot be committed.
func (d *Store) DropBlob(ctx context.Context, key string) {
	if strings.TrimSpace(key) != "" {
		d.dropObjects(ctx, []string{key})
	}
}

// PresignUpload issues a short-lived presigned PUT URL for ticket.Key and a
// sealed ticket token that commit handlers must verify before referencing the
// object in PostgreSQL.
func (d *Store) PresignUpload(ctx context.Context, ticket UploadTicket, ttl time.Duration) (string, string, error) {
	ds, ok := d.directStore()
	if !ok {
		return "", "", errors.New("direct upload unsupported")
	}
	if ticket.Size <= 0 || strings.TrimSpace(ticket.Key) == "" {
		return "", "", refusal("上传参数无效")
	}
	raw, err := json.Marshal(ticket)
	if err != nil {
		return "", "", fmt.Errorf("encode upload ticket: %w", err)
	}
	sealed, err := d.cipher.seal(string(raw))
	if err != nil {
		return "", "", fmt.Errorf("seal upload ticket: %w", err)
	}
	url, err := ds.PresignPut(ctx, ticket.Key, ticket.Size, ttl)
	if err != nil {
		return "", "", fmt.Errorf("presign upload: %w", err)
	}
	return url, base64.RawURLEncoding.EncodeToString(sealed), nil
}

// VerifyUploadTicket authenticates and decodes a ticket returned by PresignUpload.
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
	if ticket.Size <= 0 || strings.TrimSpace(ticket.Key) == "" || now.Unix() > ticket.ExpiresAt {
		return UploadTicket{}, refusal("上传凭证已过期，请重试")
	}
	return ticket, nil
}

// CheckQuotaForUpload performs a read-only quota check before issuing a
// presigned URL so over-quota uploads fail immediately before transferring bytes.
func (d *Store) CheckQuotaForUpload(ctx context.Context, ownerID, excludeResourceID string, addBytes int64, addResources int, now time.Time) error {
	if ownerID == "" || ownerID == AnonymousUserID {
		return nil
	}
	return d.withTx(ctx, func(tx pgx.Tx) error {
		limit, usage, err := quotaGate(ctx, tx, ownerID, excludeResourceID, now)
		if err != nil {
			return err
		}
		if addResources > 0 && usage.Resources+int64(addResources) > limit.Resources {
			return resourceQuotaError(limit.Resources, usage.Resources)
		}
		if usage.StorageBytes+addBytes > limit.StorageBytes {
			return storageQuotaError(limit.StorageBytes, usage.StorageBytes, addBytes)
		}
		return nil
	})
}

func validSHA256Hex(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != 64 {
		return ""
	}
	if _, err := hex.DecodeString(value); err != nil {
		return ""
	}
	return value
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

// verifyStagedObject checks via HEAD that the uploaded object exists and has
// the exact signed byte length, then reads at most prefixMax bytes for sniffing
// or magic verification without downloading the whole body back through the server.
func (d *Store) verifyStagedObject(ctx context.Context, key string, expectedSize, prefixMax int64) ([]byte, error) {
	ds, ok := d.directStore()
	if !ok {
		return nil, fmt.Errorf("direct upload unsupported: %w", ErrInternal)
	}
	actualSize, err := ds.Head(ctx, key)
	if errors.Is(err, blob.ErrNotFound) {
		return nil, refusal("上传文件尚未就绪，请重试")
	}
	if err != nil {
		return nil, fmt.Errorf("head staged blob: %w: %w", ErrInternal, err)
	}
	if actualSize != expectedSize || actualSize <= 0 {
		return nil, refusal("上传文件大小不匹配，请重试")
	}
	if prefixMax <= 0 {
		return nil, nil
	}
	end := actualSize - 1
	if end >= prefixMax {
		end = prefixMax - 1
	}
	body, _, err := d.blobs.OpenRange(ctx, key, 0, end)
	if err != nil {
		return nil, fmt.Errorf("read staged blob prefix: %w: %w", ErrInternal, err)
	}
	defer body.Close()
	prefix, err := io.ReadAll(io.LimitReader(body, prefixMax))
	if err != nil {
		return nil, fmt.Errorf("read staged blob prefix: %w: %w", ErrInternal, err)
	}
	return prefix, nil
}

// CreateAnonymousPasteFromKey commits an anonymous quick share whose body was
// uploaded directly to object storage under key.
func (d *Store) CreateAnonymousPasteFromKey(ctx context.Context, resourceID, key, filename string, size int64, sha256Hex string, ttl time.Duration, now time.Time) (Resource, Link, error) {
	if !validUUIDs(resourceID) {
		return Resource{}, Link{}, refusal("上传凭证无效")
	}
	if ttl <= 0 {
		ttl = AnonymousDefaultTTL
	}
	if ttl < AnonymousMinTTL || ttl > AnonymousMaxTTL {
		return Resource{}, Link{}, errAnonymousTTL
	}
	if size <= 0 {
		return Resource{}, Link{}, refusal("内容不能为空")
	}
	if size > AnonymousMaxBytes {
		return Resource{}, Link{}, refusalf("内容最大 %s", BytesText(AnonymousMaxBytes))
	}
	filename = strings.TrimSpace(filename)
	if err := validateFilename(filename); err != nil {
		return Resource{}, Link{}, err
	}
	prefix, err := d.verifyStagedObject(ctx, key, size, sniffPrefixMaxBytes)
	if err != nil {
		return Resource{}, Link{}, err
	}
	contentType, encoding := anonymousType(trimUTF8Tail(prefix, size))
	resource := Resource{
		ID: resourceID, OwnerID: AnonymousUserID, Name: filename, Filename: filename,
		ContentKey: key, ContentSize: size, ContentType: contentType, ContentEncoding: encoding,
		ContentSHA256: validSHA256Hex(sha256Hex), CreatedAt: now, UpdatedAt: now, Version: 1, VersionAt: now,
	}
	link := Link{ResourceID: resource.ID, ExpiresAt: shareExpiry(now, ttl)}
	err = d.withTx(ctx, func(tx pgx.Tx) error {
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
		return d.insertLink(ctx, tx, &link, now)
	})
	if err != nil {
		d.DropBlob(ctx, key)
		return Resource{}, Link{}, err
	}
	return resource, link, nil
}

// CreateQuickShareFromKey commits a signed-in quick share whose body was
// uploaded directly to object storage under key.
func (d *Store) CreateQuickShareFromKey(ctx context.Context, ownerID, resourceID, key, filename string, size int64, sha256Hex string, ttl time.Duration, now time.Time) (Resource, Link, error) {
	if !validUUIDs(ownerID, resourceID) || ownerID == AnonymousUserID {
		return Resource{}, Link{}, ErrNotFound
	}
	ttl, err := quickShareTTL(ttl)
	if err != nil {
		return Resource{}, Link{}, err
	}
	if size <= 0 {
		return Resource{}, Link{}, refusal("内容不能为空")
	}
	if size > AnonymousMaxBytes {
		return Resource{}, Link{}, refusalf("内容最大 %s", BytesText(AnonymousMaxBytes))
	}
	filename = strings.TrimSpace(filename)
	if err := validateFilename(filename); err != nil {
		return Resource{}, Link{}, err
	}
	prefix, err := d.verifyStagedObject(ctx, key, size, sniffPrefixMaxBytes)
	if err != nil {
		return Resource{}, Link{}, err
	}
	contentType, encoding := anonymousType(trimUTF8Tail(prefix, size))
	resource := Resource{
		ID: resourceID, Name: filename, Filename: filename, ContentKey: key, ContentSize: size,
		ContentType: contentType, ContentEncoding: encoding, ContentSHA256: validSHA256Hex(sha256Hex),
	}
	return d.insertQuickShareFromKey(ctx, ownerID, resource, Link{}, ttl, now)
}

// CreateSealedQuickShareFromKey commits an encrypted quick share whose
// ciphertext was uploaded directly to object storage under key.
func (d *Store) CreateSealedQuickShareFromKey(ctx context.Context, ownerID, resourceID, linkID, key string, meta, sealedKey, linkKey, ownerKey []byte, size int64, sha256Hex string, ttl time.Duration, now time.Time) (Resource, Link, error) {
	if !validUUIDs(ownerID, resourceID) || ownerID == AnonymousUserID {
		return Resource{}, Link{}, ErrNotFound
	}
	if !validUUIDs(linkID) || !validSealedMeta(meta) || len(sealedKey) != sealedKeyBytes || !validLinkKeys(linkKey, ownerKey) {
		return Resource{}, Link{}, errSealedShape
	}
	ttl, err := quickShareTTL(ttl)
	if err != nil {
		return Resource{}, Link{}, err
	}
	if size <= sealedHeaderBytes || size > AnonymousMaxBytes+sealedHeaderBytes {
		return Resource{}, Link{}, refusalf("内容最大 %s", BytesText(AnonymousMaxBytes))
	}
	prefix, err := d.verifyStagedObject(ctx, key, size, int64(len(sealedContentMagic)))
	if err != nil {
		return Resource{}, Link{}, err
	}
	if !bytes.HasPrefix(prefix, sealedContentMagic) {
		d.DropBlob(ctx, key)
		return Resource{}, Link{}, errSealedShape
	}
	resource := Resource{
		ID: resourceID, ContentKey: key, ContentSize: size, ContentType: SealedContentType,
		ContentSHA256: validSHA256Hex(sha256Hex), SealedKey: sealedKey, SealedMeta: meta,
	}
	return d.insertQuickShareFromKey(ctx, ownerID, resource, Link{ID: linkID, SealedKey: linkKey, OwnerKey: ownerKey}, ttl, now)
}

func (d *Store) insertQuickShareFromKey(ctx context.Context, ownerID string, resource Resource, link Link, ttl time.Duration, now time.Time) (Resource, Link, error) {
	link.ExpiresAt = shareExpiry(now, ttl)
	resource.OwnerID, resource.ExpiresAt = ownerID, link.ExpiresAt
	resource.CreatedAt, resource.UpdatedAt, resource.Version, resource.VersionAt = now, now, 1, now
	link.ResourceID = resource.ID

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
		dropped, err = trimHistoryTx(ctx, tx, ownerID, "", limit.StorageBytes)
		return err
	})
	if err != nil {
		d.DropBlob(ctx, resource.ContentKey)
		return Resource{}, Link{}, err
	}
	d.dropObjects(ctx, dropped)
	return resource, link, nil
}

// CreateResourceFromKey commits a new plain resource whose body was uploaded
// directly to object storage under key.
func (d *Store) CreateResourceFromKey(ctx context.Context, ownerID, resourceID, key, name, filename, uploadedAs, encodingHint string, size int64, sha256Hex string) (Resource, error) {
	if !validUUIDs(ownerID, resourceID) || ownerID == AnonymousUserID {
		return Resource{}, ErrNotFound
	}
	if size <= 0 {
		return Resource{}, refusal("resource content must not be empty")
	}
	prefix, err := d.verifyStagedObject(ctx, key, size, sniffPrefixMaxBytes)
	if err != nil {
		return Resource{}, err
	}
	sample := trimUTF8Tail(prefix, size)
	filename = RefitFilename(strings.TrimSpace(filename), uploadedAs, sample)
	if err := validateFilename(filename); err != nil {
		d.DropBlob(ctx, key)
		return Resource{}, err
	}
	contentType, encoding := DetectContent(filename, sample, encodingHint)
	now := time.Now().UTC()
	resource := Resource{
		ID: resourceID, OwnerID: ownerID, Name: resourceName(name, filename), Filename: filename,
		ContentKey: key, ContentSize: size, ContentType: contentType, ContentEncoding: encoding,
		ContentSHA256: validSHA256Hex(sha256Hex), CreatedAt: now, UpdatedAt: now, Version: 1, VersionAt: now,
	}
	var dropped []string
	err = d.withTx(ctx, func(tx pgx.Tx) error {
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
		tag, err := tx.Exec(ctx, `
INSERT INTO resources(id, owner_id, name, filename, content_key, content_size, content_type, content_encoding,
                      created_at, updated_at, version, version_at, content_sha256)
VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9, $9, 1, $9, $10)
ON CONFLICT (id) DO NOTHING
`, resource.ID, ownerID, resource.Name, resource.Filename, resource.ContentKey,
			resource.ContentSize, resource.ContentType, resource.ContentEncoding, now, resource.ContentSHA256)
		if err != nil {
			return fmt.Errorf("create resource: %w: %w", ErrInternal, err)
		}
		if tag.RowsAffected() == 0 {
			return refusal("上传凭证已使用")
		}
		dropped, err = trimHistoryTx(ctx, tx, ownerID, "", limit.StorageBytes)
		return err
	})
	if err != nil {
		d.DropBlob(ctx, key)
		return Resource{}, err
	}
	d.dropObjects(ctx, dropped)
	return resource, nil
}

// SaveResourceFromKey updates an existing plain resource with a new version
// whose body was uploaded directly to object storage under key.
func (d *Store) SaveResourceFromKey(ctx context.Context, ownerID, resourceID, key, name, filename, uploadedAs, encodingHint string, baseVersion int, size int64, sha256Hex string) (SaveResult, error) {
	current, err := d.ResourceForOwner(ctx, ownerID, resourceID)
	if err != nil {
		d.DropBlob(ctx, key)
		return SaveResult{}, err
	}
	if err := refuseQuickShare(current); err != nil {
		d.DropBlob(ctx, key)
		return SaveResult{}, err
	}
	if current.Sealed() {
		d.DropBlob(ctx, key)
		return SaveResult{}, errSealedInBrowser
	}
	prefix, err := d.verifyStagedObject(ctx, key, size, sniffPrefixMaxBytes)
	if err != nil {
		return SaveResult{}, err
	}
	sample := trimUTF8Tail(prefix, size)
	filename = RefitFilename(strings.TrimSpace(filename), uploadedAs, sample)
	if err := validateFilename(filename); err != nil {
		d.DropBlob(ctx, key)
		return SaveResult{}, err
	}
	contentType, encoding := DetectContent(filename, sample, encodingHint)
	nextName := resourceName(name, filename)
	nextSHA := validSHA256Hex(sha256Hex)

	replaceContent := true
	if nextSHA != "" && current.ContentSHA256 != "" && nextSHA == current.ContentSHA256 && encoding == current.ContentEncoding {
		replaceContent = false
	}

	now := time.Now().UTC()
	var result SaveResult
	var dropped []string
	err = d.withTx(ctx, func(tx pgx.Tx) error {
		limit, usage, err := quotaGate(ctx, tx, ownerID, resourceID, now)
		if err != nil {
			return err
		}
		locked, err := lockContentTx(ctx, tx, ownerID, resourceID)
		if err != nil {
			return err
		}
		if baseVersion > 0 && locked.Version != baseVersion && (nextSHA == "" || nextSHA != locked.SHA256) {
			return &VersionConflict{Current: locked.Version, At: locked.VersionAt}
		}
		next := locked
		next.Type, next.Encoding = contentType, encoding
		if replaceContent {
			if err := retireTx(ctx, tx, resourceID, locked, now); err != nil {
				return err
			}
			next.Version = locked.Version + 1
			next.Key, next.Size, next.SHA256 = key, size, nextSHA
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
		result.Trimmed = len(trimmed)
		dropped = trimmed
		return err
	})
	if err != nil {
		d.DropBlob(ctx, key)
		return SaveResult{}, err
	}
	if !replaceContent {
		d.DropBlob(ctx, key)
	}
	d.dropObjects(ctx, dropped)
	return result, nil
}

// CreateSealedResourceFromKey commits a new encrypted resource whose ciphertext
// was uploaded directly to object storage under key.
func (d *Store) CreateSealedResourceFromKey(ctx context.Context, ownerID, resourceID, key string, meta, sealedKey []byte, size int64, sha256Hex string) (Resource, error) {
	if !validUUIDs(ownerID, resourceID) || ownerID == AnonymousUserID {
		return Resource{}, ErrNotFound
	}
	if !validSealedMeta(meta) || len(sealedKey) != sealedKeyBytes || size <= sealedHeaderBytes {
		return Resource{}, errSealedShape
	}
	prefix, err := d.verifyStagedObject(ctx, key, size, int64(len(sealedContentMagic)))
	if err != nil {
		return Resource{}, err
	}
	if !bytes.HasPrefix(prefix, sealedContentMagic) {
		d.DropBlob(ctx, key)
		return Resource{}, errSealedShape
	}
	now := time.Now().UTC()
	resource := Resource{
		ID: resourceID, OwnerID: ownerID, ContentType: SealedContentType, ContentKey: key,
		ContentSize: size, CreatedAt: now, UpdatedAt: now, Version: 1, VersionAt: now,
		ContentSHA256: validSHA256Hex(sha256Hex), SealedKey: sealedKey, SealedMeta: meta,
	}
	var dropped []string
	err = d.withTx(ctx, func(tx pgx.Tx) error {
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
		tag, err := tx.Exec(ctx, `
INSERT INTO resources(id, owner_id, name, filename, content_key, content_size, content_type, content_encoding,
                      created_at, updated_at, version, version_at, content_sha256, sealed_key, sealed_meta)
VALUES($1, $2, '', '', $3, $4, $5, '', $6, $6, 1, $6, $7, $8, $9)
ON CONFLICT (id) DO NOTHING
`, resourceID, ownerID, resource.ContentKey, resource.ContentSize, SealedContentType, now, resource.ContentSHA256, sealedKey, meta)
		if err != nil {
			return fmt.Errorf("create sealed resource: %w: %w", ErrInternal, err)
		}
		if tag.RowsAffected() == 0 {
			return errSealedShape
		}
		dropped, err = trimHistoryTx(ctx, tx, ownerID, "", limit.StorageBytes)
		return err
	})
	if err != nil {
		d.DropBlob(ctx, key)
		return Resource{}, err
	}
	d.dropObjects(ctx, dropped)
	return resource, nil
}

// SaveSealedResourceFromKey commits a new version of an encrypted resource whose
// ciphertext was uploaded directly to object storage under key.
func (d *Store) SaveSealedResourceFromKey(ctx context.Context, ownerID, resourceID string, baseVersion int, key string, meta []byte, size int64, sha256Hex string) (SaveResult, error) {
	current, err := d.ResourceForOwner(ctx, ownerID, resourceID)
	if err != nil {
		d.DropBlob(ctx, key)
		return SaveResult{}, err
	}
	if !current.Sealed() {
		d.DropBlob(ctx, key)
		return SaveResult{}, errSealedShape
	}
	if !validSealedMeta(meta) || size <= sealedHeaderBytes {
		d.DropBlob(ctx, key)
		return SaveResult{}, errSealedShape
	}
	prefix, err := d.verifyStagedObject(ctx, key, size, int64(len(sealedContentMagic)))
	if err != nil {
		return SaveResult{}, err
	}
	if !bytes.HasPrefix(prefix, sealedContentMagic) {
		d.DropBlob(ctx, key)
		return SaveResult{}, errSealedShape
	}
	now := time.Now().UTC()
	var result SaveResult
	var dropped []string
	nextSHA := validSHA256Hex(sha256Hex)
	err = d.withTx(ctx, func(tx pgx.Tx) error {
		limit, usage, err := quotaGate(ctx, tx, ownerID, resourceID, now)
		if err != nil {
			return err
		}
		locked, err := lockContentTx(ctx, tx, ownerID, resourceID)
		if err != nil {
			return err
		}
		if baseVersion > 0 && locked.Version != baseVersion {
			return &VersionConflict{Current: locked.Version, At: locked.VersionAt}
		}
		if err := retireTx(ctx, tx, resourceID, locked, now); err != nil {
			return err
		}
		next := locked
		next.Version, next.VersionAt, next.RestoredFrom = locked.Version+1, now, 0
		next.Key, next.Size, next.SHA256 = key, size, nextSHA
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
		result.Trimmed = len(trimmed)
		dropped = trimmed
		return err
	})
	if err != nil {
		d.DropBlob(ctx, key)
		return SaveResult{}, err
	}
	d.dropObjects(ctx, dropped)
	return result, nil
}
