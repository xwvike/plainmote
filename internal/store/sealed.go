package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// End-to-end encrypted resources. The browser encrypts; the service stores
// what it is given, checks only its shape, and keeps the encrypted resource
// to the same rules as any other - versions, quota, links - without ever
// reading it. docs/encryption.md is the design.

// SealedContentType is what an encrypted resource's content is recorded as.
// Its real type is inside its encrypted metadata.
const SealedContentType = "application/vnd.plainmote.sealed"

var (
	sealedContentMagic = []byte("PMr1")
	sealedMetaMagic    = []byte("PMm1")
	// SealedBundleMagic opens what a link to an encrypted resource delivers:
	// the content key wrapped by the link key, the encrypted metadata and the
	// encrypted content, in one response.
	SealedBundleMagic = []byte("PMs1")
)

const (
	sealedHeaderBytes  = 4 + 12 + 16
	sealedMetaMaxBytes = 4096
	sealedKeyBytes     = keyringWrappedBytes
)

var (
	errSealedShape     = refusal("加密内容格式无法识别")
	errSealedNeedsKey  = refusal("加密资源的分享链接需要在浏览器中创建")
	errSealedInBrowser = refusal("加密资源需要在浏览器中解锁后修改")
	errSealedRemote    = refusal("远程资源不能加密")
	errSealedVersions  = refusal("历史版本已变化，请刷新页面后重试")
	errSealedCopy      = refusal("加密资源的历史版本需要在浏览器中另存为新资源")
)

func validSealedContent(blob []byte) bool {
	return len(blob) > sealedHeaderBytes && bytes.HasPrefix(blob, sealedContentMagic)
}

func validSealedMeta(meta []byte) bool {
	return len(meta) > sealedHeaderBytes && len(meta) <= sealedMetaMaxBytes && bytes.HasPrefix(meta, sealedMetaMagic)
}

// SealedPart is one encrypted content - the current one or an earlier
// version - with its encrypted metadata.
type SealedPart struct {
	Content []byte
	Meta    []byte
}

// PlainPart is one content as it was before encryption, or is again after.
type PlainPart struct {
	Content  []byte
	Name     string
	Filename string
}

func (p SealedPart) valid() bool { return validSealedContent(p.Content) && validSealedMeta(p.Meta) }

// CreateSealedResource stores a resource encrypted in the browser under the
// id its encryption is bound to.
func (d *Store) CreateSealedResource(ctx context.Context, ownerID, id string, part SealedPart, sealedKey []byte) (Resource, error) {
	if !validUUIDs(ownerID, id) || ownerID == AnonymousUserID {
		return Resource{}, ErrNotFound
	}
	if !part.valid() || len(sealedKey) != sealedKeyBytes {
		return Resource{}, errSealedShape
	}
	now := time.Now().UTC()
	resource := Resource{
		ID: id, OwnerID: ownerID, ContentType: SealedContentType, ContentKey: contentKey(id),
		ContentSize: int64(len(part.Content)), CreatedAt: now, UpdatedAt: now, Version: 1, VersionAt: now,
		ContentSHA256: contentSHA256(part.Content), SealedKey: sealedKey, SealedMeta: part.Meta,
	}
	if err := d.blobs.Put(ctx, resource.ContentKey, bytes.NewReader(part.Content), resource.ContentSize); err != nil {
		return Resource{}, fmt.Errorf("store sealed body: %w: %w", ErrInternal, err)
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
		tag, err := tx.Exec(ctx, `
INSERT INTO resources(id, owner_id, name, filename, content_key, content_size, content_type, content_encoding, origin_url,
                      created_at, updated_at, version, version_at, content_sha256, sealed_key, sealed_meta)
VALUES($1, $2, '', '', $3, $4, $5, '', '', $6, $6, 1, $6, $7, $8, $9)
ON CONFLICT (id) DO NOTHING
`, id, ownerID, resource.ContentKey, resource.ContentSize, SealedContentType, now, resource.ContentSHA256, sealedKey, part.Meta)
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
		_ = d.blobs.Delete(ctx, resource.ContentKey)
		return Resource{}, err
	}
	d.dropObjects(ctx, dropped)
	return resource, nil
}

