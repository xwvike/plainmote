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
// or filename and tag to those carrying it, as the resource list does.
func (d *Store) APIResources(ctx context.Context, ownerID, query, tag string) ([]Resource, error) {
	if !validUUIDs(ownerID) {
		return nil, ErrNotFound
	}
	query = strings.TrimSpace(query)
	pattern := "%" + escapeLikePattern(query) + "%"
	return d.apiResources(ctx, `
WHERE `+resourceFilterSQL+`
ORDER BY r.updated_at DESC, r.id
LIMIT $6`, ownerID, query, pattern, "", strings.TrimSpace(tag), APIListLimit)
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
  AND `+liveResource+`
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
SELECT `+resourceColumns+`
FROM resources r
`+where, args...)
	if err != nil {
		return nil, fmt.Errorf("list resources: %w: %w", ErrInternal, err)
	}
	defer rows.Close()
	var resources []Resource
	for rows.Next() {
		resource, err := scanResource(rows)
		if err != nil {
			return nil, fmt.Errorf("list resources: %w: %w", ErrInternal, err)
		}
		resources = append(resources, resource)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list resources: %w: %w", ErrInternal, err)
	}
	return resources, nil
}

// ShareLinkResource is the resource one of the owner's own share links leads
// to, found without using the link up. A link to another account's resource
// is not found, the same as a link that does not exist.
func (d *Store) ShareLinkResource(ctx context.Context, ownerID, token string) (Resource, error) {
	if !validUUIDs(ownerID) || token == "" || len(token) > 256 {
		return Resource{}, ErrNotFound
	}
	resource, err := scanResource(d.db.QueryRow(ctx, `
SELECT `+resourceColumns+`
FROM links l JOIN resources r ON r.id = l.resource_id
WHERE l.token_hash = $1 AND r.owner_id = $2 AND `+liveResource+`
`, hashToken(token), ownerID))
	if err != nil {
		return Resource{}, translateNotFound(err)
	}
	return resource, nil
}
