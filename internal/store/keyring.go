package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// The keyring's shape is fixed here and checked on every write, because the
// service cannot check what is inside it: a wrapped key is 12 bytes of IV,
// the 32-byte key encrypted, and a 16-byte tag. See docs/encryption.md.
const (
	KeyringKDF            = "pbkdf2-sha256"
	KeyringMinIterations  = 600000
	keyringMaxIterations  = 10000000
	keyringSaltBytes      = 16
	keyringWrappedBytes   = 12 + 32 + 16
	KeyringDefaultLockMin = 15
)

// KeyringLockChoices are the idle times an unlocked browser may keep the key.
var KeyringLockChoices = []int{5, 15, 60, 240}

// Keyring is what the service keeps of an account's master password: nothing
// that opens anything without the password or the recovery key.
type Keyring struct {
	KDF               string
	Iterations        int
	Salt              []byte
	WrappedByPassword []byte
	WrappedByRecovery []byte
	LockMinutes       int
	Version           int
	CreatedAt         time.Time
	PasswordAt        time.Time
	RecoveryAt        time.Time
}

// ErrKeyringExists refuses a second master password on an account.
var ErrKeyringExists = refusal("此账号已设置主密码")

// ErrKeyringChanged refuses a change made against a keyring that has since
// been changed elsewhere: writing it would undo the other change.
var ErrKeyringChanged = refusal("主密码已在其他设备上修改，请刷新页面后重试")

func validKeyringPassword(kdf string, iterations int, salt, wrapped []byte) error {
	if kdf != KeyringKDF || iterations < KeyringMinIterations || iterations > keyringMaxIterations {
		return refusal("不支持的密钥派生参数")
	}
	if len(salt) != keyringSaltBytes || len(wrapped) != keyringWrappedBytes {
		return refusal("密钥格式无法识别")
	}
	return nil
}

func validLockMinutes(minutes int) bool {
	for _, choice := range KeyringLockChoices {
		if minutes == choice {
			return true
		}
	}
	return false
}

func (d *Store) Keyring(ctx context.Context, userID string) (Keyring, error) {
	if !validUUIDs(userID) {
		return Keyring{}, ErrNotFound
	}
	var k Keyring
	err := d.db.QueryRow(ctx, `
SELECT kdf, iterations, salt, wrapped_by_password, wrapped_by_recovery, lock_minutes, version, created_at, password_at, recovery_at
FROM keyrings WHERE user_id = $1
`, userID).Scan(&k.KDF, &k.Iterations, &k.Salt, &k.WrappedByPassword, &k.WrappedByRecovery, &k.LockMinutes, &k.Version,
		&k.CreatedAt, &k.PasswordAt, &k.RecoveryAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Keyring{}, ErrNotFound
	}
	if err != nil {
		return Keyring{}, fmt.Errorf("read keyring: %w: %w", ErrInternal, err)
	}
	return k, nil
}

// CreateKeyring keeps a new master password's keyring. An account has one;
// replacing it would orphan everything the old account key wrapped.
func (d *Store) CreateKeyring(ctx context.Context, userID string, k Keyring, now time.Time) (Keyring, error) {
	if !validUUIDs(userID) || userID == AnonymousUserID {
		return Keyring{}, ErrNotFound
	}
	if err := validKeyringPassword(k.KDF, k.Iterations, k.Salt, k.WrappedByPassword); err != nil {
		return Keyring{}, err
	}
	if len(k.WrappedByRecovery) != keyringWrappedBytes {
		return Keyring{}, refusal("密钥格式无法识别")
	}
	tag, err := d.db.Exec(ctx, `
INSERT INTO keyrings(user_id, kdf, iterations, salt, wrapped_by_password, wrapped_by_recovery, lock_minutes, version, created_at, password_at, recovery_at)
VALUES($1, $2, $3, $4, $5, $6, $7, 1, $8, $8, $8)
ON CONFLICT (user_id) DO NOTHING
`, userID, k.KDF, k.Iterations, k.Salt, k.WrappedByPassword, k.WrappedByRecovery, KeyringDefaultLockMin, now)
	if err != nil {
		return Keyring{}, fmt.Errorf("create keyring: %w: %w", ErrInternal, err)
	}
	if tag.RowsAffected() == 0 {
		return Keyring{}, ErrKeyringExists
	}
	return d.Keyring(ctx, userID)
}

// ChangeKeyringPassword replaces what the master password unwraps: a new
// password, or a password set again with the recovery key. The account key
// is the same one, so nothing it wrapped changes.
func (d *Store) ChangeKeyringPassword(ctx context.Context, userID string, version int, kdf string, iterations int, salt, wrapped []byte, now time.Time) (Keyring, error) {
	if err := validKeyringPassword(kdf, iterations, salt, wrapped); err != nil {
		return Keyring{}, err
	}
	return d.updateKeyring(ctx, userID, version, `kdf = $3, iterations = $4, salt = $5, wrapped_by_password = $6, password_at = $7`,
		kdf, iterations, salt, wrapped, now)
}

// ChangeKeyringRecovery replaces the recovery key's copy of the account key.
func (d *Store) ChangeKeyringRecovery(ctx context.Context, userID string, version int, wrapped []byte, now time.Time) (Keyring, error) {
	if len(wrapped) != keyringWrappedBytes {
		return Keyring{}, refusal("密钥格式无法识别")
	}
	return d.updateKeyring(ctx, userID, version, `wrapped_by_recovery = $3, recovery_at = $4`, wrapped, now)
}

func (d *Store) updateKeyring(ctx context.Context, userID string, version int, set string, args ...any) (Keyring, error) {
	if !validUUIDs(userID) {
		return Keyring{}, ErrNotFound
	}
	tag, err := d.db.Exec(ctx, `UPDATE keyrings SET `+set+`, version = version + 1 WHERE user_id = $1 AND version = $2`,
		append([]any{userID, version}, args...)...)
	if err != nil {
		return Keyring{}, fmt.Errorf("update keyring: %w: %w", ErrInternal, err)
	}
	if tag.RowsAffected() == 0 {
		if _, err := d.Keyring(ctx, userID); err != nil {
			return Keyring{}, err
		}
		return Keyring{}, ErrKeyringChanged
	}
	return d.Keyring(ctx, userID)
}

// SetKeyringLock sets how long an unlocked browser keeps the key while idle.
func (d *Store) SetKeyringLock(ctx context.Context, userID string, minutes int) error {
	if !validUUIDs(userID) {
		return ErrNotFound
	}
	if !validLockMinutes(minutes) {
		return refusal("不支持的自动锁定时长")
	}
	tag, err := d.db.Exec(ctx, `UPDATE keyrings SET lock_minutes = $2 WHERE user_id = $1`, userID, minutes)
	if err != nil {
		return fmt.Errorf("set keyring lock: %w: %w", ErrInternal, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
