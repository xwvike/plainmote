package web

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"plainmote/internal/store"
)

// The HTTP API. Everything under apiPrefix needs a personal access token in
// the Authorization header - never a cookie, never a query parameter - and
// nothing here answers a browser on another site: no CORS headers are ever
// sent, so a page cannot read an answer, and a request carrying a token is
// never a "simple" one a page could send blind.
const (
	apiPrefix       = "/api/v1/"
	apiMaxJSON      = 4 << 10
	apiNameMaxRunes = 200
	apiShareMaxUses = 1_000_000
	apiShareNameMax = 100
	apiShareMaxTTL  = 365 * 24 * time.Hour
)

// apiLimits brakes guessing and floods. Counts are per address, and per token
// for an authenticated caller.
type apiLimits struct {
	authFailures *windowLimiter // bad tokens, per address
	requests     *windowLimiter // calls, per token
	apiShares    *windowLimiter // share links made via HTTP API, per token
	apiDeletes   *windowLimiter // trash moves via HTTP API, per token
	// MCP clients registering themselves, and metadata documents fetched,
	// per address.
	registrations   *windowLimiter
	clientDocuments *windowLimiter
	// MCP calls, per authorization and per address, and the share links and
	// trash moves an authorization makes.
	mcpCalls     *windowLimiter
	mcpAddresses *windowLimiter
	mcpShares    *windowLimiter
	mcpDeletes   *windowLimiter
	// Quick shares made without an account, and the bytes they hold, per
	// address. Every one of them comes out of a single pool the whole
	// deployment shares, which one address must not be able to drain.
	pastes     *windowLimiter
	pasteBytes *windowLimiter
}

func newAPILimits() *apiLimits {
	return &apiLimits{
		authFailures:    newWindowLimiter(30, time.Minute),
		requests:        newWindowLimiter(300, time.Minute),
		apiShares:       newWindowLimiter(60, time.Hour),
		apiDeletes:      newWindowLimiter(60, time.Hour),
		registrations:   newWindowLimiter(20, time.Hour),
		clientDocuments: newWindowLimiter(30, 10*time.Minute),
		mcpCalls:        newWindowLimiter(300, time.Minute),
		mcpAddresses:    newWindowLimiter(600, time.Minute),
		mcpShares:       newWindowLimiter(30, time.Hour),
		mcpDeletes:      newWindowLimiter(30, time.Hour),
		pastes:          newWindowLimiter(120, time.Hour),
		pasteBytes:      newWindowLimiter(256<<20, 24*time.Hour),
	}
}

type apiError struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
	// Set on a conflict: the version the save would have replaced.
	CurrentVersion int `json:"current_version,omitempty"`
}

func (a *App) writeAPI(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (a *App) apiFail(w http.ResponseWriter, status int, code, message string) {
	a.writeAPI(w, status, apiError{Error: code, Message: message})
}

// apiRefused answers what the store turned down, the way the resource page
// does: a refusal about the request says so in full; anything else is the
// service failing, named by one sentence and logged.
func (a *App) apiRefused(w http.ResponseWriter, what string, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		a.apiFail(w, http.StatusNotFound, "not_found", "resource not found")
	case errors.Is(err, store.ErrQuotaExceeded):
		a.apiFail(w, http.StatusRequestEntityTooLarge, "quota", localizePageError("en", err.Error()))
	case store.IsRefusal(err):
		a.apiFail(w, http.StatusBadRequest, "refused", localizePageError("en", err.Error()))
	default:
		fmt.Fprintf(os.Stderr, "api %s: %v\n", what, err)
		a.apiFail(w, http.StatusInternalServerError, "internal", "the service could not complete this request")
	}
}

