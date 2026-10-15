package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
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
	var add, remove []string
	if tag := r.FormValue("remove"); tag != "" {
		remove = append(remove, tag)
	}
	if tag := strings.TrimSpace(r.FormValue("add")); tag != "" {
		add = append(add, tag)
	}
	if _, err := a.retag(r.Context(), user.ID, resource, add, remove); err != nil {
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

// retag takes tags off a resource, then puts tags on, and returns what it
// carries after.
func (a *App) retag(ctx context.Context, ownerID string, resource Resource, add, remove []string) ([]string, error) {
	tags := slices.Clone(resource.Tags)
	for _, tag := range remove {
		tags = withoutTag(tags, strings.TrimSpace(tag))
	}
	return a.db.SetResourceTags(ctx, ownerID, resource.ID, append(tags, add...))
}

// checkTags refuses tags that could not be added, before anything is made
// that they were meant for.
func checkTags(tags []string) error {
	for _, tag := range tags {
		if _, err := store.NormalizeTag(tag); err != nil {
			return err
		}
	}
	return nil
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

// apiListTags is every tag on the account's resources, with how many carry
// each.
func (a *App) apiListTags(w http.ResponseWriter, r *http.Request, user User) {
	tags, err := a.db.AccountTags(r.Context(), user.ID)
	if err != nil {
		a.apiRefused(w, "list tags", err)
		return
	}
	type apiTag struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	views := make([]apiTag, 0, len(tags))
	for _, tag := range tags {
		views = append(views, apiTag{Name: tag.Name, Count: tag.Count})
	}
	a.writeAPI(w, http.StatusOK, map[string]any{"tags": views})
}

// apiRetag takes tags off a resource and puts others on, in one request.
func (a *App) apiRetag(w http.ResponseWriter, r *http.Request, user User, id string) {
	var request struct {
		Add    []string `json:"add"`
		Remove []string `json:"remove"`
	}
	if !a.readAPIJSON(w, r, &request) {
		return
	}
	resource, err := a.db.ResourceForOwner(r.Context(), user.ID, id)
	if err != nil {
		a.apiRefused(w, "tag resource", err)
		return
	}
	if resource.Tags, err = a.retag(r.Context(), user.ID, resource, request.Add, request.Remove); err != nil {
		a.apiRefused(w, "tag resource", err)
		return
	}
	a.writeAPI(w, http.StatusOK, a.apiResourceView(r, resource))
}

func (a *App) mcpListTags(x mcpExchange, args json.RawMessage) (mcpOutcome, error) {
	var in struct{}
	if problem := mcpArguments(args, &in); problem != "" {
		return mcpProblem("%s", problem)
	}
	tags, err := a.db.AccountTags(x.r.Context(), x.user.ID)
	if err != nil {
		return mcpOutcome{}, err
	}
	type mcpTag struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	items := make([]mcpTag, 0, len(tags))
	for _, tag := range tags {
		items = append(items, mcpTag{Name: tag.Name, Count: tag.Count})
	}
	return mcpOutcome{structured: map[string]any{"tags": items}}, nil
}

func (a *App) mcpTagResource(x mcpExchange, args json.RawMessage) (mcpOutcome, error) {
	var in struct {
		Resource string   `json:"resource"`
		Add      []string `json:"add"`
		Remove   []string `json:"remove"`
	}
	if problem := mcpArguments(args, &in); problem != "" {
		return mcpProblem("%s", problem)
	}
	if len(in.Add) == 0 && len(in.Remove) == 0 {
		return mcpProblem("Invalid arguments: give tags to add, to remove, or both.")
	}
	resource, problem, err := a.mcpResolve(x, in.Resource)
	if err != nil || problem != "" {
		return mcpOutcome{problem: problem}, err
	}
	tags, err := a.retag(x.r.Context(), x.user.ID, resource, in.Add, in.Remove)
	if errors.Is(err, store.ErrNotFound) {
		return mcpProblem("%s no longer exists.", mcpLabel(resource))
	}
	if err != nil {
		return mcpStoreProblem(err)
	}
	text := fmt.Sprintf("%s now has no tags.", mcpLabel(resource))
	if len(tags) > 0 {
		text = fmt.Sprintf("%s is tagged %s.", mcpLabel(resource), strings.Join(tags, ", "))
	}
	return mcpOutcome{structured: map[string]any{"id": resource.ID, "tags": tags}, text: []string{text}}, nil
}
