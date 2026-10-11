package web

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"plainmote/internal/store"
)

const testMCPResource = "https://cfg.test/mcp"

func testPKCE() (verifier, challenge string) {
	verifier = strings.Repeat("k", 43) + "-._~"
	digest := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(digest[:])
}

// call makes a request with no cookies, as an MCP client does.
func call(app *App, method, target, contentType, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "https://cfg.test"+target, strings.NewReader(body))
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	response := httptest.NewRecorder()
	app.handler.ServeHTTP(response, request)
	return response
}

func tokenCall(app *App, form url.Values) (*httptest.ResponseRecorder, map[string]any) {
	response := call(app, http.MethodPost, oauthTokenPath, "application/x-www-form-urlencoded", form.Encode())
	var body map[string]any
	_ = json.Unmarshal(response.Body.Bytes(), &body)
	return response, body
}

func registerClient(t *testing.T, app *App, redirect string) string {
	t.Helper()
	response := call(app, http.MethodPost, oauthRegisterPath, "application/json",
		`{"client_name":"Test App","redirect_uris":["`+redirect+`"],"token_endpoint_auth_method":"client_secret_post"}`)
	if response.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", response.Code, response.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["token_endpoint_auth_method"] != "none" || body["client_secret"] != nil {
		t.Fatalf("a client was given a secret: %v", body)
	}
	return body["client_id"].(string)
}

func authorizeQueryFor(clientID, redirect, challenge, scope string) url.Values {
	return url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"},
		"scope": {scope}, "state": {"st-1"}, "resource": {testMCPResource},
	}
}

func TestAuthorizationMetadata(t *testing.T) {
	db, _, _ := testDatabase(t)
	app := newTestApp(db)
	for _, path := range []string{wellKnownResource, wellKnownResource + mcpPath} {
		response := call(app, http.MethodGet, path, "", "")
		var body map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != http.StatusOK {
			t.Fatalf("%s: %d %v", path, response.Code, err)
		}
		if body["resource"] != testMCPResource || body["authorization_servers"].([]any)[0] != "https://cfg.test" {
			t.Fatalf("%s: %v", path, body)
		}
		if response.Header().Get("Access-Control-Allow-Origin") != "*" {
			t.Fatalf("%s is not readable from a browser client", path)
		}
	}
	response := call(app, http.MethodGet, wellKnownAuthServer, "", "")
	var body map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"issuer":                                         "https://cfg.test",
		"authorization_endpoint":                         "https://cfg.test/oauth/authorize",
		"token_endpoint":                                 "https://cfg.test/oauth/token",
		"registration_endpoint":                          "https://cfg.test/oauth/register",
		"client_id_metadata_document_supported":          true,
		"authorization_response_iss_parameter_supported": true,
	} {
		if body[key] != want {
			t.Errorf("%s = %v", key, body[key])
		}
	}
	if methods := body["code_challenge_methods_supported"].([]any); len(methods) != 1 || methods[0] != "S256" {
		t.Errorf("PKCE methods %v", methods)
	}
	if preflight := call(app, http.MethodOptions, oauthTokenPath, "", ""); preflight.Code != http.StatusNoContent ||
		preflight.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("token preflight: %d", preflight.Code)
	}
}

func TestClientRegistration(t *testing.T) {
	db, _, _ := testDatabase(t)
	app := newTestApp(db)
	registerClient(t, app, "http://127.0.0.1:33418/callback")
	for name, body := range map[string]string{
		"plain http":    `{"redirect_uris":["http://evil.test/cb"]}`,
		"no redirect":   `{"client_name":"x"}`,
		"javascript":    `{"redirect_uris":["javascript:alert(1)"]}`,
		"not json":      `redirect_uris=https://a.test/cb`,
		"implicit flow": `{"redirect_uris":["https://a.test/cb"],"response_types":["token"]}`,
	} {
		if response := call(app, http.MethodPost, oauthRegisterPath, "application/json", body); response.Code != http.StatusBadRequest {
			t.Errorf("%s: %d", name, response.Code)
		}
	}
	if response := call(app, http.MethodGet, oauthRegisterPath, "", ""); response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET register: %d", response.Code)
	}
}

