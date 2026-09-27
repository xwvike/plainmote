package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// EncryptedContentType marks a quick share encrypted in its creator's browser.
// It is what the resource is stored and served as, and all that is known
// about its contents.
const EncryptedContentType = "application/vnd.plainmote.e2ee"

// The envelope the browser produces (static/e2ee.js writes it):
//
//	"PMe1" | flags (1 byte) | salt (16 bytes, passphrase only) | IV (12 bytes) | AES-GCM ciphertext and tag
//
// Flag bit 0 says the key comes from a passphrase rather than the link. No
// other bit is defined, and an envelope using one is refused rather than
// guessed at.
var envelopeMagic = []byte("PMe1")

const (
	envelopePassphrase = 1 << 0
	envelopeSaltBytes  = 16
	envelopeIVBytes    = 12
	envelopeTagBytes   = 16

	// EncryptedMaxBytes is the envelope for the largest quick share: the text
	// limit, the name carried inside with it, and the envelope's own bytes.
	EncryptedMaxBytes = AnonymousMaxBytes + 1024
)

// ValidEnvelope checks the shape of an encrypted share - the header, and room
// for at least one byte of ciphertext and its tag. It cannot and does not look
// inside.
func ValidEnvelope(envelope []byte) bool {
	if len(envelope) < len(envelopeMagic)+1 || !bytes.Equal(envelope[:len(envelopeMagic)], envelopeMagic) {
		return false
	}
	flags := envelope[len(envelopeMagic)]
	if flags&^envelopePassphrase != 0 {
		return false
	}
	header := len(envelopeMagic) + 1 + envelopeIVBytes
	if flags&envelopePassphrase != 0 {
		header += envelopeSaltBytes
	}
	return len(envelope) >= header+envelopeTagBytes+1
}

// E2EEEnabled reports whether an account has turned on encrypted quick shares.
func (d *Store) E2EEEnabled(ctx context.Context, userID string) (bool, error) {
	if !validUUIDs(userID) {
		return false, ErrNotFound
	}
	var enabled bool
	err := d.db.QueryRow(ctx, `SELECT e2ee FROM users WHERE id = $1`, userID).Scan(&enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("read e2ee setting: %w", err)
	}
	return enabled, nil
}

// SetE2EE turns encrypted quick shares on or off for an account. Shares
// already made stay as they were made.
func (d *Store) SetE2EE(ctx context.Context, userID string, enabled bool) error {
	if !validUUIDs(userID) || userID == AnonymousUserID {
		return ErrNotFound
	}
	tag, err := d.db.Exec(ctx, `UPDATE users SET e2ee = $1 WHERE id = $2`, enabled, userID)
	if err != nil {
		return fmt.Errorf("write e2ee setting: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
