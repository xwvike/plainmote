package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// The admin interface sees accounts and resources as metadata and totals.
// Nothing here reads a resource body, a version body, a share token or the
// caller details of an access record - there is no query below that could.
// Every change is written to admin_audit in the same transaction as the
// change itself.

// AdminActor is who made an admin request: the key that signed it and the
// address it came from.
type AdminActor struct {
	KeyID    string
	RemoteIP string
}

var (
	ErrAlreadySuspended = errors.New("already_suspended")
	ErrNotSuspended     = errors.New("not_suspended")
	ErrAlreadyTakenDown = errors.New("already_taken_down")
	ErrNotTakenDown     = errors.New("not_taken_down")
	ErrPlanProtected    = errors.New("plan_protected")
	ErrPlanInUse        = errors.New("plan_in_use")
)

// auditEntry is one change as it is recorded. Label is what the target was
// called at the time; Detail is optional structured facts, stored as JSON.
type auditEntry struct {
	Action, TargetType, TargetID, Label, Reason string
	Detail                                      map[string]any
}

func auditTx(ctx context.Context, tx pgx.Tx, actor AdminActor, entry auditEntry, now time.Time) error {
	var detail any
	if len(entry.Detail) > 0 {
		detail = entry.Detail
	}
	_, err := tx.Exec(ctx, `
INSERT INTO admin_audit(id, at, key_id, action, target_type, target_id, target_label, reason, detail, remote_ip)
VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
`, uuid.NewString(), now, actor.KeyID, entry.Action, entry.TargetType, entry.TargetID,
		limitAccessText(entry.Label, accessHeaderMaxBytes), entry.Reason, detail, limitAccessText(actor.RemoteIP, accessIPMaxBytes))
	if err != nil {
		return fmt.Errorf("write audit record: %w: %w", ErrInternal, err)
	}
	return nil
}

// Overview.

type AdminOverview struct {
	Users struct {
		Total          int64 `json:"total"`
		Suspended      int64 `json:"suspended"`
		SignedInLast30 int64 `json:"signed_in_last_30d"`
	} `json:"users"`
	Resources struct {
		Total           int64 `json:"total"`
		Remote          int64 `json:"remote"`
		TakenDown       int64 `json:"taken_down"`
		CurrentBytes    int64 `json:"current_bytes"`
		HistoryBytes    int64 `json:"history_bytes"`
		HistoryVersions int64 `json:"history_versions"`
	} `json:"resources"`
	Links struct {
		Live        int64 `json:"live"`
		EndedLast7d int64 `json:"ended_last_7d"`
	} `json:"links"`
	Access struct {
		Last24h map[string]int64 `json:"last_24h"`
		Last7d  map[string]int64 `json:"last_7d"`
	} `json:"access"`
	Anonymous struct {
		LivePastes int64 `json:"live_pastes"`
		Bytes      int64 `json:"bytes"`
		LimitBytes int64 `json:"limit_bytes"`
	} `json:"anonymous"`
}

func (d *Store) AdminOverview(ctx context.Context, now time.Time) (AdminOverview, error) {
	var o AdminOverview
	wrap := func(err error) error { return fmt.Errorf("admin overview: %w: %w", ErrInternal, err) }
	if err := d.db.QueryRow(ctx, `
SELECT COUNT(*), COUNT(*) FILTER (WHERE suspended_at IS NOT NULL), COUNT(*) FILTER (WHERE last_signed_in_at > $2)
FROM users WHERE id <> $1
`, AnonymousUserID, now.Add(-30*24*time.Hour)).Scan(&o.Users.Total, &o.Users.Suspended, &o.Users.SignedInLast30); err != nil {
		return o, wrap(err)
	}
	if err := d.db.QueryRow(ctx, `
SELECT COUNT(*), COUNT(*) FILTER (WHERE origin_url <> ''), COUNT(*) FILTER (WHERE taken_down_at IS NOT NULL),
       COALESCE(SUM(content_size), 0),
       (SELECT COALESCE(SUM(v.content_size), 0) FROM resource_versions v),
       (SELECT COUNT(*) FROM resource_versions)
FROM resources WHERE owner_id <> $1
`, AnonymousUserID).Scan(&o.Resources.Total, &o.Resources.Remote, &o.Resources.TakenDown,
		&o.Resources.CurrentBytes, &o.Resources.HistoryBytes, &o.Resources.HistoryVersions); err != nil {
		return o, wrap(err)
	}
	if err := d.db.QueryRow(ctx, `
SELECT
  COUNT(*) FILTER (WHERE l.revoked_at IS NULL AND (l.expires_at IS NULL OR l.expires_at > $2)
                   AND (l.max_uses = 0 OR l.used_count < l.max_uses)),
  COUNT(*) FILTER (WHERE LEAST(l.revoked_at,
                               CASE WHEN l.expires_at <= $2 THEN l.expires_at END,
                               CASE WHEN l.max_uses > 0 AND l.used_count >= l.max_uses THEN l.last_used_at END) > $3)
FROM links l JOIN resources r ON r.id = l.resource_id
WHERE r.owner_id <> $1
`, AnonymousUserID, now, now.Add(-7*24*time.Hour)).Scan(&o.Links.Live, &o.Links.EndedLast7d); err != nil {
		return o, wrap(err)
	}
	var err error
	if o.Access.Last24h, err = d.outcomeCounts(ctx, now.Add(-24*time.Hour)); err != nil {
		return o, wrap(err)
	}
	if o.Access.Last7d, err = d.outcomeCounts(ctx, now.Add(-7*24*time.Hour)); err != nil {
		return o, wrap(err)
	}
	if err := d.db.QueryRow(ctx, `
SELECT COUNT(*), COALESCE(SUM(content_size), 0),
       (SELECT COALESCE(SUM(p.max_storage), 0) FROM user_plans up JOIN plans p ON p.id = up.plan_id WHERE up.user_id = $1)
FROM resources WHERE owner_id = $1
`, AnonymousUserID).Scan(&o.Anonymous.LivePastes, &o.Anonymous.Bytes, &o.Anonymous.LimitBytes); err != nil {
		return o, wrap(err)
	}
	return o, nil
}

