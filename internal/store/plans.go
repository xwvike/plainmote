package store

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// storeQuerier is the part of the pool and of a transaction that is the same,
// so a query can be written once and run either inside a transaction or on its
// own. Both *pgxpool.Pool and pgx.Tx satisfy it.
type storeQuerier interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (d *Store) QuotaForUser(ctx context.Context, userID string, at time.Time) (UserQuota, error) {
	return d.quotaForUser(ctx, d.db, userID, at)
}

func (d *Store) quotaForUser(ctx context.Context, q storeQuerier, userID string, at time.Time) (UserQuota, error) {

	if !validUUIDs(userID) {
		return UserQuota{}, ErrNotFound
	}

	var user User
	err := q.QueryRow(ctx,
		`SELECT id, github_id, login, name, avatar_url FROM users WHERE id = $1`, userID).Scan(&user.ID, &user.GitHubID, &user.Login, &user.Name, &user.AvatarURL)
	if err != nil {
		return UserQuota{}, translateNotFound(err)
	}

	plans, limit, err := activePlansForUser(ctx, q, userID, at)
	if err != nil {
		return UserQuota{}, err
	}

	usage, err := usageForUser(ctx, q, userID, "")
	if err != nil {
		return UserQuota{}, err
	}
	return UserQuota{User: user, Limit: limit, Usage: usage, ActivePlans: plans}, nil
}

// activePlansForUser reads the grants in force at `at` and the ceiling they add
// up to. The write paths take the limit without the rest of UserQuota, so the
// account is not read again just to enforce it.
func activePlansForUser(ctx context.Context, q storeQuerier, userID string, at time.Time) ([]ActivePlans, QuotaLimit, error) {
	rows, err := q.Query(ctx,
		`SELECT p.id, p.name, p.max_resources, p.max_storage, p.is_default, p.valid_from, p.valid_until, p.created_at, p.updated_at, up.granted_at, up.expires_at
		FROM user_plans up
		 JOIN plans p ON up.plan_id = p.id WHERE up.user_id = $1
				AND up.granted_at <= $2
				AND (up.expires_at is NULL OR up.expires_at > $2)
				AND (p.valid_from is NULL OR p.valid_from <= $2)
				AND (p.valid_until is NULL OR p.valid_until > $2)
		ORDER BY p.is_default DESC, up.granted_at, p.id`,
		userID, at)
	if err != nil {
		return nil, QuotaLimit{}, err
	}
	defer rows.Close()

	var plans []ActivePlans
	var limit QuotaLimit
	for rows.Next() {
		var activePlans ActivePlans
		err := rows.Scan(
			&activePlans.Plan.ID,
			&activePlans.Plan.Name,
			&activePlans.Plan.Limit.Resources,
			&activePlans.Plan.Limit.StorageBytes,
			&activePlans.Plan.IsDefault,
			&activePlans.Plan.ValidFrom,
			&activePlans.Plan.ValidUntil,
			&activePlans.Plan.CreatedAt,
			&activePlans.Plan.UpdatedAt,
			&activePlans.GrantedAt,
			&activePlans.ExpiresAt)
		if err != nil {
			return nil, QuotaLimit{}, err
		}
		limit.Resources += activePlans.Plan.Limit.Resources
		limit.StorageBytes += activePlans.Plan.Limit.StorageBytes
		plans = append(plans, activePlans)
	}
	return plans, limit, rows.Err()
}

func (d *Store) UsageForUser(ctx context.Context, userID string) (QuotaUsage, error) {
	return usageForUser(ctx, d.db, userID, "")
}

// usageForUser totals what the account holds. excludeID leaves one resource
// out, so an update is measured against its replacement rather than counted at
// both its old and its new size.
func usageForUser(ctx context.Context, q storeQuerier, userID, excludeID string) (QuotaUsage, error) {
	if !validUUIDs(userID) {
		return QuotaUsage{}, ErrNotFound
	}
	var usage QuotaUsage
	// History is reported but never excluded: it is not what a save is
	// measured against, only what gives way to one.
	err := q.QueryRow(ctx, `
SELECT COUNT(*), COALESCE(SUM(content_size), 0),
       (SELECT COALESCE(SUM(v.content_size), 0) FROM resource_versions v
        JOIN resources r ON r.id = v.resource_id WHERE r.owner_id = $1)
FROM resources r
WHERE r.owner_id = $1 AND ($2 = '' OR r.id <> NULLIF($2, '')::uuid) AND `+liveResource+`
`, userID, excludeID).Scan(&usage.Resources, &usage.StorageBytes, &usage.HistoryBytes)
	if err != nil {
		return QuotaUsage{}, err
	}
	return usage, nil
}

// quotaGate locks the account for the length of the transaction and reports the
// ceiling and the usage the caller has to fit under. The lock is what makes the
// check binding: without it two uploads can both read a usage that leaves room
// and both commit.
func quotaGate(ctx context.Context, tx pgx.Tx, ownerID, excludeID string, at time.Time) (QuotaLimit, QuotaUsage, error) {
	var locked string
	if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id = $1 FOR UPDATE`, ownerID).Scan(&locked); err != nil {
		return QuotaLimit{}, QuotaUsage{}, translateNotFound(err)
	}
	_, limit, err := activePlansForUser(ctx, tx, ownerID, at)
	if err != nil {
		return QuotaLimit{}, QuotaUsage{}, err
	}
	usage, err := usageForUser(ctx, tx, ownerID, excludeID)
	if err != nil {
		return QuotaLimit{}, QuotaUsage{}, err
	}
	return limit, usage, nil
}

// grantDefaultPlan gives a new account the plan marked default, if there is
// one. A missing default is a deployment problem, not a reason to turn someone
// away at the door: the account is created either way and lands on a zero
// quota, where it can sign in and be told what is wrong, instead of failing
// halfway through an OAuth callback with nothing to show for it.
func grantDefaultPlan(ctx context.Context, tx pgx.Tx, userID string, grantedAt time.Time) error {
	tag, err := tx.Exec(ctx, `
INSERT INTO user_plans (user_id, plan_id, granted_at)
SELECT $1, id, $2
FROM plans
WHERE is_default
ON CONFLICT (user_id, plan_id) DO NOTHING
`, userID, grantedAt)
	if err != nil {
		return fmt.Errorf("grant default plan: %w", err)
	}
	if tag.RowsAffected() == 0 {
		fmt.Fprintf(os.Stderr, "no default plan to grant user %s; the account starts with no quota\n", userID)
	}
	return nil
}
