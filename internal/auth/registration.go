package auth

import (
	"fmt"
	"strings"
)

// RegistrationMode controls whether a previously unseen GitHub or Google
// identity may create a PlainMote account. It does not govern subsequent
// logins: an existing account signs in whatever the mode is now.
type RegistrationMode string

const (
	RegistrationOpen   RegistrationMode = "open"
	RegistrationClosed RegistrationMode = "closed"
)

func ParseRegistrationMode(value string) (RegistrationMode, error) {
	mode := RegistrationMode(strings.ToLower(strings.TrimSpace(value)))
	switch mode {
	case RegistrationOpen, RegistrationClosed:
		return mode, nil
	default:
		return "", fmt.Errorf("PLAINMOTE_REGISTRATION_MODE must be open or closed")
	}
}

// AllowsNewUser fails closed for any mode but open, so a zero or manually
// constructed mode admits nobody.
func (m RegistrationMode) AllowsNewUser() bool {
	return m == RegistrationOpen
}
