package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// What a personal access token may do. A read token lists and reads; a
// write token also creates, edits, tags, trashes and shares resources.
const (
	TokenScopeRead  = "read"
	TokenScopeWrite = "write"
)

const (
	// TokenLifetime is the default token lifetime when one is chosen.
	TokenLifetime = 90 * 24 * time.Hour
	// MaxTokenTTL is the longest finite lifetime for a personal access token;
	// zero means no expiry.
	MaxTokenTTL = 365 * 24 * time.Hour
	// MaxAPITokens bounds how many live API tokens one account may hold.
	MaxAPITokens = 50

	// TokenPrefix marks a PlainMote token wherever it ends up, so a leak can
	// be recognised by secret scanners and by eye.
	TokenPrefix = "pmt_"

	deviceTextMaxRunes = 64
	// tokenTouchEvery spaces out the writes that record a token's last use:
	// a script can make several requests a second.
	tokenTouchEvery = time.Minute
)

type APIToken struct {
	ID            string
	UserID        string
	Scope         string
	DeviceName    string
	DeviceOS      string
	ClientVersion string
	CreatedAt     time.Time
	ExpiresAt     *time.Time
	LastUsedAt    *time.Time
	LastUsedIP    string
}

// CanWrite reports whether the token may change anything.
func (t APIToken) CanWrite() bool { return t.Scope == TokenScopeWrite }

// ErrBadScope is a request for a scope that does not exist.
var ErrBadScope = refusal("unknown token scope")

// ErrTokenNameRequired refuses a token without a name to identify it in the list.
var ErrTokenNameRequired = refusal("请输入令牌名称")

// ErrTokenLimitReached refuses creating another token once an account is at MaxAPITokens.
var ErrTokenLimitReached = refusalf("每个账号最多同时持有 %d 个有效令牌", MaxAPITokens)

// ErrBadTokenTTL refuses a negative or overlong token lifetime.
var ErrBadTokenTTL = refusal("令牌有效期无效")

// deviceText keeps a token's label short and printable: it is shown on the
// account page.
func deviceText(value string) string {
	var out strings.Builder
	count := 0
	for _, r := range strings.TrimSpace(value) {
		if !unicode.IsPrint(r) {
			continue
		}
		if count == deviceTextMaxRunes {
			break
		}
		out.WriteRune(r)
		count++
	}
	return out.String()
}

