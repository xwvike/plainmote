package store

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	httpStatusOK           = 200
	httpStatusUnauthorized = 401
)

// Access outcomes. Every value the public handler can record lives here so the
// log filter and the store cannot drift apart.
const (
	OutcomeSuccess       = "success"
	OutcomeExpired       = "expired"
	OutcomeExhausted     = "exhausted"
	OutcomeRevoked       = "revoked"
	OutcomeUpstreamError = "upstream_error"
	// The operator took the resource down, or suspended its owner.
	OutcomeTakenDown = "taken_down"
	OutcomeSuspended = "suspended"

	// ReasonInvalid is reported to the caller but never stored: a token nobody
	// issued has no resource to hang the row off, and so no owner to read it.
	ReasonInvalid = "invalid"
)

// AccessOutcomes are the stored outcomes, in the order the filter offers them.
var AccessOutcomes = []string{OutcomeSuccess, OutcomeExpired, OutcomeExhausted, OutcomeRevoked, OutcomeUpstreamError, OutcomeTakenDown, OutcomeSuspended}

// accessFoldWindow is how long one caller's repeat of the same refusal folds
// into the row already there instead of adding another.
const accessFoldWindow = time.Minute

// foldable reports whether an outcome may be folded. Only refusals are: a dead
// link can be hit forever by anyone who has the address, and the thousandth
// attempt from one caller in a minute says nothing the first did not. A
// delivery is not folded - the owner published a live link and every hit on it
// is something they asked to see.
func foldable(outcome string) bool {
	switch outcome {
	case OutcomeExpired, OutcomeExhausted, OutcomeRevoked, OutcomeUpstreamError, OutcomeTakenDown, OutcomeSuspended:
		return true
	}
	return false
}

const (
	accessTokenMaxBytes   = 32
	accessIPMaxBytes      = 64
	accessAddressMaxBytes = 128
	accessHeaderMaxBytes  = 512
	accessTextMaxBytes    = 1024
	accessTargetMaxBytes  = 2048
)

func limitAccessText(value string, maxBytes int) string {
	value = strings.ToValidUTF8(value, "\uFFFD")
	value = strings.ReplaceAll(value, "\x00", "\uFFFD")
	if len(value) <= maxBytes {
		return value
	}

	const suffix = "…"
	cut := maxBytes - len(suffix)
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + suffix
}

func limitRequestMeta(meta RequestMeta) RequestMeta {
	meta.RemoteIP = limitAccessText(meta.RemoteIP, accessIPMaxBytes)
	meta.RemoteAddr = limitAccessText(meta.RemoteAddr, accessAddressMaxBytes)
	meta.Host = limitAccessText(meta.Host, accessHeaderMaxBytes)
	meta.Query = limitAccessText(meta.Query, accessTargetMaxBytes)
	meta.Proto = limitAccessText(meta.Proto, accessTokenMaxBytes)
	meta.UserAgent = limitAccessText(meta.UserAgent, accessTextMaxBytes)
	meta.Referer = limitAccessText(meta.Referer, accessTextMaxBytes)
	meta.Forwarded = limitAccessText(meta.Forwarded, accessHeaderMaxBytes)
	meta.XForwardedFor = limitAccessText(meta.XForwardedFor, accessHeaderMaxBytes)
	meta.CFConnectingIP = limitAccessText(meta.CFConnectingIP, accessIPMaxBytes)
	meta.CFRay = limitAccessText(meta.CFRay, accessAddressMaxBytes)
	meta.ContentLength = limitAccessText(meta.ContentLength, accessTokenMaxBytes)
	meta.Method = limitAccessText(meta.Method, accessTokenMaxBytes)
	meta.Path = limitAccessText(meta.Path, accessTargetMaxBytes)
	return meta
}