func (d *Store) outcomeCounts(ctx context.Context, since time.Time) (map[string]int64, error) {
	counts := make(map[string]int64, len(AccessOutcomes))
	for _, outcome := range AccessOutcomes {
		counts[outcome] = 0
	}
	rows, err := d.db.Query(ctx, `SELECT outcome, COALESCE(SUM(hits), 0) FROM access_logs WHERE occurred_at > $1 GROUP BY outcome`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var outcome string
		var count int64
		if err := rows.Scan(&outcome, &count); err != nil {
			return nil, err
		}
		counts[outcome] = count
	}
	return counts, rows.Err()
}

// Health reports whether the database and the object store answer. The
// object store is asked with a metadata request only, where it supports one.
func (d *Store) Health(ctx context.Context) (database, objects string) {
	database, objects = "ok", "unknown"
	if err := d.db.Ping(ctx); err != nil {
		database = "error"
	}
	if pinger, ok := d.blobs.(interface{ Ping(context.Context) error }); ok {
		objects = "ok"
		if err := pinger.Ping(ctx); err != nil {
			objects = "error"
		}
	}
	return database, objects
}

// LastPrune is the last prune that finished, and when; the zero time if none
// has since the process started.
func (d *Store) LastPrune() (PruneResult, time.Time) {
	d.pruneMu.Lock()
	defer d.pruneMu.Unlock()
	return d.pruneLast, d.pruneAt
}

// Users.

type AdminUser struct {
	ID              string     `json:"id"`
	GitHubID        string     `json:"github_id"`
	Login           string     `json:"login"`
	Name            string     `json:"name"`
	CreatedAt       time.Time  `json:"created_at"`
	LastSignedInAt  *time.Time `json:"last_signed_in_at"`
	Status          string     `json:"status"`
	SuspendedReason string     `json:"suspended_reason"`
	Resources       int64      `json:"resources"`
	ResourcesLimit  int64      `json:"resources_limit"`
	LiveLinks       int64      `json:"live_links"`
	Storage         struct {
		CurrentBytes int64 `json:"current_bytes"`
		HistoryBytes int64 `json:"history_bytes"`
		LimitBytes   int64 `json:"limit_bytes"`
	} `json:"storage"`
	Plans []AdminGrant `json:"plans,omitempty"`
}

type AdminGrant struct {
	PlanID    string     `json:"plan_id"`
	Name      string     `json:"name"`
	Default   bool       `json:"default"`
	GrantedAt time.Time  `json:"granted_at"`
	ExpiresAt *time.Time `json:"expires_at"`
}

const adminUserColumns = `
  u.id, u.github_id, u.login, u.name, u.created_at, u.last_signed_in_at, u.suspended_at IS NOT NULL, u.suspended_reason,
  (SELECT COUNT(*) FROM resources r WHERE r.owner_id = u.id),
  (SELECT COUNT(*) FROM links l JOIN resources r ON r.id = l.resource_id
     WHERE r.owner_id = u.id AND l.revoked_at IS NULL AND (l.expires_at IS NULL OR l.expires_at > $1)
       AND (l.max_uses = 0 OR l.used_count < l.max_uses)),
  (SELECT COALESCE(SUM(r.content_size), 0) FROM resources r WHERE r.owner_id = u.id),
  (SELECT COALESCE(SUM(v.content_size), 0) FROM resource_versions v JOIN resources r ON r.id = v.resource_id WHERE r.owner_id = u.id),
  limits.storage, limits.resources`

