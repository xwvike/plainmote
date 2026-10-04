package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func testKeyring() Keyring {
	return Keyring{
		KDF: KeyringKDF, Iterations: KeyringMinIterations, Salt: bytes.Repeat([]byte{1}, 16),
		WrappedByPassword: bytes.Repeat([]byte{2}, 60), WrappedByRecovery: bytes.Repeat([]byte{3}, 60),
	}
}

func TestKeyringLifecycle(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if _, err := db.Keyring(ctx, user.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no keyring yet: %v", err)
	}
	created, err := db.CreateKeyring(ctx, user.ID, testKeyring(), now)
	if err != nil || created.Version != 1 || created.LockMinutes != KeyringDefaultLockMin || !bytes.Equal(created.WrappedByRecovery, testKeyring().WrappedByRecovery) {
		t.Fatalf("created: %+v %v", created, err)
	}
	if _, err := db.CreateKeyring(ctx, user.ID, testKeyring(), now); !errors.Is(err, ErrKeyringExists) {
		t.Fatalf("a second keyring: %v", err)
	}

	salt, wrapped := bytes.Repeat([]byte{4}, 16), bytes.Repeat([]byte{5}, 60)
	changed, err := db.ChangeKeyringPassword(ctx, user.ID, 1, KeyringKDF, 700000, salt, wrapped, now.Add(time.Minute))
	if err != nil || changed.Version != 2 || changed.Iterations != 700000 || !bytes.Equal(changed.Salt, salt) || !bytes.Equal(changed.WrappedByPassword, wrapped) ||
		!bytes.Equal(changed.WrappedByRecovery, created.WrappedByRecovery) {
		t.Fatalf("password changed: %+v %v", changed, err)
	}
	// A change made against the version before is refused, not merged.
	if _, err := db.ChangeKeyringRecovery(ctx, user.ID, 1, bytes.Repeat([]byte{6}, 60), now); !errors.Is(err, ErrKeyringChanged) {
		t.Fatalf("a stale change: %v", err)
	}
	recovered, err := db.ChangeKeyringRecovery(ctx, user.ID, 2, bytes.Repeat([]byte{6}, 60), now)
	if err != nil || recovered.Version != 3 || !bytes.Equal(recovered.WrappedByPassword, wrapped) {
		t.Fatalf("recovery changed: %+v %v", recovered, err)
	}

	if err := db.SetKeyringLock(ctx, user.ID, 60); err != nil {
		t.Fatal(err)
	}
	if err := db.SetKeyringLock(ctx, user.ID, 7); err == nil || !IsRefusal(err) {
		t.Fatalf("an unsupported lock time: %v", err)
	}
	if got, _ := db.Keyring(ctx, user.ID); got.LockMinutes != 60 || got.Version != 3 {
		t.Fatalf("lock time: %+v", got)
	}

	// Another account's keyring is not this one's to read or change.
	other, err := db.UpsertUser(ctx, "200", "bob", "Bob", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Keyring(ctx, other.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another account: %v", err)
	}
	if _, err := db.ChangeKeyringRecovery(ctx, other.ID, 3, bytes.Repeat([]byte{7}, 60), now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another account changed: %v", err)
	}

	// Deleting the account takes the keyring with it.
	if err := db.DeleteAccount(ctx, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Keyring(ctx, user.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after the account: %v", err)
	}
}

func TestKeyringShapeIsChecked(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for name, change := range map[string]func(*Keyring){
		"kdf":        func(k *Keyring) { k.KDF = "argon2id" },
		"iterations": func(k *Keyring) { k.Iterations = 1000 },
		"salt":       func(k *Keyring) { k.Salt = []byte{1} },
		"password":   func(k *Keyring) { k.WrappedByPassword = make([]byte, 59) },
		"recovery":   func(k *Keyring) { k.WrappedByRecovery = make([]byte, 61) },
	} {
		k := testKeyring()
		change(&k)
		if _, err := db.CreateKeyring(ctx, user.ID, k, now); err == nil || !IsRefusal(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := db.CreateKeyring(ctx, AnonymousUserID, testKeyring(), now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the anonymous account: %v", err)
	}
}
