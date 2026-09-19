package store

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// A share token is the whole address and the whole credential, so its shape is
// worth asserting rather than assuming: 32 bytes straight from crypto/rand,
// carried as URL-safe base64.
func TestShareTokenShape(t *testing.T) {
	token, err := generateToken()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		t.Fatalf("a token must be raw URL-safe base64: %v", err)
	}
	if len(raw) != tokenLength {
		t.Fatalf("token carries %d bytes of entropy, want %d", len(raw), tokenLength)
	}
	if len(raw)*8 < 128 {
		t.Fatalf("a bearer token needs at least 128 bits, got %d", len(raw)*8)
	}
	// The alphabet has to survive being a path segment untouched.
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	for _, r := range token {
		if !strings.ContainsRune(alphabet, r) {
			t.Fatalf("token contains %q, which is not URL-safe", r)
		}
	}
}

// Distinctness plus a crude bit balance: not a randomness test, but it would
// catch a source that got stuck, seeded, or wired to a counter.
func TestShareTokensAreDistinctAndBalanced(t *testing.T) {
	const rounds = 2000
	seen := make(map[string]bool, rounds)
	ones := 0
	for i := 0; i < rounds; i++ {
		token, err := generateToken()
		if err != nil {
			t.Fatal(err)
		}
		if seen[token] {
			t.Fatalf("generated the same token twice after %d rounds", i)
		}
		seen[token] = true
		raw, _ := base64.RawURLEncoding.DecodeString(token)
		for _, b := range raw {
			for bit := 0; bit < 8; bit++ {
				if b&(1<<bit) != 0 {
					ones++
				}
			}
		}
	}
	total := rounds * tokenLength * 8
	ratio := float64(ones) / float64(total)
	if ratio < 0.48 || ratio > 0.52 {
		t.Fatalf("bit balance is %.4f across %d bits, which does not look random", ratio, total)
	}
}

// Session and CSRF secrets come off the same source and must be just as
// unguessable.
func TestSessionSecretsAreLongEnough(t *testing.T) {
	for _, length := range []int{24, 32} {
		secret, err := randomSecret(length)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := base64.RawURLEncoding.DecodeString(secret)
		if err != nil || len(raw) != length {
			t.Fatalf("secret of %d bytes decoded to %d: %v", length, len(raw), err)
		}
		if len(raw)*8 < 128 {
			t.Fatalf("%d-byte secret is only %d bits", length, len(raw)*8)
		}
	}
}

// The lookup index is a plain SHA-256 of the token. That is the right tool
// here precisely because the token is already 256 random bits: there is no
// low-entropy secret to slow an attacker down with, so no salt or KDF buys
// anything.
func TestTokenHashIsStableAndWide(t *testing.T) {
	token, err := generateToken()
	if err != nil {
		t.Fatal(err)
	}
	digest := hashToken(token)
	if digest != hashToken(token) {
		t.Fatal("the same token must always hash the same, or lookups break")
	}
	raw, err := hex.DecodeString(digest)
	if err != nil || len(raw) != 32 {
		t.Fatalf("hash should be 32 hex-encoded bytes, got %q", digest)
	}
	other, _ := generateToken()
	if hashToken(other) == digest {
		t.Fatal("two different tokens hashed the same")
	}
	if strings.Contains(digest, token) {
		t.Fatal("the hash leaks the token")
	}
}

// Tokens are also kept encrypted so the page can show a link again. AES-GCM
// with a reused nonce would be catastrophic, so check that every sealing is
// fresh and that tampering is caught.
func TestTokenCipherUsesAFreshNonceAndDetectsTampering(t *testing.T) {
	cipher, err := newTokenCipher(bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	const secret = "a-token-value"
	first, err := cipher.seal(secret)
	if err != nil {
		t.Fatal(err)
	}
	second, err := cipher.seal(secret)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) {
		t.Fatal("sealing the same value twice produced identical bytes: the nonce is being reused")
	}
	for _, sealed := range [][]byte{first, second} {
		got, err := cipher.open(sealed)
		if err != nil || got != secret {
			t.Fatalf("round trip failed: %q %v", got, err)
		}
		if bytes.Contains(sealed, []byte(secret)) {
			t.Fatal("the stored bytes contain the plaintext")
		}
	}
	tampered := append([]byte(nil), first...)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := cipher.open(tampered); err == nil {
		t.Fatal("a tampered ciphertext was accepted")
	}
	wrongKey, _ := newTokenCipher(bytes.Repeat([]byte{4}, 32))
	if _, err := wrongKey.open(first); err == nil {
		t.Fatal("the wrong key decrypted a token")
	}
}

func TestDeliveryGrantIsBoundToTokenAndExpires(t *testing.T) {
	cipher, err := newTokenCipher(bytes.Repeat([]byte{5}, 32))
	if err != nil {
		t.Fatal(err)
	}
	db := &Store{cipher: cipher}
	token, err := generateToken()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	linkID := uuid.NewString()
	grant, expires, err := db.IssueDeliveryGrant(token, linkID, now)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := db.openDeliveryGrant(token, grant, now); !ok || got != linkID {
		t.Fatalf("valid delivery grant: id=%q ok=%v", got, ok)
	}
	other, _ := generateToken()
	if _, ok := db.openDeliveryGrant(other, grant, now); ok {
		t.Fatal("delivery grant was accepted with another share token")
	}
	if _, ok := db.openDeliveryGrant(token, grant, expires); ok {
		t.Fatal("expired delivery grant was accepted")
	}
	tampered := []byte(grant)
	tampered[len(tampered)-1] ^= 1
	if _, ok := db.openDeliveryGrant(token, string(tampered), now); ok {
		t.Fatal("tampered delivery grant was accepted")
	}
}

// Probability is not a safety mechanism, so the schema carries a hard one: the
// token hash is UNIQUE. Even in the world where crypto/rand repeats itself,
// the second link fails loudly at insert instead of quietly aliasing the
// first - two resources could never end up sharing one address.
func TestDuplicateTokenIsRejectedByTheSchema(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	share, err := db.CreateShare(ctx, user.ID, resource.ID, "first", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := db.cipher.seal(share.Token)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.db.Exec(ctx, `
INSERT INTO links(id, resource_id, name, token_ciphertext, token_hash, max_uses, used_count, created_at)
VALUES($1, $2, $3, $4, $5, 0, 0, $6)`,
		uuid.NewString(), resource.ID, "collision", sealed, hashToken(share.Token), time.Now().UTC())
	if err == nil {
		t.Fatal("a repeated token was accepted; the address would be ambiguous")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("expected the uniqueness constraint to catch it, got %v", err)
	}
}
