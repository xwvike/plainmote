package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"plainmote/internal/store"
)

// The authorization server MCP clients sign in through (OAuth 2.1 with the
// MCP authorization profile). A client finds it from the protected resource
// metadata of the MCP endpoint, identifies itself by the address of its
// metadata document or by registering, sends its owner here to consent, and
// trades the code it gets back - with its PKCE verifier - for tokens bound to
// the MCP endpoint alone.
//
// None of these endpoints reads a cookie except the consent page, so the ones
// a client calls from a browser answer any origin: there is nothing ambient
// for another site to borrow.
const (
	mcpPath                  = "/mcp"
	wellKnownResource        = "/.well-known/oauth-protected-resource"
	wellKnownAuthServer      = "/.well-known/oauth-authorization-server"
	oauthAuthorizePath       = "/oauth/authorize"
	oauthTokenPath           = "/oauth/token"
	oauthRegisterPath        = "/oauth/register"
	oauthRevokePath          = "/oauth/revoke"
	oauthMaxBody             = 16 << 10
	clientDocumentMaxBytes   = 16 << 10
	clientDocumentMaxURL     = 2000
	oauthStateMaxBytes       = 1024
	oauthChallengeMethodS256 = "S256"
)

// mcpResource is the MCP endpoint's own address: the audience of every token
// issued here.
func (a *App) mcpResource(r *http.Request) string { return a.baseURL(r) + mcpPath }

