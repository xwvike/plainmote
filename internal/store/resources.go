package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"plainmote/internal/upstream"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// filenameMaxRunes bounds the name for the database column, the URL tail and
// the Content-Disposition header at once. Counted in runes, because a limit in
// bytes would give a Chinese name a third of the room an English one gets.
const filenameMaxRunes = 128

// validateFilename refuses what is dangerous and nothing else. This is the
// user's own file: Japanese, Arabic, Persian, emoji and spaces are all how
// people name things, and none of them threaten anything here - the delivery
// header carries non-ASCII in the RFC 6266 extended form and escapes the rest,
// and the URL tail is percent-encoded.
//
// What is left out is genuinely hazardous rather than merely unusual:
//
//   - path separators, so a name can never steer where a downloader writes;
//   - control characters, which would end a header line and begin one of the
//     caller's own, and which a terminal may act on rather than print;
//   - bidi controls, the one class of otherwise-printable character that makes
//     a name render as something it is not - U+202E turns "photo\u202Egnp.exe"
//     into "photoexe.png" in front of whoever received the link. Joiners are
//     deliberately not in this set: U+200C is required to write Persian
//     correctly, and U+200D holds emoji sequences together.
//
// "." and ".." are refused as whole names because they are directory entries,
// not files. Inside a name those dots are ordinary, and without a separator
// they cannot climb anywhere.
func validateFilename(name string) error {
	if name == "" {
		return nil
	}
	if !utf8.ValidString(name) {
		return errors.New("文件名必须是有效的 UTF-8 文本")
	}
	if utf8.RuneCountInString(name) > filenameMaxRunes {
		return fmt.Errorf("文件名最长 %d 个字符", filenameMaxRunes)
	}
	if name == "." || name == ".." {
		return errors.New("文件名不能是 . 或 ..")
	}
	for _, r := range name {
		switch {
		case r == '/' || r == '\\':
			return errors.New("文件名不能包含路径分隔符")
		case unicode.IsControl(r):
			return errors.New("文件名不能包含控制字符")
		case unicode.Is(unicode.Bidi_Control, r):
			return errors.New("文件名不能包含文字方向控制符")
		}
	}
	return nil
}

type resourceInput struct {
	Name        string
	Filename    string
	ContentType string
	Encoding    string
	OriginURL   string
	Content     []byte
}

func normalizeResourceInput(name, filename string, content []byte, contentEncoding, originURL string, allowPrivateUpstream bool) (resourceInput, error) {
	filename = strings.TrimSpace(filename)
	if err := validateFilename(filename); err != nil {
		return resourceInput{}, err
	}
	originURL = strings.TrimSpace(originURL)
	if originURL != "" {
		parsed, err := url.Parse(originURL)
		if err != nil {
			return resourceInput{}, fmt.Errorf("parse remote link: %w", err)
		}
		if err := upstream.ValidateURL(parsed, allowPrivateUpstream); err != nil {
			return resourceInput{}, err
		}
		content = nil
	} else if len(content) == 0 {
		return resourceInput{}, errors.New("resource content must not be empty")
	}
	contentType := ""
	detectedEncoding := ""
	if originURL == "" {
		contentType, detectedEncoding = DetectContent(filename, content, contentEncoding)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = filename
	}
	if name == "" {
		name = "未命名资源"
	}
	return resourceInput{Name: name, Filename: filename, ContentType: contentType, Encoding: detectedEncoding, OriginURL: originURL, Content: content}, nil
}

