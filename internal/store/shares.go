package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const maxShareTTL = 365 * 24 * time.Hour

type Link struct {
	ID         string
	ResourceID string
	Name       string
	Token      string
	MaxUses    int
	UsedCount  int
	ExpiresAt  *time.Time
	RevokedAt  *time.Time
	LastUsedAt *time.Time
	CreatedAt  time.Time
}

func (l Link) Live(now time.Time) bool {
	switch {
	case l.RevokedAt != nil:
		return false
	case l.ExpiresAt != nil && !now.Before(*l.ExpiresAt):
		return false
	case l.MaxUses > 0 && l.UsedCount >= l.MaxUses:
		return false
	}
	return true
}

func (l Link) Never() bool { return l.ExpiresAt == nil }

func (d *Store) insertLink(ctx context.Context, q storeQuerier, link *Link, now time.Time) error {
	token, err := generateToken()
	if err != nil {
		return err
	}
	ciphertext, err := d.cipher.seal(token)
	if err != nil {
		return err
	}
	link.ID = uuid.NewString()
	link.Token = token
	link.CreatedAt = now
	_, err = q.Exec(ctx, `
INSERT INTO links(
  id, resource_id, name, token_ciphertext, token_hash, max_uses, used_count,
  expires_at, revoked_at, last_used_at, created_at
)
VALUES($1, $2, $3, $4, $5, $6, 0, $7, NULL, NULL, $8)
`, link.ID, link.ResourceID, link.Name, ciphertext, hashToken(token), link.MaxUses, link.ExpiresAt, now)
	if err != nil {
		return fmt.Errorf("create link: %w", err)
	}
	return nil
}

const linkColumns = `id, resource_id, name, token_ciphertext, max_uses, used_count, expires_at, revoked_at, last_used_at, created_at`

type rowScanner interface{ Scan(...any) error }

func timePointer(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	t := value.Time
	return &t
}

func (d *Store) scanLink(row rowScanner) (Link, error) {
	var link Link
	var ciphertext []byte
	var expires, revoked, lastUsed pgtype.Timestamptz
	if err := row.Scan(
		&link.ID, &link.ResourceID, &link.Name, &ciphertext, &link.MaxUses,
		&link.UsedCount, &expires, &revoked, &lastUsed, &link.CreatedAt,
	); err != nil {
		return Link{}, err
	}
	token, err := d.cipher.open(ciphertext)
	if err != nil {
		return Link{}, err
	}
	link.Token = token
	link.ExpiresAt = timePointer(expires)
	link.RevokedAt = timePointer(revoked)
	link.LastUsedAt = timePointer(lastUsed)
	return link, nil
}

func (d *Store) assertOwnsResource(ctx context.Context, ownerID, resourceID string) error {
	if !validUUIDs(ownerID, resourceID) {
		return ErrNotFound
	}
	var found string
	err := d.db.QueryRow(ctx, `SELECT id FROM resources WHERE id = $1 AND owner_id = $2`, resourceID, ownerID).Scan(&found)
	return translateNotFound(err)
}

func validateShareTerms(ttl time.Duration, maxUses int) error {
	if ttl < 0 {
		return errors.New("存活时长不能为负")
	}
	if ttl > maxShareTTL {
		return errors.New("存活时长最长一年，如需更久请选择「永不过期」")
	}
	if maxUses < 0 {
		return errors.New("使用次数不能为负")
	}
	return nil
}

func shareExpiry(now time.Time, ttl time.Duration) *time.Time {
	if ttl <= 0 {
		return nil
	}
	deadline := now.Add(ttl)
	return &deadline
}

func (d *Store) CreateShare(ctx context.Context, ownerID, resourceID, name string, ttl time.Duration, maxUses int) (Link, error) {
	if err := d.assertOwnsResource(ctx, ownerID, resourceID); err != nil {
		return Link{}, err
	}
	if err := validateShareTerms(ttl, maxUses); err != nil {
		return Link{}, err
	}
	now := time.Now().UTC()
	link := Link{ResourceID: resourceID, Name: name, MaxUses: maxUses, ExpiresAt: shareExpiry(now, ttl)}
	err := d.insertLink(ctx, d.db, &link, now)
	return link, err
}