// TestMCPAuthorization walks one client from consent to revocation.
func TestMCPAuthorization(t *testing.T) {
	db, alice, _ := testDatabase(t)
	client := signedIn(t, db, alice)
	app := client.app
	redirect := "http://127.0.0.1:33418/callback"
	clientID := registerClient(t, app, redirect)
	verifier, challenge := testPKCE()
	// A desktop app on another port than it registered: loopback ports vary.
	used := "http://127.0.0.1:50123/callback"
	// Asking for less than everything changes nothing: an app is allowed all
	// it can do or nothing.
	query := authorizeQueryFor(clientID, used, challenge, "read")

	// Signed out: to the sign-in page, which comes back here.
	signedOut := call(app, http.MethodGet, oauthAuthorizePath+"?"+query.Encode(), "", "")
	location, _ := url.Parse(signedOut.Header().Get("Location"))
	if signedOut.Code != http.StatusSeeOther || location.Path != "/login" || !strings.HasPrefix(location.Query().Get("next"), oauthAuthorizePath+"?") {
		t.Fatalf("signed out: %d %s", signedOut.Code, signedOut.Header().Get("Location"))
	}

	page := client.do(http.MethodGet, oauthAuthorizePath+"?"+query.Encode(), nil)
	body := page.Body.String()
	if page.Code != http.StatusOK || !strings.Contains(body, "Test App") || !strings.Contains(body, `name="client_id" value="`+clientID+`"`) {
		t.Fatalf("consent page: %d", page.Code)
	}
	if strings.Contains(body, `type="checkbox"`) || !strings.Contains(body, "Create and revoke share links") {
		t.Fatal("the consent page should list everything the app may do, with nothing to tick")
	}
	if !strings.Contains(body, "127.0.0.1:50123") || !strings.Contains(body, "oauth-warn") {
		t.Fatal("the consent page does not say the answer goes to this machine")
	}
	if page.Header().Get("Content-Security-Policy") == "" || !strings.Contains(page.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
		t.Fatal("the consent page can be framed")
	}

	consent := url.Values{}
	for key, values := range query {
		consent[key] = values
	}
	consent.Set("decision", "allow")
	if forged := client.do(http.MethodPost, oauthAuthorizePath, consent); forged.Code != http.StatusForbidden {
		t.Fatalf("a consent without the CSRF token: %d", forged.Code)
	}
	consent.Set("csrf", client.csrf)
	allowed := client.do(http.MethodPost, oauthAuthorizePath, consent)
	back, _ := url.Parse(allowed.Header().Get("Location"))
	if allowed.Code != http.StatusSeeOther || back.Host != "127.0.0.1:50123" || back.Query().Get("state") != "st-1" ||
		back.Query().Get("iss") != "https://cfg.test" || back.Query().Get("code") == "" {
		t.Fatalf("allowed: %d %s", allowed.Code, allowed.Header().Get("Location"))
	}
	code := back.Query().Get("code")

	exchange := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID},
		"redirect_uri": {used}, "code_verifier": {verifier}, "resource": {testMCPResource}}
	if response := call(app, http.MethodPost, oauthTokenPath+"?"+exchange.Encode(), "", ""); response.Code != http.StatusBadRequest {
		t.Fatalf("a code in the query string was taken: %d", response.Code)
	}
	wrong := url.Values{}
	for key, values := range exchange {
		wrong[key] = values
	}
	wrong.Set("code_verifier", strings.Repeat("x", 43))
	if response, body := tokenCall(app, wrong); response.Code != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("a wrong verifier: %d %v", response.Code, body)
	}
	response, tokens := tokenCall(app, exchange)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("exchange: %d %v", response.Code, tokens)
	}
	// Only read was asked for, and everything is granted.
	if tokens["scope"] != "read write share" || tokens["token_type"] != "Bearer" {
		t.Fatalf("tokens %v", tokens)
	}
	access, refresh := tokens["access_token"].(string), tokens["refresh_token"].(string)
	user, granted, err := db.OAuthAccessUser(context.Background(), access, "", time.Now().UTC())
	if err != nil || user.ID != alice.ID || granted.Resource != testMCPResource || !granted.Has(store.OAuthScopeShare) {
		t.Fatalf("the access token reads as %+v %+v %v", user, granted, err)
	}
	// Not a token for the command line's interface.
	request := httptest.NewRequest(http.MethodGet, "https://cfg.test/api/v1/me", nil)
	request.Header.Set("Authorization", "Bearer "+access)
	apiResponse := httptest.NewRecorder()
	app.handler.ServeHTTP(apiResponse, request)
	if apiResponse.Code != http.StatusUnauthorized {
		t.Fatalf("an MCP token reached the CLI API: %d", apiResponse.Code)
	}

	if response, body := tokenCall(app, exchange); response.Code != http.StatusBadRequest || body["error"] != "invalid_grant" {
		t.Fatalf("a code exchanged twice: %d %v", response.Code, body)
	}
	if _, _, err := db.OAuthAccessUser(context.Background(), access, "", time.Now().UTC()); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a replayed code left its tokens working")
	}
	if response, _ := tokenCall(app, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID}}); response.Code != http.StatusBadRequest {
		t.Fatalf("a refresh token outlived its replayed code: %d", response.Code)
	}

	// Again, with a fresh consent, to the authorized apps page and out.
	allowed = client.do(http.MethodPost, oauthAuthorizePath, consent)
	back, _ = url.Parse(allowed.Header().Get("Location"))
	exchange.Set("code", back.Query().Get("code"))
	response, tokens = tokenCall(app, exchange)
	if response.Code != http.StatusOK || tokens["scope"] != "read write share" {
		t.Fatalf("second exchange: %d %v", response.Code, tokens)
	}
	response, refreshed := tokenCall(app, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {tokens["refresh_token"].(string)}, "client_id": {clientID}})
	if response.Code != http.StatusOK || refreshed["refresh_token"] == tokens["refresh_token"] {
		t.Fatalf("refresh: %d %v", response.Code, refreshed)
	}
	devices := client.do(http.MethodGet, accountDevicesPath, nil).Body.String()
	if !strings.Contains(devices, "Test App") || !strings.Contains(devices, "revoke_app=") {
		t.Fatal("the authorized apps page does not list the app")
	}
	if response := call(app, http.MethodPost, oauthRevokePath, "application/x-www-form-urlencoded",
		url.Values{"token": {refreshed["refresh_token"].(string)}, "client_id": {clientID}}.Encode()); response.Code != http.StatusOK {
		t.Fatalf("revoke: %d", response.Code)
	}
	if _, _, err := db.OAuthAccessUser(context.Background(), refreshed["access_token"].(string), "", time.Now().UTC()); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a revoked grant's access token works")
	}
	if devices := client.do(http.MethodGet, accountDevicesPath, nil).Body.String(); strings.Contains(devices, "revoke_app=") {
		t.Fatal("a revoked app is still listed")
	}
}