// openCORS lets a browser-based client read the answer. Only endpoints that
// read no cookie use it.
func openCORS(w http.ResponseWriter, r *http.Request, methods string) bool {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Methods", methods+", OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, MCP-Protocol-Version")
		w.Header().Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

func (a *App) handleResourceMetadata(w http.ResponseWriter, r *http.Request) {
	if openCORS(w, r, http.MethodGet) {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", http.MethodGet)
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	a.writeAPI(w, http.StatusOK, map[string]any{
		"resource":                 a.mcpResource(r),
		"authorization_servers":    []string{a.baseURL(r)},
		"scopes_supported":         store.OAuthScopes,
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "PlainMote",
	})
}

func (a *App) handleAuthServerMetadata(w http.ResponseWriter, r *http.Request) {
	if openCORS(w, r, http.MethodGet) {
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", http.MethodGet)
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	base := a.baseURL(r)
	a.writeAPI(w, http.StatusOK, map[string]any{
		"issuer":                                         base,
		"authorization_endpoint":                         base + oauthAuthorizePath,
		"token_endpoint":                                 base + oauthTokenPath,
		"registration_endpoint":                          base + oauthRegisterPath,
		"revocation_endpoint":                            base + oauthRevokePath,
		"scopes_supported":                               store.OAuthScopes,
		"response_types_supported":                       []string{"code"},
		"response_modes_supported":                       []string{"query"},
		"grant_types_supported":                          []string{"authorization_code", "refresh_token"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"revocation_endpoint_auth_methods_supported":     []string{"none"},
		"code_challenge_methods_supported":               []string{oauthChallengeMethodS256},
		"client_id_metadata_document_supported":          true,
		"authorization_response_iss_parameter_supported": true,
	})
}

// oauthError is an error as OAuth writes it, for the endpoints a client calls
// directly.
type oauthError struct {
	Error       string `json:"error"`
	Description string `json:"error_description,omitempty"`
}

func (a *App) oauthFail(w http.ResponseWriter, status int, code, description string) {
	w.Header().Set("Pragma", "no-cache")
	a.writeAPI(w, status, oauthError{Error: code, Description: description})
}

// readOAuthForm parses a form-encoded body and nothing else: a token or a code
// in a query string ends up in logs along the way.
func readOAuthForm(w http.ResponseWriter, r *http.Request) bool {
	mediaType := strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])
	if !strings.EqualFold(mediaType, "application/x-www-form-urlencoded") {
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, oauthMaxBody)
	return r.ParseForm() == nil && r.URL.RawQuery == ""
}

// handleOAuthRegister registers a client (RFC 7591). Every client is public:
// one that asks for a secret is told it has none, as the RFC lets a server
// do, and proves itself with PKCE like the rest.
func (a *App) handleOAuthRegister(w http.ResponseWriter, r *http.Request) {
	if openCORS(w, r, http.MethodPost) {
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	now := time.Now()
	ip := a.clientIP(r)
	if !a.limits.registrations.allow(ip, now) {
		a.oauthFail(w, http.StatusTooManyRequests, "invalid_request", "too many registrations from this address; try again later")
		return
	}
	var request struct {
		RedirectURIs  []string `json:"redirect_uris"`
		ClientName    string   `json:"client_name"`
		ClientURI     string   `json:"client_uri"`
		GrantTypes    []string `json:"grant_types"`
		ResponseTypes []string `json:"response_types"`
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, oauthMaxBody))
	if err != nil || json.Unmarshal(body, &request) != nil {
		a.oauthFail(w, http.StatusBadRequest, "invalid_client_metadata", "the body must be a JSON object of client metadata")
		return
	}
	for _, grant := range request.GrantTypes {
		if grant != "authorization_code" && grant != "refresh_token" {
			a.oauthFail(w, http.StatusBadRequest, "invalid_client_metadata", "only the authorization_code and refresh_token grants are supported")
			return
		}
	}
	for _, response := range request.ResponseTypes {
		if response != "code" {
			a.oauthFail(w, http.StatusBadRequest, "invalid_client_metadata", "only the code response type is supported")
			return
		}
	}
	client, err := a.db.RegisterOAuthClient(r.Context(), request.ClientName, request.ClientURI, request.RedirectURIs, ip, now.UTC())
	if store.IsRefusal(err) {
		a.oauthFail(w, http.StatusBadRequest, "invalid_redirect_uri", localizePageError("en", err.Error()))
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "oauth register: %v\n", err)
		a.oauthFail(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	response := map[string]any{
		"client_id":                  client.ID,
		"client_id_issued_at":        client.CreatedAt.Unix(),
		"redirect_uris":              client.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	}
	if client.Name != "" {
		response["client_name"] = client.Name
	}
	if client.ClientURI != "" {
		response["client_uri"] = client.ClientURI
	}
	a.writeAPI(w, http.StatusCreated, response)
}

// errClientUnknown is a client id that names no client this server can use.
var errClientUnknown = errors.New("unknown client")

// errClientLimited is a metadata document not fetched because the address
// asking has fetched too many.
var errClientLimited = errors.New("client document fetches limited")

// oauthClient resolves a client id: a registered client from the database, or
// a client metadata document fetched from its https address and kept for an
// hour.
func (a *App) oauthClient(ctx context.Context, r *http.Request, id string) (store.OAuthClient, error) {
	switch {
	case strings.HasPrefix(id, store.OAuthClientPrefix):
		client, err := a.db.OAuthClient(ctx, id)
		if errors.Is(err, store.ErrNotFound) || (err == nil && client.Kind != store.OAuthClientRegistered) {
			return store.OAuthClient{}, errClientUnknown
		}
		return client, err
	case !clientDocumentURL(id):
		return store.OAuthClient{}, errClientUnknown
	}
	now := time.Now().UTC()
	cached, err := a.db.OAuthClient(ctx, id)
	if err == nil && cached.Kind == store.OAuthClientDocument && now.Sub(cached.FetchedAt) < store.OAuthDocumentFresh {
		return cached, nil
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return store.OAuthClient{}, err
	}
	if !a.limits.clientDocuments.allow(a.clientIP(r), now) {
		return store.OAuthClient{}, errClientLimited
	}
	fetchCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	body, _, err := a.clientDocs.Fetch(fetchCtx, id)
	if err != nil {
		return store.OAuthClient{}, fmt.Errorf("%w: %v", errClientUnknown, err)
	}
	var document struct {
		ClientID     string   `json:"client_id"`
		ClientName   string   `json:"client_name"`
		ClientURI    string   `json:"client_uri"`
		RedirectURIs []string `json:"redirect_uris"`
		AuthMethod   string   `json:"token_endpoint_auth_method"`
	}
	if json.Unmarshal(body, &document) != nil || document.ClientID != id || (document.AuthMethod != "" && document.AuthMethod != "none") {
		return store.OAuthClient{}, errClientUnknown
	}
	client, err := a.db.SaveOAuthDocumentClient(ctx, id, document.ClientName, document.ClientURI, document.RedirectURIs, now)
	if store.IsRefusal(err) {
		return store.OAuthClient{}, errClientUnknown
	}
	return client, err
}

// clientDocumentURL is a client id that may be a metadata document's address:
// https, with a path, and nothing a fetch or a comparison would read two ways.
func clientDocumentURL(id string) bool {
	if len(id) > clientDocumentMaxURL || !strings.HasPrefix(id, "https://") || strings.ContainsAny(id, "#\\ ") {
		return false
	}
	parsed, err := url.Parse(id)
	if err != nil || parsed.User != nil || parsed.Host == "" || parsed.Path == "" || parsed.Path == "/" || parsed.String() != id {
		return false
	}
	for _, segment := range strings.Split(parsed.Path, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

// authorizeRequest is an authorization request once its client and redirect
// URI are known to belong together - the point from which a problem can be
// reported back to the client rather than shown here.
type authorizeRequest struct {
	Client      store.OAuthClient
	RedirectURI string
	State       string
	Challenge   string
	Scope       string
	Resource    string
}

// authorizeProblem is what is wrong with a request. A page problem is shown
// here: the client or its redirect URI cannot be trusted with an answer. Any
// other goes back to the client as OAuth says.
type authorizeProblem struct {
	page        string
	code        string
	description string
}

func (a *App) readAuthorize(r *http.Request) (authorizeRequest, *authorizeProblem) {
	form := r.URL.Query()
	if r.Method == http.MethodPost {
		form = r.PostForm
	}
	client, err := a.oauthClient(r.Context(), r, form.Get("client_id"))
	switch {
	case errors.Is(err, errClientLimited):
		return authorizeRequest{}, &authorizeProblem{page: "oauth_busy"}
	case errors.Is(err, errClientUnknown):
		return authorizeRequest{}, &authorizeProblem{page: "oauth_bad_client"}
	case err != nil:
		fmt.Fprintf(os.Stderr, "oauth client: %v\n", err)
		return authorizeRequest{}, &authorizeProblem{page: "oauth_bad_client"}
	}
	request := authorizeRequest{Client: client, RedirectURI: form.Get("redirect_uri"), State: form.Get("state")}
	if request.RedirectURI == "" || !client.RedirectURI(request.RedirectURI) {
		return authorizeRequest{}, &authorizeProblem{page: "oauth_bad_redirect"}
	}
	fail := func(code, description string) (authorizeRequest, *authorizeProblem) {
		return request, &authorizeProblem{code: code, description: description}
	}
	if len(request.State) > oauthStateMaxBytes {
		request.State = ""
		return fail("invalid_request", "state is too long")
	}
	if form.Get("response_type") != "code" {
		return fail("unsupported_response_type", "only the code response type is supported")
	}
	request.Challenge = form.Get("code_challenge")
	if form.Get("code_challenge_method") != oauthChallengeMethodS256 || !store.ValidCodeChallenge(request.Challenge) {
		return fail("invalid_request", "PKCE with code_challenge_method S256 is required")
	}
	scope, ok := store.NormalizeOAuthScope(form.Get("scope"))
	if !ok {
		return fail("invalid_scope", "the scopes are read, write and share")
	}
	request.Scope = scope
	// Without a resource the MCP endpoint is meant: it is the only one.
	request.Resource = form.Get("resource")
	if request.Resource == "" {
		request.Resource = a.mcpResource(r)
	}
	if request.Resource != a.mcpResource(r) {
		return fail("invalid_target", "the resource must be this server's MCP endpoint")
	}
	return request, nil
}

// replyTo sends the browser back to the client with the outcome. iss names
// this server, so a client talking to several can tell whose answer it is
// (RFC 9207).
func (a *App) replyTo(w http.ResponseWriter, r *http.Request, request authorizeRequest, values url.Values, status int) {
	target, err := url.Parse(request.RedirectURI)
	if err != nil {
		a.renderAuthorizeError(w, r, "oauth_bad_redirect")
		return
	}
	query := target.Query()
	for key, value := range values {
		query[key] = value
	}
	if request.State != "" {
		query.Set("state", request.State)
	}
	query.Set("iss", a.baseURL(r))
	target.RawQuery = query.Encode()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, target.String(), status)
}

// oauthConsent is what the consent page shows of a request.
type oauthConsent struct {
	Request authorizeRequest
	Name    string
	// Host is the client's verified home: the host its metadata document was
	// fetched from. Empty for a client that registered itself, whose name is
	// only its claim.
	Host         string
	RedirectShow string
	Loopback     bool
}

func (a *App) handleOAuthAuthorize(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, oauthMaxBody)
		if err := r.ParseForm(); err != nil {
			writePlainError(w, http.StatusBadRequest, "bad request")
			return
		}
	default:
		w.Header().Set("Allow", "GET, POST")
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	request, problem := a.readAuthorize(r)
	if problem != nil && problem.page != "" {
		a.renderAuthorizeError(w, r, problem.page)
		return
	}
	if problem != nil {
		a.replyTo(w, r, request, url.Values{"error": {problem.code}, "error_description": {problem.description}}, http.StatusFound)
		return
	}
	user, sessionID, ok := a.currentUser(r)
	if !ok {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(oauthAuthorizePath+"?"+authorizeQuery(request, r)), http.StatusSeeOther)
		return
	}
	if r.Method == http.MethodGet {
		a.renderConsent(w, r, user, request)
		return
	}
	if !a.checkCSRF(r, sessionID) {
		writePlainError(w, http.StatusForbidden, "invalid csrf token")
		return
	}
	if r.PostForm.Get("decision") != "allow" {
		a.replyTo(w, r, request, url.Values{"error": {"access_denied"}}, http.StatusSeeOther)
		return
	}
	// An app is allowed all it can do or nothing, whatever it asked for: the
	// page offers no choice, and one that sent no scope works all the same.
	// Nothing an app can do deletes anything; making a share link, the one
	// step that reaches outside, is marked as such for the client to confirm.
	code, err := a.db.CreateOAuthCode(r.Context(), store.OAuthAuthorization{
		UserID: user.ID, Client: request.Client, Scope: strings.Join(store.OAuthScopes, " "),
		Resource: request.Resource, RedirectURI: request.RedirectURI, CodeChallenge: request.Challenge,
	}, time.Now().UTC())
	if errors.Is(err, store.ErrNotFound) {
		a.replyTo(w, r, request, url.Values{"error": {"access_denied"}}, http.StatusSeeOther)
		return
	}
	if err != nil {
		a.serverError(w, "oauth code", err)
		return
	}
	a.replyTo(w, r, request, url.Values{"code": {code}}, http.StatusSeeOther)
}

// authorizeQuery writes a request back out as the query it came in as, for the
// sign-in page to return to.
func authorizeQuery(request authorizeRequest, r *http.Request) string {
	values := url.Values{
		"response_type":         {"code"},
		"client_id":             {request.Client.ID},
		"redirect_uri":          {request.RedirectURI},
		"code_challenge":        {request.Challenge},
		"code_challenge_method": {oauthChallengeMethodS256},
		"scope":                 {request.Scope},
		"resource":              {request.Resource},
	}
	if request.State != "" {
		values.Set("state", request.State)
	}
	return values.Encode()
}

func (a *App) renderConsent(w http.ResponseWriter, r *http.Request, user User, request authorizeRequest) {
	consent := oauthConsent{
		Request: request,
		Name:    request.Client.Name,
	}
	if request.Client.Kind == store.OAuthClientDocument {
		if parsed, err := url.Parse(request.Client.ID); err == nil {
			consent.Host = parsed.Host
		}
	}
	if parsed, err := url.Parse(request.RedirectURI); err == nil {
		switch parsed.Scheme {
		case "https":
			consent.RedirectShow = parsed.Host
		case "http":
			consent.RedirectShow, consent.Loopback = parsed.Host, true
		default:
			consent.RedirectShow = parsed.Scheme + ":"
		}
	}
	data := a.basePage(r, user)
	data.Active = ""
	data.OAuth = &consent
	a.renderTemplate(w, r, http.StatusOK, "authorize.html", data)
}

func (a *App) renderAuthorizeError(w http.ResponseWriter, r *http.Request, key string) {
	var data pageData
	if user, _, ok := a.currentUser(r); ok {
		data = a.basePage(r, user)
		data.Active = ""
	}
	data.Error = translate(requestLanguage(r).Locale, key)
	a.renderTemplate(w, r, http.StatusBadRequest, "authorize.html", data)
}

// handleOAuthToken exchanges a code, or a refresh token, for tokens.
func (a *App) handleOAuthToken(w http.ResponseWriter, r *http.Request) {
	if openCORS(w, r, http.MethodPost) {
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	now := time.Now()
	ip := a.clientIP(r)
	if a.limits.authFailures.blocked(ip, now) {
		a.oauthFail(w, http.StatusTooManyRequests, "invalid_request", "too many failed attempts; try again in a minute")
		return
	}
	if !readOAuthForm(w, r) {
		a.oauthFail(w, http.StatusBadRequest, "invalid_request", "send the parameters as a form-encoded body")
		return
	}
	form := r.PostForm
	clientID := form.Get("client_id")
	if clientID == "" {
		a.oauthFail(w, http.StatusUnauthorized, "invalid_client", "client_id is required")
		return
	}
	var tokens store.OAuthTokens
	var err error
	switch form.Get("grant_type") {
	case "authorization_code":
		tokens, err = a.db.ExchangeOAuthCode(r.Context(), store.OAuthExchange{
			Code: form.Get("code"), ClientID: clientID, RedirectURI: form.Get("redirect_uri"),
			Verifier: form.Get("code_verifier"), Resource: form.Get("resource"),
		}, now.UTC())
	case "refresh_token":
		tokens, err = a.db.RefreshOAuthToken(r.Context(), form.Get("refresh_token"), clientID, form.Get("scope"), form.Get("resource"), now.UTC())
	default:
		a.oauthFail(w, http.StatusBadRequest, "unsupported_grant_type", "the grants are authorization_code and refresh_token")
		return
	}
	switch {
	case errors.Is(err, store.ErrOAuthGrant):
		a.limits.authFailures.allow(ip, now)
		a.oauthFail(w, http.StatusBadRequest, "invalid_grant", "the code or refresh token is invalid, expired, used, or was issued to another client")
		return
	case errors.Is(err, store.ErrOAuthScope):
		a.oauthFail(w, http.StatusBadRequest, "invalid_scope", "a refresh cannot add scopes")
		return
	case err != nil:
		fmt.Fprintf(os.Stderr, "oauth token: %v\n", err)
		a.oauthFail(w, http.StatusInternalServerError, "server_error", "")
		return
	}
	w.Header().Set("Pragma", "no-cache")
	a.writeAPI(w, http.StatusOK, map[string]any{
		"access_token":  tokens.AccessToken,
		"token_type":    "Bearer",
		"expires_in":    tokens.ExpiresIn,
		"refresh_token": tokens.RefreshToken,
		"scope":         tokens.Scope,
	})
}

// handleOAuthRevoke ends the grant a token belongs to (RFC 7009). The answer
// is the same whether or not there was anything to end.
func (a *App) handleOAuthRevoke(w http.ResponseWriter, r *http.Request) {
	if openCORS(w, r, http.MethodPost) {
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !readOAuthForm(w, r) {
		a.oauthFail(w, http.StatusBadRequest, "invalid_request", "send the parameters as a form-encoded body")
		return
	}
	if err := a.db.RevokeOAuthToken(r.Context(), r.PostForm.Get("token"), r.PostForm.Get("client_id")); err != nil {
		fmt.Fprintf(os.Stderr, "oauth revoke: %v\n", err)
		a.oauthFail(w, http.StatusServiceUnavailable, "temporarily_unavailable", "")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}