// SaveSealedResource writes an edit made in the browser: new encrypted
// content as a new version, or with Content nil, only new metadata - a
// rename. The version check is the one every save has.
func (d *Store) SaveSealedResource(ctx context.Context, ownerID, id string, baseVersion int, part SealedPart) (SaveResult, error) {
	current, err := d.ResourceForOwner(ctx, ownerID, id)
	if err != nil {
		return SaveResult{}, err
	}
	if !current.Sealed() {
		return SaveResult{}, errSealedShape
	}
	if !validSealedMeta(part.Meta) || (part.Content != nil && !validSealedContent(part.Content)) {
		return SaveResult{}, errSealedShape
	}
	replace := part.Content != nil
	nextKey, nextSHA := "", ""
	if replace {
		nextKey, nextSHA = contentKey(id), contentSHA256(part.Content)
		if err := d.blobs.Put(ctx, nextKey, bytes.NewReader(part.Content), int64(len(part.Content))); err != nil {
			return SaveResult{}, fmt.Errorf("store sealed body: %w: %w", ErrInternal, err)
		}
	}
	now := time.Now().UTC()
	var result SaveResult
	var dropped []string
	err = d.withTx(ctx, func(tx pgx.Tx) error {
		limit, usage, err := quotaGate(ctx, tx, ownerID, id, now)
		if err != nil {
			return err
		}
		locked, err := lockContentTx(ctx, tx, ownerID, id)
		if err != nil {
			return err
		}
		// Encrypting the same text twice never gives the same bytes, so
		// unlike a plain save there is no telling an unchanged resubmission
		// apart: any save against an older version is a conflict.
		if baseVersion > 0 && locked.Version != baseVersion {
			return &VersionConflict{Current: locked.Version, At: locked.VersionAt}
		}
		next := locked
		if replace {
			if err := retireTx(ctx, tx, id, locked, now); err != nil {
				return err
			}
			next.Version, next.VersionAt, next.RestoredFrom = locked.Version+1, now, 0
			next.Key, next.Size, next.SHA256 = nextKey, int64(len(part.Content)), nextSHA
			result.NewVersion = true
		}
		if usage.StorageBytes+next.Size > limit.StorageBytes {
			return storageQuotaError(limit.StorageBytes, usage.StorageBytes, next.Size)
		}
		if _, err := tx.Exec(ctx, `
UPDATE resources
SET content_key = $1, content_size = $2, content_sha256 = $3, version = $4, version_at = $5,
    restored_from = NULLIF($6, 0), sealed_meta = $7, updated_at = $8
WHERE id = $9 AND owner_id = $10
`, next.Key, next.Size, next.SHA256, next.Version, next.VersionAt, next.RestoredFrom, part.Meta, now, id, ownerID); err != nil {
			return fmt.Errorf("save sealed resource: %w: %w", ErrInternal, err)
		}
		result.Version = next.Version
		trimmed, err := trimHistoryTx(ctx, tx, ownerID, id, limit.StorageBytes)
		result.Trimmed = len(trimmed)
		dropped = trimmed
		return err
	})
	if err != nil {
		if nextKey != "" {
			_ = d.blobs.Delete(ctx, nextKey)
		}
		return SaveResult{}, err
	}
	d.dropObjects(ctx, dropped)
	return result, nil
}