// A request that cannot be granted is answered here, never by a redirect: a
// client's return address is whatever its registration says, and sending a
// malformed request there at once would make this site a redirector for
// anyone. Only the owner's own decision on the consent page goes back.
func TestAuthorizeAnswersOnlyTrustedReturns(t *testing.T) {
	db, alice, _ := testDatabase(t)
	client := signedIn(t, db, alice)
	app := client.app
	redirect := "https://app.test/cb"
	clientID := registerClient(t, app, redirect)
	_, challenge := testPKCE()

	queries := map[string]url.Values{
		"unknown client":   authorizeQueryFor("pmc_nobody", redirect, challenge, ""),
		"another redirect": authorizeQueryFor(clientID, "https://evil.test/cb", challenge, ""),
		"no redirect":      authorizeQueryFor(clientID, "", challenge, ""),
		"http document":    authorizeQueryFor("http://app.test/client.json", redirect, challenge, ""),
	}
	for name, change := range map[string]func(url.Values){
		"no PKCE":       func(q url.Values) { q.Del("code_challenge") },
		"plain PKCE":    func(q url.Values) { q.Set("code_challenge_method", "plain") },
		"implicit":      func(q url.Values) { q.Set("response_type", "token") },
		"no type":       func(q url.Values) { q.Del("response_type") },
		"unknown scope": func(q url.Values) { q.Set("scope", "read admin") },
		"other target":  func(q url.Values) { q.Set("resource", "https://other.test/mcp") },
		"long state":    func(q url.Values) { q.Set("state", strings.Repeat("s", oauthStateMaxBytes+1)) },
	} {
		query := authorizeQueryFor(clientID, redirect, challenge, "")
		change(query)
		queries[name] = query
	}
	anonymous := func(method, target string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, target, nil)
		response := httptest.NewRecorder()
		app.handler.ServeHTTP(response, request)
		return response
	}
	for name, query := range queries {
		target := oauthAuthorizePath + "?" + query.Encode()
		for who, response := range map[string]*httptest.ResponseRecorder{
			"signed in":  client.do(http.MethodGet, target, nil),
			"signed out": anonymous(http.MethodGet, target),
		} {
			if response.Code != http.StatusBadRequest || response.Header().Get("Location") != "" {
				t.Errorf("%s, %s: %d %s", name, who, response.Code, response.Header().Get("Location"))
			}
		}
	}
	// A request that can be granted sends a visitor to sign in, not anywhere
	// the client named.
	valid := anonymous(http.MethodGet, oauthAuthorizePath+"?"+authorizeQueryFor(clientID, redirect, challenge, "").Encode())
	if location := valid.Header().Get("Location"); valid.Code != http.StatusSeeOther || !strings.HasPrefix(location, "/login?next=") {
		t.Errorf("signed out, valid: %d %s", valid.Code, location)
	}

	deny := authorizeQueryFor(clientID, redirect, challenge, "")
	deny.Set("csrf", client.csrf)
	deny.Set("decision", "deny")
	response := client.do(http.MethodPost, oauthAuthorizePath, deny)
	back, _ := url.Parse(response.Header().Get("Location"))
	if back.Query().Get("error") != "access_denied" || back.Query().Get("code") != "" {
		t.Fatalf("deny: %s", response.Header().Get("Location"))
	}
}

