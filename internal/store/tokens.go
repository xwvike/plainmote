package store

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// What a personal access token may do. A read token lists and reads; a
// write token also saves new content and creates resources. Neither deletes
// anything, touches share links or changes the account.
const (
	TokenScopeRead  = "read"
	TokenScopeWrite = "write"
)

const (
	// TokenLifetime is fixed from sign-in: using a token does not extend it.
	TokenLifetime = 90 * 24 * time.Hour
	// DeviceGrantLifetime is how long a code shown by the command line can be
	// entered in the browser.
	DeviceGrantLifetime = 10 * time.Minute
	// DevicePollInterval is the slowest a command line may ask whether it has
	// been approved; asking faster adds DevicePollBackoff to its interval.
	DevicePollInterval = 5
	DevicePollBackoff  = 5
	devicePollCeiling  = 60

	// TokenPrefix marks a PlainMote token wherever it ends up, so a leak can
	// be recognised by secret scanners and by eye.
	TokenPrefix = "pmt_"

	// The user code is eight characters from an alphabet without the ones a
	// person mixes up - 0 and O, 1 and I - so it can be read off one screen
	// and typed into another. 32 symbols: one byte masked to five bits picks
	// one without bias.
	userCodeAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	userCodeLength   = 8

	deviceTextMaxRunes = 64
	// tokenTouchEvery spaces out the writes that record a token's last use:
	// a command line can make several requests a second.
	tokenTouchEvery = time.Minute
)

// Outcomes of exchanging a device code, as RFC 8628 names them.
const (
	DevicePending  = "authorization_pending"
	DeviceSlowDown = "slow_down"
	DeviceDenied   = "access_denied"
	DeviceExpired  = "expired_token"
	DeviceApproved = "approved"
)

// DeviceRequest is what a command line says about itself when it asks to be
// signed in. Everything here comes from the command line and is shown to the
// person approving it as a claim, not as fact - except RequestIP, which the
// server saw.
type DeviceRequest struct {
	Scope         string
	DeviceName    string
	DeviceOS      string
	ClientVersion string
	RequestIP     string
}

type DeviceGrant struct {
	ID            string
	Scope         string
	DeviceName    string
	DeviceOS      string
	ClientVersion string
	RequestIP     string
	CreatedAt     time.Time
	ExpiresAt     time.Time
}

// DeviceExchange is the answer to a command line polling with its device
// code. Token is set only when State is DeviceApproved.
type DeviceExchange struct {
	State    string
	Interval int
	Token    string
	User     User
	APIToken APIToken
}

type APIToken struct {
	ID            string
	UserID        string
	Scope         string
	DeviceName    string
	DeviceOS      string
	ClientVersion string
	CreatedAt     time.Time
	ExpiresAt     time.Time
	LastUsedAt    *time.Time
	LastUsedIP    string
}

// CanWrite reports whether the token may change anything.
func (t APIToken) CanWrite() bool { return t.Scope == TokenScopeWrite }

// ErrBadScope is a request for a scope that does not exist.
var ErrBadScope = refusal("unknown token scope")

// CreateDeviceGrant starts a sign-in. It returns the device code, which only
// the command line ever holds, and the user code, which its owner types into
// the browser.
func (d *Store) CreateDeviceGrant(ctx context.Context, request DeviceRequest, now time.Time) (deviceCode, userCode string, grant DeviceGrant, err error) {
	if request.Scope != TokenScopeRead && request.Scope != TokenScopeWrite {
		return "", "", DeviceGrant{}, ErrBadScope
	}
	grant = DeviceGrant{
		ID: uuid.NewString(), Scope: request.Scope,
		DeviceName:    deviceText(request.DeviceName),
		DeviceOS:      deviceText(request.DeviceOS),
		ClientVersion: deviceText(request.ClientVersion),
		RequestIP:     limitAccessText(request.RequestIP, accessIPMaxBytes),
		CreatedAt:     now, ExpiresAt: now.Add(DeviceGrantLifetime),
	}
	deviceCode, err = randomSecret(32)
	if err != nil {
		return "", "", DeviceGrant{}, fmt.Errorf("create device code: %w", err)
	}
	// A clash on the user code is astronomically unlikely among the few
	// grants alive at once, but it is a unique column: draw again.
	for attempt := 0; attempt < 3; attempt++ {
		userCode, err = newUserCode()
		if err != nil {
			return "", "", DeviceGrant{}, err
		}
		_, err = d.db.Exec(ctx, `
INSERT INTO device_grants(id, device_code_hash, user_code_hash, scope, device_name, device_os, client_version, request_ip, created_at, expires_at, poll_interval)
VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
`, grant.ID, hashToken(deviceCode), hashToken(userCode), grant.Scope, grant.DeviceName, grant.DeviceOS,
			grant.ClientVersion, grant.RequestIP, now, grant.ExpiresAt, DevicePollInterval)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			continue
		}
		break
	}
	if err != nil {
		return "", "", DeviceGrant{}, fmt.Errorf("save device grant: %w: %w", ErrInternal, err)
	}
	return deviceCode, userCode, grant, nil
}

