package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	ProviderGitHub = "github"
	ProviderGoogle = "google"
)

// Identity is one way to sign in to an account: an account at a provider,
// named by the provider's own stable ID.
type Identity struct {
	Provider   string
	Subject    string
	Login      string
	Name       string
	AvatarURL  string
	CreatedAt  time.Time
	LastUsedAt *time.Time
}

var (
	// ErrIdentityTaken is a link to a provider account that already signs in
	// to another account here. The two are never merged.
	ErrIdentityTaken = refusal("store: identity belongs to another account")
	// ErrProviderLinked is a link to a second account at a provider the
	// account already signs in with.
	ErrProviderLinked = refusal("store: account already has this provider")
	// ErrLastIdentity is an unlink that would leave no way to sign in.
	ErrLastIdentity = refusal("store: the last sign-in method cannot be removed")
)

func validIdentity(identity Identity) error {
	switch identity.Provider {
	case ProviderGitHub, ProviderGoogle:
	default:
		return fmt.Errorf("unknown identity provider %q", identity.Provider)
	}
	if strings.TrimSpace(identity.Subject) == "" {
		return errors.New("identity subject must not be empty")
	}
	return nil
}

// lockIdentity serialises everything done to one provider identity, so two
// first sign-ins at once make one account, not a conflict.
func lockIdentity(ctx context.Context, tx pgx.Tx, identity Identity) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, "identity:"+identity.Provider+":"+identity.Subject)
	return err
}

// identityOwner is the account an identity signs in to. A GitHub identity
// written only to users.github_id - by a build from before identities, run
// against this database after it - is found there and recorded.
func identityOwner(ctx context.Context, tx pgx.Tx, identity Identity, now time.Time) (string, error) {
	var userID string
	err := tx.QueryRow(ctx, `SELECT user_id FROM user_identities WHERE provider = $1 AND subject = $2`,
		identity.Provider, identity.Subject).Scan(&userID)
	if err == nil || !errors.Is(err, pgx.ErrNoRows) || identity.Provider != ProviderGitHub {
		return userID, translateNotFound(err)
	}
	err = tx.QueryRow(ctx, `SELECT id FROM users WHERE github_id = $1`, identity.Subject).Scan(&userID)
	if err != nil {
		return "", translateNotFound(err)
	}
	if err := insertIdentity(ctx, tx, userID, identity, now); err != nil {
		return "", err
	}
	return userID, nil
}

func insertIdentity(ctx context.Context, tx pgx.Tx, userID string, identity Identity, now time.Time) error {
	_, err := tx.Exec(ctx, `
INSERT INTO user_identities (provider, subject, user_id, login, name, avatar_url, created_at, last_used_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
`, identity.Provider, identity.Subject, userID, identity.Login, identity.Name, identity.AvatarURL, now)
	if isUniqueViolation(err) {
		return ErrProviderLinked
	}
	return err
}

// refreshProfile copies the account's name and picture from its GitHub
// identity, or from its other one when it has no GitHub identity, and keeps
// users.github_id the GitHub identity's.
func refreshProfile(ctx context.Context, tx pgx.Tx, userID string, now time.Time) (User, error) {
	var user User
	err := tx.QueryRow(ctx, `
UPDATE users u
SET login = i.login, name = i.name, avatar_url = i.avatar_url, updated_at = $2,
    github_id = (SELECT g.subject FROM user_identities g WHERE g.user_id = u.id AND g.provider = 'github')
FROM (SELECT login, name, avatar_url FROM user_identities WHERE user_id = $1
      ORDER BY provider = 'github' DESC, created_at LIMIT 1) i
WHERE u.id = $1
RETURNING u.id, COALESCE(u.github_id, ''), u.login, u.name, u.avatar_url
`, userID, now).Scan(&user.ID, &user.GitHubID, &user.Login, &user.Name, &user.AvatarURL)
	return user, translateNotFound(err)
}