// historyNumbers lists a resource's earlier versions under its row lock.
func historyNumbers(ctx context.Context, tx pgx.Tx, id string) ([]int, []string, error) {
	rows, err := tx.Query(ctx, `SELECT version, content_key FROM resource_versions WHERE resource_id = $1 ORDER BY version FOR UPDATE`, id)
	if err != nil {
		return nil, nil, fmt.Errorf("read history: %w: %w", ErrInternal, err)
	}
	defer rows.Close()
	var numbers []int
	var keys []string
	for rows.Next() {
		var number int
		var key string
		if err := rows.Scan(&number, &key); err != nil {
			return nil, nil, fmt.Errorf("read history: %w: %w", ErrInternal, err)
		}
		numbers = append(numbers, number)
		keys = append(keys, key)
	}
	return numbers, keys, rows.Err()
}

func sameNumbers[T any](have []int, given map[int]T) bool {
	if len(have) != len(given) {
		return false
	}
	for _, number := range have {
		if _, ok := given[number]; !ok {
			return false
		}
	}
	return true
}

// putParts writes each content to a new object, keyed by version, and gives
// back the keys - all of them written, or none left behind.
func (d *Store) putParts(ctx context.Context, id string, contents map[int][]byte) (map[int]string, error) {
	keys := make(map[int]string, len(contents))
	numbers := make([]int, 0, len(contents))
	for number := range contents {
		numbers = append(numbers, number)
	}
	sort.Ints(numbers)
	for _, number := range numbers {
		key := contentKey(id)
		if err := d.blobs.Put(ctx, key, bytes.NewReader(contents[number]), int64(len(contents[number]))); err != nil {
			for _, written := range keys {
				_ = d.blobs.Delete(ctx, written)
			}
			return nil, fmt.Errorf("store body: %w: %w", ErrInternal, err)
		}
		keys[number] = key
	}
	return keys, nil
}

// SealResource turns a resource into an encrypted one: the browser sends the
// current content and every earlier version encrypted, and in one transaction
// they replace the plaintext, the name and filename go, and every link the
// resource had is revoked - their addresses carry no key, so all they could
// deliver now is ciphertext. The plaintext objects go after the commit.
// The versions sent must be exactly the versions there are.
func (d *Store) SealResource(ctx context.Context, ownerID, id string, baseVersion int, current SealedPart, versions map[int]SealedPart, sealedKey []byte) (revoked int, err error) {
	resource, err := d.ResourceForOwner(ctx, ownerID, id)
	if err != nil {
		return 0, err
	}
	switch {
	case resource.Sealed():
		return 0, errSealedShape
	case resource.Remote():
		return 0, errSealedRemote
	case resource.TakenDown:
		return 0, ErrTakenDown
	}
	if err := refuseQuickShare(resource); err != nil {
		return 0, err
	}
	if !current.valid() || len(sealedKey) != sealedKeyBytes {
		return 0, errSealedShape
	}
	contents := map[int][]byte{0: current.Content}
	for number, part := range versions {
		if number <= 0 || !part.valid() {
			return 0, errSealedShape
		}
		contents[number] = part.Content
	}
	keys, err := d.putParts(ctx, id, contents)
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	var dropped []string
	err = d.withTx(ctx, func(tx pgx.Tx) error {
		locked, err := lockContentTx(ctx, tx, ownerID, id)
		if err != nil {
			return err
		}
		if locked.Version != baseVersion {
			return &VersionConflict{Current: locked.Version, At: locked.VersionAt}
		}
		numbers, oldKeys, err := historyNumbers(ctx, tx, id)
		if err != nil {
			return err
		}
		if !sameNumbers(numbers, versions) {
			return errSealedVersions
		}
		for _, number := range numbers {
			part := versions[number]
			if _, err := tx.Exec(ctx, `
UPDATE resource_versions
SET content_key = $3, content_size = $4, content_type = $5, content_encoding = '', content_sha256 = $6, filename = '', sealed_meta = $7
WHERE resource_id = $1 AND version = $2
`, id, number, keys[number], len(part.Content), SealedContentType, contentSHA256(part.Content), part.Meta); err != nil {
				return fmt.Errorf("seal version %d: %w: %w", number, ErrInternal, err)
			}
		}
		if _, err := tx.Exec(ctx, `
UPDATE resources
SET name = '', filename = '', content_key = $3, content_size = $4, content_type = $5, content_encoding = '',
    content_sha256 = $6, sealed_key = $7, sealed_meta = $8, updated_at = $9
WHERE id = $1 AND owner_id = $2
`, id, ownerID, keys[0], len(current.Content), SealedContentType, contentSHA256(current.Content), sealedKey, current.Meta, now); err != nil {
			return fmt.Errorf("seal resource: %w: %w", ErrInternal, err)
		}
		tag, err := tx.Exec(ctx, `UPDATE links SET revoked_at = $2 WHERE resource_id = $1 AND revoked_at IS NULL`, id, now)
		if err != nil {
			return fmt.Errorf("revoke links: %w: %w", ErrInternal, err)
		}
		revoked = int(tag.RowsAffected())
		dropped = append(oldKeys, locked.Key)
		return nil
	})
	if err != nil {
		for _, key := range keys {
			_ = d.blobs.Delete(ctx, key)
		}
		return 0, err
	}
	d.dropObjects(ctx, dropped)
	return revoked, nil
}