func TestClientMetadataDocuments(t *testing.T) {
	db, alice, _ := testDatabase(t)
	client := signedIn(t, db, alice)
	app := client.app
	id := "https://app.test/oauth/client.json"
	var fetches atomic.Int32
	document := `{"client_id":"` + id + `","client_name":"Doc App","redirect_uris":["https://app.test/cb"]}`
	app.clientDocs = upstreamFunc(func(_ context.Context, rawURL string) ([]byte, string, error) {
		fetches.Add(1)
		if rawURL != id {
			return nil, "", errors.New("unexpected fetch")
		}
		return []byte(document), "application/json", nil
	})
	_, challenge := testPKCE()
	query := authorizeQueryFor(id, "https://app.test/cb", challenge, "")
	for range 2 {
		page := client.do(http.MethodGet, oauthAuthorizePath+"?"+query.Encode(), nil)
		if body := page.Body.String(); page.Code != http.StatusOK || !strings.Contains(body, "Doc App") || !strings.Contains(body, "app.test") {
			t.Fatalf("document client consent: %d", page.Code)
		}
	}
	if fetches.Load() != 1 {
		t.Fatalf("the document was fetched %d times, not kept", fetches.Load())
	}

	// A document that names another client id is not that client's.
	other := "https://app.test/oauth/other.json"
	document = `{"client_id":"https://evil.test/client.json","client_name":"Doc App","redirect_uris":["https://app.test/cb"]}`
	app.clientDocs = upstreamFunc(func(context.Context, string) ([]byte, string, error) {
		return []byte(document), "application/json", nil
	})
	if page := client.do(http.MethodGet, oauthAuthorizePath+"?"+authorizeQueryFor(other, "https://app.test/cb", challenge, "").Encode(), nil); page.Code != http.StatusBadRequest {
		t.Fatalf("a mismatched document: %d", page.Code)
	}
	for _, bad := range []string{"https://app.test", "https://app.test/", "https://app.test/a/../b.json", "https://u:p@app.test/c.json", "https://app.test/c.json#x"} {
		if clientDocumentURL(bad) {
			t.Errorf("%q was taken as a document address", bad)
		}
	}
}

