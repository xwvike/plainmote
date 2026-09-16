package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

type storeQuerier interface {
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
		return UserQuota{}, err
	}
	defer rows.Close()

	var userQuota UserQuota

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
			return UserQuota{}, err
		}
		userQuota.Limit.Resources += activePlans.Plan.Limit.Resources
		userQuota.Limit.StorageBytes += activePlans.Plan.Limit.StorageBytes
		userQuota.ActivePlans = append(userQuota.ActivePlans, activePlans)
	}

	if err := rows.Err(); err != nil {
		return UserQuota{}, err
	}

	userQuota.User = user
	userQuota.Usage, err = d.usageForUser(ctx, q, userID)
	if err != nil {
		return UserQuota{}, err
	}
	return userQuota, nil
}
func (d *Store) UsageForUser(ctx context.Context, userID string) (QuotaUsage, error) {
	return d.usageForUser(ctx, d.db, userID)
}

func (d *Store) usageForUser(ctx context.Context, q storeQuerier, userID string) (QuotaUsage, error) {
	if !validUUIDs(userID) {
		return QuotaUsage{}, ErrNotFound
	}
	var usage QuotaUsage
	err := q.QueryRow(ctx, `SELECT COUNT(*), COALESCE(SUM(content_size),0) FROM resources WHERE owner_id = $1`, userID).Scan(&usage.Resources, &usage.StorageBytes)
	if err != nil {
		return QuotaUsage{}, err
	}
	return usage, nil
}

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
	if tag.RowsAffected() != 1 {
		return errors.New("grant default plan: default plan not found")
	}
	return nil
}