// UnsealResource turns an encrypted resource back into a plain one: the
// browser sends the current content and every earlier version decrypted, and
// they are stored and typed as any save would be. Its links stay, and deliver
// plaintext from now on; the keys they held for the ciphertext go.
func (d *Store) UnsealResource(ctx context.Context, ownerID, id string, baseVersion int, current PlainPart, versions map[int]PlainPart) error {
	resource, err := d.ResourceForOwner(ctx, ownerID, id)
	if err != nil {
		return err
	}
	if !resource.Sealed() {
		return errSealedShape
	}
	if resource.TakenDown {
		return ErrTakenDown
	}
	head, err := normalizeResourceInput(current.Name, current.Filename, current.Content, "", "")
	if err != nil {
		return err
	}
	typed := map[int]resourceInput{}
	contents := map[int][]byte{0: head.Content}
	for number, part := range versions {
		if number <= 0 {
			return errSealedShape
		}
		input, err := normalizeResourceInput(part.Filename, part.Filename, part.Content, "", "")
		if err != nil {
			return err
		}
		typed[number] = input
		contents[number] = input.Content
	}
	keys, err := d.putParts(ctx, id, contents)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	var dropped []string
	err = d.withTx(ctx, func(tx pgx.Tx) error {
		locked, err := lockContentTx(ctx, tx, ownerID, id)
		if err != nil {
			return err
		}
		if locked.Version != baseVersion {
			return &VersionConflict{Current: locked.Version, At: locked.VersionAt}
		}
		numbers, oldKeys, err := historyNumbers(ctx, tx, id)
		if err != nil {
			return err
		}
		if !sameNumbers(numbers, versions) {
			return errSealedVersions
		}
		for _, number := range numbers {
			input := typed[number]
			if _, err := tx.Exec(ctx, `
UPDATE resource_versions
SET content_key = $3, content_size = $4, content_type = $5, content_encoding = $6, content_sha256 = $7, filename = $8, sealed_meta = NULL
WHERE resource_id = $1 AND version = $2
`, id, number, keys[number], len(input.Content), input.ContentType, input.Encoding, contentSHA256(input.Content), input.Filename); err != nil {
				return fmt.Errorf("unseal version %d: %w: %w", number, ErrInternal, err)
			}
		}
		if _, err := tx.Exec(ctx, `
UPDATE resources
SET name = $3, filename = $4, content_key = $5, content_size = $6, content_type = $7, content_encoding = $8,
    content_sha256 = $9, sealed_key = NULL, sealed_meta = NULL, updated_at = $10
WHERE id = $1 AND owner_id = $2
`, id, ownerID, head.Name, head.Filename, keys[0], len(head.Content), head.ContentType, head.Encoding, contentSHA256(head.Content), now); err != nil {
			return fmt.Errorf("unseal resource: %w: %w", ErrInternal, err)
		}
		if _, err := tx.Exec(ctx, `UPDATE links SET sealed_key = NULL, owner_key = NULL WHERE resource_id = $1`, id); err != nil {
			return fmt.Errorf("unseal links: %w: %w", ErrInternal, err)
		}
		dropped = append(oldKeys, locked.Key)
		return nil
	})
	if err != nil {
		for _, key := range keys {
			_ = d.blobs.Delete(ctx, key)
		}
		return err
	}
	d.dropObjects(ctx, dropped)
	return nil
}

