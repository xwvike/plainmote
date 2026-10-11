package store

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// The authorization server MCP clients sign in through. It is separate from
// the command line's tokens on purpose: a token issued here names the MCP
// endpoint as its audience and is refused by the command line's interface,
// and a command line token is refused here.

// What an MCP client may do. read is always part of a grant; write saves and
// creates; share makes and ends share links, which put content in front of
// people outside the account, so it is asked for on its own.
const (
	OAuthScopeRead  = "read"
	OAuthScopeWrite = "write"
	OAuthScopeShare = "share"
)

// OAuthScopes is every scope, in the order a grant's scope is written.
var OAuthScopes = []string{OAuthScopeRead, OAuthScopeWrite, OAuthScopeShare}

const (
	OAuthCodeLifetime    = time.Minute
	OAuthAccessLifetime  = time.Hour
	OAuthRefreshLifetime = 30 * 24 * time.Hour
	// OAuthDocumentFresh is how long a client's metadata document is used
	// before it is fetched again.
	OAuthDocumentFresh = time.Hour
	// oauthClientIdle is how long a client nobody has granted anything is
	// kept: a registration that never led to a consent is a stray.
	oauthClientIdle = 24 * time.Hour

	OAuthClientPrefix  = "pmc_"
	OAuthAccessPrefix  = "pma_"
	OAuthRefreshPrefix = "pmr_"

	OAuthClientRegistered = "registered"
	OAuthClientDocument   = "document"

	oauthMaxRedirectURIs = 10
	oauthMaxURIBytes     = 2000
	oauthTokenMaxBytes   = 128
)

// ErrOAuthGrant is a code or refresh token that does not lead to a grant -
// unknown, used, expired, or presented by another client. OAuth names all of
// these invalid_grant and says no more, and neither does this.
var ErrOAuthGrant = errors.New("store: invalid grant")

// ErrOAuthScope is a refresh asking for more than its grant holds.
var ErrOAuthScope = errors.New("store: invalid scope")

type OAuthClient struct {
	ID           string
	Kind         string
	Name         string
	ClientURI    string
	RedirectURIs []string
	CreatedAt    time.Time
	FetchedAt    time.Time
}

// RedirectURI reports whether a client may be sent back to uri. The match is
// exact, except that a client listening on the loopback address may be on any
// port: a native app takes whichever is free when it starts (RFC 8252, 7.3).
func (c OAuthClient) RedirectURI(uri string) bool {
	for _, registered := range c.RedirectURIs {
		if registered == uri || sameLoopback(registered, uri) {
			return true
		}
	}
	return false
}

func sameLoopback(registered, requested string) bool {
	a, errA := url.Parse(registered)
	b, errB := url.Parse(requested)
	if errA != nil || errB != nil || a.Scheme != "http" || b.Scheme != "http" || !loopbackHost(a.Hostname()) {
		return false
	}
	return a.Hostname() == b.Hostname() && a.Path == b.Path && a.RawQuery == b.RawQuery
}

func loopbackHost(host string) bool {
	return host == "127.0.0.1" || host == "::1" || host == "localhost"
}

// unsafeSchemes run or reveal something in the browser itself rather than
// handing an address to an app.
var unsafeSchemes = []string{"javascript", "data", "vbscript", "file", "about", "blob", "filesystem", "view-source"}