func newUserCode() (string, error) {
	raw := make([]byte, userCodeLength)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("create user code: %w", err)
	}
	code := make([]byte, userCodeLength)
	for i, b := range raw {
		code[i] = userCodeAlphabet[b&31]
	}
	return string(code), nil
}

// FormatUserCode writes a user code the way it is shown: two groups of four.
func FormatUserCode(code string) string {
	if len(code) != userCodeLength {
		return code
	}
	return code[:4] + "-" + code[4:]
}

// NormalizeUserCode reads a code as a person typed it: any case, with or
// without the dash or spaces. Anything outside the alphabet makes it no code
// at all, rather than a near miss to be corrected.
func NormalizeUserCode(input string) (string, bool) {
	var code strings.Builder
	for _, r := range strings.ToUpper(input) {
		switch {
		case r == '-' || unicode.IsSpace(r):
			continue
		case r < unicode.MaxASCII && strings.ContainsRune(userCodeAlphabet, r):
			code.WriteRune(r)
		default:
			return "", false
		}
	}
	if code.Len() != userCodeLength {
		return "", false
	}
	return code.String(), true
}

// deviceText keeps what a command line says about itself short and printable:
// it is shown on the approval page and the account page.
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

// PendingDeviceGrant finds the sign-in a user code belongs to, while it can
// still be approved. Every other state reads as not found: the page says the
// same thing for a mistyped code and a used one.
func (d *Store) PendingDeviceGrant(ctx context.Context, userCode string, now time.Time) (DeviceGrant, error) {
	var grant DeviceGrant
	err := d.db.QueryRow(ctx, `
SELECT id, scope, device_name, device_os, client_version, request_ip, created_at, expires_at
FROM device_grants
WHERE user_code_hash = $1 AND state = 'pending' AND expires_at > $2
`, hashToken(userCode), now).Scan(&grant.ID, &grant.Scope, &grant.DeviceName, &grant.DeviceOS,
		&grant.ClientVersion, &grant.RequestIP, &grant.CreatedAt, &grant.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return DeviceGrant{}, ErrNotFound
	}
	if err != nil {
		return DeviceGrant{}, fmt.Errorf("read device grant: %w: %w", ErrInternal, err)
	}
	return grant, nil
}

