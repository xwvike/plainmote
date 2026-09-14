package store

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const httpStatusUnauthorized = 401

func optionalUUID(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func insertAccessTx(ctx context.Context, tx pgx.Tx, resourceID, linkID, linkName, outcome string, meta RequestMeta, status int, detail string, now time.Time) error {
	_, err := tx.Exec(ctx, `
INSERT INTO access_logs(
  id, resource_id, link_id, link_name, outcome, remote_ip, remote_addr, host, query, proto,
  user_agent, referer, forwarded, x_forwarded_for, cf_connecting_ip, cf_ray,
  content_length, tls, method, path, status, detail, occurred_at
)
VALUES(
  $1, $2, $3, $4, $5, $6, $7, $8, $9, $10,
  $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23
)
`, uuid.NewString(), optionalUUID(resourceID), optionalUUID(linkID), linkName, outcome,
		meta.RemoteIP, meta.RemoteAddr, meta.Host, meta.Query, meta.Proto,
		meta.UserAgent, meta.Referer, meta.Forwarded, meta.XForwardedFor, meta.CFConnectingIP,
		meta.CFRay, meta.ContentLength, meta.TLS, meta.Method, meta.Path, status, detail, now)
	return err
}

func (d *Store) RecordAccess(ctx context.Context, resourceID, linkID, linkName, outcome string, meta RequestMeta, status int, detail string) error {
	return d.withTx(ctx, func(tx pgx.Tx) error {
		return insertAccessTx(ctx, tx, resourceID, linkID, linkName, outcome, meta, status, detail, time.Now().UTC())
	})
}

func (d *Store) ListAccess(ctx context.Context, ownerID, resourceID, outcome string, limit int) ([]AccessLog, error) {
	if !validUUIDs(ownerID) || resourceID != "" && !validUUIDs(resourceID) {
		return nil, ErrNotFound
	}
	rows, err := d.db.Query(ctx, `
SELECT
  a.id, COALESCE(a.resource_id::text, ''), COALESCE(r.name, ''), COALESCE(r.filename, ''),
  COALESCE(a.link_id::text, ''), a.link_name, a.outcome, a.remote_ip, a.remote_addr,
  a.host, a.query, a.proto, a.user_agent, a.referer, a.forwarded, a.x_forwarded_for,
  a.cf_connecting_ip, a.cf_ray, a.content_length, a.tls, a.method, a.path, a.status,
  a.detail, a.occurred_at
FROM access_logs a
JOIN resources r ON r.id = a.resource_id
WHERE r.owner_id = $1
  AND ($2 = '' OR a.resource_id = NULLIF($2, '')::uuid)
  AND ($3 = '' OR a.outcome = $3)
ORDER BY a.occurred_at DESC
LIMIT $4
`, ownerID, resourceID, outcome, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	logs := make([]AccessLog, 0, limit)
	for rows.Next() {
		var item AccessLog
		if err := rows.Scan(
			&item.ID, &item.ResourceID, &item.ResourceName, &item.ResourceFile,
			&item.LinkID, &item.LinkName, &item.Outcome, &item.RemoteIP, &item.RemoteAddr,
			&item.Host, &item.Query, &item.Proto, &item.UserAgent, &item.Referer,
			&item.Forwarded, &item.XForwardedFor, &item.CFConnectingIP, &item.CFRay,
			&item.ContentLength, &item.TLS, &item.Method, &item.Path, &item.Status,
			&item.Detail, &item.OccurredAt,
		); err != nil {
			return nil, err
		}
		logs = append(logs, item)
	}
	return logs, rows.Err()
}

// Retention is the one piece of housekeeping that cannot be done lazily. A
// share's expiry is derived when the link is used, so nothing has to run on a
// timer; rows that have aged out are different, because there is no question
// to answer at read time - they simply have to go.
const (
	// pruneLockKey namespaces the advisory lock the pruner holds. Replicas
	// share one database, so without it every replica would run the same
	// deletes against the same rows at the same time. It is only a courtesy:
	// the work is idempotent, and losing the race costs nothing.
	pruneLockKey = 8964

	// pruneBatch bounds one DELETE. A first run against a database that has
	// been accumulating for months would otherwise take a single long
	// transaction, holding locks and bloating WAL for its whole duration.
	pruneBatch = 5000
)

// PruneResult reports what one pass removed, so a caller can log a pass that
// did something and stay quiet about the many that did not.
type PruneResult struct {
	AccessLogs int64
	Sessions   int64
}

// Prune removes access logs older than retention, and sessions that have
// expired. A retention of zero keeps access logs for good; sessions are
// pruned regardless, because their expiry is a fact about the session rather
// than a policy about storage.
//
// Sessions need this: SessionUser only deletes the row for the token actually
// presented, so a session whose token is never presented again - a cleared
// cookie, a discarded device - would otherwise sit in the table for good.
func (d *Store) Prune(ctx context.Context, retention time.Duration, now time.Time) (PruneResult, error) {
	var result PruneResult

	// The lock is session-scoped, so every statement below has to run on this
	// one connection rather than on whichever the pool hands out next.
	conn, err := d.db.Acquire(ctx)
	if err != nil {
		return result, fmt.Errorf("prune: acquire connection: %w", err)
	}
	defer conn.Release()

	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, int64(pruneLockKey)).Scan(&locked); err != nil {
		return result, fmt.Errorf("prune: take lock: %w", err)
	}
	if !locked {
		// Another replica is already doing it. Nothing to report and nothing
		// to retry: the next tick will find the work done.
		return result, nil
	}
	defer func() {
		// Release on its own deadline. A cancelled ctx is exactly when this
		// matters, and an unreleased lock would block every later pass until
		// the connection is recycled.
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, _ = conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, int64(pruneLockKey))
	}()

	if retention > 0 {
		result.AccessLogs, err = deleteInBatches(ctx, conn, `
DELETE FROM access_logs
WHERE id IN (SELECT id FROM access_logs WHERE occurred_at < $1 ORDER BY occurred_at LIMIT $2)
`, now.Add(-retention))
		if err != nil {
			return result, fmt.Errorf("prune access logs: %w", err)
		}
	}

	result.Sessions, err = deleteInBatches(ctx, conn, `
DELETE FROM sessions
WHERE id IN (SELECT id FROM sessions WHERE expires_at < $1 ORDER BY expires_at LIMIT $2)
`, now)
	if err != nil {
		return result, fmt.Errorf("prune sessions: %w", err)
	}
	return result, nil
}

// deleteInBatches runs statement until it stops finding rows, so a large
// backlog is cleared by many short transactions rather than one long one.
// Cancellation is honoured between batches: a shutdown mid-prune leaves the
// remaining rows for the next start, which is harmless.
func deleteInBatches(ctx context.Context, conn *pgxpool.Conn, statement string, cutoff time.Time) (int64, error) {
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, nil
		}
		tag, err := conn.Exec(ctx, statement, cutoff, pruneBatch)
		if err != nil {
			return total, err
		}
		removed := tag.RowsAffected()
		total += removed
		if removed < pruneBatch {
			return total, nil
		}
	}
}