// ValidRedirectURI is an address a client may name for its way back: https,
// plain http to this machine only, or a scheme of the client's own - what a
// desktop app registers with the system. Never a fragment, credentials, or
// anything the browser would repair into something else.
func ValidRedirectURI(raw string) bool {
	if raw == "" || len(raw) > oauthMaxURIBytes || strings.ContainsAny(raw, "#\\ ") {
		return false
	}
	for _, r := range raw {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.User != nil || parsed.Opaque != "" {
		return false
	}
	scheme := parsed.Scheme
	switch {
	case scheme == "https":
		return parsed.Host != ""
	case scheme == "http":
		return loopbackHost(parsed.Hostname())
	case scheme == "" || scheme != strings.ToLower(scheme) || slices.Contains(unsafeSchemes, scheme):
		return false
	}
	return parsed.Host != "" || parsed.Path != ""
}

// NormalizeOAuthScope reads a space-separated scope. read is always in it;
// an unknown scope makes the whole request one that cannot be granted.
func NormalizeOAuthScope(raw string) (string, bool) {
	asked := map[string]bool{OAuthScopeRead: true}
	for _, scope := range strings.Fields(raw) {
		if !slices.Contains(OAuthScopes, scope) {
			return "", false
		}
		asked[scope] = true
	}
	var scopes []string
	for _, scope := range OAuthScopes {
		if asked[scope] {
			scopes = append(scopes, scope)
		}
	}
	return strings.Join(scopes, " "), true
}

// ScopeHas reports whether a scope as stored includes one scope.
func ScopeHas(scope, want string) bool {
	return slices.Contains(strings.Fields(scope), want)
}

func scopeWithin(scope, grant string) bool {
	for _, s := range strings.Fields(scope) {
		if !ScopeHas(grant, s) {
			return false
		}
	}
	return true
}

// oauthText keeps what a client says about itself short and printable.
func oauthText(value string) string { return deviceText(value) }

// clientURI keeps a client's home page only when it is an https address.
func clientURI(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || len(raw) > 512 {
		return ""
	}
	return parsed.String()
}

func checkRedirectURIs(uris []string) error {
	if len(uris) == 0 {
		return refusal("a client needs a redirect URI")
	}
	if len(uris) > oauthMaxRedirectURIs {
		return refusalf("a client may register at most %d redirect URIs", oauthMaxRedirectURIs)
	}
	for _, uri := range uris {
		if !ValidRedirectURI(uri) {
			return refusal("redirect URIs must be https, http to a loopback address, or an app's own scheme, without a fragment")
		}
	}
	return nil
}

// RegisterOAuthClient records a client that registered itself. What it says
// about itself is kept as its claim; only the redirect URIs are binding.
func (d *Store) RegisterOAuthClient(ctx context.Context, name, uri string, redirectURIs []string, ip string, now time.Time) (OAuthClient, error) {
	if err := checkRedirectURIs(redirectURIs); err != nil {
		return OAuthClient{}, err
	}
	secret, err := randomSecret(18)
	if err != nil {
		return OAuthClient{}, fmt.Errorf("create client id: %w", err)
	}
	client := OAuthClient{
		ID: OAuthClientPrefix + secret, Kind: OAuthClientRegistered,
		Name: oauthText(name), ClientURI: clientURI(uri),
		RedirectURIs: slices.Clone(redirectURIs), CreatedAt: now, FetchedAt: now,
	}
	_, err = d.db.Exec(ctx, `
INSERT INTO oauth_clients(id, kind, name, client_uri, redirect_uris, request_ip, created_at, fetched_at)
VALUES($1, $2, $3, $4, $5, $6, $7, $7)
`, client.ID, client.Kind, client.Name, client.ClientURI, client.RedirectURIs, limitAccessText(ip, accessIPMaxBytes), now)
	if err != nil {
		return OAuthClient{}, fmt.Errorf("register client: %w: %w", ErrInternal, err)
	}
	return client, nil
}

// SaveOAuthDocumentClient keeps a client's metadata document as just fetched.
func (d *Store) SaveOAuthDocumentClient(ctx context.Context, id, name, uri string, redirectURIs []string, now time.Time) (OAuthClient, error) {
	if err := checkRedirectURIs(redirectURIs); err != nil {
		return OAuthClient{}, err
	}
	client := OAuthClient{
		ID: id, Kind: OAuthClientDocument, Name: oauthText(name), ClientURI: clientURI(uri),
		RedirectURIs: slices.Clone(redirectURIs), CreatedAt: now, FetchedAt: now,
	}
	err := d.db.QueryRow(ctx, `
INSERT INTO oauth_clients(id, kind, name, client_uri, redirect_uris, created_at, fetched_at)
VALUES($1, $2, $3, $4, $5, $6, $6)
ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, client_uri = EXCLUDED.client_uri,
  redirect_uris = EXCLUDED.redirect_uris, fetched_at = EXCLUDED.fetched_at
WHERE oauth_clients.kind = $2
RETURNING created_at
`, client.ID, client.Kind, client.Name, client.ClientURI, client.RedirectURIs, now).Scan(&client.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return OAuthClient{}, refusal("client id is taken")
	}
	if err != nil {
		return OAuthClient{}, fmt.Errorf("save client: %w: %w", ErrInternal, err)
	}
	return client, nil
}

// OAuthClient reads a client by its id.
func (d *Store) OAuthClient(ctx context.Context, id string) (OAuthClient, error) {
	if id == "" || len(id) > oauthMaxURIBytes {
		return OAuthClient{}, ErrNotFound
	}
	var client OAuthClient
	err := d.db.QueryRow(ctx, `
SELECT id, kind, name, client_uri, redirect_uris, created_at, fetched_at FROM oauth_clients WHERE id = $1
`, id).Scan(&client.ID, &client.Kind, &client.Name, &client.ClientURI, &client.RedirectURIs, &client.CreatedAt, &client.FetchedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return OAuthClient{}, ErrNotFound
	}
	if err != nil {
		return OAuthClient{}, fmt.Errorf("read client: %w: %w", ErrInternal, err)
	}
	return client, nil
}

// OAuthAuthorization is a consent as the account gave it: which client, for
// what, and where the code goes.
type OAuthAuthorization struct {
	UserID        string
	Client        OAuthClient
	Scope         string
	Resource      string
	RedirectURI   string
	CodeChallenge string
}

// CreateOAuthCode records a consent as a new grant and returns its one
// authorization code. A suspended account gets nothing.
func (d *Store) CreateOAuthCode(ctx context.Context, authz OAuthAuthorization, now time.Time) (string, error) {
	if !validUUIDs(authz.UserID) {
		return "", ErrNotFound
	}
	scope, ok := NormalizeOAuthScope(authz.Scope)
	if !ok || !ValidCodeChallenge(authz.CodeChallenge) || !authz.Client.RedirectURI(authz.RedirectURI) || authz.Resource == "" {
		return "", refusal("invalid authorization request")
	}
	code, err := randomSecret(32)
	if err != nil {
		return "", fmt.Errorf("create code: %w", err)
	}
	expires := now.Add(OAuthCodeLifetime)
	tag, err := d.db.Exec(ctx, `
INSERT INTO oauth_grants(id, user_id, client_id, client_name, scope, resource, redirect_uri, code_hash, code_challenge, code_expires_at, created_at, expires_at)
SELECT $1, u.id, $3, $4, $5, $6, $7, $8, $9, $10, $11, $10
FROM users u WHERE u.id = $2 AND u.suspended_at IS NULL
`, uuid.NewString(), authz.UserID, authz.Client.ID, authz.Client.Name, scope, authz.Resource, authz.RedirectURI,
		hashToken(code), authz.CodeChallenge, expires, now)
	if err != nil {
		return "", fmt.Errorf("save grant: %w: %w", ErrInternal, err)
	}
	if tag.RowsAffected() == 0 {
		return "", ErrNotFound
	}
	return code, nil
}

// ValidCodeChallenge is an S256 challenge: a SHA-256 digest in unpadded
// base64url.
func ValidCodeChallenge(challenge string) bool {
	raw, err := base64.RawURLEncoding.DecodeString(challenge)
	return err == nil && len(raw) == sha256.Size
}

// verifierMatches checks a PKCE verifier (RFC 7636: 43 to 128 unreserved
// characters) against its challenge.
func verifierMatches(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	for _, r := range verifier {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("-._~", r)) {
			return false
		}
	}
	digest := sha256.Sum256([]byte(verifier))
	return subtle.ConstantTimeCompare([]byte(base64.RawURLEncoding.EncodeToString(digest[:])), []byte(challenge)) == 1
}

