package store

import (
	"context"
	"errors"
	"fmt"
	"os"
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
	// TermsAt is when the current expiry and use limit were set: the link's
	// creation, or the last change to its terms. ExpiresAt counts from here.
	TermsAt time.Time
	// Unreadable marks a link whose stored token no longer decrypts with the
	// configured key. Delivery looks links up by hash and never decrypts, so
	// the link still works for whoever holds the address; only its owner can
	// no longer see what that address is.
	Unreadable bool
}

// errTokenUnreadable is a stored token that the configured key cannot open -
// almost always PLAINMOTE_TOKEN_KEY having been changed or restored wrongly.
var errTokenUnreadable = errors.New("share token cannot be decrypted with the configured key")

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

// Ending says why a link stopped working and when: "revoked", "expired" or
// "exhausted", whichever came first. A live link has no ending and returns "".
func (l Link) Ending(now time.Time) (string, time.Time) {
	reason, at := "", time.Time{}
	consider := func(r string, t *time.Time) {
		if t != nil && !t.After(now) && (reason == "" || t.Before(at)) {
			reason, at = r, *t
		}
	}
	consider("revoked", l.RevokedAt)
	consider("expired", l.ExpiresAt)
	if l.MaxUses > 0 && l.UsedCount >= l.MaxUses {
		consider("exhausted", l.LastUsedAt)
	}
	return reason, at
}

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
	link.TermsAt = now
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

const linkColumns = `id, resource_id, name, token_ciphertext, max_uses, used_count, expires_at, revoked_at, last_used_at, created_at, COALESCE(terms_at, created_at)`

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
		&link.UsedCount, &expires, &revoked, &lastUsed, &link.CreatedAt, &link.TermsAt,
	); err != nil {
		return Link{}, err
	}
	link.ExpiresAt = timePointer(expires)
	link.RevokedAt = timePointer(revoked)
	link.LastUsedAt = timePointer(lastUsed)
	token, err := d.cipher.open(ciphertext)
	if err != nil {
		// Everything but the token is still returned, so a caller that can
		// live without the address - a list the owner revokes from - can keep
		// the row instead of failing the page it is on.
		link.Unreadable = true
		return link, fmt.Errorf("%w: link %s: %v", errTokenUnreadable, link.ID, err)
	}
	link.Token = token
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

// ErrTakenDown refuses a new link for a resource the operator took down.
var ErrTakenDown = errors.New("此资源已被下架，不能创建分享链接")