func (d *Store) UpdateShare(ctx context.Context, ownerID, resourceID, linkID, name string, ttl time.Duration, maxUses int) error {
	if err := d.assertOwnsResource(ctx, ownerID, resourceID); err != nil {
		return err
	}
	if err := validateShareTerms(ttl, maxUses); err != nil {
		return err
	}
	if !validUUIDs(linkID) {
		return ErrNotFound
	}
	now := time.Now().UTC()
	tag, err := d.db.Exec(ctx, `
UPDATE links
SET name = $1, max_uses = $2, expires_at = $3, used_count = 0
WHERE id = $4 AND resource_id = $5 AND revoked_at IS NULL
`, name, maxUses, shareExpiry(now, ttl), linkID, resourceID)
	if err != nil {
		return fmt.Errorf("update share: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (d *Store) RevokeLink(ctx context.Context, ownerID, resourceID, linkID string) error {
	if err := d.assertOwnsResource(ctx, ownerID, resourceID); err != nil {
		return err
	}
	if !validUUIDs(linkID) {
		return ErrNotFound
	}
	tag, err := d.db.Exec(ctx, `
UPDATE links SET revoked_at = $1
WHERE id = $2 AND resource_id = $3 AND revoked_at IS NULL
`, time.Now().UTC(), linkID, resourceID)
	if err != nil {
		return fmt.Errorf("revoke link: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (d *Store) RevokeShares(ctx context.Context, ownerID, resourceID string) error {
	if err := d.assertOwnsResource(ctx, ownerID, resourceID); err != nil {
		return err
	}
	_, err := d.db.Exec(ctx, `
UPDATE links SET revoked_at = $1
WHERE resource_id = $2 AND revoked_at IS NULL
`, time.Now().UTC(), resourceID)
	if err != nil {
		return fmt.Errorf("revoke shares: %w", err)
	}
	return nil
}

func (d *Store) ListShares(ctx context.Context, ownerID, resourceID string, now time.Time) ([]Link, error) {
	if err := d.assertOwnsResource(ctx, ownerID, resourceID); err != nil {
		return nil, err
	}
	rows, err := d.db.Query(ctx, `SELECT `+linkColumns+` FROM links
WHERE resource_id = $1 AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > $2)
ORDER BY created_at DESC, id DESC`, resourceID, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	links := make([]Link, 0)
	for rows.Next() {
		link, err := d.scanLink(rows)
		if err != nil {
			return nil, err
		}
		if link.Live(now) {
			links = append(links, link)
		}
	}
	return links, rows.Err()
}

func (d *Store) ConsumeToken(ctx context.Context, token string, meta RequestMeta, now time.Time) (result ConsumeResult, err error) {
	hash := hashToken(token)
	err = d.withTx(ctx, func(tx pgx.Tx) error {
		var linkID, linkName, resourceID string
		err := tx.QueryRow(ctx, `
UPDATE links
SET used_count = used_count + 1, last_used_at = $1
WHERE token_hash = $2
  AND revoked_at IS NULL
  AND (expires_at IS NULL OR expires_at > $1)
  AND (max_uses = 0 OR used_count < max_uses)
RETURNING id, resource_id, name
`, now, hash).Scan(&linkID, &resourceID, &linkName)
		if errors.Is(err, pgx.ErrNoRows) {
			return d.recordRefusalTx(ctx, tx, hash, meta, now, &result)
		}
		if err != nil {
			return err
		}
		resource, err := resourceByIDTx(ctx, tx, resourceID)
		if err != nil {
			return err
		}
		result.Resource = resource
		result.LinkID = linkID
		result.LinkName = displayLinkName(linkName)
		result.Allowed = true
		result.Reason = OutcomeSuccess
		return nil
	})
	return result, err
}

// ResumeDelivery validates a short-lived grant issued after a counted use.
// The use limit is intentionally not checked again: one browser navigation can
// require several HTTP and Range requests. Expiry and revocation are checked
// on every request so the owner can still stop an in-progress grant.
func (d *Store) ResumeDelivery(ctx context.Context, token, grant string, now time.Time) (ConsumeResult, bool, error) {
	linkID, ok := d.openDeliveryGrant(token, grant, now)
	if !ok {
		return ConsumeResult{}, false, nil
	}
	var result ConsumeResult
	var linkName string
	err := d.db.QueryRow(ctx, `
SELECT l.name,
       r.id, r.owner_id, r.name, r.filename, r.content_key, r.content_size,
       r.content_type, r.content_encoding, r.origin_url
FROM links l
JOIN resources r ON r.id = l.resource_id
WHERE l.id = $1 AND l.token_hash = $2
  AND l.revoked_at IS NULL
  AND (l.expires_at IS NULL OR l.expires_at > $3)
`, linkID, hashToken(token), now).Scan(
		&linkName,
		&result.Resource.ID, &result.Resource.OwnerID, &result.Resource.Name,
		&result.Resource.Filename, &result.Resource.ContentKey, &result.Resource.ContentSize,
		&result.Resource.ContentType, &result.Resource.ContentEncoding, &result.Resource.OriginURL,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ConsumeResult{}, false, nil
	}
	if err != nil {
		return ConsumeResult{}, false, err
	}
	result.Allowed = true
	result.LinkID = linkID
	result.LinkName = displayLinkName(linkName)
	result.Reason = OutcomeSuccess
	return result, true, nil
}

func (d *Store) recordRefusalTx(ctx context.Context, tx pgx.Tx, hash string, meta RequestMeta, now time.Time, result *ConsumeResult) error {
	var linkID, resourceID, name, ownerID, resourceName, resourceFile string
	var expires, revoked pgtype.Timestamptz
	var maxUses, usedCount int
	// The resource comes along so the refusal is recorded against an owner and
	// keeps the name it was reached under. The join is inner because a link
	// cannot outlive its resource - links cascade with it - and a row that
	// somehow did would have nothing to serve, so reading it as an unissued
	// token is the honest answer.
	err := tx.QueryRow(ctx, `
SELECT l.id, l.resource_id, l.name, l.expires_at, l.revoked_at, l.max_uses, l.used_count,
       r.owner_id, r.name, r.filename
FROM links l
JOIN resources r ON r.id = l.resource_id
WHERE l.token_hash = $1
`, hash).Scan(&linkID, &resourceID, &name, &expires, &revoked, &maxUses, &usedCount,
		&ownerID, &resourceName, &resourceFile)
	if errors.Is(err, pgx.ErrNoRows) {
		result.Reason = ReasonInvalid
		return nil
	}
	if err != nil {
		return err
	}
	result.LinkID = linkID
	result.LinkName = displayLinkName(name)
	reason, detail := OutcomeExhausted, "link is used up"
	switch {
	case revoked.Valid:
		reason, detail = OutcomeRevoked, "link was revoked"
	case expires.Valid && !now.Before(expires.Time):
		reason, detail = OutcomeExpired, "link expired"
	}
	result.Reason = reason
	return insertAccessTx(ctx, tx, AccessEvent{
		OwnerID: ownerID, ResourceID: resourceID, ResourceName: resourceName, ResourceFile: resourceFile,
		LinkID: linkID, LinkName: result.LinkName,
		Outcome: reason, Status: httpStatusUnauthorized, Detail: detail,
	}, meta, now)
}

func resourceByIDTx(ctx context.Context, tx pgx.Tx, id string) (Resource, error) {
	var resource Resource
	err := tx.QueryRow(ctx, `
SELECT id, owner_id, name, filename, content_key, content_size, content_type, content_encoding, origin_url
FROM resources WHERE id = $1
`, id).Scan(
		&resource.ID, &resource.OwnerID, &resource.Name, &resource.Filename,
		&resource.ContentKey, &resource.ContentSize, &resource.ContentType, &resource.ContentEncoding, &resource.OriginURL,
	)
	return resource, err
}

func displayLinkName(name string) string {
	if name == "" {
		return "未命名分享"
	}
	return name
}
