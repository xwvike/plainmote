package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (d *Store) UpsertUser(ctx context.Context, githubID, login, name, avatar string) (User, error) {
	if strings.TrimSpace(githubID) == "" {
		return User{}, errors.New("github user id must not be empty")
	}
	now := time.Now().UTC()
	var user User
	err := d.withTx(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
INSERT INTO users(id, github_id, login, name, avatar_url, created_at, updated_at)
VALUES($1, $2, $3, $4, $5, $6, $6)
ON CONFLICT(github_id) DO NOTHING
RETURNING id, github_id, login, name, avatar_url
		`, uuid.NewString(), githubID, login, name, avatar, now).Scan(
			&user.ID, &user.GitHubID, &user.Login, &user.Name, &user.AvatarURL,
		)
		if err == nil {
			return grantDefaultPlan(ctx, tx, user.ID, now)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("create user: %w", err)
		}

		return tx.QueryRow(ctx, `
UPDATE users
SET login = $2, name = $3, avatar_url = $4, updated_at = $5
WHERE github_id = $1
RETURNING id, github_id, login, name, avatar_url
		`, githubID, login, name, avatar, now).Scan(
			&user.ID, &user.GitHubID, &user.Login, &user.Name, &user.AvatarURL,
		)
	})
	if err != nil {
		return User{}, fmt.Errorf("save user: %w", err)
	}
	return user, nil
}

// ErrSuspended is a sign-in by an account the operator has suspended.
var ErrSuspended = refusal("store: account suspended")

func (d *Store) CreateSession(ctx context.Context, userID string, ttl time.Duration) (token, csrf string, expires time.Time, err error) {
	token, err = randomSecret(32)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("create session token: %w", err)
	}
	csrf, err = randomSecret(24)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("create csrf token: %w", err)
	}
	now := time.Now().UTC()
	expires = now.Add(ttl)
	err = d.withTx(ctx, func(tx pgx.Tx) error {
		// A suspended account gets no session, however it got this far.
		tag, err := tx.Exec(ctx, `UPDATE users SET last_signed_in_at = $2 WHERE id = $1 AND suspended_at IS NULL`, userID, now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrSuspended
		}
		_, err = tx.Exec(ctx, `
INSERT INTO sessions(id, user_id, token_hash, csrf_hash, expires_at, created_at)
VALUES($1, $2, $3, $4, $5, $6)
`, uuid.NewString(), userID, hashToken(token), hashToken(csrf), expires, now)
		return err
	})
	if errors.Is(err, ErrSuspended) {
		return "", "", time.Time{}, err
	}
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("save session: %w", err)
	}
	return token, csrf, expires, nil
}

func (d *Store) SessionUser(ctx context.Context, token string) (User, string, error) {
	var user User
	var sessionID string
	tokenHash := hashToken(token)
	err := d.db.QueryRow(ctx, `
SELECT s.id, u.id, u.github_id, u.login, u.name, u.avatar_url
FROM sessions s
JOIN users u ON u.id = s.user_id
WHERE s.token_hash = $1 AND s.expires_at > $2 AND u.suspended_at IS NULL
`, tokenHash, time.Now().UTC()).Scan(
		&sessionID, &user.ID, &user.GitHubID, &user.Login, &user.Name, &user.AvatarURL,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		_, _ = d.db.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1 AND expires_at <= $2`, tokenHash, time.Now().UTC())
		return User{}, "", ErrNotFound
	}
	if err != nil {
		return User{}, "", fmt.Errorf("read session: %w", err)
	}
	return user, sessionID, nil
}

func (d *Store) SessionCSRF(ctx context.Context, sessionID, csrf string) bool {
	var hash string
	err := d.db.QueryRow(ctx, `SELECT csrf_hash FROM sessions WHERE id = $1 AND expires_at > $2`, sessionID, time.Now().UTC()).Scan(&hash)
	return err == nil && hash == hashToken(csrf)
}

func (d *Store) DeleteSession(ctx context.Context, sessionID string) error {
	_, err := d.db.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, sessionID)
	return err
}

func (d *Store) GetUser(ctx context.Context, githubID string) (User, error) {
	var user User
	err := d.db.QueryRow(ctx, `
SELECT id, github_id, login, name, avatar_url, suspended_at IS NOT NULL, suspended_reason FROM users WHERE github_id = $1
`, githubID).Scan(
		&user.ID, &user.GitHubID, &user.Login, &user.Name, &user.AvatarURL, &user.Suspended, &user.SuspendedReason,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("read user: %w", err)
	}
	return user, nil
}