func TestAppRevocationStaysWithinTheAccount(t *testing.T) {
	db, alice, _ := testDatabase(t)
	ctx := context.Background()
	now := time.Now().UTC()
	bob, err := db.UpsertUser(ctx, "200", "bob", "Bob", "")
	if err != nil {
		t.Fatal(err)
	}
	registered, err := db.RegisterOAuthClient(ctx, "Alice's App", "", []string{"https://app.test/cb"}, "", now)
	if err != nil {
		t.Fatal(err)
	}
	verifier, challenge := testPKCE()
	code, err := db.CreateOAuthCode(ctx, store.OAuthAuthorization{UserID: alice.ID, Client: registered, Scope: "read",
		Resource: testMCPResource, RedirectURI: "https://app.test/cb", CodeChallenge: challenge}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExchangeOAuthCode(ctx, store.OAuthExchange{Code: code, ClientID: registered.ID, RedirectURI: "https://app.test/cb", Verifier: verifier}, now); err != nil {
		t.Fatal(err)
	}
	grants, err := db.ListOAuthGrants(ctx, alice.ID, now)
	if err != nil || len(grants) != 1 {
		t.Fatalf("alice's grants %v %v", grants, err)
	}
	bobClient := signedIn(t, db, bob)
	if page := bobClient.do(http.MethodGet, accountDevicesPath+"?revoke_app="+grants[0].ID, nil).Body.String(); strings.Contains(page, "Alice&#39;s App") || strings.Contains(page, "Alice's App") {
		t.Fatal("bob's page shows alice's app")
	}
	bobClient.do(http.MethodPost, accountTokensPath, url.Values{"csrf": {bobClient.csrf}, "action": {"revoke_app"}, "grant": {grants[0].ID}})
	if grants, _ := db.ListOAuthGrants(ctx, alice.ID, now); len(grants) != 1 {
		t.Fatal("bob revoked alice's app")
	}
}

// Alice opens the consent page; bob signs in in the same browser before it is
// sent. The form carries alice's token, which is not bob's session's: nothing
// is granted to anyone.
func TestConsentIsBoundToTheSessionThatSawIt(t *testing.T) {
	db, alice, _ := testDatabase(t)
	aliceClient := signedIn(t, db, alice)
	bob, err := db.UpsertUser(context.Background(), "200", "bob", "Bob", "")
	if err != nil {
		t.Fatal(err)
	}
	bobClient := signedIn(t, db, bob)
	bobClient.app = aliceClient.app
	clientID := registerClient(t, aliceClient.app, "https://app.test/cb")
	_, challenge := testPKCE()
	consent := authorizeQueryFor(clientID, "https://app.test/cb", challenge, "write")
	consent.Set("decision", "allow")
	consent.Set("csrf", aliceClient.csrf)
	response := bobClient.do(http.MethodPost, oauthAuthorizePath, consent)
	if response.Code != http.StatusForbidden || response.Header().Get("Location") != "" {
		t.Fatalf("a consent shown to alice was sent in bob's session: %d %s", response.Code, response.Header().Get("Location"))
	}
	for _, user := range []User{alice, bob} {
		if grants, _ := db.ListOAuthGrants(context.Background(), user.ID, time.Now().UTC()); len(grants) != 0 {
			t.Fatalf("%s got a grant", user.Login)
		}
	}
}
