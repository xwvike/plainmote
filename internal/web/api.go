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

	"plainmote/internal/store"
)

// The command line's interface. Everything under apiPrefix except the two
// device endpoints needs a personal access token in the Authorization header
// - never a cookie, never a query parameter - and nothing here answers a
// browser on another site: no CORS headers are ever sent, so a page cannot
// read an answer, and a request carrying a token is never a "simple" one a
// page could send blind.
const (
	apiPrefix        = "/api/v1/"
	apiDeviceCode    = apiPrefix + "device/code"
	apiDeviceToken   = apiPrefix + "device/token"
	deviceVerifyPath = "/cli/device"
	apiMaxJSON       = 4 << 10
)

// apiLimits brakes guessing and floods. Counts are per address, and per token
// for an authenticated caller.
type apiLimits struct {
	deviceCodes  *windowLimiter // codes asked for, per address
	devicePolls  *windowLimiter // polls, per address
	codeFailures *windowLimiter // wrong codes typed in the browser, per account
	codeAddress  *windowLimiter // and per address
	authFailures *windowLimiter // bad tokens, per address
	requests     *windowLimiter // calls, per token
}

func newAPILimits() *apiLimits {
	return &apiLimits{
		deviceCodes:  newWindowLimiter(10, 10*time.Minute),
		devicePolls:  newWindowLimiter(120, time.Minute),
		codeFailures: newWindowLimiter(5, 15*time.Minute),
		codeAddress:  newWindowLimiter(20, 15*time.Minute),
		authFailures: newWindowLimiter(30, time.Minute),
		requests:     newWindowLimiter(300, time.Minute),
	}
}