func (d *Store) CreateResource(ctx context.Context, ownerID, name, filename string, content []byte, contentEncoding, originURL string) (Resource, error) {
	input, err := normalizeResourceInput(name, filename, content, contentEncoding, originURL, d.allowPrivateUpstream)
	if err != nil {
		return Resource{}, err
	}
	now := time.Now().UTC()
	resource := Resource{
		ID: uuid.NewString(), OwnerID: ownerID, Name: input.Name, Filename: input.Filename,
		ContentType: input.ContentType, ContentEncoding: input.Encoding,
		OriginURL: input.OriginURL, CreatedAt: now, UpdatedAt: now,
	}

	// The body is written before the transaction opens. Object storage has no
	// transaction to join, and pulling its round trip inside one would hold the
	// account lock and a pooled connection for the length of an S3 call. The
	// failure this ordering leaves behind is an object no row points at, which
	// the cleanup below removes; the other order leaves a row pointing at an
	// object that does not exist.
	if len(input.Content) > 0 {
		resource.ContentKey = contentKey(resource.ID)
		resource.ContentSize = int64(len(input.Content))
		if err := d.blobs.Put(ctx, resource.ContentKey, bytes.NewReader(input.Content), resource.ContentSize); err != nil {
			return Resource{}, fmt.Errorf("store resource body: %w: %w", ErrInternal, err)
		}
	}

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
		_, err = tx.Exec(ctx, `
INSERT INTO resources(id, owner_id, name, filename, content_key, content_size, content_type, content_encoding, origin_url, created_at, updated_at)
VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10)
`, resource.ID, ownerID, resource.Name, resource.Filename, resource.ContentKey,
			resource.ContentSize, resource.ContentType, resource.ContentEncoding, resource.OriginURL, now)
		if err != nil {
			return fmt.Errorf("create resource: %w: %w", ErrInternal, err)
		}
		return nil
	})
	if err != nil {
		if resource.ContentKey != "" {
			_ = d.blobs.Delete(ctx, resource.ContentKey)
		}
		return Resource{}, err
	}
	return resource, nil
}

func (d *Store) ResourceForOwner(ctx context.Context, ownerID, id string) (Resource, error) {
	return d.resourceForOwner(ctx, d.db, ownerID, id)
}

func (d *Store) resourceForOwner(ctx context.Context, q storeQuerier, ownerID, id string) (Resource, error) {
	if !validUUIDs(ownerID, id) {
		return Resource{}, ErrNotFound
	}
	var resource Resource
	err := q.QueryRow(ctx, `
SELECT id, owner_id, name, filename, content_key, content_size, content_type, content_encoding, origin_url, created_at, updated_at
FROM resources
WHERE id = $1 AND owner_id = $2
`, id, ownerID).Scan(
		&resource.ID, &resource.OwnerID, &resource.Name, &resource.Filename,
		&resource.ContentKey, &resource.ContentSize, &resource.ContentType, &resource.ContentEncoding, &resource.OriginURL,
		&resource.CreatedAt, &resource.UpdatedAt,
	)
	if err != nil {
		return Resource{}, translateNotFound(err)
	}
	return resource, nil
}

func (d *Store) ListResources(ctx context.Context, ownerID, query string, limit, offset int) ([]Resource, int, error) {
	query = strings.TrimSpace(query)
	pattern := "%" + escapeLikePattern(query) + "%"
	var total int
	if err := d.db.QueryRow(ctx, `
SELECT COUNT(*)
FROM resources
WHERE owner_id = $1 AND ($2 = '' OR name ILIKE $3 ESCAPE '\' OR filename ILIKE $3 ESCAPE '\')
`, ownerID, query, pattern).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count resources: %w: %w", ErrInternal, err)
	}
	if limit <= 0 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := d.db.Query(ctx, `
SELECT r.id, r.owner_id, r.name, r.filename, r.content_type, r.content_encoding, r.origin_url, r.updated_at,
       (SELECT COUNT(*) FROM links l WHERE l.resource_id = r.id AND l.revoked_at IS NULL
          AND (l.expires_at IS NULL OR l.expires_at > $1)
          AND (l.max_uses = 0 OR l.used_count < l.max_uses))
FROM resources r
WHERE r.owner_id = $2 AND ($3 = '' OR r.name ILIKE $4 ESCAPE '\' OR r.filename ILIKE $4 ESCAPE '\')
ORDER BY r.updated_at DESC, r.id
LIMIT $5 OFFSET $6
`, time.Now().UTC(), ownerID, query, pattern, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	resources := make([]Resource, 0, limit)
	for rows.Next() {
		var resource Resource
		if err := rows.Scan(
			&resource.ID, &resource.OwnerID, &resource.Name, &resource.Filename,
			&resource.ContentType, &resource.ContentEncoding, &resource.OriginURL, &resource.UpdatedAt, &resource.LiveShares,
		); err != nil {
			return nil, 0, err
		}
		resources = append(resources, resource)
	}
	return resources, total, rows.Err()
}

func escapeLikePattern(value string) string {
	return strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(value)
}

