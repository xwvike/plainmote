package web

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"plainmote/internal/store"
)

// Validators for share delivery, so a program that polls a link can ask
// "has this changed?" and get an empty 304 when it has not. They are for
// clients that keep their own copy - curl --etag-compare, wget -N, a config
// agent - and change nothing about caching: delivery stays no-store.

// storedETag names one saved version of a resource as delivered. Every save
// moves updated_at, and the delivered type is part of the name because the
// same bytes under another charset are another representation. Derived from
// the row, so answering a 304 never reads the content.
func storedETag(resource store.Resource, contentType string) string {
	sum := sha256.Sum256([]byte(resource.ID + "\x00" + resource.UpdatedAt.UTC().Format(time.RFC3339Nano) + "\x00" + contentType))
	return `"` + hex.EncodeToString(sum[:12]) + `"`
}

// bodyETag names a remote resource by what the origin returned this time:
// there is no saved version to go by, only the bytes.
func bodyETag(contentType string, body []byte) string {
	digest := sha256.New()
	digest.Write([]byte(contentType + "\x00"))
	digest.Write(body)
	return `"` + hex.EncodeToString(digest.Sum(nil)[:12]) + `"`
}

// notModified evaluates If-None-Match, or failing that If-Modified-Since, the
// way RFC 9110 has a GET or HEAD do it. modified is zero when the
// representation has no date of its own.
func notModified(r *http.Request, etag string, modified time.Time) bool {
	if list := r.Header.Get("If-None-Match"); list != "" {
		return etagListMatches(list, etag)
	}
	if modified.IsZero() {
		return false
	}
	since, err := http.ParseTime(r.Header.Get("If-Modified-Since"))
	if err != nil {
		return false
	}
	return !modified.Truncate(time.Second).After(since)
}

// etagListMatches is the weak comparison If-None-Match calls for. The tags
// made here are hex, so a comma can only separate them.
func etagListMatches(list, etag string) bool {
	if strings.TrimSpace(list) == "*" {
		return true
	}
	for _, candidate := range strings.Split(list, ",") {
		if strings.TrimPrefix(strings.TrimSpace(candidate), "W/") == etag {
			return true
		}
	}
	return false
}