func optionalUUID(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// AccessEvent is one delivery attempt as the log will keep it. The resource
// fields are copied rather than referenced: the row has to stay readable after
// what it describes is gone.
type AccessEvent struct {
	// ID names the row when the caller needs to find it again; empty means
	// a new one is made up.
	ID           string
	OwnerID      string
	ResourceID   string
	ResourceName string
	ResourceFile string
	LinkID       string
	LinkName     string
	Outcome      string
	Status       int
	Detail       string
	// Version is the content version delivered; 0 when nothing stored was.
	Version int
}

func insertAccessTx(ctx context.Context, tx pgx.Tx, event AccessEvent, meta RequestMeta, now time.Time) error {
	meta = limitRequestMeta(meta)
	// Folding keeps the count, which is what the earlier sampling attempt got
	// wrong: it decided whether to write at all, so the attempts it skipped
	// left no trace. The row is only reused for the same link, the same
	// outcome and the same caller, so a second party probing the same dead
	// link still gets a row of their own.
	if foldable(event.Outcome) && event.LinkID != "" {
		tag, err := tx.Exec(ctx, `
UPDATE access_logs SET hits = hits + 1, occurred_at = $1
WHERE id = (
  SELECT id FROM access_logs
  WHERE link_id = $2 AND outcome = $3 AND remote_ip = $4 AND occurred_at >= $5
  ORDER BY occurred_at DESC
  LIMIT 1
)
`, now, event.LinkID, event.Outcome, meta.RemoteIP, now.Add(-accessFoldWindow))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			return nil
		}
	}
	event.ResourceName = limitAccessText(event.ResourceName, accessHeaderMaxBytes)
	event.ResourceFile = limitAccessText(event.ResourceFile, accessHeaderMaxBytes)
	event.LinkName = limitAccessText(event.LinkName, accessHeaderMaxBytes)
	event.Outcome = limitAccessText(event.Outcome, accessIPMaxBytes)
	event.Detail = limitAccessText(event.Detail, accessTextMaxBytes)

	if event.ID == "" {
		event.ID = uuid.NewString()
	}
	_, err := tx.Exec(ctx, `
INSERT INTO access_logs(
  id, owner_id, resource_id, resource_name, resource_file, link_id, link_name, outcome,
  remote_ip, remote_addr, host, query, proto,
  user_agent, referer, forwarded, x_forwarded_for, cf_connecting_ip, cf_ray,
  content_length, tls, method, path, status, detail, hits, first_at, occurred_at, resource_version
)
VALUES(
  $1, $2, $3, $4, $5, $6, $7, $8,
  $9, $10, $11, $12, $13,
  $14, $15, $16, $17, $18, $19,
  $20, $21, $22, $23, $24, $25, 1, $26, $26, NULLIF($27, 0)
)
`, event.ID, optionalUUID(event.OwnerID), optionalUUID(event.ResourceID),
		event.ResourceName, event.ResourceFile, optionalUUID(event.LinkID), event.LinkName, event.Outcome,
		meta.RemoteIP, meta.RemoteAddr, meta.Host, meta.Query, meta.Proto,
		meta.UserAgent, meta.Referer, meta.Forwarded, meta.XForwardedFor, meta.CFConnectingIP, meta.CFRay,
		meta.ContentLength, meta.TLS, meta.Method, meta.Path, event.Status, event.Detail, now, event.Version)
	return err
}

func (d *Store) RecordAccess(ctx context.Context, event AccessEvent, meta RequestMeta) error {
	return d.withTx(ctx, func(tx pgx.Tx) error {
		return insertAccessTx(ctx, tx, event, meta, time.Now().UTC())
	})
}

// AmendAccess corrects the row a delivery was recorded under once its end is
// known: a 304, an upstream that failed, a body that could not be read. The
// row itself is written with the use it records (see ConsumeToken), so this
// only ever narrows what it says; delivered=false clears the version, since
// no content went out.
func (d *Store) AmendAccess(ctx context.Context, id, outcome string, status int, detail string, delivered bool) error {
	if !validUUIDs(id) {
		return ErrNotFound
	}
	_, err := d.db.Exec(ctx, `
UPDATE access_logs
SET outcome = $2, status = $3, detail = $4,
    resource_version = CASE WHEN $5 THEN resource_version ELSE NULL END
WHERE id = $1
`, id, limitAccessText(outcome, accessIPMaxBytes), status, limitAccessText(detail, accessTextMaxBytes), delivered)
	if err != nil {
		return fmt.Errorf("amend access log: %w: %w", ErrInternal, err)
	}
	return nil
}

