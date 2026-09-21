package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
)

// tokenLength is 32 bytes read straight from the operating system's CSPRNG:
// 256 bits, carried as 43 URL-safe base64 characters. A share token is both
// the address and the credential, so it is sized to be unguessable rather than
// short. A UUID would be the wrong tool here twice over - v4 carries 122 bits,
// and a UUID is an identifier meant to be shown and logged, not a secret.
const tokenLength = 32

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
