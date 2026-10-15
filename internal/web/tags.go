package web

import (
	"errors"
	"net/http"
	"strings"

	"plainmote/internal/store"
)

// tagFormMaxBytes holds one tag added and one removed, with the csrf token,
// in either form encoding.
const tagFormMaxBytes = 16 << 10

// tagChip is one of the account's tags as the pages show it: on the resource
// list, a filter with its count; on a resource, a suggestion to add.
type tagChip struct {
	Name  string
	Count int
	URL   string
	On    bool
}

// handleResourceTags adds one tag to a resource or takes one off, then shows
// the resource again. Through parts.js only its tags and notices change, so
// an edit in progress stays as it is.
func (a *App) handleResourceTags(w http.ResponseWriter, r *http.Request, user User, sessionID, resourceID string) {
	if r.Method != http.MethodPost {
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, tagFormMaxBytes)
	if err := r.ParseMultipartForm(tagFormMaxBytes); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		writePlainError(w, http.StatusBadRequest, "invalid request")
		return
	}
	if !a.checkCSRF(r, sessionID) {
		writePlainError(w, http.StatusForbidden, "invalid csrf token")
		return
	}
	resource, err := a.db.ResourceForOwner(r.Context(), user.ID, resourceID)
	if errors.Is(err, store.ErrNotFound) {
		writePlainError(w, http.StatusNotFound, "resource not found")
		return
	}
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	tags := resource.Tags
	if remove := r.FormValue("remove"); remove != "" {
		tags = withoutTag(tags, remove)
	}
	if add := strings.TrimSpace(r.FormValue("add")); add != "" {
		tags = append(tags, add)
	}
	if _, err := a.db.SetResourceTags(r.Context(), user.ID, resourceID, tags); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writePlainError(w, http.StatusNotFound, "resource not found")
			return
		}
		text, status := a.writeErrorText("tag resource", err)
		a.renderResourcePage(w, r, user, resource, text, status, nil, nil)
		return
	}
	http.Redirect(w, r, "/resources/"+resourceID, http.StatusSeeOther)
}

func withoutTag(tags []string, tag string) []string {
	kept := make([]string, 0, len(tags))
	for _, have := range tags {
		if !strings.EqualFold(have, tag) {
			kept = append(kept, have)
		}
	}
	return kept
}

// tagSuggestions are the account's tags a resource does not carry yet.
func tagSuggestions(tags []store.Tag, carried []string) []tagChip {
	chips := make([]tagChip, 0, len(tags))
	for _, tag := range tags {
		if !containsFold(carried, tag.Name) {
			chips = append(chips, tagChip{Name: tag.Name, Count: tag.Count})
		}
	}
	return chips
}

func containsFold(tags []string, tag string) bool {
	for _, have := range tags {
		if strings.EqualFold(have, tag) {
			return true
		}
	}
	return false
}
