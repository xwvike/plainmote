package store

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func testEnvelope(passphrase bool, cipherBytes int) []byte {
	envelope := append([]byte("PMe1"), 0)
	if passphrase {
		envelope[4] = envelopePassphrase
		envelope = append(envelope, bytes.Repeat([]byte{1}, envelopeSaltBytes)...)
	}
	envelope = append(envelope, bytes.Repeat([]byte{2}, envelopeIVBytes)...)
	return append(envelope, bytes.Repeat([]byte{3}, cipherBytes)...)
}

// The server cannot read an envelope, but it can refuse what is not one.
func TestValidEnvelope(t *testing.T) {
	for name, c := range map[string]struct {
		envelope []byte
		valid    bool
	}{
		"link key":            {testEnvelope(false, envelopeTagBytes+1), true},
		"passphrase":          {testEnvelope(true, envelopeTagBytes+1), true},
		"no ciphertext":       {testEnvelope(false, envelopeTagBytes), false},
		"passphrase, no salt": {testEnvelope(false, envelopeTagBytes+1)[:5], false},
		"other magic":         {append([]byte("PMe2"), testEnvelope(false, 20)[4:]...), false},
		"unknown flag":        {append([]byte("PMe1\x02"), testEnvelope(false, 40)[5:]...), false},
		"plain text":          {[]byte("port: 7890\n"), false},
	} {
		if got := ValidEnvelope(c.envelope); got != c.valid {
			t.Errorf("%s: valid=%v, want %v", name, got, c.valid)
		}
	}
}

// An encrypted quick share is stored as ciphertext with nothing about it but
// its type, opens through the decryption page, and can never become a resource.
func TestEncryptedPaste(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC()

	if on, err := db.E2EEEnabled(ctx, user.ID); err != nil || on {
		t.Fatalf("encryption must start off: %v %v", on, err)
	}
	if err := db.SetE2EE(ctx, user.ID, true); err != nil {
		t.Fatal(err)
	}
	if on, _ := db.E2EEEnabled(ctx, user.ID); !on {
		t.Fatal("the setting did not stick")
	}

	envelope := testEnvelope(false, 64)
	if _, _, err := db.CreateEncryptedPaste(ctx, "", envelope, 0, now); err == nil {
		t.Fatal("an encrypted paste needs a signed-in creator")
	}
	if _, _, err := db.CreateEncryptedPaste(ctx, user.ID, []byte("port: 7890\n"), 0, now); err == nil {
		t.Fatal("text that is not an envelope must be refused")
	}
	if _, _, err := db.CreateEncryptedPaste(ctx, user.ID, testEnvelope(false, EncryptedMaxBytes), 0, now); err == nil {
		t.Fatal("an envelope over the limit must be refused")
	}

	resource, link, err := db.CreateEncryptedPaste(ctx, user.ID, envelope, 5*time.Minute, now)
	if err != nil {
		t.Fatal(err)
	}
	if resource.ContentType != EncryptedContentType || resource.Name != "" || resource.Filename != "" || resource.OwnerID != AnonymousUserID {
		t.Fatalf("stored as %+v", resource)
	}
	if got := link.ExpiresAt.Sub(now); got != 5*time.Minute {
		t.Fatalf("lifetime %v", got)
	}
	body, _, err := db.OpenContent(ctx, resource)
	if err != nil {
		t.Fatal(err)
	}
	stored := new(bytes.Buffer)
	_, _ = stored.ReadFrom(body)
	body.Close()
	if !bytes.Equal(stored.Bytes(), envelope) {
		t.Fatal("the envelope must be stored byte for byte")
	}

	if name := FallbackFilename(resource.ContentType); name != "file" {
		t.Fatalf("the address names it %q; it must not say it is encrypted", name)
	}
	if shell, err := db.ShareShell(ctx, link.Token); err != nil || shell != ShellEncrypted {
		t.Fatalf("shell %q %v", shell, err)
	}
	if _, _, err := db.AnonymousPasteResult(ctx, resource.ID, now); err != nil {
		t.Fatalf("the result page must still open: %v", err)
	}
	if _, _, err := db.ClaimableAnonymousPaste(ctx, user.ID, resource.ID, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an encrypted paste must not be offered as a resource: %v", err)
	}
	if _, err := db.ClaimAnonymousPaste(ctx, user.ID, resource.ID, now); err == nil {
		t.Fatal("an encrypted paste must not become a resource")
	}
}