// adminUserLimits sums the plans in force at $1, the same way the quota does.
const adminUserLimits = `
CROSS JOIN LATERAL (
  SELECT COALESCE(SUM(p.max_storage), 0) AS storage, COALESCE(SUM(p.max_resources), 0) AS resources
  FROM user_plans up JOIN plans p ON p.id = up.plan_id
  WHERE up.user_id = u.id AND up.granted_at <= $1 AND (up.expires_at IS NULL OR up.expires_at > $1)
    AND (p.valid_from IS NULL OR p.valid_from <= $1) AND (p.valid_until IS NULL OR p.valid_until > $1)
) limits`

func scanAdminUser(row rowScanner) (AdminUser, error) {
	var u AdminUser
	var lastSignedIn pgtype.Timestamptz
	var suspended bool
	err := row.Scan(&u.ID, &u.GitHubID, &u.Login, &u.Name, &u.CreatedAt, &lastSignedIn, &suspended, &u.SuspendedReason,
		&u.Resources, &u.LiveLinks, &u.Storage.CurrentBytes, &u.Storage.HistoryBytes, &u.Storage.LimitBytes, &u.ResourcesLimit)
	u.LastSignedInAt = timePointer(lastSignedIn)
	u.Status = "active"
	if suspended {
		u.Status = "suspended"
	}
	return u, err
}

// AdminListUsers lists accounts, newest first. query matches a login or a
// GitHub ID; status is "active", "suspended" or empty for both.
func (d *Store) AdminListUsers(ctx context.Context, query, status string, limit, offset int, now time.Time) ([]AdminUser, int, error) {
	query = strings.TrimSpace(query)
	pattern := "%" + escapeLikePattern(query) + "%"
	where := `u.id <> $2 AND ($3 = '' OR u.login ILIKE $4 ESCAPE '\' OR u.github_id = $3)
  AND ($5 = '' OR ($5 = 'suspended') = (u.suspended_at IS NOT NULL))`
	var total int
	if err := d.db.QueryRow(ctx, `SELECT COUNT(*) FROM users u WHERE $1::timestamptz IS NOT NULL AND `+where,
		now, AnonymousUserID, query, pattern, status).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("admin users: %w: %w", ErrInternal, err)
	}
	rows, err := d.db.Query(ctx, `SELECT `+adminUserColumns+` FROM users u`+adminUserLimits+` WHERE `+where+`
ORDER BY u.created_at DESC, u.id LIMIT $6 OFFSET $7`, now, AnonymousUserID, query, pattern, status, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("admin users: %w: %w", ErrInternal, err)
	}
	users, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (AdminUser, error) { return scanAdminUser(row) })
	if err != nil {
		return nil, 0, fmt.Errorf("admin users: %w: %w", ErrInternal, err)
	}
	return users, total, nil
}

func (d *Store) AdminGetUser(ctx context.Context, id string, now time.Time) (AdminUser, error) {
	if !validUUIDs(id) || id == AnonymousUserID {
		return AdminUser{}, ErrNotFound
	}
	user, err := scanAdminUser(d.db.QueryRow(ctx, `SELECT `+adminUserColumns+` FROM users u`+adminUserLimits+` WHERE u.id = $2`, now, id))
	if err != nil {
		return AdminUser{}, translateNotFound(err)
	}
	plans, _, err := activePlansForUser(ctx, d.db, id, now)
	if err != nil {
		return AdminUser{}, fmt.Errorf("admin user plans: %w: %w", ErrInternal, err)
	}
	user.Plans = []AdminGrant{}
	for _, plan := range plans {
		user.Plans = append(user.Plans, AdminGrant{
			PlanID: plan.Plan.ID, Name: plan.Plan.Name, Default: plan.Plan.IsDefault,
			GrantedAt: plan.GrantedAt, ExpiresAt: plan.ExpiresAt,
		})
	}
	return user, nil
}

