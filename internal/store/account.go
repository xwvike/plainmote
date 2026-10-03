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

// Account is what the account page and the export say about the account itself.
type Account struct {
	User      User
	CreatedAt time.Time
}

func (d *Store) Account(ctx context.Context, userID string) (Account, error) {
	if !validUUIDs(userID) || userID == AnonymousUserID {
		return Account{}, ErrNotFound
	}
	var account Account
	err := d.db.QueryRow(ctx, `
SELECT id, github_id, login, name, avatar_url, created_at FROM users WHERE id = $1
`, userID).Scan(&account.User.ID, &account.User.GitHubID, &account.User.Login,
		&account.User.Name, &account.User.AvatarURL, &account.CreatedAt)
	if err != nil {
		return Account{}, translateNotFound(err)
	}
	return account, nil
}

// ExportResources returns every resource the account owns, oldest first.
func (d *Store) ExportResources(ctx context.Context, ownerID string) ([]Resource, error) {
	if !validUUIDs(ownerID) {
		return nil, ErrNotFound
	}
	rows, err := d.db.Query(ctx, `
SELECT `+resourceColumns+`
FROM resources r WHERE r.owner_id = $1 ORDER BY r.created_at, r.id
	`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("export resources: %w: %w", ErrInternal, err)
	}
	resources, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Resource, error) {
		return scanResource(row)
	})
	if err != nil {
		return nil, fmt.Errorf("export resources: %w: %w", ErrInternal, err)
	}
	return resources, nil
}

// ExportVersions returns the earlier versions of every resource the account
// owns, by resource and then oldest first.
func (d *Store) ExportVersions(ctx context.Context, ownerID string) ([]Version, error) {
	if !validUUIDs(ownerID) {
		return nil, ErrNotFound
	}
	rows, err := d.db.Query(ctx, `
SELECT `+versionColumns+`
FROM resource_versions v JOIN resources r ON r.id = v.resource_id
WHERE r.owner_id = $1 ORDER BY v.resource_id, v.version
`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("export versions: %w: %w", ErrInternal, err)
	}
	versions, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Version, error) { return scanVersion(row) })
	if err != nil {
		return nil, fmt.Errorf("export versions: %w: %w", ErrInternal, err)
	}
	return versions, nil
}

// EachAccessLog streams the account's access log, oldest first. It is a
// callback rather than a slice because the log is the one part of an account
// with no quota on it: a month of traffic on a popular link is a lot of rows.
func (d *Store) EachAccessLog(ctx context.Context, ownerID string, fn func(AccessLog) error) error {
	if !validUUIDs(ownerID) {
		return ErrNotFound
	}
	rows, err := d.db.Query(ctx, `
SELECT
  id, COALESCE(resource_id::text, ''), resource_name, resource_file,
  COALESCE(link_id::text, ''), link_name, outcome, remote_ip, remote_addr,
  host, query, proto, user_agent, referer, forwarded, x_forwarded_for,
  cf_connecting_ip, cf_ray, content_length, tls, method, path, status,
  detail, hits, first_at, occurred_at, COALESCE(resource_version, 0)
FROM access_logs WHERE owner_id = $1 ORDER BY occurred_at, id
`, ownerID)
	if err != nil {
		return fmt.Errorf("export access logs: %w: %w", ErrInternal, err)
	}
	defer rows.Close()
	for rows.Next() {
		var item AccessLog
		if err := rows.Scan(
			&item.ID, &item.ResourceID, &item.ResourceName, &item.ResourceFile,
			&item.LinkID, &item.LinkName, &item.Outcome, &item.RemoteIP, &item.RemoteAddr,
			&item.Host, &item.Query, &item.Proto, &item.UserAgent, &item.Referer,
			&item.Forwarded, &item.XForwardedFor, &item.CFConnectingIP, &item.CFRay,
			&item.ContentLength, &item.TLS, &item.Method, &item.Path, &item.Status,
			&item.Detail, &item.Hits, &item.FirstAt, &item.OccurredAt, &item.Version,
		); err != nil {
			return fmt.Errorf("export access logs: %w: %w", ErrInternal, err)
		}
		if err := fn(item); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("export access logs: %w: %w", ErrInternal, err)
	}
	return nil
}