// OAuthTokens is what the token endpoint hands a client.
type OAuthTokens struct {
	AccessToken  string
	RefreshToken string
	Scope        string
	ExpiresIn    int
}

// OAuthExchange is a client's request to turn its code into tokens. Resource
// may be empty; when it is not it must be the grant's.
type OAuthExchange struct {
	Code        string
	ClientID    string
	RedirectURI string
	Verifier    string
	Resource    string
}

// ExchangeOAuthCode turns an authorization code into tokens, once. A code
// presented a second time means it was seen by someone else: the grant, and
// whatever was issued from it, is ended.
func (d *Store) ExchangeOAuthCode(ctx context.Context, ex OAuthExchange, now time.Time) (OAuthTokens, error) {
	if ex.Code == "" || len(ex.Code) > oauthTokenMaxBytes {
		return OAuthTokens{}, ErrOAuthGrant
	}
	var tokens OAuthTokens
	refused := false
	err := d.withTx(ctx, func(tx pgx.Tx) error {
		var grantID, clientID, redirectURI, challenge, scope, resource string
		var codeExpires time.Time
		var used pgtype.Timestamptz
		var active bool
		err := tx.QueryRow(ctx, `
SELECT g.id, g.client_id, g.redirect_uri, g.code_challenge, g.scope, g.resource, g.code_expires_at, g.code_used_at, u.suspended_at IS NULL
FROM oauth_grants g JOIN users u ON u.id = g.user_id
WHERE g.code_hash = $1
FOR UPDATE OF g
`, hashToken(ex.Code)).Scan(&grantID, &clientID, &redirectURI, &challenge, &scope, &resource, &codeExpires, &used, &active)
		if errors.Is(err, pgx.ErrNoRows) {
			refused = true
			return nil
		}
		if err != nil {
			return err
		}
		if used.Valid {
			refused = true
			_, err := tx.Exec(ctx, `DELETE FROM oauth_grants WHERE id = $1`, grantID)
			return err
		}
		if !now.Before(codeExpires) || !active || clientID != ex.ClientID || redirectURI != ex.RedirectURI ||
			(ex.Resource != "" && ex.Resource != resource) || !verifierMatches(ex.Verifier, challenge) {
			refused = true
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE oauth_grants SET code_used_at = $2 WHERE id = $1`, grantID, now); err != nil {
			return err
		}
		tokens, err = issueOAuthTokens(ctx, tx, grantID, scope, scope, now)
		return err
	})
	if err != nil {
		return OAuthTokens{}, fmt.Errorf("exchange code: %w: %w", ErrInternal, err)
	}
	if refused {
		return OAuthTokens{}, ErrOAuthGrant
	}
	return tokens, nil
}

// RefreshOAuthToken rotates a refresh token: the one presented is spent and a
// new pair is issued. A spent one presented again ends the grant - one of the
// two holders is not the client. scope, when set, narrows the access token
// and may not widen it.
func (d *Store) RefreshOAuthToken(ctx context.Context, refreshToken, clientID, scope, resource string, now time.Time) (OAuthTokens, error) {
	if !strings.HasPrefix(refreshToken, OAuthRefreshPrefix) || len(refreshToken) > oauthTokenMaxBytes {
		return OAuthTokens{}, ErrOAuthGrant
	}
	var tokens OAuthTokens
	var refusedWith error
	err := d.withTx(ctx, func(tx pgx.Tx) error {
		var grantID, grantClient, grantScope, grantResource string
		var expires time.Time
		var used pgtype.Timestamptz
		var active bool
		err := tx.QueryRow(ctx, `
SELECT g.id, g.client_id, g.scope, g.resource, t.expires_at, t.used_at, u.suspended_at IS NULL
FROM oauth_tokens t
JOIN oauth_grants g ON g.id = t.grant_id
JOIN users u ON u.id = g.user_id
WHERE t.token_hash = $1 AND t.kind = 'refresh'
FOR UPDATE OF t, g
`, hashToken(refreshToken)).Scan(&grantID, &grantClient, &grantScope, &grantResource, &expires, &used, &active)
		if errors.Is(err, pgx.ErrNoRows) {
			refusedWith = ErrOAuthGrant
			return nil
		}
		if err != nil {
			return err
		}
		if used.Valid {
			refusedWith = ErrOAuthGrant
			_, err := tx.Exec(ctx, `DELETE FROM oauth_grants WHERE id = $1`, grantID)
			return err
		}
		if !now.Before(expires) || !active || grantClient != clientID || (resource != "" && resource != grantResource) {
			refusedWith = ErrOAuthGrant
			return nil
		}
		accessScope := grantScope
		if strings.TrimSpace(scope) != "" {
			narrowed, ok := NormalizeOAuthScope(scope)
			if !ok || !scopeWithin(narrowed, grantScope) {
				refusedWith = ErrOAuthScope
				return nil
			}
			accessScope = narrowed
		}
		if _, err := tx.Exec(ctx, `UPDATE oauth_tokens SET used_at = $2 WHERE token_hash = $1`, hashToken(refreshToken), now); err != nil {
			return err
		}
		tokens, err = issueOAuthTokens(ctx, tx, grantID, grantScope, accessScope, now)
		return err
	})
	if err != nil {
		return OAuthTokens{}, fmt.Errorf("refresh token: %w: %w", ErrInternal, err)
	}
	if refusedWith != nil {
		return OAuthTokens{}, refusedWith
	}
	return tokens, nil
}

// issueOAuthTokens adds an access token and a refresh token to a grant and
// moves the grant's end to the refresh token's.
func issueOAuthTokens(ctx context.Context, tx pgx.Tx, grantID, grantScope, accessScope string, now time.Time) (OAuthTokens, error) {
	access, err := randomSecret(32)
	if err != nil {
		return OAuthTokens{}, err
	}
	refresh, err := randomSecret(32)
	if err != nil {
		return OAuthTokens{}, err
	}
	tokens := OAuthTokens{
		AccessToken: OAuthAccessPrefix + access, RefreshToken: OAuthRefreshPrefix + refresh,
		Scope: accessScope, ExpiresIn: int(OAuthAccessLifetime / time.Second),
	}
	refreshExpires := now.Add(OAuthRefreshLifetime)
	if _, err := tx.Exec(ctx, `
INSERT INTO oauth_tokens(token_hash, grant_id, kind, scope, created_at, expires_at)
VALUES($1, $3, 'access', $4, $6, $7), ($2, $3, 'refresh', $5, $6, $8)
`, hashToken(tokens.AccessToken), hashToken(tokens.RefreshToken), grantID, accessScope, grantScope,
		now, now.Add(OAuthAccessLifetime), refreshExpires); err != nil {
		return OAuthTokens{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE oauth_grants SET expires_at = $2 WHERE id = $1`, grantID, refreshExpires); err != nil {
		return OAuthTokens{}, err
	}
	return tokens, nil
}

// OAuthAccess is what an access token allows: one client, one resource, the
// scope it was issued with.
type OAuthAccess struct {
	GrantID    string
	ClientID   string
	ClientName string
	Scope      string
	Resource   string
}

// Has reports whether the token carries scope.
func (a OAuthAccess) Has(scope string) bool { return ScopeHas(a.Scope, scope) }

// OAuthAccessUser resolves an access token to its account. An expired token,
// one whose grant was revoked and one whose account is suspended all read as
// not found. The caller checks the resource is its own.
func (d *Store) OAuthAccessUser(ctx context.Context, token, ip string, now time.Time) (User, OAuthAccess, error) {
	if !strings.HasPrefix(token, OAuthAccessPrefix) || len(token) > oauthTokenMaxBytes {
		return User{}, OAuthAccess{}, ErrNotFound
	}
	var user User
	var access OAuthAccess
	var lastUsed pgtype.Timestamptz
	var lastIP string
	err := d.db.QueryRow(ctx, `
SELECT t.scope, g.id, g.client_id, g.client_name, g.resource, g.last_used_at, g.last_used_ip,
       u.id, COALESCE(u.github_id, ''), u.login, u.name, u.avatar_url
FROM oauth_tokens t
JOIN oauth_grants g ON g.id = t.grant_id
JOIN users u ON u.id = g.user_id
WHERE t.token_hash = $1 AND t.kind = 'access' AND t.expires_at > $2 AND u.suspended_at IS NULL
`, hashToken(token), now).Scan(&access.Scope, &access.GrantID, &access.ClientID, &access.ClientName, &access.Resource,
		&lastUsed, &lastIP, &user.ID, &user.GitHubID, &user.Login, &user.Name, &user.AvatarURL)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, OAuthAccess{}, ErrNotFound
	}
	if err != nil {
		return User{}, OAuthAccess{}, fmt.Errorf("read access token: %w: %w", ErrInternal, err)
	}
	ip = limitAccessText(ip, accessIPMaxBytes)
	if !lastUsed.Valid || now.Sub(lastUsed.Time) >= tokenTouchEvery || lastIP != ip {
		if _, err := d.db.Exec(ctx, `UPDATE oauth_grants SET last_used_at = $2, last_used_ip = $3 WHERE id = $1`, access.GrantID, now, ip); err != nil {
			return User{}, OAuthAccess{}, fmt.Errorf("record token use: %w: %w", ErrInternal, err)
		}
	}
	return user, access, nil
}

// RevokeOAuthToken ends the grant a token belongs to, as the revocation
// endpoint asks (RFC 7009). A token of another client is left alone, and a
// token that leads nowhere is not an error: either way the answer is the same.
func (d *Store) RevokeOAuthToken(ctx context.Context, token, clientID string) error {
	if len(token) > oauthTokenMaxBytes || (!strings.HasPrefix(token, OAuthAccessPrefix) && !strings.HasPrefix(token, OAuthRefreshPrefix)) {
		return nil
	}
	_, err := d.db.Exec(ctx, `
DELETE FROM oauth_grants g
USING oauth_tokens t
WHERE t.token_hash = $1 AND g.id = t.grant_id AND g.client_id = $2
`, hashToken(token), clientID)
	if err != nil {
		return fmt.Errorf("revoke token: %w: %w", ErrInternal, err)
	}
	return nil
}

// OAuthGrant is one consent as the account's authorized apps list shows it.
type OAuthGrant struct {
	ID          string
	ClientID    string
	ClientKind  string
	ClientName  string
	Scope       string
	RedirectURI string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	LastUsedAt  *time.Time
	LastUsedIP  string
}

// Has reports whether the grant includes scope.
func (g OAuthGrant) Has(scope string) bool { return ScopeHas(g.Scope, scope) }

// ListOAuthGrants is every live grant of an account whose code was exchanged,
// newest first.
func (d *Store) ListOAuthGrants(ctx context.Context, userID string, now time.Time) ([]OAuthGrant, error) {
	if !validUUIDs(userID) {
		return nil, ErrNotFound
	}
	rows, err := d.db.Query(ctx, `
SELECT g.id, g.client_id, c.kind, g.client_name, g.scope, g.redirect_uri, g.created_at, g.expires_at, g.last_used_at, g.last_used_ip
FROM oauth_grants g JOIN oauth_clients c ON c.id = g.client_id
WHERE g.user_id = $1 AND g.code_used_at IS NOT NULL AND g.expires_at > $2
ORDER BY g.created_at DESC
`, userID, now)
	if err != nil {
		return nil, fmt.Errorf("list grants: %w: %w", ErrInternal, err)
	}
	defer rows.Close()
	var grants []OAuthGrant
	for rows.Next() {
		var grant OAuthGrant
		var lastUsed pgtype.Timestamptz
		if err := rows.Scan(&grant.ID, &grant.ClientID, &grant.ClientKind, &grant.ClientName, &grant.Scope, &grant.RedirectURI,
			&grant.CreatedAt, &grant.ExpiresAt, &lastUsed, &grant.LastUsedIP); err != nil {
			return nil, fmt.Errorf("list grants: %w: %w", ErrInternal, err)
		}
		if lastUsed.Valid {
			at := lastUsed.Time
			grant.LastUsedAt = &at
		}
		grants = append(grants, grant)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list grants: %w: %w", ErrInternal, err)
	}
	return grants, nil
}

// RevokeOAuthGrant ends a grant from the account page, with every token
// issued from it.
func (d *Store) RevokeOAuthGrant(ctx context.Context, userID, grantID string) error {
	if !validUUIDs(userID, grantID) {
		return ErrNotFound
	}
	tag, err := d.db.Exec(ctx, `DELETE FROM oauth_grants WHERE id = $1 AND user_id = $2`, grantID, userID)
	if err != nil {
		return fmt.Errorf("revoke grant: %w: %w", ErrInternal, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