// AdminSuspendUser suspends an account: its sessions end at once, it cannot
// sign in again, and its links stop delivering. Nothing it holds is deleted.
func (d *Store) AdminSuspendUser(ctx context.Context, actor AdminActor, id, reason string, now time.Time) error {
	if !validUUIDs(id) || id == AnonymousUserID {
		return ErrNotFound
	}
	return d.withTx(ctx, func(tx pgx.Tx) error {
		var suspended bool
		var login string
		if err := tx.QueryRow(ctx, `SELECT suspended_at IS NOT NULL, login FROM users WHERE id = $1 FOR UPDATE`, id).Scan(&suspended, &login); err != nil {
			return translateNotFound(err)
		}
		if suspended {
			return ErrAlreadySuspended
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET suspended_at = $2, suspended_reason = $3 WHERE id = $1`, id, now, reason); err != nil {
			return fmt.Errorf("suspend user: %w: %w", ErrInternal, err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, id); err != nil {
			return fmt.Errorf("end sessions: %w: %w", ErrInternal, err)
		}
		return auditTx(ctx, tx, actor, auditEntry{Action: "user.suspend", TargetType: "user", TargetID: id, Label: login, Reason: reason}, now)
	})
}

func (d *Store) AdminUnsuspendUser(ctx context.Context, actor AdminActor, id, reason string, now time.Time) error {
	if !validUUIDs(id) || id == AnonymousUserID {
		return ErrNotFound
	}
	return d.withTx(ctx, func(tx pgx.Tx) error {
		var suspended bool
		var login string
		if err := tx.QueryRow(ctx, `SELECT suspended_at IS NOT NULL, login FROM users WHERE id = $1 FOR UPDATE`, id).Scan(&suspended, &login); err != nil {
			return translateNotFound(err)
		}
		if !suspended {
			return ErrNotSuspended
		}
		if _, err := tx.Exec(ctx, `UPDATE users SET suspended_at = NULL, suspended_reason = '' WHERE id = $1`, id); err != nil {
			return fmt.Errorf("unsuspend user: %w: %w", ErrInternal, err)
		}
		return auditTx(ctx, tx, actor, auditEntry{Action: "user.unsuspend", TargetType: "user", TargetID: id, Label: login, Reason: reason}, now)
	})
}

// Plans.

type AdminPlan struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	MaxResources int64  `json:"max_resources"`
	MaxStorage   int64  `json:"max_storage"`
	Default      bool   `json:"default"`
	Users        int64  `json:"users"`
}

func (d *Store) AdminListPlans(ctx context.Context) ([]AdminPlan, error) {
	rows, err := d.db.Query(ctx, `
SELECT p.id, p.name, p.max_resources, p.max_storage, p.is_default,
       (SELECT COUNT(*) FROM user_plans up WHERE up.plan_id = p.id AND up.user_id <> $1)
FROM plans p WHERE p.id <> $2 ORDER BY p.is_default DESC, p.name
`, AnonymousUserID, anonymousPlanID)
	if err != nil {
		return nil, fmt.Errorf("admin plans: %w: %w", ErrInternal, err)
	}
	plans, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (AdminPlan, error) {
		var p AdminPlan
		err := row.Scan(&p.ID, &p.Name, &p.MaxResources, &p.MaxStorage, &p.Default, &p.Users)
		return p, err
	})
	if err != nil {
		return nil, fmt.Errorf("admin plans: %w: %w", ErrInternal, err)
	}
	return plans, nil
}

// AdminCreatePlan adds a plan that can be granted on top of the default.
func (d *Store) AdminCreatePlan(ctx context.Context, actor AdminActor, name string, maxResources, maxStorage int64, reason string, now time.Time) (AdminPlan, error) {
	plan := AdminPlan{ID: uuid.NewString(), Name: strings.TrimSpace(name), MaxResources: maxResources, MaxStorage: maxStorage}
	if plan.Name == "" || maxResources < 0 || maxStorage < 0 {
		return AdminPlan{}, errors.New("invalid_plan")
	}
	err := d.withTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
INSERT INTO plans(id, name, max_resources, max_storage, is_default, created_at, updated_at)
VALUES($1, $2, $3, $4, FALSE, $5, $5) ON CONFLICT (name) DO NOTHING
`, plan.ID, plan.Name, maxResources, maxStorage, now)
		if err != nil {
			return fmt.Errorf("create plan: %w: %w", ErrInternal, err)
		}
		if tag.RowsAffected() == 0 {
			return errors.New("plan_exists")
		}
		return auditTx(ctx, tx, actor, auditEntry{Action: "plan.create", TargetType: "plan", TargetID: plan.ID, Label: plan.Name, Reason: reason,
			Detail: map[string]any{"max_resources": maxResources, "max_storage": maxStorage}}, now)
	})
	return plan, err
}