type apiError struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
	// Set on a conflict: the version the save would have replaced.
	CurrentVersion int `json:"current_version,omitempty"`
	// Set on a device poll: how long to wait before the next one.
	Interval int `json:"interval,omitempty"`
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
	switch r.URL.Path {
	case apiDeviceCode:
		a.handleDeviceCode(w, r)
		return
	case apiDeviceToken:
		a.handleDeviceToken(w, r)
		return
	}
	user, token, ok := a.apiUser(w, r)
	if !ok {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, apiPrefix)
	switch {
	case path == "me" && r.Method == http.MethodGet:
		a.writeAPI(w, http.StatusOK, map[string]any{
			"login": user.Login, "user_id": user.ID, "scope": token.Scope, "expires_at": token.ExpiresAt,
			"device": token.DeviceName, "server_version": a.cfg.Version,
		})
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
	case path == "quick-shares" && r.Method == http.MethodPost:
		if a.apiWritable(w, token) {
			a.apiCreateQuickShare(w, r, user)
		}
	case strings.HasPrefix(path, "resources/"):
		rest := strings.Split(strings.TrimPrefix(path, "resources/"), "/")
		switch {
		case len(rest) == 1 && r.Method == http.MethodGet:
			a.apiGetResource(w, r, user, rest[0])
		case len(rest) == 2 && rest[1] == "content" && r.Method == http.MethodGet:
			a.apiReadContent(w, r, user, rest[0])
		case len(rest) == 2 && rest[1] == "content" && r.Method == http.MethodPut:
			if a.apiWritable(w, token) {
				a.apiWriteContent(w, r, user, rest[0])
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
	if a.limits.authFailures.blocked(ip, now) {
		a.apiFail(w, http.StatusTooManyRequests, "rate_limited", "too many failed attempts; try again in a minute")
		return User{}, store.APIToken{}, false
	}
	value, found := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	value = strings.TrimSpace(value)
	if !found || value == "" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="plainmote"`)
		a.apiFail(w, http.StatusUnauthorized, "unauthorized", "sign in with `plainmote login`")
		return User{}, store.APIToken{}, false
	}
	user, token, err := a.db.TokenUser(r.Context(), value, ip, now)
	if errors.Is(err, store.ErrNotFound) {
		a.limits.authFailures.allow(ip, now)
		w.Header().Set("WWW-Authenticate", `Bearer realm="plainmote", error="invalid_token"`)
		a.apiFail(w, http.StatusUnauthorized, "unauthorized", "this sign-in has expired or was revoked; run `plainmote login`")
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
	a.apiFail(w, http.StatusForbidden, "read_only", "this sign-in is read-only; run `plainmote login` to sign in with write access")
	return false
}

type apiResource struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Filename  string    `json:"filename"`
	Size      int64     `json:"size"`
	Type      string    `json:"type"`
	Encoding  string    `json:"encoding,omitempty"`
	Version   int       `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
	Remote    bool      `json:"remote"`
	Editable  bool      `json:"editable"`
	Encrypted bool      `json:"encrypted,omitempty"`
	TakenDown bool      `json:"taken_down,omitempty"`
	// ExpiresAt is set on a quick share not yet kept: read only, and
	// deleted at that time.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	// SealedKey and SealedMeta are set on an end-to-end encrypted resource:
	// its content key wrapped by the account key, and its encrypted
	// metadata (docs/encryption.md). Its content is ciphertext.
	SealedKey  string `json:"sealed_key,omitempty"`
	SealedMeta string `json:"sealed_meta,omitempty"`
	URL        string `json:"url"`
}

func (a *App) apiResourceView(r *http.Request, resource Resource) apiResource {
	encrypted := resource.ContentType == store.EncryptedContentType || resource.Sealed()
	return apiResource{
		ID: resource.ID, Name: resource.Name, Filename: resource.Filename, Size: resource.ContentSize,
		Type: resource.ContentType, Encoding: resource.ContentEncoding, Version: resource.Version,
		UpdatedAt: resource.UpdatedAt, Remote: resource.Remote(), Encrypted: encrypted,
		Editable:  resource.Editable() && !encrypted && !resource.Remote() && !resource.QuickShare(),
		TakenDown: resource.TakenDown, ExpiresAt: resource.ExpiresAt, URL: a.baseURL(r) + "/resources/" + resource.ID,
		SealedKey: b64url(resource.SealedKey), SealedMeta: b64url(resource.SealedMeta),
	}
}

// apiListResources lists, or with ref resolves a reference typed on the
// command line to every resource it can mean.
func (a *App) apiListResources(w http.ResponseWriter, r *http.Request, user User) {
	var resources []Resource
	var err error
	if ref := r.URL.Query().Get("ref"); ref != "" {
		resources, err = a.db.ResolveResources(r.Context(), user.ID, ref)
	} else {
		resources, err = a.db.APIResources(r.Context(), user.ID, r.URL.Query().Get("q"))
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
	a.writeAPI(w, http.StatusOK, a.apiResourceView(r, resource))
}

// apiVersionTag is the ETag of a resource's content: its version number.
func apiVersionTag(version int) string { return `"v` + strconv.Itoa(version) + `"` }

// apiReadContent sends the stored bytes as they are, in the encoding they
// were saved in, which the headers name. A reference has no stored bytes.
func (a *App) apiReadContent(w http.ResponseWriter, r *http.Request, user User, id string) {
	resource, err := a.db.ResourceForOwner(r.Context(), user.ID, id)
	if err != nil {
		a.apiRefused(w, "read content", err)
		return
	}
	if resource.Remote() {
		a.apiFail(w, http.StatusConflict, "reference", "this resource points at a remote address and has no stored content")
		return
	}
	body, size, err := a.db.OpenContent(r.Context(), resource)
	if err != nil {
		a.apiRefused(w, "read content", fmt.Errorf("%w: %w", store.ErrInternal, err))
		return
	}
	defer body.Close()
	w.Header().Set("Content-Type", store.ContentTypeWithEncoding(store.SafeContentType(resource.ContentType), resource.ContentEncoding))
	w.Header().Set("Content-Disposition", "attachment")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("ETag", apiVersionTag(resource.Version))
	w.Header().Set("X-PlainMote-Resource-Version", strconv.Itoa(resource.Version))
	if resource.ContentEncoding != "" {
		w.Header().Set("X-PlainMote-Encoding", resource.ContentEncoding)
	}
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
}

// apiWriteContent saves new content as the resource's next version. The
// version it was edited from must come in If-Match, as the resource page's
// hidden field does; "*" saves over whatever is current, on purpose.
func (a *App) apiWriteContent(w http.ResponseWriter, r *http.Request, user User, id string) {
	resource, err := a.db.ResourceForOwner(r.Context(), user.ID, id)
	if err != nil {
		a.apiRefused(w, "save content", err)
		return
	}
	if resource.Remote() {
		a.apiFail(w, http.StatusConflict, "reference", "this resource points at a remote address and has no stored content")
		return
	}
	base := 0
	switch match := strings.TrimSpace(r.Header.Get("If-Match")); {
	case match == "*":
	case strings.HasPrefix(match, `"v`) && strings.HasSuffix(match, `"`):
		base, err = strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(match, `"v`), `"`))
		if err != nil || base < 1 {
			a.apiFail(w, http.StatusBadRequest, "bad_request", "If-Match must be a version tag such as \"v5\", or *")
			return
		}
	default:
		a.apiFail(w, http.StatusPreconditionRequired, "precondition_required", "send the version the edit started from in If-Match, or * to save over the current one")
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

// apiBody reads a request body no larger than a resource may be.
func (a *App) apiBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, bool) {
	if r.ContentLength > limit {
		a.apiFail(w, http.StatusRequestEntityTooLarge, "too_large", "content is too large")
		return nil, false
	}
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

// apiCreateResource makes a new resource from a multipart form: name,
// filename and encoding fields beside a content file.
func (a *App) apiCreateResource(w http.ResponseWriter, r *http.Request, user User) {
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "multipart/form-data" {
		a.apiFail(w, http.StatusUnsupportedMediaType, "bad_request", "send a multipart form with a content file")
		return
	}
	limit := a.cfg.MaxContent + 64<<10
	if r.ContentLength > limit {
		a.apiFail(w, http.StatusRequestEntityTooLarge, "too_large", "content is too large")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := r.ParseMultipartForm(a.cfg.MaxContent + 1); err != nil {
		a.apiFail(w, http.StatusBadRequest, "bad_request", "could not read the form")
		return
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	file, _, err := r.FormFile("content")
	if err != nil {
		a.apiFail(w, http.StatusBadRequest, "bad_request", "the form has no content file")
		return
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, a.cfg.MaxContent+sealedOverhead+1))
	if err != nil || int64(len(content)) > a.cfg.MaxContent+sealedOverhead {
		a.apiFail(w, http.StatusRequestEntityTooLarge, "too_large", "content is too large")
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
		a.writeAPI(w, http.StatusCreated, a.apiResourceView(r, resource))
		return
	}
	if int64(len(content)) > a.cfg.MaxContent {
		a.apiFail(w, http.StatusRequestEntityTooLarge, "too_large", "content is too large")
		return
	}
	resource, err := a.db.CreateResource(r.Context(), user.ID, r.FormValue("name"), strings.TrimSpace(r.FormValue("filename")),
		content, strings.TrimSpace(r.FormValue("encoding")), "")
	if err != nil {
		a.apiRefused(w, "create resource", err)
		return
	}
	a.writeAPI(w, http.StatusCreated, a.apiResourceView(r, resource))
}

// apiCreateQuickShare is the home page's box for a signed-in account: the
// content and one link that ends with it, kept in the account's resources.
// It exists where quick sharing does.
func (a *App) apiCreateQuickShare(w http.ResponseWriter, r *http.Request, user User) {
	if !a.cfg.AnonymousEnabled {
		a.apiFail(w, http.StatusNotFound, "not_found", "this server does not offer quick shares")
		return
	}
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "multipart/form-data" {
		a.apiFail(w, http.StatusUnsupportedMediaType, "bad_request", "send a multipart form with a content file")
		return
	}
	limit := int64(store.AnonymousMaxBytes) + 64<<10
	if r.ContentLength > limit {
		a.apiFail(w, http.StatusRequestEntityTooLarge, "too_large", "content is too large")
		return
	}
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
	file, _, err := r.FormFile("content")
	if err != nil {
		a.apiFail(w, http.StatusBadRequest, "bad_request", "the form has no content file")
		return
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, store.AnonymousMaxBytes+1))
	if err != nil || len(content) > store.AnonymousMaxBytes {
		a.apiFail(w, http.StatusRequestEntityTooLarge, "too_large", "content is too large")
		return
	}
	resource, link, err := a.db.CreateQuickShare(r.Context(), user.ID, strings.TrimSpace(r.FormValue("filename")), content, ttl, time.Now().UTC())
	if err != nil {
		a.apiRefused(w, "create quick share", err)
		return
	}
	a.writeAPI(w, http.StatusCreated, struct {
		apiResource
		ShareURL string `json:"share_url"`
	}{a.apiResourceView(r, resource), a.baseURL(r) + shareAddress(link.Token, deliveryFilename(resource, resource.ContentType))})
}

// handleDeviceCode starts a command line's sign-in. It needs no account - it
// is how a command line gets one - so it is limited per address, and asks for
// JSON so a page on another site cannot send it as a simple request.
func (a *App) handleDeviceCode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		a.apiFail(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	var request struct {
		Scope   string `json:"scope"`
		Device  string `json:"device"`
		OS      string `json:"os"`
		Version string `json:"version"`
	}
	if !a.readAPIJSON(w, r, &request) {
		return
	}
	now := time.Now().UTC()
	ip := a.clientIP(r)
	if !a.limits.deviceCodes.allow(ip, now) {
		a.apiFail(w, http.StatusTooManyRequests, "rate_limited", "too many sign-ins started from this address; try again later")
		return
	}
	deviceCode, userCode, grant, err := a.db.CreateDeviceGrant(r.Context(), store.DeviceRequest{
		Scope: request.Scope, DeviceName: request.Device, DeviceOS: request.OS,
		ClientVersion: request.Version, RequestIP: ip,
	}, now)
	if errors.Is(err, store.ErrBadScope) {
		a.apiFail(w, http.StatusBadRequest, "invalid_scope", "scope must be read or write")
		return
	}
	if err != nil {
		a.apiRefused(w, "start sign-in", err)
		return
	}
	a.writeAPI(w, http.StatusOK, map[string]any{
		"device_code":      deviceCode,
		"user_code":        store.FormatUserCode(userCode),
		"verification_uri": a.baseURL(r) + deviceVerifyPath,
		"expires_in":       int(grant.ExpiresAt.Sub(now).Seconds()),
		"interval":         store.DevicePollInterval,
	})
}

// handleDeviceToken answers a command line polling with its device code, in
// the shape RFC 8628 gives: a token once approved, otherwise an error naming
// why not yet, or not at all.
func (a *App) handleDeviceToken(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		a.apiFail(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	var request struct {
		DeviceCode string `json:"device_code"`
	}
	if !a.readAPIJSON(w, r, &request) {
		return
	}
	now := time.Now().UTC()
	if !a.limits.devicePolls.allow(a.clientIP(r), now) {
		a.writeAPI(w, http.StatusTooManyRequests, apiError{Error: store.DeviceSlowDown, Interval: store.DevicePollInterval * 2})
		return
	}
	exchange, err := a.db.ExchangeDeviceCode(r.Context(), request.DeviceCode, now)
	if err != nil {
		a.apiRefused(w, "finish sign-in", err)
		return
	}
	if exchange.State != store.DeviceApproved {
		a.writeAPI(w, http.StatusBadRequest, apiError{Error: exchange.State, Interval: exchange.Interval})
		return
	}
	a.writeAPI(w, http.StatusOK, map[string]any{
		"access_token": exchange.Token,
		"token_type":   "bearer",
		"scope":        exchange.APIToken.Scope,
		"expires_at":   exchange.APIToken.ExpiresAt,
		"login":        exchange.User.Login,
	})
}

func (a *App) readAPIJSON(w http.ResponseWriter, r *http.Request, into any) bool {
	if mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mediaType != "application/json" {
		a.apiFail(w, http.StatusUnsupportedMediaType, "bad_request", "send JSON")
		return false
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, apiMaxJSON))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		a.apiFail(w, http.StatusBadRequest, "bad_request", "could not read the request")
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

// apiKeyring hands the command line the account's keyring, wrapped as it is
// kept: what it needs to unlock with the master password, and nothing that
// opens without it.
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

// apiWriteSealed saves content the command line encrypted: the body is the
// ciphertext, and X-PlainMote-Sealed-Meta the encrypted metadata to go with
// it.
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