func (d *Store) ListAccess(ctx context.Context, ownerID, resourceID, outcome string, limit int) ([]AccessLog, error) {
	logs, _, err := d.ListAccessPage(ctx, ownerID, resourceID, outcome, limit, 0)
	return logs, err
}

func (d *Store) ListAccessPage(ctx context.Context, ownerID, resourceID, outcome string, limit, offset int) ([]AccessLog, int, error) {
	if !validUUIDs(ownerID) || resourceID != "" && !validUUIDs(resourceID) {
		return nil, 0, ErrNotFound
	}
	if limit <= 0 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	var total int
	if err := d.db.QueryRow(ctx, `
SELECT COUNT(*)
FROM access_logs
WHERE owner_id = $1
  AND ($2 = '' OR resource_id = NULLIF($2, '')::uuid)
  AND ($3 = '' OR outcome = $3)
`, ownerID, resourceID, outcome).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count access logs: %w: %w", ErrInternal, err)
	}
	// The owner is on the row, so a deleted resource takes nothing with it.
	// The join is only there for the displayed name, which prefers the live
	// resource - it may have been renamed since - and otherwise shows what the
	// row recorded at the time.
	rows, err := d.db.Query(ctx, `
SELECT
  a.id, COALESCE(a.resource_id::text, ''),
  COALESCE(NULLIF(r.name, ''), a.resource_name), COALESCE(NULLIF(r.filename, ''), a.resource_file),
  COALESCE(a.link_id::text, ''), a.link_name, a.outcome, a.remote_ip, a.remote_addr,
  a.host, a.query, a.proto, a.user_agent, a.referer, a.forwarded, a.x_forwarded_for,
  a.cf_connecting_ip, a.cf_ray, a.content_length, a.tls, a.method, a.path, a.status,
  a.detail, a.hits, a.first_at, a.occurred_at,
  COALESCE(a.resource_version, 0), COALESCE(r.version, 0),
  COALESCE(a.resource_version = r.version OR EXISTS (
    SELECT 1 FROM resource_versions v WHERE v.resource_id = a.resource_id AND v.version = a.resource_version
  ), FALSE)
FROM access_logs a
LEFT JOIN resources r ON r.id = a.resource_id
WHERE a.owner_id = $1
  AND ($2 = '' OR a.resource_id = NULLIF($2, '')::uuid)
  AND ($3 = '' OR a.outcome = $3)
ORDER BY a.occurred_at DESC, a.id DESC
LIMIT $4 OFFSET $5
`, ownerID, resourceID, outcome, limit, offset)
	if err != nil {
		return nil, 0, err
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
			&item.Detail, &item.Hits, &item.FirstAt, &item.OccurredAt,
			&item.Version, &item.CurrentVersion, &item.VersionAvailable,
		); err != nil {
			return nil, 0, err
		}
		logs = append(logs, item)
	}
	return logs, total, rows.Err()
}

const (
	// Session-scoped, so the lock and the deletes have to share one connection.
	pruneLockKey = 8964
	pruneBatch   = 5000
)

type PruneResult struct {
	AccessLogs int64
	Sessions   int64
	Pastes     int64
	Versions   int64
}

// Prune removes access logs older than retention, sessions that have expired,
// anonymous pastes nothing can reach any more, and versions replaced longer
// ago than HistoryRetention. Retention zero keeps the
// logs; sessions go either way, since SessionUser only deletes the row for the
// token actually presented, and pastes go either way too - a lifetime measured
// in minutes is what the open endpoint rests on, not a retention setting.
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

	result.Pastes, err = d.pruneAnonymous(ctx, conn, now)
	if err != nil {
		return result, fmt.Errorf("prune anonymous pastes: %w", err)
	}

	result.Versions, err = d.pruneHistory(ctx, conn, now)
	if err != nil {
		return result, fmt.Errorf("prune history: %w", err)
	}

	if _, err = deleteInBatches(ctx, conn, `
DELETE FROM account_exports
WHERE id IN (SELECT id FROM account_exports WHERE created_at < $1 ORDER BY created_at LIMIT $2)
`, now.Add(-ExportWindow)); err != nil {
		return result, fmt.Errorf("prune account exports: %w", err)
	}
	d.pruneMu.Lock()
	d.pruneAt, d.pruneLast = now, result
	d.pruneMu.Unlock()
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
