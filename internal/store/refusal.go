package store

import (
	"errors"
	"fmt"
)

// Refusal is an error about what the caller asked for, written for them to
// read: a filename that is not allowed, content too large, a lifetime out of
// range. It is the only kind of error whose text leaves the service. Every
// other error is the service failing, and its text - which can name tables,
// hosts and object keys - belongs in the log.
type Refusal struct{ message string }

func (e *Refusal) Error() string { return e.message }

func refusal(message string) error { return &Refusal{message: message} }

func refusalf(format string, args ...any) error {
	return &Refusal{message: fmt.Sprintf(format, args...)}
}

// IsRefusal reports whether err may be shown to whoever made the request: a
// Refusal, a quota reached, a save that conflicts, or something that does not
// exist. Anything else is the service failing.
func IsRefusal(err error) bool {
	var refused *Refusal
	var conflict *VersionConflict
	return errors.As(err, &refused) || errors.Is(err, ErrQuotaExceeded) || errors.As(err, &conflict) || errors.Is(err, ErrNotFound)
}