func (d *Store) UpdateResource(ctx context.Context, ownerID, id, name, filename string, content []byte, contentEncoding, originURL string) error {
	current, err := d.ResourceForOwner(ctx, ownerID, id)
	if err != nil {
		return err
	}
	replaceContent := content != nil
	if !replaceContent && !current.Remote() && strings.TrimSpace(originURL) == "" {
		content, err = d.ReadContent(ctx, current)
		if err != nil {
			return err
		}
		// Encoding describes the stored bytes. A metadata-only request cannot
		// change it without replacing those bytes, or the next read may decode
		// the same object as an unrelated character set.
		contentEncoding = current.ContentEncoding
	}
	input, err := normalizeResourceInput(name, filename, content, contentEncoding, originURL, d.allowPrivateUpstream)
	if err != nil {
		return err
	}

	// Same ordering as CreateResource: the object is written before the
	// transaction, so no S3 round trip happens under the account lock.
	previousKey := current.ContentKey
	nextKey, nextSize := previousKey, current.ContentSize
	wroteNewObject := false
	if replaceContent && len(input.Content) > 0 {
		nextKey = contentKey(id)
		nextSize = int64(len(input.Content))
		if err := d.blobs.Put(ctx, nextKey, bytes.NewReader(input.Content), nextSize); err != nil {
			return fmt.Errorf("store resource body: %w: %w", ErrInternal, err)
		}
		wroteNewObject = true
	} else if input.OriginURL != "" {
		nextKey, nextSize = "", 0
	}

	now := time.Now().UTC()
	err = d.withTx(ctx, func(tx pgx.Tx) error {
		// The resource is left out of the usage total, so the replacement is
		// measured in place of what it replaces rather than on top of it.
		limit, usage, err := quotaGate(ctx, tx, ownerID, id, now)
		if err != nil {
			return err
		}
		if usage.StorageBytes+nextSize > limit.StorageBytes {
			return storageQuotaError(limit.StorageBytes, usage.StorageBytes, nextSize)
		}
		tag, err := tx.Exec(ctx, `
UPDATE resources
SET name = $1, filename = $2, content_key = $3, content_size = $4, content_type = $5, content_encoding = $6, origin_url = $7, updated_at = $8
WHERE id = $9 AND owner_id = $10
`, input.Name, input.Filename, nextKey, nextSize, input.ContentType, input.Encoding, input.OriginURL, now, id, ownerID)
		if err != nil {
			return fmt.Errorf("update resource: %w: %w", ErrInternal, err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		if wroteNewObject {
			_ = d.blobs.Delete(ctx, nextKey)
		}
		return err
	}
	if previousKey != "" && previousKey != nextKey {
		_ = d.blobs.Delete(ctx, previousKey)
	}
	return nil
}

// DeleteResource removes a resource, the links that point at it and the object
// holding its body. A quota that can only be spent is a trap: without this the
// first account to fill its plan stays full for good.
//
// The access log is deliberately not touched. Its rows carry their own owner
// and a copy of the name they were reached under, so the history of a resource
// survives the resource and stays readable to whoever owned it.
func (d *Store) DeleteResource(ctx context.Context, ownerID, id string) error {
	if !validUUIDs(ownerID, id) {
		return ErrNotFound
	}
	var contentKey string
	err := d.withTx(ctx, func(tx pgx.Tx) error {
		// Links go with it through ON DELETE CASCADE, so every address handed
		// out for this resource stops resolving in the same statement.
		err := tx.QueryRow(ctx, `
DELETE FROM resources WHERE id = $1 AND owner_id = $2 RETURNING content_key
`, id, ownerID).Scan(&contentKey)
		return translateNotFound(err)
	})
	if err != nil {
		return err
	}
	// The object goes last. An object left behind by a failure here costs
	// storage; a row pointing at an object already gone costs a resource.
	if contentKey != "" {
		if err := d.blobs.Delete(ctx, contentKey); err != nil {
			return fmt.Errorf("delete resource body: %w: %w", ErrInternal, err)
		}
	}
	return nil
}

func contentKey(resourceID string) string {
	return resourceID + "/" + uuid.NewString()
}

func (d *Store) OpenContent(ctx context.Context, resource Resource) (io.ReadCloser, int64, error) {
	if resource.ContentKey == "" {
		return io.NopCloser(strings.NewReader("")), 0, nil
	}
	return d.blobs.Open(ctx, resource.ContentKey)
}

func (d *Store) OpenContentRange(ctx context.Context, resource Resource, start, end int64) (io.ReadCloser, int64, error) {
	if resource.ContentKey == "" {
		return io.NopCloser(strings.NewReader("")), 0, nil
	}
	return d.blobs.OpenRange(ctx, resource.ContentKey, start, end)
}

func (d *Store) ReadContent(ctx context.Context, resource Resource) ([]byte, error) {
	reader, _, err := d.OpenContent(ctx, resource)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}