// DecideDeviceGrant records the browser's answer. The user code is checked
// again with the id, so a form cannot approve a grant its sender never saw
// the code of, and only a pending, live grant can be decided - once.
func (d *Store) DecideDeviceGrant(ctx context.Context, userID, grantID, userCode string, approve bool, now time.Time) error {
	if !validUUIDs(userID, grantID) {
		return ErrNotFound
	}
	state := "denied"
	if approve {
		state = "approved"
	}
	tag, err := d.db.Exec(ctx, `
UPDATE device_grants g
SET state = $1, user_id = $2
FROM users u
WHERE g.id = $3 AND g.user_code_hash = $4 AND g.state = 'pending' AND g.expires_at > $5
  AND u.id = $2 AND u.suspended_at IS NULL
`, state, userID, grantID, hashToken(userCode), now)
	if err != nil {
		return fmt.Errorf("decide device grant: %w: %w", ErrInternal, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ExchangeDeviceCode answers a command line polling to learn whether it has
// been approved. An approval is turned into a token here, once: the grant is
// deleted in the same transaction, so a device code yields one token at most.
func (d *Store) ExchangeDeviceCode(ctx context.Context, deviceCode string, now time.Time) (DeviceExchange, error) {
	var result DeviceExchange
	err := d.withTx(ctx, func(tx pgx.Tx) error {
		var grantID, state, scope, deviceName, deviceOS, clientVersion string
		var userID pgtype.UUID
		var expires time.Time
		var polled pgtype.Timestamptz
		var interval int
		err := tx.QueryRow(ctx, `
SELECT id, state, scope, device_name, device_os, client_version, user_id, expires_at, polled_at, poll_interval
FROM device_grants WHERE device_code_hash = $1 FOR UPDATE
`, hashToken(deviceCode)).Scan(&grantID, &state, &scope, &deviceName, &deviceOS, &clientVersion,
			&userID, &expires, &polled, &interval)
		if errors.Is(err, pgx.ErrNoRows) {
			result.State = DeviceExpired
			return nil
		}
		if err != nil {
			return err
		}
		drop := func() error {
			_, err := tx.Exec(ctx, `DELETE FROM device_grants WHERE id = $1`, grantID)
			return err
		}
		if !now.Before(expires) {
			result.State = DeviceExpired
			return drop()
		}
		// Asked again before its interval was up: wait longer from now on.
		if polled.Valid && now.Sub(polled.Time) < time.Duration(interval)*time.Second && state == "pending" {
			interval = min(interval+DevicePollBackoff, devicePollCeiling)
			result.State, result.Interval = DeviceSlowDown, interval
			_, err := tx.Exec(ctx, `UPDATE device_grants SET polled_at = $2, poll_interval = $3 WHERE id = $1`, grantID, now, interval)
			return err
		}
		result.Interval = interval
		switch state {
		case "pending":
			result.State = DevicePending
			_, err := tx.Exec(ctx, `UPDATE device_grants SET polled_at = $2 WHERE id = $1`, grantID, now)
			return err
		case "denied":
			result.State = DeviceDenied
			return drop()
		case "approved":
		default:
			result.State = DeviceExpired
			return drop()
		}
		// Approved. The account is read again: one suspended between the
		// approval and this poll gets nothing.
		var user User
		err = tx.QueryRow(ctx, `
SELECT id, COALESCE(github_id, ''), login, name, avatar_url FROM users WHERE id = $1 AND suspended_at IS NULL
`, userID).Scan(&user.ID, &user.GitHubID, &user.Login, &user.Name, &user.AvatarURL)
		if errors.Is(err, pgx.ErrNoRows) {
			result.State = DeviceDenied
			return drop()
		}
		if err != nil {
			return err
		}
		secret, err := randomSecret(32)
		if err != nil {
			return err
		}
		token := APIToken{
			ID: uuid.NewString(), UserID: user.ID, Scope: scope,
			DeviceName: deviceName, DeviceOS: deviceOS, ClientVersion: clientVersion,
			CreatedAt: now, ExpiresAt: now.Add(TokenLifetime),
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO api_tokens(id, user_id, token_hash, scope, device_name, device_os, client_version, created_at, expires_at)
VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9)
`, token.ID, token.UserID, hashToken(TokenPrefix+secret), token.Scope, token.DeviceName, token.DeviceOS,
			token.ClientVersion, token.CreatedAt, token.ExpiresAt); err != nil {
			return err
		}
		result = DeviceExchange{State: DeviceApproved, Interval: interval, Token: TokenPrefix + secret, User: user, APIToken: token}
		return drop()
	})
	if err != nil {
		return DeviceExchange{}, fmt.Errorf("exchange device code: %w: %w", ErrInternal, err)
	}
	return result, nil
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
	var lastUsed pgtype.Timestamptz
	err := d.db.QueryRow(ctx, `
SELECT t.id, t.scope, t.device_name, t.device_os, t.client_version, t.created_at, t.expires_at, t.last_used_at, t.last_used_ip,
       u.id, COALESCE(u.github_id, ''), u.login, u.name, u.avatar_url
FROM api_tokens t
JOIN users u ON u.id = t.user_id
WHERE t.token_hash = $1 AND t.expires_at > $2 AND u.suspended_at IS NULL
`, hashToken(token), now).Scan(&apiToken.ID, &apiToken.Scope, &apiToken.DeviceName, &apiToken.DeviceOS,
		&apiToken.ClientVersion, &apiToken.CreatedAt, &apiToken.ExpiresAt, &lastUsed, &apiToken.LastUsedIP,
		&user.ID, &user.GitHubID, &user.Login, &user.Name, &user.AvatarURL)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, APIToken{}, ErrNotFound
	}
	if err != nil {
		return User{}, APIToken{}, fmt.Errorf("read token: %w: %w", ErrInternal, err)
	}
	apiToken.UserID = user.ID
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
FROM api_tokens WHERE user_id = $1 AND expires_at > $2
ORDER BY created_at DESC
`, userID, now)
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w: %w", ErrInternal, err)
	}
	defer rows.Close()
	var tokens []APIToken
	for rows.Next() {
		token := APIToken{UserID: userID}
		var lastUsed pgtype.Timestamptz
		if err := rows.Scan(&token.ID, &token.Scope, &token.DeviceName, &token.DeviceOS, &token.ClientVersion,
			&token.CreatedAt, &token.ExpiresAt, &lastUsed, &token.LastUsedIP); err != nil {
			return nil, fmt.Errorf("list tokens: %w: %w", ErrInternal, err)
		}
		if lastUsed.Valid {
			at := lastUsed.Time
			token.LastUsedAt = &at
		}
		tokens = append(tokens, token)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tokens: %w: %w", ErrInternal, err)
	}
	return tokens, nil
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

// RevokeAPITokenByID deletes the token a request was made with: what
// `plainmote logout` asks for.
func (d *Store) RevokeAPITokenByID(ctx context.Context, tokenID string) error {
	if !validUUIDs(tokenID) {
		return ErrNotFound
	}
	if _, err := d.db.Exec(ctx, `DELETE FROM api_tokens WHERE id = $1`, tokenID); err != nil {
		return fmt.Errorf("revoke token: %w: %w", ErrInternal, err)
	}
	return nil
}
