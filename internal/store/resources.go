package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"time"

	"plainmote/internal/upstream"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var filenameRule = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@+-]{0,127}$`)

func validateFilename(name string) error {
	if name == "" {
		return nil
	}
	if strings.Contains(name, "..") {
		return errors.New("文件名不能包含 ..")
	}
	if !filenameRule.MatchString(name) {
		return errors.New("文件名只能用字母、数字和 . _ - + @，且以字母或数字开头")
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

	err = d.withTx(ctx, func(tx pgx.Tx) error {
		var lockedUserID string
		if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id = $1 FOR UPDATE`, ownerID).Scan(&lockedUserID); err != nil {
			return translateNotFound(err)
		}
		userQuota, err := d.quotaForUser(ctx, tx, ownerID, now)
		if err != nil {
			return err
		}
		if userQuota.Usage.Resources+1 > userQuota.Limit.Resources {
			return fmt.Errorf("quota exceeded")
		}
		if len(input.Content) > 0 {
			resource.ContentKey = contentKey(resource.ID)
			resource.ContentSize = int64(len(input.Content))
			if userQuota.Usage.StorageBytes+resource.ContentSize > userQuota.Limit.StorageBytes {
				return fmt.Errorf("quota exceeded")
			}
			if err := d.blobs.Put(ctx, resource.ContentKey, bytes.NewReader(input.Content), resource.ContentSize); err != nil {
				return fmt.Errorf("store resource body: %w", err)
			}
		}

		_, err = tx.Exec(ctx, `
	INSERT INTO resources(id, owner_id, name, filename, content_key, content_size, content_type, content_encoding, origin_url, created_at, updated_at)
	VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10)
	`, resource.ID, ownerID, resource.Name, resource.Filename, resource.ContentKey,
			resource.ContentSize, resource.ContentType, resource.ContentEncoding, resource.OriginURL, now)
		if err != nil {
			return fmt.Errorf("create resource: %w", err)
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
		return nil, 0, fmt.Errorf("count resources: %w", err)
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

	var (
		previousKey    string
		nextKey        string
		nextSize       int64
		wroteNewObject bool
	)
	err := d.withTx(ctx, func(tx pgx.Tx) error {
		var lockedUserID string
		if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id = $1 FOR UPDATE`, ownerID).Scan(&lockedUserID); err != nil {
			return translateNotFound(err)
		}
		userQuota, err := d.quotaForUser(ctx, tx, ownerID, time.Now().UTC())
		if err != nil {
			return fmt.Errorf("get quota for user: %w", err)
		}

		current, err := d.resourceForOwner(ctx, tx, ownerID, id)

		if err != nil {
			return err
		}
		replaceContent := content != nil
		if !replaceContent && !current.Remote() && strings.TrimSpace(originURL) == "" {
			content, err = d.ReadContent(ctx, current)
			if err != nil {
				return err
			}

			contentEncoding = current.ContentEncoding
		}
		input, err := normalizeResourceInput(name, filename, content, contentEncoding, originURL, d.allowPrivateUpstream)
		if err != nil {
			return err
		}

		previousKey = current.ContentKey
		nextKey, nextSize = previousKey, current.ContentSize
		wroteNewObject = false
		if replaceContent && len(input.Content) > 0 {
			nextKey = contentKey(id)
			nextSize = int64(len(input.Content))
			if nextSize > current.ContentSize &&
				userQuota.Usage.StorageBytes-current.ContentSize+nextSize > userQuota.Limit.StorageBytes {
				return fmt.Errorf("quota exceeded")
			}
			if err := d.blobs.Put(ctx, nextKey, bytes.NewReader(input.Content), nextSize); err != nil {
				return fmt.Errorf("store resource body: %w", err)
			}
			wroteNewObject = true
		} else if input.OriginURL != "" {
			nextKey, nextSize = "", 0
		}

		tag, err := tx.Exec(ctx, `
	UPDATE resources
	SET name = $1, filename = $2, content_key = $3, content_size = $4, content_type = $5, content_encoding = $6, origin_url = $7, updated_at = $8
	WHERE id = $9 AND owner_id = $10
	`, input.Name, input.Filename, nextKey, nextSize, input.ContentType, input.Encoding, input.OriginURL, time.Now().UTC(), id, ownerID)
		if err != nil || tag.RowsAffected() == 0 {
			if err != nil {
				return fmt.Errorf("update resource: %w", err)
			}
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

func contentKey(resourceID string) string {
	return resourceID + "/" + uuid.NewString()
}

func (d *Store) OpenContent(ctx context.Context, resource Resource) (io.ReadCloser, int64, error) {
	if resource.ContentKey == "" {
		return io.NopCloser(strings.NewReader("")), 0, nil
	}
	return d.blobs.Open(ctx, resource.ContentKey)
}

func (d *Store) ReadContent(ctx context.Context, resource Resource) ([]byte, error) {
	reader, _, err := d.OpenContent(ctx, resource)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}