// AdminDeletePlan deletes a plan nobody holds. One still granted - expired
// grants included, until they are revoked - is refused rather than taken
// from its holders behind their backs, and the default plan is what every
// account stands on.
func (d *Store) AdminDeletePlan(ctx context.Context, actor AdminActor, id, reason string, now time.Time) error {
	if !validUUIDs(id) || id == anonymousPlanID {
		return ErrNotFound
	}
	return d.withTx(ctx, func(tx pgx.Tx) error {
		var name string
		var isDefault bool
		var maxResources, maxStorage int64
		if err := tx.QueryRow(ctx, `SELECT name, is_default, max_resources, max_storage FROM plans WHERE id = $1 FOR UPDATE`, id).
			Scan(&name, &isDefault, &maxResources, &maxStorage); err != nil {
			return translateNotFound(err)
		}
		if isDefault {
			return ErrPlanProtected
		}
		var holders int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM user_plans WHERE plan_id = $1`, id).Scan(&holders); err != nil {
			return fmt.Errorf("count plan holders: %w: %w", ErrInternal, err)
		}
		if holders > 0 {
			return ErrPlanInUse
		}
		if _, err := tx.Exec(ctx, `DELETE FROM plans WHERE id = $1`, id); err != nil {
			return fmt.Errorf("delete plan: %w: %w", ErrInternal, err)
		}
		return auditTx(ctx, tx, actor, auditEntry{Action: "plan.delete", TargetType: "plan", TargetID: id, Label: name, Reason: reason,
			Detail: map[string]any{"max_resources": maxResources, "max_storage": maxStorage}}, now)
	})
}

// anonymousPlanID is the fuse on quick shares; it is not something to grant.
const anonymousPlanID = "00000000-0000-0000-0000-000000000002"

// AdminGrantPlan adds a plan to an account, on top of what it has; granting
// one it already has changes the expiry.
func (d *Store) AdminGrantPlan(ctx context.Context, actor AdminActor, userID, planID string, expiresAt *time.Time, reason string, now time.Time) error {
	if !validUUIDs(userID, planID) || userID == AnonymousUserID || planID == anonymousPlanID {
		return ErrNotFound
	}
	return d.withTx(ctx, func(tx pgx.Tx) error {
		var login, planName string
		if err := tx.QueryRow(ctx, `SELECT login FROM users WHERE id = $1`, userID).Scan(&login); err != nil {
			return translateNotFound(err)
		}
		if err := tx.QueryRow(ctx, `SELECT name FROM plans WHERE id = $1`, planID).Scan(&planName); err != nil {
			return translateNotFound(err)
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO user_plans(user_id, plan_id, granted_at, expires_at) VALUES($1, $2, $3, $4)
ON CONFLICT (user_id, plan_id) DO UPDATE SET expires_at = EXCLUDED.expires_at
`, userID, planID, now, expiresAt); err != nil {
			return fmt.Errorf("grant plan: %w: %w", ErrInternal, err)
		}
		return auditTx(ctx, tx, actor, auditEntry{Action: "user.plan.grant", TargetType: "user", TargetID: userID, Label: login, Reason: reason,
			Detail: map[string]any{"plan_id": planID, "plan_name": planName, "expires_at": expiresAt}}, now)
	})
}

