package store

import (
	"context"
	"fmt"
	"strings"
)

// APIListLimit is the most resources one listing returns to the command line.
const APIListLimit = 500

// APIResources lists an account's resources for the command line, with what
// it shows and needs to pick one: size, version, type. query narrows by name
// or filename, as the resource list does.
func (d *Store) APIResources(ctx context.Context, ownerID, query string) ([]Resource, error) {
	if !validUUIDs(ownerID) {
		return nil, ErrNotFound
	}
	query = strings.TrimSpace(query)
	pattern := "%" + escapeLikePattern(query) + "%"
	return d.apiResources(ctx, `
WHERE r.owner_id = $1 AND ($2 = '' OR r.name ILIKE $3 ESCAPE '\' OR r.filename ILIKE $3 ESCAPE '\')
ORDER BY r.updated_at DESC, r.id
LIMIT $4`, ownerID, query, pattern, APIListLimit)
}

// ResolveResources finds what a reference typed on the command line can mean:
// a full id, an id prefix of at least six characters, or an exact name or
// filename. Every match comes back, so a reference that names two resources
// is reported as ambiguous instead of picking one.
func (d *Store) ResolveResources(ctx context.Context, ownerID, ref string) ([]Resource, error) {
	if !validUUIDs(ownerID) {
		return nil, ErrNotFound
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, nil
	}
	prefix := ""
	if len(ref) >= 6 && isIDPrefix(ref) {
		prefix = escapeLikePattern(strings.ToLower(ref)) + "%"
	}
	return d.apiResources(ctx, `
WHERE r.owner_id = $1 AND (r.id::text = lower($2) OR ($3 <> '' AND r.id::text LIKE $3 ESCAPE '\') OR r.name = $2 OR r.filename = $2)
ORDER BY r.updated_at DESC, r.id
LIMIT 20`, ownerID, ref, prefix)
}

func isIDPrefix(value string) bool {
	for _, r := range strings.ToLower(value) {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && r != '-' {
			return false
		}
	}
	return true
}

func (d *Store) apiResources(ctx context.Context, where string, args ...any) ([]Resource, error) {
	rows, err := d.db.Query(ctx, `
SELECT r.id, r.owner_id, r.name, r.filename, r.content_size, r.content_type, r.content_encoding, r.origin_url,
       r.created_at, r.updated_at, r.version, r.taken_down_at IS NOT NULL
FROM resources r
`+where, args...)
	if err != nil {
		return nil, fmt.Errorf("list resources: %w: %w", ErrInternal, err)
	}
	defer rows.Close()
	var resources []Resource
	for rows.Next() {
		var resource Resource
		if err := rows.Scan(&resource.ID, &resource.OwnerID, &resource.Name, &resource.Filename, &resource.ContentSize,
			&resource.ContentType, &resource.ContentEncoding, &resource.OriginURL, &resource.CreatedAt, &resource.UpdatedAt,
			&resource.Version, &resource.TakenDown); err != nil {
			return nil, fmt.Errorf("list resources: %w: %w", ErrInternal, err)
		}
		resources = append(resources, resource)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list resources: %w: %w", ErrInternal, err)
	}
	return resources, nil
}
