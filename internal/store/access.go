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

const (
	// Session-scoped, so the lock and the deletes have to share one connection.
	pruneLockKey = 8964
	pruneBatch   = 5000
)

type PruneResult struct {
	AccessLogs int64
	Sessions   int64
}

// Prune removes access logs older than retention and sessions that have
// expired. Retention zero keeps the logs; sessions go either way, since
// SessionUser only deletes the row for the token actually presented.
func (d *Store) Prune(ctx context.Context, retention time.Duration, now time.Time) (PruneResult, error) {
	var result PruneResult
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
		return result, nil
	}
	defer func() {
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

// deleteInBatches keeps a months-old backlog from becoming one long transaction.
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