// AdminRevokePlan takes back a granted plan. The default plan is what every
// account stands on and cannot be taken back.
func (d *Store) AdminRevokePlan(ctx context.Context, actor AdminActor, userID, planID, reason string, now time.Time) error {
	if !validUUIDs(userID, planID) || userID == AnonymousUserID {
		return ErrNotFound
	}
	return d.withTx(ctx, func(tx pgx.Tx) error {
		var isDefault bool
		var planName, login string
		if err := tx.QueryRow(ctx, `SELECT is_default, name FROM plans WHERE id = $1`, planID).Scan(&isDefault, &planName); err != nil {
			return translateNotFound(err)
		}
		if err := tx.QueryRow(ctx, `SELECT login FROM users WHERE id = $1`, userID).Scan(&login); err != nil {
			return translateNotFound(err)
		}
		if isDefault {
			return ErrPlanProtected
		}
		tag, err := tx.Exec(ctx, `DELETE FROM user_plans WHERE user_id = $1 AND plan_id = $2`, userID, planID)
		if err != nil {
			return fmt.Errorf("revoke plan: %w: %w", ErrInternal, err)
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		return auditTx(ctx, tx, actor, auditEntry{Action: "user.plan.revoke", TargetType: "user", TargetID: userID, Label: login, Reason: reason,
			Detail: map[string]any{"plan_id": planID, "plan_name": planName}}, now)
	})
}

// Resources.

type AdminResource struct {
	ID    string `json:"id"`
	Owner struct {
		ID    string `json:"id"`
		Login string `json:"login"`
	} `json:"owner"`
	Name            string    `json:"name"`
	Filename        string    `json:"filename"`
	Kind            string    `json:"kind"`
	ContentType     string    `json:"content_type"`
	Size            int64     `json:"size"`
	OriginHost      string    `json:"origin_host"`
	Version         int       `json:"version"`
	HistoryVersions int64     `json:"history_versions"`
	HistoryBytes    int64     `json:"history_bytes"`
	LiveLinks       int64     `json:"live_links"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	Status          string    `json:"status"`
	TakedownReason  string    `json:"takedown_reason"`
}

const adminResourceColumns = `
  r.id, r.owner_id, u.login, r.name, r.filename, r.origin_url, r.content_type, r.content_size, r.version,
  (SELECT COUNT(*) FROM resource_versions v WHERE v.resource_id = r.id),
  (SELECT COALESCE(SUM(v.content_size), 0) FROM resource_versions v WHERE v.resource_id = r.id),
  (SELECT COUNT(*) FROM links l WHERE l.resource_id = r.id AND l.revoked_at IS NULL
     AND (l.expires_at IS NULL OR l.expires_at > $1) AND (l.max_uses = 0 OR l.used_count < l.max_uses)),
  r.created_at, r.updated_at, r.taken_down_at IS NOT NULL, r.takedown_reason`

func scanAdminResource(row rowScanner) (AdminResource, error) {
	var r AdminResource
	var origin string
	var takenDown bool
	err := row.Scan(&r.ID, &r.Owner.ID, &r.Owner.Login, &r.Name, &r.Filename, &origin, &r.ContentType, &r.Size, &r.Version,
		&r.HistoryVersions, &r.HistoryBytes, &r.LiveLinks, &r.CreatedAt, &r.UpdatedAt, &takenDown, &r.TakedownReason)
	// A reference is named by its host alone: the full address is the owner's
	// and may carry a path or query that says more than the operator needs.
	r.Kind = "stored"
	if origin != "" {
		r.Kind = "remote"
		if parsed, err := url.Parse(origin); err == nil {
			r.OriginHost = parsed.Hostname()
		}
	}
	r.Status = "active"
	if takenDown {
		r.Status = "taken_down"
	}
	return r, err
}

// AdminListResources lists resources, newest first. owner narrows to one
// account; query matches the name or filename; status is "active",
// "taken_down" or empty for both.
func (d *Store) AdminListResources(ctx context.Context, owner, query, status string, limit, offset int, now time.Time) ([]AdminResource, int, error) {
	if owner != "" && !validUUIDs(owner) {
		return []AdminResource{}, 0, nil
	}
	query = strings.TrimSpace(query)
	pattern := "%" + escapeLikePattern(query) + "%"
	where := `($2 = '' OR r.owner_id = NULLIF($2, '')::uuid)
  AND ($3 = '' OR r.name ILIKE $4 ESCAPE '\' OR r.filename ILIKE $4 ESCAPE '\')
  AND ($5 = '' OR ($5 = 'taken_down') = (r.taken_down_at IS NOT NULL))`
	var total int
	if err := d.db.QueryRow(ctx, `SELECT COUNT(*) FROM resources r WHERE $1::timestamptz IS NOT NULL AND `+where,
		now, owner, query, pattern, status).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("admin resources: %w: %w", ErrInternal, err)
	}
	rows, err := d.db.Query(ctx, `SELECT `+adminResourceColumns+`
FROM resources r JOIN users u ON u.id = r.owner_id WHERE `+where+`
ORDER BY r.created_at DESC, r.id LIMIT $6 OFFSET $7`, now, owner, query, pattern, status, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("admin resources: %w: %w", ErrInternal, err)
	}
	resources, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (AdminResource, error) { return scanAdminResource(row) })
	if err != nil {
		return nil, 0, fmt.Errorf("admin resources: %w: %w", ErrInternal, err)
	}
	return resources, total, nil
}

func (d *Store) AdminGetResource(ctx context.Context, id string, now time.Time) (AdminResource, error) {
	if !validUUIDs(id) {
		return AdminResource{}, ErrNotFound
	}
	resource, err := scanAdminResource(d.db.QueryRow(ctx, `SELECT `+adminResourceColumns+`
FROM resources r JOIN users u ON u.id = r.owner_id WHERE r.id = $2`, now, id))
	if err != nil {
		return AdminResource{}, translateNotFound(err)
	}
	return resource, nil
}

// AdminLink is a link as the operator sees it: its terms and state, never
// its token.
type AdminLink struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at"`
	MaxUses   int        `json:"max_uses"`
	UsedCount int        `json:"used_count"`
	RevokedAt *time.Time `json:"revoked_at"`
	Live      bool       `json:"live"`
}

const adminLinkColumns = `id, name, created_at, expires_at, max_uses, used_count, revoked_at`

func scanAdminLink(row rowScanner, now time.Time) (AdminLink, error) {
	var link AdminLink
	var expires, revoked pgtype.Timestamptz
	err := row.Scan(&link.ID, &link.Name, &link.CreatedAt, &expires, &link.MaxUses, &link.UsedCount, &revoked)
	link.ExpiresAt, link.RevokedAt = timePointer(expires), timePointer(revoked)
	link.Live = Link{ExpiresAt: link.ExpiresAt, RevokedAt: link.RevokedAt, MaxUses: link.MaxUses, UsedCount: link.UsedCount}.Live(now)
	return link, err
}

// AdminResourceLinks lists a resource's links, newest first, ended ones
// included, so each can be revoked by its ID. Tokens are not read.
func (d *Store) AdminResourceLinks(ctx context.Context, resourceID string, limit, offset int, now time.Time) ([]AdminLink, int, error) {
	if !validUUIDs(resourceID) {
		return nil, 0, ErrNotFound
	}
	var total int
	var found string
	if err := d.db.QueryRow(ctx, `SELECT r.id::text, (SELECT COUNT(*) FROM links WHERE resource_id = r.id) FROM resources r WHERE r.id = $1`,
		resourceID).Scan(&found, &total); err != nil {
		return nil, 0, translateNotFound(err)
	}
	rows, err := d.db.Query(ctx, `SELECT `+adminLinkColumns+` FROM links WHERE resource_id = $1
ORDER BY created_at DESC, id DESC LIMIT $2 OFFSET $3`, resourceID, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("admin links: %w: %w", ErrInternal, err)
	}
	links, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (AdminLink, error) { return scanAdminLink(row, now) })
	if err != nil {
		return nil, 0, fmt.Errorf("admin links: %w: %w", ErrInternal, err)
	}
	return links, total, nil
}

// AdminLookup finds the resource a share token opens, for a report that
// quotes the address. Nothing is spent and nothing is recorded against the
// link.
func (d *Store) AdminLookup(ctx context.Context, token string, now time.Time) (AdminResource, AdminLink, error) {
	if !ValidShareToken(token) {
		return AdminResource{}, AdminLink{}, ErrNotFound
	}
	var resourceID string
	if err := d.db.QueryRow(ctx, `SELECT resource_id::text FROM links WHERE token_hash = $1`, hashToken(token)).Scan(&resourceID); err != nil {
		return AdminResource{}, AdminLink{}, translateNotFound(err)
	}
	link, err := scanAdminLink(d.db.QueryRow(ctx, `SELECT `+adminLinkColumns+` FROM links WHERE token_hash = $1`, hashToken(token)), now)
	if err != nil {
		return AdminResource{}, AdminLink{}, translateNotFound(err)
	}
	resource, err := d.AdminGetResource(ctx, resourceID, now)
	return resource, link, err
}

// AdminTakedown takes a resource down: its links stop delivering and no new
// ones can be made; its owner sees the reason. A quick share has no owner to
// see anything, so taking one down deletes it. deleted reports which.
func (d *Store) AdminTakedown(ctx context.Context, actor AdminActor, id, reason string, now time.Time) (deleted bool, err error) {
	if !validUUIDs(id) {
		return false, ErrNotFound
	}
	var owner, label string
	if err := d.db.QueryRow(ctx, `SELECT owner_id, COALESCE(NULLIF(name, ''), filename) FROM resources WHERE id = $1`, id).Scan(&owner, &label); err != nil {
		return false, translateNotFound(err)
	}
	if owner == AnonymousUserID {
		return true, d.adminDelete(ctx, actor, auditEntry{Action: "resource.takedown", TargetType: "resource", TargetID: id, Label: label, Reason: reason,
			Detail: map[string]any{"deleted": true}}, owner, now)
	}
	return false, d.withTx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE resources SET taken_down_at = $2, takedown_reason = $3 WHERE id = $1 AND taken_down_at IS NULL`, id, now, reason)
		if err != nil {
			return fmt.Errorf("take down: %w: %w", ErrInternal, err)
		}
		if tag.RowsAffected() == 0 {
			return ErrAlreadyTakenDown
		}
		return auditTx(ctx, tx, actor, auditEntry{Action: "resource.takedown", TargetType: "resource", TargetID: id, Label: label, Reason: reason}, now)
	})
}

func (d *Store) AdminRestoreResource(ctx context.Context, actor AdminActor, id, reason string, now time.Time) error {
	if !validUUIDs(id) {
		return ErrNotFound
	}
	return d.withTx(ctx, func(tx pgx.Tx) error {
		var takenDown bool
		var label string
		if err := tx.QueryRow(ctx, `SELECT taken_down_at IS NOT NULL, COALESCE(NULLIF(name, ''), filename) FROM resources WHERE id = $1 FOR UPDATE`, id).Scan(&takenDown, &label); err != nil {
			return translateNotFound(err)
		}
		if !takenDown {
			return ErrNotTakenDown
		}
		if _, err := tx.Exec(ctx, `UPDATE resources SET taken_down_at = NULL, takedown_reason = '' WHERE id = $1`, id); err != nil {
			return fmt.Errorf("restore resource: %w: %w", ErrInternal, err)
		}
		return auditTx(ctx, tx, actor, auditEntry{Action: "resource.restore", TargetType: "resource", TargetID: id, Label: label, Reason: reason}, now)
	})
}

// AdminDeleteResource deletes a resource exactly as its owner would: body,
// versions and links go, the access history stays.
func (d *Store) AdminDeleteResource(ctx context.Context, actor AdminActor, id, reason string, now time.Time) error {
	if !validUUIDs(id) {
		return ErrNotFound
	}
	var owner, label string
	if err := d.db.QueryRow(ctx, `SELECT owner_id, COALESCE(NULLIF(name, ''), filename) FROM resources WHERE id = $1`, id).Scan(&owner, &label); err != nil {
		return translateNotFound(err)
	}
	return d.adminDelete(ctx, actor, auditEntry{Action: "resource.delete", TargetType: "resource", TargetID: id, Label: label, Reason: reason}, owner, now)
}

// adminDelete records the deletion before making it: a record of a deletion
// that then failed is a smaller wrong than a deletion with no record.
func (d *Store) adminDelete(ctx context.Context, actor AdminActor, entry auditEntry, owner string, now time.Time) error {
	if err := d.withTx(ctx, func(tx pgx.Tx) error {
		return auditTx(ctx, tx, actor, entry, now)
	}); err != nil {
		return err
	}
	return d.DeleteResource(ctx, owner, entry.TargetID)
}

// AdminRevokeLink revokes one link, as its owner could.
func (d *Store) AdminRevokeLink(ctx context.Context, actor AdminActor, id, reason string, now time.Time) error {
	if !validUUIDs(id) {
		return ErrNotFound
	}
	return d.withTx(ctx, func(tx pgx.Tx) error {
		// An unnamed link is called by its resource's name.
		var label, resourceID string
		if err := tx.QueryRow(ctx, `
SELECT COALESCE(NULLIF(l.name, ''), NULLIF(r.name, ''), r.filename), r.id::text
FROM links l JOIN resources r ON r.id = l.resource_id WHERE l.id = $1
`, id).Scan(&label, &resourceID); err != nil {
			return translateNotFound(err)
		}
		tag, err := tx.Exec(ctx, `UPDATE links SET revoked_at = $2 WHERE id = $1 AND revoked_at IS NULL`, id, now)
		if err != nil {
			return fmt.Errorf("revoke link: %w: %w", ErrInternal, err)
		}
		if tag.RowsAffected() == 0 {
			return errors.New("already_revoked")
		}
		return auditTx(ctx, tx, actor, auditEntry{Action: "link.revoke", TargetType: "link", TargetID: id, Label: label, Reason: reason,
			Detail: map[string]any{"resource_id": resourceID}}, now)
	})
}

// Audit.

type AdminAuditEntry struct {
	ID          string          `json:"id"`
	At          time.Time       `json:"at"`
	Key         string          `json:"key"`
	Action      string          `json:"action"`
	TargetType  string          `json:"target_type"`
	TargetID    string          `json:"target_id"`
	TargetLabel string          `json:"target_label"`
	Reason      string          `json:"reason"`
	Detail      json.RawMessage `json:"detail"`
	RemoteIP    string          `json:"remote_ip"`
}

func (d *Store) AdminAudit(ctx context.Context, target string, limit, offset int) ([]AdminAuditEntry, int, error) {
	var total int
	if err := d.db.QueryRow(ctx, `SELECT COUNT(*) FROM admin_audit WHERE $1 = '' OR target_id = $1`, target).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("admin audit: %w: %w", ErrInternal, err)
	}
	rows, err := d.db.Query(ctx, `
SELECT id, at, key_id, action, target_type, target_id, target_label, reason, COALESCE(detail, 'null'::jsonb)::text, remote_ip FROM admin_audit
WHERE $1 = '' OR target_id = $1 ORDER BY at DESC, id DESC LIMIT $2 OFFSET $3
`, target, limit, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("admin audit: %w: %w", ErrInternal, err)
	}
	entries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (AdminAuditEntry, error) {
		var e AdminAuditEntry
		var detail string
		err := row.Scan(&e.ID, &e.At, &e.Key, &e.Action, &e.TargetType, &e.TargetID, &e.TargetLabel, &e.Reason, &detail, &e.RemoteIP)
		e.Detail = json.RawMessage(detail)
		return e, err
	})
	if err != nil {
		return nil, 0, fmt.Errorf("admin audit: %w: %w", ErrInternal, err)
	}
	return entries, total, nil
}