// SignIn returns the account an identity signs in to, refreshed with what
// the provider says about it now, and creates the account the first time.
// Whether a new account may be created is the caller's decision.
func (d *Store) SignIn(ctx context.Context, identity Identity) (User, error) {
	if err := validIdentity(identity); err != nil {
		return User{}, err
	}
	now := time.Now().UTC()
	var user User
	err := d.withTx(ctx, func(tx pgx.Tx) error {
		if err := lockIdentity(ctx, tx, identity); err != nil {
			return err
		}
		userID, err := identityOwner(ctx, tx, identity, now)
		if errors.Is(err, ErrNotFound) {
			userID = uuid.NewString()
			var githubID *string
			if identity.Provider == ProviderGitHub {
				githubID = &identity.Subject
			}
			if _, err := tx.Exec(ctx, `
INSERT INTO users (id, github_id, login, name, avatar_url, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $6)
`, userID, githubID, identity.Login, identity.Name, identity.AvatarURL, now); err != nil {
				return fmt.Errorf("create user: %w", err)
			}
			if err := grantDefaultPlan(ctx, tx, userID, now); err != nil {
				return err
			}
			if err := insertIdentity(ctx, tx, userID, identity, now); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else if _, err := tx.Exec(ctx, `
UPDATE user_identities SET login = $3, name = $4, avatar_url = $5, last_used_at = $6
WHERE provider = $1 AND subject = $2
`, identity.Provider, identity.Subject, identity.Login, identity.Name, identity.AvatarURL, now); err != nil {
			return err
		}
		user, err = refreshProfile(ctx, tx, userID, now)
		return err
	})
	if err != nil {
		return User{}, fmt.Errorf("sign in: %w", err)
	}
	return user, nil
}

// IdentityUser is the account an identity signs in to, suspended or not.
func (d *Store) IdentityUser(ctx context.Context, provider, subject string) (User, error) {
	var user User
	err := d.db.QueryRow(ctx, `
SELECT u.id, COALESCE(u.github_id, ''), u.login, u.name, u.avatar_url, u.suspended_at IS NOT NULL, u.suspended_reason
FROM users u
WHERE u.id = (SELECT user_id FROM user_identities WHERE provider = $1 AND subject = $2)
   OR ($1 = 'github' AND u.github_id = $2)
LIMIT 1
`, provider, subject).Scan(&user.ID, &user.GitHubID, &user.Login, &user.Name, &user.AvatarURL, &user.Suspended, &user.SuspendedReason)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("read user: %w", err)
	}
	return user, nil
}

// LinkIdentity adds a way to sign in to an account. Linking what is already
// linked to the same account changes nothing.
func (d *Store) LinkIdentity(ctx context.Context, userID string, identity Identity) error {
	if err := validIdentity(identity); err != nil {
		return err
	}
	if !validUUIDs(userID) || userID == AnonymousUserID {
		return ErrNotFound
	}
	now := time.Now().UTC()
	return d.withTx(ctx, func(tx pgx.Tx) error {
		if err := lockIdentity(ctx, tx, identity); err != nil {
			return err
		}
		owner, err := identityOwner(ctx, tx, identity, now)
		switch {
		case err == nil && owner == userID:
			return nil
		case err == nil:
			return ErrIdentityTaken
		case !errors.Is(err, ErrNotFound):
			return err
		}
		if err := insertIdentity(ctx, tx, userID, identity, now); err != nil {
			return err
		}
		_, err = refreshProfile(ctx, tx, userID, now)
		return err
	})
}

// UnlinkIdentity removes the account's sign-in with a provider, unless it is
// the only one left.
func (d *Store) UnlinkIdentity(ctx context.Context, userID, provider string) error {
	if !validUUIDs(userID) {
		return ErrNotFound
	}
	now := time.Now().UTC()
	return d.withTx(ctx, func(tx pgx.Tx) error {
		var providers []string
		rows, err := tx.Query(ctx, `SELECT provider FROM user_identities WHERE user_id = $1 FOR UPDATE`, userID)
		if err != nil {
			return err
		}
		if providers, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return err
		}
		found := false
		for _, have := range providers {
			found = found || have == provider
		}
		if !found {
			return ErrNotFound
		}
		if len(providers) < 2 {
			return ErrLastIdentity
		}
		if _, err := tx.Exec(ctx, `DELETE FROM user_identities WHERE user_id = $1 AND provider = $2`, userID, provider); err != nil {
			return err
		}
		_, err = refreshProfile(ctx, tx, userID, now)
		return err
	})
}

// Identities lists the ways to sign in to an account, GitHub first.
func (d *Store) Identities(ctx context.Context, userID string) ([]Identity, error) {
	return identities(ctx, d.db, userID)
}

func identities(ctx context.Context, q storeQuerier, userID string) ([]Identity, error) {
	rows, err := q.Query(ctx, `
SELECT provider, subject, login, name, avatar_url, created_at, last_used_at
FROM user_identities WHERE user_id = $1 ORDER BY provider = 'github' DESC, created_at
`, userID)
	if err != nil {
		return nil, fmt.Errorf("read identities: %w", err)
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Identity, error) {
		var identity Identity
		var lastUsed pgtype.Timestamptz
		err := row.Scan(&identity.Provider, &identity.Subject, &identity.Login, &identity.Name,
			&identity.AvatarURL, &identity.CreatedAt, &lastUsed)
		identity.LastUsedAt = timePointer(lastUsed)
		return identity, err
	})
}