func (d *Store) CreateShare(ctx context.Context, ownerID, resourceID, name string, ttl time.Duration, maxUses int) (Link, error) {
	if err := d.assertOwnsResource(ctx, ownerID, resourceID); err != nil {
		return Link{}, err
	}
	var takenDown bool
	if err := d.db.QueryRow(ctx, `SELECT taken_down_at IS NOT NULL FROM resources WHERE id = $1`, resourceID).Scan(&takenDown); err != nil {
		return Link{}, translateNotFound(err)
	}
	if takenDown {
		return Link{}, ErrTakenDown
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
SET name = $1, max_uses = $2, expires_at = $3, used_count = 0, terms_at = $4
WHERE id = $5 AND resource_id = $6 AND revoked_at IS NULL
`, name, maxUses, shareExpiry(now, ttl), now, linkID, resourceID)
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

// DeleteEndedLink removes a link that no longer works - revoked, expired or
// used up - from the resource. A live link has to be revoked first, so this
// can never cut off someone who is still meant to have access. Its access
// history stays: the log keeps the link's name and loses only the reference.
func (d *Store) DeleteEndedLink(ctx context.Context, ownerID, resourceID, linkID string, now time.Time) error {
	if err := d.assertOwnsResource(ctx, ownerID, resourceID); err != nil {
		return err
	}
	if !validUUIDs(linkID) {
		return ErrNotFound
	}
	tag, err := d.db.Exec(ctx, `
DELETE FROM links
WHERE id = $1 AND resource_id = $2
  AND (revoked_at IS NOT NULL
    OR (expires_at IS NOT NULL AND expires_at <= $3)
    OR (max_uses > 0 AND used_count >= max_uses))
`, linkID, resourceID, now)
	if err != nil {
		return fmt.Errorf("delete link: %w", err)
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
	return d.collectLinks(rows, func(link Link) bool { return link.Live(now) })
}

// ListEndedShares returns the links of a resource that stopped working since
// the given time - revoked, expired or used up - most recently ended first,
// at most limit of them. The owner sees what just went dead next to what is
// still live; older ones stay in the access history.
func (d *Store) ListEndedShares(ctx context.Context, ownerID, resourceID string, now, since time.Time, limit int) ([]Link, error) {
	if err := d.assertOwnsResource(ctx, ownerID, resourceID); err != nil {
		return nil, err
	}
	rows, err := d.db.Query(ctx, `
WITH ended AS (
  SELECT *, LEAST(
    revoked_at,
    CASE WHEN expires_at <= $2 THEN expires_at END,
    CASE WHEN max_uses > 0 AND used_count >= max_uses THEN last_used_at END
  ) AS ended_at
  FROM links WHERE resource_id = $1
)
SELECT `+linkColumns+` FROM ended
WHERE ended_at IS NOT NULL AND ended_at <= $2 AND ended_at >= $3
ORDER BY ended_at DESC, id DESC
LIMIT $4`, resourceID, now, since, limit)
	if err != nil {
		return nil, err
	}
	return d.collectLinks(rows, func(link Link) bool { return !link.Live(now) })
}

func (d *Store) collectLinks(rows pgx.Rows, keep func(Link) bool) ([]Link, error) {
	defer rows.Close()
	links := make([]Link, 0)
	for rows.Next() {
		link, err := d.scanLink(rows)
		if errors.Is(err, errTokenUnreadable) {
			// One undecryptable row used to fail the whole resource page. The
			// link still works for its holder and has to stay revocable, so it
			// is listed without an address and the cause goes to the log.
			fmt.Fprintf(os.Stderr, "list shares: %v\n", err)
			err = nil
		}
		if err != nil {
			return nil, err
		}
		if keep(link) {
			links = append(links, link)
		}
	}
	return links, rows.Err()
}

func (d *Store) ConsumeToken(ctx context.Context, token string, meta RequestMeta, now time.Time) (result ConsumeResult, err error) {
	hash := hashToken(token)
	err = d.withTx(ctx, func(tx pgx.Tx) error {
		var linkID, linkName, resourceID string
		// A link delivers only while its resource is up and its owner is not
		// suspended; otherwise nothing is spent and the refusal says why.
		err := tx.QueryRow(ctx, `
UPDATE links l
SET used_count = l.used_count + 1, last_used_at = $1
FROM resources r JOIN users u ON u.id = r.owner_id
WHERE l.token_hash = $2
  AND r.id = l.resource_id
  AND r.taken_down_at IS NULL
  AND u.suspended_at IS NULL
  AND l.revoked_at IS NULL
  AND (l.expires_at IS NULL OR l.expires_at > $1)
  AND (l.max_uses = 0 OR l.used_count < l.max_uses)
RETURNING l.id, l.resource_id, l.name
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
		// The use and its record are one write: a use is never spent without
		// a row saying who spent it, and the caller sends nothing until both
		// are committed. Whatever later goes wrong with the delivery is then
		// a correction to an existing row, never a row that failed to appear.
		event := AccessEvent{
			ID: uuid.NewString(), OwnerID: resource.OwnerID, ResourceID: resource.ID,
			ResourceName: resource.Name, ResourceFile: resource.Filename,
			LinkID: linkID, LinkName: linkName,
			Outcome: OutcomeSuccess, Status: httpStatusOK, Detail: "link accepted",
		}
		if !resource.Remote() {
			event.Version = resource.Version
		}
		if err := insertAccessTx(ctx, tx, event, meta, now); err != nil {
			return err
		}
		result.Resource = resource
		result.LinkID = linkID
		result.LinkName = linkName
		result.AccessID = event.ID
		result.Allowed = true
		result.Reason = OutcomeSuccess
		return nil
	})
	if err != nil {
		return ConsumeResult{}, err
	}
	return result, nil
}