func (a *App) handleAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-PlainMote-Version", a.cfg.Version)
	user, token, ok := a.apiUser(w, r)
	if !ok {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, apiPrefix)
	switch {
	case path == "me" && r.Method == http.MethodGet:
		me := map[string]any{
			"login": user.Login, "user_id": user.ID, "scope": token.Scope,
			"name": token.DeviceName, "server_version": a.cfg.Version,
		}
		if token.ExpiresAt != nil {
			me["expires_at"] = token.ExpiresAt
		}
		a.writeAPI(w, http.StatusOK, me)
	case path == "keyring" && r.Method == http.MethodGet:
		a.apiKeyring(w, r, user)
	case path == "token" && r.Method == http.MethodDelete:
		if err := a.db.RevokeAPITokenByID(r.Context(), token.ID); err != nil {
			a.apiRefused(w, "revoke token", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case path == "resources" && r.Method == http.MethodGet:
		a.apiListResources(w, r, user)
	case path == "resources" && r.Method == http.MethodPost:
		if a.apiWritable(w, token) {
			a.apiCreateResource(w, r, user)
		}
	case path == "tags" && r.Method == http.MethodGet:
		a.apiListTags(w, r, user)
	case path == "quick-shares" && r.Method == http.MethodPost:
		if a.apiWritable(w, token) {
			a.apiCreateQuickShare(w, r, user)
		}
	case strings.HasPrefix(path, "resources/"):
		rest := strings.Split(strings.TrimPrefix(path, "resources/"), "/")
		switch {
		case len(rest) == 1 && r.Method == http.MethodGet:
			a.apiGetResource(w, r, user, rest[0])
		case len(rest) == 1 && r.Method == http.MethodPatch:
			if a.apiWritable(w, token) {
				a.apiUpdateResourceMeta(w, r, user, rest[0])
			}
		case len(rest) == 1 && r.Method == http.MethodDelete:
			if a.apiWritable(w, token) {
				a.apiDeleteResource(w, r, user, token, rest[0])
			}
		case len(rest) == 2 && rest[1] == "content" && r.Method == http.MethodGet:
			a.apiReadContent(w, r, user, rest[0])
		case len(rest) == 2 && rest[1] == "content" && r.Method == http.MethodPut:
			if a.apiWritable(w, token) {
				a.apiWriteContent(w, r, user, rest[0])
			}
		case len(rest) == 2 && rest[1] == "versions" && r.Method == http.MethodGet:
			a.apiListVersions(w, r, user, rest[0])
		case len(rest) == 2 && rest[1] == "links" && r.Method == http.MethodGet:
			a.apiListLinks(w, r, user, rest[0])
		case len(rest) == 2 && rest[1] == "links" && r.Method == http.MethodPost:
			if a.apiWritable(w, token) {
				a.apiCreateLink(w, r, user, token, rest[0])
			}
		case len(rest) == 3 && rest[1] == "links" && r.Method == http.MethodDelete:
			if a.apiWritable(w, token) {
				a.apiRevokeLink(w, r, user, rest[0], rest[2])
			}
		case len(rest) == 2 && rest[1] == "tags" && r.Method == http.MethodPatch:
			if a.apiWritable(w, token) {
				a.apiRetag(w, r, user, rest[0])
			}
		default:
			a.apiFail(w, http.StatusNotFound, "not_found", "no such endpoint")
		}
	default:
		a.apiFail(w, http.StatusNotFound, "not_found", "no such endpoint")
	}
}

// apiUser authenticates a call by its bearer token.
func (a *App) apiUser(w http.ResponseWriter, r *http.Request) (User, store.APIToken, bool) {
	now := time.Now().UTC()
	ip := a.clientIP(r)
	if a.limits.authFailures.blocked(addressKey(ip), now) {
		a.apiFail(w, http.StatusTooManyRequests, "rate_limited", "too many failed attempts; try again in a minute")
		return User{}, store.APIToken{}, false
	}
	value, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	value = strings.TrimSpace(value)
	if !found || value == "" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="plainmote"`)
		a.apiFail(w, http.StatusUnauthorized, "unauthorized", "missing bearer token; create an API token under Settings > Authorized apps")
		return User{}, store.APIToken{}, false
	}
	user, token, err := a.db.TokenUser(r.Context(), value, ip, now)
	if errors.Is(err, store.ErrNotFound) {
		a.limits.authFailures.allow(addressKey(ip), now)
		w.Header().Set("WWW-Authenticate", `Bearer realm="plainmote", error="invalid_token"`)
		a.apiFail(w, http.StatusUnauthorized, "unauthorized", "this API token is invalid, expired, or revoked")
		return User{}, store.APIToken{}, false
	}
	if err != nil {
		a.apiRefused(w, "authenticate", err)
		return User{}, store.APIToken{}, false
	}
	if !a.limits.requests.allow(token.ID, now) {
		a.apiFail(w, http.StatusTooManyRequests, "rate_limited", "too many requests; slow down")
		return User{}, store.APIToken{}, false
	}
	return user, token, true
}

func (a *App) apiWritable(w http.ResponseWriter, token store.APIToken) bool {
	if token.CanWrite() {
		return true
	}
	a.apiFail(w, http.StatusForbidden, "read_only", "this token is read-only; create a write token to modify resources")
	return false
}

type apiResource struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Filename   string    `json:"filename"`
	Size       int64     `json:"size"`
	Type       string    `json:"type"`
	Encoding   string    `json:"encoding,omitempty"`
	Version    int       `json:"version"`
	UpdatedAt  time.Time `json:"updated_at"`
	Editable   bool      `json:"editable"`
	Encrypted  bool      `json:"encrypted,omitempty"`
	TakenDown  bool      `json:"taken_down,omitempty"`
	ShareLinks int       `json:"share_links,omitempty"`
	// ExpiresAt is set on a quick share not yet kept: read only, and
	// deleted at that time.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// SealedKey and SealedMeta are set on an end-to-end encrypted resource:
	// its content key wrapped by the account key, and its encrypted
	// metadata (docs/encryption.md). Its content is ciphertext.
	SealedKey  string   `json:"sealed_key,omitempty"`
	SealedMeta string   `json:"sealed_meta,omitempty"`
	URL        string   `json:"url"`
	Tags       []string `json:"tags"`
}

func (a *App) apiResourceView(r *http.Request, resource Resource) apiResource {
	encrypted := resource.ContentType == store.EncryptedContentType || resource.Sealed()
	return apiResource{
		ID: resource.ID, Name: resource.Name, Filename: resource.Filename, Size: resource.ContentSize,
		Type: resource.ContentType, Encoding: resource.ContentEncoding, Version: resource.Version,
		UpdatedAt: resource.UpdatedAt, Encrypted: encrypted,
		Editable:   resource.Editable() && !encrypted && !resource.QuickShare(),
		TakenDown:  resource.TakenDown,
		ShareLinks: resource.LiveShares,
		ExpiresAt:  resource.ExpiresAt,
		URL:        a.baseURL(r) + "/resources/" + resource.ID,
		SealedKey:  b64url(resource.SealedKey), SealedMeta: b64url(resource.SealedMeta),
		Tags: append([]string{}, resource.Tags...),
	}
}

// apiListResources lists resources filtered by q, tag and kind, or with ref
// resolves an ID, prefix, exact name or filename.
func (a *App) apiListResources(w http.ResponseWriter, r *http.Request, user User) {
	var resources []Resource
	var err error
	if ref := r.URL.Query().Get("ref"); ref != "" {
		resources, err = a.db.ResolveResources(r.Context(), user.ID, ref)
	} else {
		filter := store.ResourceFilter{Query: r.URL.Query().Get("q")}
		switch kind := strings.TrimSpace(r.URL.Query().Get("kind")); kind {
		case "":
		case "resource":
			filter.Kind = store.KindResource
		case "quick", "quick_share":
			filter.Kind = store.KindQuickShare
		default:
			a.apiFail(w, http.StatusBadRequest, "bad_request", "kind must be resource or quick_share")
			return
		}
		if tag := strings.TrimSpace(r.URL.Query().Get("tag")); tag != "" {
			filter.Tags = []string{tag}
		}
		resources, _, err = a.db.ListResourcesBy(r.Context(), user.ID, filter, store.APIListLimit, 0)
	}
	if err != nil {
		a.apiRefused(w, "list resources", err)
		return
	}
	views := make([]apiResource, 0, len(resources))
	for _, resource := range resources {
		views = append(views, a.apiResourceView(r, resource))
	}
	a.writeAPI(w, http.StatusOK, map[string]any{"resources": views})
}

func (a *App) apiGetResource(w http.ResponseWriter, r *http.Request, user User, id string) {
	resource, err := a.db.ResourceForOwner(r.Context(), user.ID, id)
	if err != nil {
		a.apiRefused(w, "read resource", err)
		return
	}
	if live, err := a.db.ListShares(r.Context(), user.ID, resource.ID, time.Now().UTC()); err == nil {
		resource.LiveShares = len(live)
	}
	a.writeAPI(w, http.StatusOK, a.apiResourceView(r, resource))
}

// apiUpdateResourceMeta updates a resource's name, filename, and/or tags.
func (a *App) apiUpdateResourceMeta(w http.ResponseWriter, r *http.Request, user User, id string) {
	var in struct {
		Name     *string   `json:"name"`
		Filename *string   `json:"filename"`
		Tags     *[]string `json:"tags"`
	}
	if !a.readAPIJSON(w, r, &in) {
		return
	}
	if in.Name == nil && in.Filename == nil && in.Tags == nil {
		a.apiFail(w, http.StatusBadRequest, "bad_request", "provide at least one of name, filename, or tags")
		return
	}
	if in.Name != nil && utf8.RuneCountInString(*in.Name) > apiNameMaxRunes {
		a.apiFail(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("name can be at most %d characters", apiNameMaxRunes))
		return
	}
	if in.Tags != nil {
		if err := checkTags(*in.Tags); err != nil {
			a.apiRefused(w, "update resource", err)
			return
		}
	}
	ctx := r.Context()
	resource, err := a.db.ResourceForOwner(ctx, user.ID, id)
	if err != nil {
		a.apiRefused(w, "update resource", err)
		return
	}
	if in.Name != nil || in.Filename != nil {
		name, filename := resource.Name, resource.Filename
		if in.Name != nil {
			name = *in.Name
		}
		if in.Filename != nil {
			filename = strings.TrimSpace(*in.Filename)
		}
		if _, err := a.db.SaveResource(ctx, user.ID, resource.ID, store.ResourceEdit{Name: name, Filename: filename}); err != nil {
			a.apiRefused(w, "update resource", err)
			return
		}
	}
	if in.Tags != nil {
		if _, err := a.db.SetResourceTags(ctx, user.ID, resource.ID, *in.Tags); err != nil {
			a.apiRefused(w, "tag resource", err)
			return
		}
	}
	updated, err := a.db.ResourceForOwner(ctx, user.ID, resource.ID)
	if err != nil {
		a.apiRefused(w, "read resource", err)
		return
	}
	if live, err := a.db.ListShares(ctx, user.ID, updated.ID, time.Now().UTC()); err == nil {
		updated.LiveShares = len(live)
	}
	a.writeAPI(w, http.StatusOK, a.apiResourceView(r, updated))
}

// apiDeleteResource moves a resource to the trash and revokes its share links.
func (a *App) apiDeleteResource(w http.ResponseWriter, r *http.Request, user User, token store.APIToken, id string) {
	ctx := r.Context()
	resource, err := a.db.ResourceForOwner(ctx, user.ID, id)
	if err != nil {
		a.apiRefused(w, "delete resource", err)
		return
	}
	if resource.Sealed() || resource.Encrypted() {
		a.apiFail(w, http.StatusBadRequest, "refused", "end-to-end encrypted resources can only be deleted in the browser")
		return
	}
	now := time.Now().UTC()
	if !a.limits.apiDeletes.allow(token.ID, now) {
		a.apiFail(w, http.StatusTooManyRequests, "rate_limited", "too many deletions; try again later")
		return
	}
	revoked, err := a.db.TrashResource(ctx, user.ID, resource.ID, now)
	if err != nil {
		a.apiRefused(w, "delete resource", err)
		return
	}
	purgeAfter := now.Add(store.TrashRetention)
	if resource.ExpiresAt != nil && resource.ExpiresAt.Before(purgeAfter) {
		purgeAfter = *resource.ExpiresAt
	}
	a.writeAPI(w, http.StatusOK, map[string]any{
		"id":            resource.ID,
		"trashed":       true,
		"revoked_links": revoked,
		"purge_after":   purgeAfter,
	})
}

// apiVersionTag is the ETag of a resource's content: its version number.
func apiVersionTag(version int) string { return `"v` + strconv.Itoa(version) + `"` }

// apiReadContent sends the stored bytes as they are, in the encoding they
// were saved in, which the headers name. An optional ?version=N query reads
// an earlier version from history.
func (a *App) apiReadContent(w http.ResponseWriter, r *http.Request, user User, id string) {
	ctx := r.Context()
	resource, err := a.db.ResourceForOwner(ctx, user.ID, id)
	if err != nil {
		a.apiRefused(w, "read content", err)
		return
	}
	version := resource.Version
	contentType, encoding := resource.ContentType, resource.ContentEncoding
	var body io.ReadCloser
	var size int64
	if raw := strings.TrimSpace(r.URL.Query().Get("version")); raw != "" {
		num, err := strconv.Atoi(raw)
		if err != nil || num < 1 {
			a.apiFail(w, http.StatusBadRequest, "bad_request", "version must be a positive integer")
			return
		}
		if num != resource.Version {
			v, err := a.db.VersionForOwner(ctx, user.ID, resource.ID, num)
			if err != nil {
				a.apiRefused(w, "read version", err)
				return
			}
			version, contentType, encoding = v.Number, v.ContentType, v.ContentEncoding
			body, size, err = a.db.OpenVersion(ctx, v)
			if err != nil {
				a.apiRefused(w, "read version", fmt.Errorf("%w: %w", store.ErrInternal, err))
				return
			}
		}
	}
	if body == nil {
		body, size, err = a.db.OpenContent(ctx, resource)
		if err != nil {
			a.apiRefused(w, "read content", fmt.Errorf("%w: %w", store.ErrInternal, err))
			return
		}
	}
	defer body.Close()
	w.Header().Set("Content-Type", store.ContentTypeWithEncoding(store.SafeContentType(contentType), encoding))
	w.Header().Set("Content-Disposition", "attachment")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("ETag", apiVersionTag(version))
	w.Header().Set("X-PlainMote-Resource-Version", strconv.Itoa(version))
	if encoding != "" {
		w.Header().Set("X-PlainMote-Encoding", encoding)
	}
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
}

// apiWriteContent saves new content as the resource's next version. If-Match
// is optional: omit it (or send "*") to save over the current version, or send
// a version tag such as "v5" to guard against concurrent edits (412 conflict).
func (a *App) apiWriteContent(w http.ResponseWriter, r *http.Request, user User, id string) {
	resource, err := a.db.ResourceForOwner(r.Context(), user.ID, id)
	if err != nil {
		a.apiRefused(w, "save content", err)
		return
	}
	base := 0
	switch match := strings.TrimSpace(r.Header.Get("If-Match")); {
	case match == "", match == "*":
	case strings.HasPrefix(match, `"v`) && strings.HasSuffix(match, `"`):
		base, err = strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(match, `"v`), `"`))
		if err != nil || base < 1 {
			a.apiFail(w, http.StatusBadRequest, "bad_request", `If-Match must be a version tag such as "v5", or *`)
			return
		}
	default:
		a.apiFail(w, http.StatusBadRequest, "bad_request", `If-Match must be a version tag such as "v5", or *`)
		return
	}
	if resource.Sealed() {
		a.apiWriteSealed(w, r, user, resource, base)
		return
	}
	body, ok := a.apiBody(w, r, a.cfg.MaxContent)
	if !ok {
		return
	}
	result, err := a.db.SaveResource(r.Context(), user.ID, resource.ID, store.ResourceEdit{
		Name: resource.Name, Filename: resource.Filename, Content: body,
		ContentEncoding: strings.TrimSpace(r.Header.Get("X-PlainMote-Encoding")), BaseVersion: base,
	})
	if conflict := (*store.VersionConflict)(nil); errors.As(err, &conflict) {
		a.writeAPI(w, http.StatusPreconditionFailed, apiError{
			Error: "conflict", Message: "this resource was saved elsewhere while you were editing", CurrentVersion: conflict.Current,
		})
		return
	}
	if err != nil {
		a.apiRefused(w, "save content", err)
		return
	}
	a.writeAPI(w, http.StatusOK, map[string]any{"version": result.Version, "new_version": result.NewVersion, "trimmed": result.Trimmed})
}

// apiListVersions lists the current version and every retained earlier version.
func (a *App) apiListVersions(w http.ResponseWriter, r *http.Request, user User, id string) {
	ctx := r.Context()
	resource, err := a.db.ResourceForOwner(ctx, user.ID, id)
	if err != nil {
		a.apiRefused(w, "list versions", err)
		return
	}
	earlier, err := a.db.ListVersions(ctx, user.ID, resource.ID)
	if err != nil {
		a.apiRefused(w, "list versions", err)
		return
	}
	type apiVersion struct {
		Version      int       `json:"version"`
		Size         int64     `json:"size"`
		SizeDelta    *int64    `json:"size_delta,omitempty"`
		SHA256       string    `json:"sha256,omitempty"`
		SavedAt      time.Time `json:"saved_at"`
		Current      bool      `json:"current,omitempty"`
		RestoredFrom int       `json:"restored_from,omitempty"`
	}
	encrypted := resource.Sealed() || resource.Encrypted()
	versions := make([]apiVersion, 0, len(earlier)+1)
	versions = append(versions, apiVersion{
		Version: resource.Version, Size: resource.ContentSize, SHA256: resource.ContentSHA256,
		SavedAt: resource.VersionAt, Current: true, RestoredFrom: resource.RestoredFrom,
	})
	for _, v := range earlier {
		versions = append(versions, apiVersion{
			Version: v.Number, Size: v.ContentSize, SHA256: v.ContentSHA256,
			SavedAt: v.SavedAt, RestoredFrom: v.RestoredFrom,
		})
	}
	for i := range versions {
		if encrypted {
			versions[i].SHA256 = ""
		}
		if i+1 < len(versions) {
			delta := versions[i].Size - versions[i+1].Size
			versions[i].SizeDelta = &delta
		}
	}
	a.writeAPI(w, http.StatusOK, map[string]any{
		"id": resource.ID, "current_version": resource.Version, "versions": versions,
	})
}

type apiLink struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	URL        string     `json:"url,omitempty"`
	BaseURL    string     `json:"base_url,omitempty"`
	Sealed     bool       `json:"sealed,omitempty"`
	CodeLink   bool       `json:"code_link,omitempty"`
	OwnerKey   string     `json:"owner_key,omitempty"`
	MaxUses    int        `json:"max_uses"`
	UsedCount  int        `json:"used_count"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

func (a *App) apiLinkView(r *http.Request, resource Resource, link Link) apiLink {
	view := apiLink{
		ID: link.ID, Name: link.Name, MaxUses: link.MaxUses, UsedCount: link.UsedCount,
		CreatedAt: link.CreatedAt, ExpiresAt: link.ExpiresAt, LastUsedAt: link.LastUsedAt,
	}
	if resource.Sealed() {
		view.Sealed = true
		view.CodeLink = link.CodeLink()
		if len(link.OwnerKey) > 0 {
			view.OwnerKey = base64.RawURLEncoding.EncodeToString(link.OwnerKey)
		}
	}
	if !link.Unreadable && link.Token != "" {
		address := a.baseURL(r) + shareAddress(link.Token, deliveryFilename(resource, resource.ContentType))
		if resource.Sealed() && !link.CodeLink() {
			// Key-in-fragment links cannot decrypt without appending #k=<key>
			// unwrapped from owner_key on the client; expose base_url instead of
			// an incomplete url that would fail if shared verbatim.
			view.BaseURL = address
		} else {
			view.URL = address
		}
	}
	return view
}

// apiListLinks lists a resource's active share links.
func (a *App) apiListLinks(w http.ResponseWriter, r *http.Request, user User, id string) {
	ctx := r.Context()
	resource, err := a.db.ResourceForOwner(ctx, user.ID, id)
	if err != nil {
		a.apiRefused(w, "list links", err)
		return
	}
	live, err := a.db.ListShares(ctx, user.ID, resource.ID, time.Now().UTC())
	if err != nil {
		a.apiRefused(w, "list links", err)
		return
	}
	views := make([]apiLink, 0, len(live))
	for _, link := range live {
		views = append(views, a.apiLinkView(r, resource, link))
	}
	a.writeAPI(w, http.StatusOK, map[string]any{"links": views})
}

// apiCreateLink creates a share link for a resource. Body is optional JSON:
// {"name": "...", "expires_in": "7d"|"never", "max_uses": 0}.
func (a *App) apiCreateLink(w http.ResponseWriter, r *http.Request, user User, token store.APIToken, id string) {
	var in struct {
		Name      string `json:"name"`
		ExpiresIn string `json:"expires_in"`
		MaxUses   int    `json:"max_uses"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if !a.readAPIJSON(w, r, &in) {
			return
		}
	}
	var ttl time.Duration
	switch value := strings.ToLower(strings.TrimSpace(in.ExpiresIn)); value {
	case "", "never":
	default:
		parsed, err := parseShareDuration(value)
		if err != nil || parsed <= 0 || parsed > apiShareMaxTTL {
			a.apiFail(w, http.StatusBadRequest, "bad_request", "expires_in must be a duration such as 1h, 7d or 30d (up to 365d), or never")
			return
		}
		ttl = parsed
	}
	if in.MaxUses < 0 || in.MaxUses > apiShareMaxUses {
		a.apiFail(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("max_uses must be between 0 and %d", apiShareMaxUses))
		return
	}
	if utf8.RuneCountInString(in.Name) > apiShareNameMax {
		a.apiFail(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("name can be at most %d characters", apiShareNameMax))
		return
	}
	ctx := r.Context()
	resource, err := a.db.ResourceForOwner(ctx, user.ID, id)
	if err != nil {
		a.apiRefused(w, "create link", err)
		return
	}
	if !a.limits.apiShares.allow(token.ID, time.Now().UTC()) {
		a.apiFail(w, http.StatusTooManyRequests, "rate_limited", "too many share links created; try again later")
		return
	}
	link, err := a.db.CreateShare(ctx, user.ID, resource.ID, strings.TrimSpace(in.Name), ttl, in.MaxUses)
	if err != nil {
		a.apiRefused(w, "create link", err)
		return
	}
	a.writeAPI(w, http.StatusCreated, a.apiLinkView(r, resource, link))
}

// apiRevokeLink revokes a single share link of a resource.
func (a *App) apiRevokeLink(w http.ResponseWriter, r *http.Request, user User, resourceID, linkID string) {
	if err := a.db.RevokeLink(r.Context(), user.ID, resourceID, linkID); err != nil {
		a.apiRefused(w, "revoke link", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// apiBody reads a request body no larger than a resource may be.
func (a *App) apiBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, bool) {
	if r.ContentLength > limit {
		a.apiFail(w, http.StatusRequestEntityTooLarge, "too_large", "content is too large")
		return nil, false
	}
	allowUpload(w, limit)
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			a.apiFail(w, http.StatusRequestEntityTooLarge, "too_large", "content is too large")
		} else {
			a.apiFail(w, http.StatusBadRequest, "bad_request", "could not read the request body")
		}
		return nil, false
	}
	return body, true
}

// apiCreateResource creates a new resource from either application/json
// ({"content": "...", "filename": "...", "name": "...", "tags": [...]}) or
// multipart/form-data (for binary files or encrypted payloads).
func (a *App) apiCreateResource(w http.ResponseWriter, r *http.Request, user User) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		a.apiFail(w, http.StatusUnsupportedMediaType, "bad_request", "send application/json or multipart/form-data")
		return
	}
	switch mediaType {
	case "application/json":
		a.apiCreateResourceJSON(w, r, user)
		return
	case "multipart/form-data":
	default:
		a.apiFail(w, http.StatusUnsupportedMediaType, "bad_request", "send application/json or multipart/form-data")
		return
	}
	limit := a.cfg.MaxContent + 64<<10
	if r.ContentLength > limit {
		a.apiFail(w, http.StatusRequestEntityTooLarge, "too_large", "content is too large")
		return
	}
	allowUpload(w, limit)
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := r.ParseMultipartForm(a.cfg.MaxContent + 1); err != nil {
		a.apiFail(w, http.StatusBadRequest, "bad_request", "could not read the form")
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	content, ok := a.readMultipartContent(w, r, a.cfg.MaxContent+sealedOverhead)
	if !ok {
		return
	}
	tags := r.MultipartForm.Value["tag"]
	if err := checkTags(tags); err != nil {
		a.apiRefused(w, "create resource", err)
		return
	}
	// Encrypted on the client: stored under the id its encryption is bound
	// to, with its wrapped key and encrypted metadata.
	if r.FormValue("sealed_key") != "" {
		resource, err := a.db.CreateSealedResource(r.Context(), user.ID, r.FormValue("id"),
			store.SealedPart{Content: content, Meta: base64Field(r, "sealed_meta")}, base64Field(r, "sealed_key"))
		if err != nil {
			a.apiRefused(w, "create sealed resource", err)
			return
		}
		a.apiCreated(w, r, user, resource, tags)
		return
	}
	if int64(len(content)) > a.cfg.MaxContent {
		a.apiFail(w, http.StatusRequestEntityTooLarge, "too_large", "content is too large")
		return
	}
	resource, err := a.db.CreateResource(r.Context(), user.ID, r.FormValue("name"), strings.TrimSpace(r.FormValue("filename")),
		content, strings.TrimSpace(r.FormValue("encoding")))
	if err != nil {
		a.apiRefused(w, "create resource", err)
		return
	}
	a.apiCreated(w, r, user, resource, tags)
}

func (a *App) apiCreateResourceJSON(w http.ResponseWriter, r *http.Request, user User) {
	var in struct {
		Content  *string  `json:"content"`
		Name     string   `json:"name"`
		Filename string   `json:"filename"`
		Encoding string   `json:"encoding"`
		Tags     []string `json:"tags"`
	}
	if !a.readAPIJSONLimit(w, r, &in, a.cfg.MaxContent+64<<10) {
		return
	}
	if in.Content == nil {
		a.apiFail(w, http.StatusBadRequest, "bad_request", "content is required")
		return
	}
	if int64(len(*in.Content)) > a.cfg.MaxContent {
		a.apiFail(w, http.StatusRequestEntityTooLarge, "too_large", "content is too large")
		return
	}
	if utf8.RuneCountInString(in.Name) > apiNameMaxRunes {
		a.apiFail(w, http.StatusBadRequest, "bad_request", fmt.Sprintf("name can be at most %d characters", apiNameMaxRunes))
		return
	}
	if err := checkTags(in.Tags); err != nil {
		a.apiRefused(w, "create resource", err)
		return
	}
	resource, err := a.db.CreateResource(r.Context(), user.ID, in.Name, strings.TrimSpace(in.Filename),
		[]byte(*in.Content), strings.TrimSpace(in.Encoding))
	if err != nil {
		a.apiRefused(w, "create resource", err)
		return
	}
	a.apiCreated(w, r, user, resource, in.Tags)
}

// readMultipartContent reads the "content" field whether it was uploaded as a
// file (-F content=@file) or sent as a plain form field (-F content=text).
func (a *App) readMultipartContent(w http.ResponseWriter, r *http.Request, maxBytes int64) ([]byte, bool) {
	file, _, err := r.FormFile("content")
	if err == nil {
		defer file.Close()
		content, readErr := io.ReadAll(io.LimitReader(file, maxBytes+1))
		if readErr != nil || int64(len(content)) > maxBytes {
			a.apiFail(w, http.StatusRequestEntityTooLarge, "too_large", "content is too large")
			return nil, false
		}
		return content, true
	}
	if values := r.MultipartForm.Value["content"]; len(values) > 0 {
		content := []byte(values[0])
		if int64(len(content)) > maxBytes {
			a.apiFail(w, http.StatusRequestEntityTooLarge, "too_large", "content is too large")
			return nil, false
		}
		return content, true
	}
	a.apiFail(w, http.StatusBadRequest, "bad_request", "the form has no content field")
	return nil, false
}

// apiCreated tags a resource just made, and answers with it.
func (a *App) apiCreated(w http.ResponseWriter, r *http.Request, user User, resource Resource, tags []string) {
	if len(tags) > 0 {
		var err error
		if resource.Tags, err = a.db.SetResourceTags(r.Context(), user.ID, resource.ID, tags); err != nil {
			a.apiRefused(w, "tag resource", err)
			return
		}
	}
	a.writeAPI(w, http.StatusCreated, a.apiResourceView(r, resource))
}

// apiCreateQuickShare is the home page's box for a signed-in account: the
// content and one link that ends with it, kept in the account's resources.
// Accepts application/json or multipart/form-data.
func (a *App) apiCreateQuickShare(w http.ResponseWriter, r *http.Request, user User) {
	if !a.cfg.AnonymousEnabled {
		a.apiFail(w, http.StatusNotFound, "not_found", "this server does not offer quick shares")
		return
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		a.apiFail(w, http.StatusUnsupportedMediaType, "bad_request", "send application/json or multipart/form-data")
		return
	}
	if mediaType == "application/json" {
		var in struct {
			Content  *string `json:"content"`
			Filename string  `json:"filename"`
			TTL      string  `json:"ttl"`
		}
		if !a.readAPIJSONLimit(w, r, &in, int64(store.AnonymousMaxBytes)+64<<10) {
			return
		}
		if in.Content == nil {
			a.apiFail(w, http.StatusBadRequest, "bad_request", "content is required")
			return
		}
		if len(*in.Content) > store.AnonymousMaxBytes {
			a.apiFail(w, http.StatusRequestEntityTooLarge, "too_large", "content is too large")
			return
		}
		ttl := store.AnonymousDefaultTTL
		if value := strings.TrimSpace(in.TTL); value != "" {
			choice, ok := lifetimeChoice(value)
			if !ok {
				a.apiFail(w, http.StatusBadRequest, "invalid_ttl", "ttl must be one of 10m, 1h, 1d, 7d or 30d")
				return
			}
			ttl = choice.Duration
		}
		resource, link, err := a.db.CreateQuickShare(r.Context(), user.ID, strings.TrimSpace(in.Filename), []byte(*in.Content), ttl, time.Now().UTC())
		if err != nil {
			a.apiRefused(w, "create quick share", err)
			return
		}
		a.writeAPI(w, http.StatusCreated, struct {
			apiResource
			ShareURL string `json:"share_url"`
		}{a.apiResourceView(r, resource), a.baseURL(r) + shareAddress(link.Token, deliveryFilename(resource, resource.ContentType))})
		return
	}
	if mediaType != "multipart/form-data" {
		a.apiFail(w, http.StatusUnsupportedMediaType, "bad_request", "send application/json or multipart/form-data")
		return
	}
	limit := int64(store.AnonymousMaxBytes) + 64<<10
	if r.ContentLength > limit {
		a.apiFail(w, http.StatusRequestEntityTooLarge, "too_large", "content is too large")
		return
	}
	allowUpload(w, limit)
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := r.ParseMultipartForm(store.AnonymousMaxBytes + 1); err != nil {
		a.apiFail(w, http.StatusBadRequest, "bad_request", "could not read the form")
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	ttl := store.AnonymousDefaultTTL
	if value := strings.TrimSpace(r.FormValue("ttl")); value != "" {
		choice, ok := lifetimeChoice(value)
		if !ok {
			a.apiFail(w, http.StatusBadRequest, "invalid_ttl", "ttl must be one of 10m, 1h, 1d, 7d or 30d")
			return
		}
		ttl = choice.Duration
	}
	content, ok := a.readMultipartContent(w, r, int64(store.AnonymousMaxBytes)+sealedOverhead)
	if !ok {
		return
	}
	var resource Resource
	var link Link
	if r.FormValue("sealed_key") != "" {
		resource, link, err = a.db.CreateSealedQuickShare(r.Context(), user.ID, r.FormValue("id"), r.FormValue("link_id"),
			store.SealedPart{Content: content, Meta: base64Field(r, "sealed_meta")},
			base64Field(r, "sealed_key"), base64Field(r, "link_key"), base64Field(r, "owner_key"), ttl, time.Now().UTC())
	} else if len(content) > store.AnonymousMaxBytes {
		a.apiFail(w, http.StatusRequestEntityTooLarge, "too_large", "content is too large")
		return
	} else {
		resource, link, err = a.db.CreateQuickShare(r.Context(), user.ID, strings.TrimSpace(r.FormValue("filename")), content, ttl, time.Now().UTC())
	}
	if err != nil {
		a.apiRefused(w, "create quick share", err)
		return
	}
	a.writeAPI(w, http.StatusCreated, struct {
		apiResource
		ShareURL string `json:"share_url"`
	}{a.apiResourceView(r, resource), a.baseURL(r) + shareAddress(link.Token, deliveryFilename(resource, resource.ContentType))})
}

func (a *App) readAPIJSON(w http.ResponseWriter, r *http.Request, into any) bool {
	return a.readAPIJSONLimit(w, r, into, apiMaxJSON)
}

func (a *App) readAPIJSONLimit(w http.ResponseWriter, r *http.Request, into any, limit int64) bool {
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/json" {
		a.apiFail(w, http.StatusUnsupportedMediaType, "bad_request", "send JSON")
		return false
	}
	if r.ContentLength > limit {
		a.apiFail(w, http.StatusRequestEntityTooLarge, "too_large", "content is too large")
		return false
	}
	if limit > apiMaxJSON {
		allowUpload(w, limit)
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, limit))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			a.apiFail(w, http.StatusRequestEntityTooLarge, "too_large", "content is too large")
		} else {
			a.apiFail(w, http.StatusBadRequest, "bad_request", "could not read the request")
		}
		return false
	}
	return true
}

// sealedOverhead is what encryption adds to content: the magic, the IV and
// the tag.
const sealedOverhead = 64

func b64url(value []byte) string {
	if len(value) == 0 {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(value)
}

// apiKeyring returns the account's wrapped keyring for end-to-end encrypted
// resources.
func (a *App) apiKeyring(w http.ResponseWriter, r *http.Request, user User) {
	keyring, err := a.db.Keyring(r.Context(), user.ID)
	if errors.Is(err, store.ErrNotFound) {
		a.apiFail(w, http.StatusNotFound, "not_found", "this account has no master password")
		return
	}
	if err != nil {
		a.apiRefused(w, "read keyring", err)
		return
	}
	a.writeAPI(w, http.StatusOK, struct {
		keyringView
		UserID string `json:"user_id"`
	}{keyringJSON(keyring), user.ID})
}

// apiWriteSealed saves client-encrypted content: the body is the ciphertext,
// and X-PlainMote-Sealed-Meta the encrypted metadata to go with it.
func (a *App) apiWriteSealed(w http.ResponseWriter, r *http.Request, user User, resource Resource, base int) {
	meta, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(r.Header.Get("X-PlainMote-Sealed-Meta")))
	if err != nil || len(meta) == 0 {
		a.apiFail(w, http.StatusBadRequest, "bad_request", "an encrypted resource is saved with its encrypted metadata in X-PlainMote-Sealed-Meta")
		return
	}
	body, ok := a.apiBody(w, r, a.cfg.MaxContent+sealedOverhead)
	if !ok {
		return
	}
	result, err := a.db.SaveSealedResource(r.Context(), user.ID, resource.ID, base, store.SealedPart{Content: body, Meta: meta})
	if conflict := (*store.VersionConflict)(nil); errors.As(err, &conflict) {
		a.writeAPI(w, http.StatusPreconditionFailed, apiError{
			Error: "conflict", Message: "this resource was saved elsewhere while you were editing", CurrentVersion: conflict.Current,
		})
		return
	}
	if err != nil {
		a.apiRefused(w, "save sealed content", err)
		return
	}
	a.writeAPI(w, http.StatusOK, map[string]any{"version": result.Version, "new_version": result.NewVersion, "trimmed": result.Trimmed})
}