// CreateSealedShare makes a link to an encrypted resource, under the id its
// keys are bound to, carrying the content key wrapped by the link key and the
// link key wrapped by the account key.
func (d *Store) CreateSealedShare(ctx context.Context, ownerID, resourceID, linkID, name string, ttl time.Duration, maxUses int, sealedKey, ownerKey []byte) (Link, error) {
	if !validUUIDs(linkID) {
		return Link{}, errSealedShape
	}
	resource, err := d.ResourceForOwner(ctx, ownerID, resourceID)
	if err != nil {
		return Link{}, err
	}
	if !resource.Sealed() || len(sealedKey) != sealedKeyBytes || len(ownerKey) != sealedKeyBytes {
		return Link{}, errSealedShape
	}
	if resource.TakenDown {
		return Link{}, ErrTakenDown
	}
	if err := validateShareTerms(ttl, maxUses); err != nil {
		return Link{}, err
	}
	now := time.Now().UTC()
	link := Link{ID: linkID, ResourceID: resourceID, Name: name, MaxUses: maxUses, ExpiresAt: shareExpiry(now, ttl), SealedKey: sealedKey, OwnerKey: ownerKey}
	if err := d.insertLink(ctx, d.db, &link, now); err != nil {
		if isUniqueViolation(err) {
			return Link{}, errSealedShape
		}
		return Link{}, err
	}
	return link, nil
}

// SealedEntry is what the browser needs to show an encrypted resource's
// name once unlocked: its id, its wrapped content key and its encrypted
// metadata.
type SealedEntry struct {
	ID         string
	SealedKey  []byte
	SealedMeta []byte
}

// SealedIndex lists the account's encrypted resources, for its lists and
// searches to name them in the browser.
func (d *Store) SealedIndex(ctx context.Context, ownerID string) ([]SealedEntry, error) {
	if !validUUIDs(ownerID) {
		return nil, ErrNotFound
	}
	rows, err := d.db.Query(ctx, `
SELECT r.id::text, r.sealed_key, r.sealed_meta FROM resources r
WHERE r.owner_id = $1 AND r.sealed_key IS NOT NULL AND `+liveResource+`
ORDER BY r.updated_at DESC
`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("list sealed resources: %w: %w", ErrInternal, err)
	}
	entries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (SealedEntry, error) {
		var e SealedEntry
		return e, row.Scan(&e.ID, &e.SealedKey, &e.SealedMeta)
	})
	if err != nil {
		return nil, fmt.Errorf("list sealed resources: %w: %w", ErrInternal, err)
	}
	return entries, nil
}

// SealedBundle is what a link to an encrypted resource delivers ahead of
// the content: the magic, then the resource id - the page that opens it knows
// only the link's token, and the content's encryption is bound to the id - the
// content key wrapped by the link key, and the encrypted metadata, each after
// a two-byte big-endian length.
func SealedBundle(resourceID string, linkKey, meta []byte) ([]byte, error) {
	if len(linkKey) > 0xffff || len(meta) > 0xffff {
		return nil, errors.New("sealed bundle part too long")
	}
	out := make([]byte, 0, len(SealedBundleMagic)+6+len(resourceID)+len(linkKey)+len(meta))
	out = append(out, SealedBundleMagic...)
	for _, part := range [][]byte{[]byte(resourceID), linkKey, meta} {
		out = append(out, byte(len(part)>>8), byte(len(part)))
		out = append(out, part...)
	}
	return out, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
