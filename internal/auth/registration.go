package auth

import (
	"fmt"
	"strings"
)

// RegistrationMode controls which previously unseen GitHub identities may
// create a PlainMote account. It does not govern subsequent logins: once a
// user exists, disabling that account is a separate product decision.
type RegistrationMode string

const (
	RegistrationOpen      RegistrationMode = "open"
	RegistrationAllowlist RegistrationMode = "allowlist"
	RegistrationClosed    RegistrationMode = "closed"
)

func ParseRegistrationMode(value string) (RegistrationMode, error) {
	mode := RegistrationMode(strings.ToLower(strings.TrimSpace(value)))
	switch mode {
	case RegistrationOpen, RegistrationAllowlist, RegistrationClosed:
		return mode, nil
	default:
		return "", fmt.Errorf("PLAINMOTE_REGISTRATION_MODE must be open, allowlist, or closed")
	}
}

// AllowsNewUser deliberately fails closed for an invalid zero or manually
// constructed mode. Config parsing rejects such a mode before production
// starts, while this default protects tests and other direct App constructors.
func (m RegistrationMode) AllowsNewUser(githubID string, allowedIDs map[string]bool) bool {
	switch m {
	case RegistrationOpen:
		return true
	case RegistrationAllowlist:
		return allowedIDs[githubID]
	default:
		return false
	}
}