func (d *Store) recordRefusalTx(ctx context.Context, tx pgx.Tx, hash string, meta RequestMeta, now time.Time, result *ConsumeResult) error {
	var linkID, resourceID, name, ownerID, resourceName, resourceFile string
	var expires, revoked pgtype.Timestamptz
	var maxUses, usedCount int
	var takenDown, suspended bool
	// The resource comes along so the refusal is recorded against an owner and
	// keeps the name it was reached under. The join is inner because a link
	// cannot outlive its resource - links cascade with it - and a row that
	// somehow did would have nothing to serve, so reading it as an unissued
	// token is the honest answer.
	err := tx.QueryRow(ctx, `
SELECT l.id, l.resource_id, l.name, l.expires_at, l.revoked_at, l.max_uses, l.used_count,
       r.owner_id, r.name, r.filename, r.taken_down_at IS NOT NULL, u.suspended_at IS NOT NULL
FROM links l
JOIN resources r ON r.id = l.resource_id
JOIN users u ON u.id = r.owner_id
WHERE l.token_hash = $1
`, hash).Scan(&linkID, &resourceID, &name, &expires, &revoked, &maxUses, &usedCount,
		&ownerID, &resourceName, &resourceFile, &takenDown, &suspended)
	if errors.Is(err, pgx.ErrNoRows) {
		result.Reason = ReasonInvalid
		return nil
	}
	if err != nil {
		return err
	}
	result.LinkID = linkID
	result.LinkName = name
	reason, detail := OutcomeExhausted, "link is used up"
	switch {
	case takenDown:
		reason, detail = OutcomeTakenDown, "resource was taken down"
	case suspended:
		reason, detail = OutcomeSuspended, "owner is suspended"
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
SELECT id, owner_id, name, filename, content_key, content_size, content_type, content_encoding, origin_url, created_at, updated_at, version
FROM resources WHERE id = $1
`, id).Scan(
		&resource.ID, &resource.OwnerID, &resource.Name, &resource.Filename,
		&resource.ContentKey, &resource.ContentSize, &resource.ContentType, &resource.ContentEncoding, &resource.OriginURL,
		&resource.CreatedAt, &resource.UpdatedAt, &resource.Version,
	)
	return resource, err
}

// Presentations a browser can be given in place of the bytes.
const (
	ShellNone      = ""
	ShellMedia     = "media"
	ShellEncrypted = "encrypted"
)

// ShareShell only selects the browser presentation: the media player for audio
// and video, the decryption page for an encrypted share, or none. It grants no
// access to content - the page's full-body GET must still pass ConsumeToken -
// and says nothing about whether the link is still valid.
func (d *Store) ShareShell(ctx context.Context, token string) (string, error) {
	var shell string
	err := d.db.QueryRow(ctx, `
SELECT CASE
  WHEN r.content_type = $2 THEN 'encrypted'
  WHEN r.content_type LIKE 'audio/%' OR r.content_type LIKE 'video/%' THEN 'media'
  ELSE '' END
FROM links l JOIN resources r ON r.id = l.resource_id JOIN users u ON u.id = r.owner_id
WHERE l.token_hash = $1 AND r.origin_url = '' AND r.taken_down_at IS NULL AND u.suspended_at IS NULL`, hashToken(token), EncryptedContentType).Scan(&shell)
	if errors.Is(err, pgx.ErrNoRows) {
		return ShellNone, nil
	}
	return shell, err
}
