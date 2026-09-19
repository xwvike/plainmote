package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// tokenLength is 32 bytes read straight from the operating system's CSPRNG:
// 256 bits, carried as 43 URL-safe base64 characters. A share token is both
// the address and the credential, so it is sized to be unguessable rather than
// short. A UUID would be the wrong tool here twice over - v4 carries 122 bits,
// and a UUID is an identifier meant to be shown and logged, not a secret.
const tokenLength = 32

// A delivery grant lets one counted use finish the follow-up requests that a
// browser needs for media playback and byte-range downloads. It remains bound
// to both the share token and link row, and every resumed request still checks
// that the link has not expired or been revoked.
const deliveryGrantTTL = time.Hour

type deliveryGrant struct {
	Version   int    `json:"v"`
	LinkID    string `json:"l"`
	TokenHash string `json:"t"`
	ExpiresAt int64  `json:"e"`
}

type tokenCipher struct {
	aead cipher.AEAD
}

func newTokenCipher(key []byte) (*tokenCipher, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("create token cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create token AEAD: %w", err)
	}
	return &tokenCipher{aead: aead}, nil
}

func (c *tokenCipher) seal(value string) ([]byte, error) {
	nonce := make([]byte, c.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate token nonce: %w", err)
	}
	return c.aead.Seal(nonce, nonce, []byte(value), nil), nil
}

func (c *tokenCipher) open(value []byte) (string, error) {
	if len(value) < c.aead.NonceSize() {
		return "", fmt.Errorf("encrypted token is too short")
	}
	nonce, ciphertext := value[:c.aead.NonceSize()], value[c.aead.NonceSize():]
	plaintext, err := c.aead.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt token: %w", err)
	}
	return string(plaintext), nil
}

func (d *Store) IssueDeliveryGrant(token, linkID string, now time.Time) (string, time.Time, error) {
	expires := now.Add(deliveryGrantTTL)
	payload, err := json.Marshal(deliveryGrant{
		Version: 1, LinkID: linkID, TokenHash: hashToken(token), ExpiresAt: expires.Unix(),
	})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("encode delivery grant: %w", err)
	}
	sealed, err := d.cipher.seal(string(payload))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("seal delivery grant: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(sealed), expires, nil
}

func (d *Store) openDeliveryGrant(token, value string, now time.Time) (string, bool) {
	// A valid value is currently about 210 bytes. The cap keeps arbitrary
	// Cookie headers from turning into unbounded decode allocations.
	if value == "" || len(value) > 512 {
		return "", false
	}
	sealed, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil {
		return "", false
	}
	payload, err := d.cipher.open(sealed)
	if err != nil {
		return "", false
	}
	var grant deliveryGrant
	if err := json.Unmarshal([]byte(payload), &grant); err != nil ||
		grant.Version != 1 || !now.Before(time.Unix(grant.ExpiresAt, 0)) ||
		!validUUIDs(grant.LinkID) ||
		subtle.ConstantTimeCompare([]byte(grant.TokenHash), []byte(hashToken(token))) != 1 {
		return "", false
	}
	return grant.LinkID, true
}

func generateToken() (string, error) {
	data := make([]byte, tokenLength)
	if _, err := io.ReadFull(rand.Reader, data); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

// ValidShareToken reports whether value has the exact canonical form emitted
// by generateToken: 32 bytes encoded as unpadded URL-safe base64.
func ValidShareToken(value string) bool {
	if len(value) != base64.RawURLEncoding.EncodedLen(tokenLength) {
		return false
	}
	var decoded [tokenLength]byte
	n, err := base64.RawURLEncoding.Strict().Decode(decoded[:], []byte(value))
	return err == nil && n == tokenLength
}

func randomSecret(length int) (string, error) {
	data := make([]byte, length)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

// hashToken is what the database is indexed on, so a dumped table hands over
// no working links. A plain SHA-256 is correct precisely because the input is
// already 256 random bits: there is no low-entropy secret for an attacker to
// grind, so a salt or a password KDF would cost time and buy nothing.
func hashToken(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}