// CreateAPIToken issues a personal access token for an account directly from
// its settings page. ttl == 0 means the token never expires. Only the hash is
// stored; the plaintext secret is returned once to show to the owner.
func (d *Store) CreateAPIToken(ctx context.Context, userID, name, scope string, ttl time.Duration, now time.Time) (string, APIToken, error) {
	if !validUUIDs(userID) {
		return "", APIToken{}, ErrNotFound
	}
	if scope != TokenScopeRead && scope != TokenScopeWrite {
		return "", APIToken{}, ErrBadScope
	}
	cleanName := deviceText(name)
	if cleanName == "" {
		return "", APIToken{}, ErrTokenNameRequired
	}
	if ttl < 0 || ttl > MaxTokenTTL {
		return "", APIToken{}, ErrBadTokenTTL
	}
	secret, err := randomSecret(32)
	if err != nil {
		return "", APIToken{}, fmt.Errorf("create api token: %w", err)
	}
	full := TokenPrefix + secret
	var expires *time.Time
	if ttl > 0 {
		at := now.Add(ttl)
		expires = &at
	}
	token := APIToken{
		ID: uuid.NewString(), UserID: userID, Scope: scope, DeviceName: cleanName,
		CreatedAt: now, ExpiresAt: expires,
	}
	err = d.withTx(ctx, func(tx pgx.Tx) error {
		var suspended bool
		if err := tx.QueryRow(ctx, `SELECT suspended_at IS NOT NULL FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&suspended); err != nil {
			return translateNotFound(err)
		}
		if suspended {
			return ErrNotFound
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM api_tokens WHERE user_id = $1 AND (expires_at IS NULL OR expires_at > $2)`, userID, now).Scan(&count); err != nil {
			return fmt.Errorf("count tokens: %w: %w", ErrInternal, err)
		}
		if count >= MaxAPITokens {
			return ErrTokenLimitReached
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO api_tokens(id, user_id, token_hash, scope, device_name, device_os, client_version, created_at, expires_at)
VALUES($1, $2, $3, $4, $5, '', '', $6, $7)
`, token.ID, token.UserID, hashToken(full), token.Scope, token.DeviceName, token.CreatedAt, token.ExpiresAt); err != nil {
			return fmt.Errorf("save token: %w: %w", ErrInternal, err)
		}
		return nil
	})
	if err != nil {
		return "", APIToken{}, err
	}
	return full, token, nil
}

// TokenUser resolves a presented token to its account. An expired token, a
// revoked one and one whose account is suspended all read as not found. The
// last use is recorded at most once a minute, or at once from a new address.
func (d *Store) TokenUser(ctx context.Context, token, ip string, now time.Time) (User, APIToken, error) {
	if !strings.HasPrefix(token, TokenPrefix) || len(token) > 128 {
		return User{}, APIToken{}, ErrNotFound
	}
	var user User
	var apiToken APIToken
	var expires, lastUsed pgtype.Timestamptz
	err := d.db.QueryRow(ctx, `
SELECT t.id, t.scope, t.device_name, t.device_os, t.client_version, t.created_at, t.expires_at, t.last_used_at, t.last_used_ip,
       u.id, COALESCE(u.github_id, ''), u.login, u.name, u.avatar_url
FROM api_tokens t
JOIN users u ON u.id = t.user_id
WHERE t.token_hash = $1 AND (t.expires_at IS NULL OR t.expires_at > $2) AND u.suspended_at IS NULL
`, hashToken(token), now).Scan(&apiToken.ID, &apiToken.Scope, &apiToken.DeviceName, &apiToken.DeviceOS,
		&apiToken.ClientVersion, &apiToken.CreatedAt, &expires, &lastUsed, &apiToken.LastUsedIP,
		&user.ID, &user.GitHubID, &user.Login, &user.Name, &user.AvatarURL)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, APIToken{}, ErrNotFound
	}
	if err != nil {
		return User{}, APIToken{}, fmt.Errorf("read token: %w: %w", ErrInternal, err)
	}
	apiToken.UserID = user.ID
	apiToken.ExpiresAt = timePointer(expires)
	ip = limitAccessText(ip, accessIPMaxBytes)
	if !lastUsed.Valid || now.Sub(lastUsed.Time) >= tokenTouchEvery || apiToken.LastUsedIP != ip {
		if _, err := d.db.Exec(ctx, `UPDATE api_tokens SET last_used_at = $2, last_used_ip = $3 WHERE id = $1`, apiToken.ID, now, ip); err != nil {
			return User{}, APIToken{}, fmt.Errorf("record token use: %w: %w", ErrInternal, err)
		}
		lastUsed = pgtype.Timestamptz{Time: now, Valid: true}
		apiToken.LastUsedIP = ip
	}
	at := lastUsed.Time
	apiToken.LastUsedAt = &at
	return user, apiToken, nil
}

// ListAPITokens is the account page's list: every live token, newest first.
func (d *Store) ListAPITokens(ctx context.Context, userID string, now time.Time) ([]APIToken, error) {
	if !validUUIDs(userID) {
		return nil, ErrNotFound
	}
	rows, err := d.db.Query(ctx, `
SELECT id, scope, device_name, device_os, client_version, created_at, expires_at, last_used_at, last_used_ip
FROM api_tokens WHERE user_id = $1 AND (expires_at IS NULL OR expires_at > $2)
ORDER BY created_at DESC
`, userID, now)
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w: %w", ErrInternal, err)
	}
	defer rows.Close()
	var tokens []APIToken
	for rows.Next() {
		token := APIToken{UserID: userID}
		var expires, lastUsed pgtype.Timestamptz
		if err := rows.Scan(&token.ID, &token.Scope, &token.DeviceName, &token.DeviceOS, &token.ClientVersion,
			&token.CreatedAt, &expires, &lastUsed, &token.LastUsedIP); err != nil {
			return nil, fmt.Errorf("list tokens: %w: %w", ErrInternal, err)
		}
		token.ExpiresAt = timePointer(expires)
		token.LastUsedAt = timePointer(lastUsed)
		tokens = append(tokens, token)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tokens: %w: %w", ErrInternal, err)
	}
	return tokens, nil
}

// HasAPITokens is whether any live API token exists for the account.
func (d *Store) HasAPITokens(ctx context.Context, userID string, now time.Time) (bool, error) {
	if !validUUIDs(userID) {
		return false, ErrNotFound
	}
	var has bool
	err := d.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM api_tokens WHERE user_id = $1 AND (expires_at IS NULL OR expires_at > $2))`, userID, now).Scan(&has)
	if err != nil {
		return false, fmt.Errorf("check tokens: %w: %w", ErrInternal, err)
	}
	return has, nil
}

// APITokenForOwner reads one live token of an account, for the revoke dialog.
func (d *Store) APITokenForOwner(ctx context.Context, userID, tokenID string, now time.Time) (APIToken, error) {
	tokens, err := d.ListAPITokens(ctx, userID, now)
	if err != nil {
		return APIToken{}, err
	}
	for _, token := range tokens {
		if token.ID == tokenID {
			return token, nil
		}
	}
	return APIToken{}, ErrNotFound
}

// RevokeAPIToken deletes a token from the account page. The next request made
// with it is refused.
func (d *Store) RevokeAPIToken(ctx context.Context, userID, tokenID string) error {
	if !validUUIDs(userID, tokenID) {
		return ErrNotFound
	}
	tag, err := d.db.Exec(ctx, `DELETE FROM api_tokens WHERE id = $1 AND user_id = $2`, tokenID, userID)
	if err != nil {
		return fmt.Errorf("revoke token: %w: %w", ErrInternal, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// RevokeAPITokenByID deletes the token a request was made with.
func (d *Store) RevokeAPITokenByID(ctx context.Context, tokenID string) error {
	if !validUUIDs(tokenID) {
		return ErrNotFound
	}
	if _, err := d.db.Exec(ctx, `DELETE FROM api_tokens WHERE id = $1`, tokenID); err != nil {
		return fmt.Errorf("revoke token: %w: %w", ErrInternal, err)
	}
	return nil
}