// DeleteAccount removes the account and everything it owns: resources, their
// earlier versions, links, sessions, plan grants, pending quick-share claims
// and the access log all go with the user row, and the stored bodies after it.
//
// The user row is locked first. Every write that adds or replaces a body takes
// the same lock in quotaGate, so one that was already running finishes before
// the resources are read here, and one that starts after finds no account and
// removes the object it wrote. Reading the keys any earlier could miss a body
// written in between and leave it in the bucket for good.
func (d *Store) DeleteAccount(ctx context.Context, userID string) error {
	if !validUUIDs(userID) || userID == AnonymousUserID {
		return ErrNotFound
	}
	var keys []string
	err := d.withTx(ctx, func(tx pgx.Tx) error {
		var locked string
		if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&locked); err != nil {
			return translateNotFound(err)
		}
		rows, err := tx.Query(ctx, `
DELETE FROM resource_versions WHERE resource_id IN (SELECT id FROM resources WHERE owner_id = $1)
RETURNING content_key`, userID)
		if err != nil {
			return fmt.Errorf("delete account history: %w: %w", ErrInternal, err)
		}
		if keys, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return fmt.Errorf("delete account history: %w: %w", ErrInternal, err)
		}
		rows, err = tx.Query(ctx, `DELETE FROM resources WHERE owner_id = $1 RETURNING content_key`, userID)
		if err != nil {
			return fmt.Errorf("delete account resources: %w: %w", ErrInternal, err)
		}
		current, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return fmt.Errorf("delete account resources: %w: %w", ErrInternal, err)
		}
		keys = append(keys, current...)
		if _, err := tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID); err != nil {
			return fmt.Errorf("delete account: %w: %w", ErrInternal, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	// The account is gone at this point whatever happens below. A body that
	// fails to delete costs storage, not correctness, and failing the request
	// would tell someone whose account no longer exists to try again.
	// Detach cleanup from the HTTP request: once the database commit succeeds,
	// a client disconnect must not cancel every object deletion. The timeout
	// still bounds a stalled object store.
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	for _, key := range keys {
		if key == "" {
			continue
		}
		if err := d.blobs.Delete(cleanupCtx, key); err != nil {
			fmt.Fprintf(os.Stderr, "delete account %s: body %s: %v\n", userID, key, err)
		}
	}
	return nil
}

// An export reads every body in the account, so it is rationed per account.
const (
	ExportLimit  = 2
	ExportWindow = 24 * time.Hour
)

// ExportAllowance is where an account stands against the export limit.
// NextAt is when the oldest export in the window leaves it, and is only
// meaningful when nothing remains.
type ExportAllowance struct {
	Remaining int
	NextAt    time.Time
}

// ErrExportLimit is returned when the account has used every export the
// window allows.
var ErrExportLimit = errors.New("export limit reached")

func (d *Store) ExportAllowance(ctx context.Context, userID string, now time.Time) (ExportAllowance, error) {
	if !validUUIDs(userID) {
		return ExportAllowance{}, ErrNotFound
	}
	return exportAllowance(ctx, d.db, userID, now)
}

func exportAllowance(ctx context.Context, q storeQuerier, userID string, now time.Time) (ExportAllowance, error) {
	var used int
	var oldest pgtype.Timestamptz
	if err := q.QueryRow(ctx, `
SELECT COUNT(*), MIN(created_at) FROM account_exports WHERE user_id = $1 AND created_at > $2
`, userID, now.Add(-ExportWindow)).Scan(&used, &oldest); err != nil {
		return ExportAllowance{}, fmt.Errorf("read export allowance: %w: %w", ErrInternal, err)
	}
	allowance := ExportAllowance{Remaining: max(ExportLimit-used, 0)}
	if oldest.Valid {
		allowance.NextAt = oldest.Time.Add(ExportWindow)
	}
	return allowance, nil
}

// ReserveExport spends one export, or returns ErrExportLimit with the time the
// next one becomes available. The user row is locked so that two requests at
// once cannot both see the last remaining export.
func (d *Store) ReserveExport(ctx context.Context, userID string, now time.Time) (ExportAllowance, error) {
	if !validUUIDs(userID) {
		return ExportAllowance{}, ErrNotFound
	}
	var allowance ExportAllowance
	err := d.withTx(ctx, func(tx pgx.Tx) error {
		var locked string
		if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&locked); err != nil {
			return translateNotFound(err)
		}
		var err error
		allowance, err = exportAllowance(ctx, tx, userID, now)
		if err != nil {
			return err
		}
		if allowance.Remaining == 0 {
			return ErrExportLimit
		}
		if _, err := tx.Exec(ctx, `INSERT INTO account_exports(id, user_id, created_at) VALUES($1, $2, $3)`, uuid.NewString(), userID, now); err != nil {
			return fmt.Errorf("record export: %w: %w", ErrInternal, err)
		}
		allowance.Remaining--
		return nil
	})
	return allowance, err
}
