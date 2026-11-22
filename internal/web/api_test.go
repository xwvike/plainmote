package web

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"plainmote/internal/store"
)

// issueToken creates a personal access token for an account.
func issueToken(t *testing.T, db *store.Store, user User, scope string) string {
	t.Helper()
	secret, _, err := db.CreateAPIToken(context.Background(), user.ID, "test-box", scope, store.TokenLifetime, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return secret
}

type apiCall struct {
	method, path, token string
	body                io.Reader
	header              map[string]string
}

func callAPI(t *testing.T, app *App, call apiCall) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(call.method, "https://cfg.test"+call.path, call.body)
	if call.token != "" {
		request.Header.Set("Authorization", "Bearer "+call.token)
	}
	for key, value := range call.header {
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	app.handler.ServeHTTP(response, request)
	if origin := response.Header().Get("Access-Control-Allow-Origin"); origin != "" {
		t.Fatalf("%s %s answered with CORS for %q", call.method, call.path, origin)
	}
	return response
}

func decodeJSON(t *testing.T, response *httptest.ResponseRecorder, into any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), into); err != nil {
		t.Fatalf("not JSON (%d): %q", response.Code, response.Body.String())
	}
}

// TestCreateAPITokenFromAccountPage tests creating personal access tokens
// directly from /account/devices with CSRF protection, validation, one-time
// reveal, and immediate usability against /api/v1/.
func TestCreateAPITokenFromAccountPage(t *testing.T) {
	db, user, _ := testDatabase(t)
	client := newVersionClient(t, db, user)
	app := client.app

	// Old device-auth endpoints are gone.
	if gone := callAPI(t, app, apiCall{method: http.MethodPost, path: apiPrefix + "device/code"}); gone.Code != http.StatusUnauthorized {
		t.Fatalf("removed device/code should require auth (not be public): %d", gone.Code)
	}
	if gone := client.do(http.MethodGet, "/cli/device", nil); gone.Code != http.StatusNotFound {
		t.Fatalf("removed /cli/device should be 404: %d", gone.Code)
	}

	// Opening the create-token dialog.
	dialog := client.partsPage("/account/devices?new_token=1")
	if !strings.Contains(dialog, `data-part="dialog"`) || !strings.Contains(dialog, `name="action" value="create"`) {
		t.Fatalf("new token dialog missing form: %s", dialog)
	}

	// Missing CSRF is refused.
	forgedReq := httptest.NewRequest(http.MethodPost, "https://cfg.test/account/tokens", strings.NewReader("action=create&name=ci&scope=read&ttl=90d"))
	forgedReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	forgedReq.AddCookie(&http.Cookie{Name: sessionCookie, Value: client.session})
	forged := httptest.NewRecorder()
	app.handler.ServeHTTP(forged, forgedReq)
	if forged.Code != http.StatusForbidden {
		t.Fatalf("missing csrf: %d", forged.Code)
	}

	// Validation errors reopen the dialog with an alert.
	blank := client.do(http.MethodPost, "/account/tokens", url.Values{"action": {"create"}, "name": {"  "}, "scope": {"read"}, "ttl": {"90d"}})
	if blank.Code != http.StatusBadRequest || !strings.Contains(blank.Body.String(), "Enter a name for this token.") {
		t.Fatalf("blank name: %d %s", blank.Code, blank.Body.String())
	}
	badTTL := client.do(http.MethodPost, "/account/tokens", url.Values{"action": {"create"}, "name": {"ci"}, "scope": {"read"}, "ttl": {"10y"}})
	if badTTL.Code != http.StatusBadRequest || !strings.Contains(badTTL.Body.String(), "Choose a valid expiration.") {
		t.Fatalf("bad ttl: %d %s", badTTL.Code, badTTL.Body.String())
	}

	// Creating a never-expiring write token via parts.js returns both updated token panel and one-time reveal dialog.
	form := url.Values{"csrf": {client.csrf}, "action": {"create"}, "name": {"CI Deploy"}, "scope": {"write"}, "ttl": {"never"}}
	req := httptest.NewRequest(http.MethodPost, "https://cfg.test/account/tokens", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-PlainMote-Fragment", "1")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: client.session})
	req.AddCookie(&http.Cookie{Name: csrfCookie, Value: client.csrf})
	created := httptest.NewRecorder()
	app.handler.ServeHTTP(created, req)
	body := created.Body.String()
	if created.Code != http.StatusOK || created.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create token: %d %v", created.Code, created.Header())
	}
	if !strings.Contains(body, `data-part="tokens"`) || !strings.Contains(body, "CI Deploy") || !strings.Contains(body, "Never") {
		t.Fatalf("updated token panel missing created entry: %s", body)
	}
	match := regexp.MustCompile(`pmt_[A-Za-z0-9_-]{20,}`).FindString(body)
	if match == "" {
		t.Fatalf("created token not revealed in dialog: %s", body)
	}

	// The revealed token works immediately against /api/v1/me.
	me := callAPI(t, app, apiCall{method: http.MethodGet, path: apiPrefix + "me", token: match})
	if me.Code != http.StatusOK || !strings.Contains(me.Body.String(), `"login":"alice"`) || !strings.Contains(me.Body.String(), `"name":"CI Deploy"`) || strings.Contains(me.Body.String(), `"expires_at"`) {
		t.Fatalf("me with new token: %d %s", me.Code, me.Body.String())
	}
}

// TestTheResourceAPI covers listing, JSON & multipart creation, reading
// current and historical versions, optional If-Match updates, PATCH metadata,
// share link management, soft delete to trash, and self-revocation.
func TestTheResourceAPI(t *testing.T) {
	db, user, resource := testDatabase(t)
	app := newTestApp(db)
	token := issueToken(t, db, user, store.TokenScopeWrite)
	reader := issueToken(t, db, user, store.TokenScopeRead)
	base := apiPrefix + "resources/" + resource.ID

	listed := callAPI(t, app, apiCall{method: http.MethodGet, path: apiPrefix + "resources", token: token})
	var list struct{ Resources []apiResource }
	decodeJSON(t, listed, &list)
	if listed.Code != http.StatusOK || len(list.Resources) != 1 || list.Resources[0].ID != resource.ID || !list.Resources[0].Editable || list.Resources[0].Version != 1 {
		t.Fatalf("listing: %d %+v", listed.Code, list)
	}
	resolved := callAPI(t, app, apiCall{method: http.MethodGet, path: apiPrefix + "resources?ref=" + resource.ID[:8], token: reader})
	decodeJSON(t, resolved, &list)
	if len(list.Resources) != 1 || list.Resources[0].ID != resource.ID {
		t.Fatalf("resolve by prefix: %+v", list)
	}

	read := callAPI(t, app, apiCall{method: http.MethodGet, path: base + "/content", token: reader})
	if read.Code != http.StatusOK || read.Body.String() != "answer=42\n" || read.Header().Get("ETag") != `"v1"` ||
		read.Header().Get("X-PlainMote-Resource-Version") != "1" || read.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("read: %d %q %v", read.Code, read.Body.String(), read.Header())
	}

	put := func(tok, match, body string) *httptest.ResponseRecorder {
		header := map[string]string{"Content-Type": "application/octet-stream"}
		if match != "" {
			header["If-Match"] = match
		}
		return callAPI(t, app, apiCall{method: http.MethodPut, path: base + "/content", token: tok, body: strings.NewReader(body), header: header})
	}
	if denied := put(reader, "", "answer=0\n"); denied.Code != http.StatusForbidden || !strings.Contains(denied.Body.String(), "read_only") {
		t.Fatalf("a read-only token saves nothing: %d", denied.Code)
	}
	// If-Match is optional: omitting it saves cleanly as the next version.
	savedNoMatch := put(token, "", "answer=43\n")
	if savedNoMatch.Code != http.StatusOK || !strings.Contains(savedNoMatch.Body.String(), `"version":2`) || !strings.Contains(savedNoMatch.Body.String(), `"new_version":true`) {
		t.Fatalf("save without If-Match: %d %s", savedNoMatch.Code, savedNoMatch.Body.String())
	}
	// Matching If-Match still works for optimistic locking.
	savedWithMatch := put(token, `"v2"`, "answer=44\n")
	if savedWithMatch.Code != http.StatusOK || !strings.Contains(savedWithMatch.Body.String(), `"version":3`) {
		t.Fatalf("save with If-Match: %d %s", savedWithMatch.Code, savedWithMatch.Body.String())
	}
	// Stale If-Match returns 412 conflict.
	stale := put(token, `"v1"`, "answer=45\n")
	var conflict apiError
	decodeJSON(t, stale, &conflict)
	if stale.Code != http.StatusPreconditionFailed || conflict.Error != "conflict" || conflict.CurrentVersion != 3 {
		t.Fatalf("a stale save: %d %+v", stale.Code, conflict)
	}
	if forced := put(token, "*", "answer=45\n"); forced.Code != http.StatusOK || !strings.Contains(forced.Body.String(), `"version":4`) {
		t.Fatalf("saving over with *: %d %s", forced.Code, forced.Body.String())
	}

	// List versions and read an earlier version's bytes.
	versionsRes := callAPI(t, app, apiCall{method: http.MethodGet, path: base + "/versions", token: reader})
	var versions struct {
		CurrentVersion int `json:"current_version"`
		Versions       []struct {
			Version int  `json:"version"`
			Current bool `json:"current"`
		} `json:"versions"`
	}
	decodeJSON(t, versionsRes, &versions)
	if versionsRes.Code != http.StatusOK || versions.CurrentVersion != 4 || len(versions.Versions) != 4 || !versions.Versions[0].Current {
		t.Fatalf("versions: %d %+v", versionsRes.Code, versions)
	}
	v1 := callAPI(t, app, apiCall{method: http.MethodGet, path: base + "/content?version=1", token: reader})
	if v1.Code != http.StatusOK || v1.Body.String() != "answer=42\n" || v1.Header().Get("X-PlainMote-Resource-Version") != "1" {
		t.Fatalf("read v1: %d %q", v1.Code, v1.Body.String())
	}

	// Create resource with JSON.
	createdJSON := callAPI(t, app, apiCall{
		method: http.MethodPost, path: apiPrefix + "resources", token: token,
		body:   strings.NewReader(`{"name":"Nginx Prod","filename":"nginx.conf","content":"listen 80;\n","tags":["prod","edge"]}`),
		header: map[string]string{"Content-Type": "application/json"},
	})
	var madeJSON apiResource
	decodeJSON(t, createdJSON, &madeJSON)
	if createdJSON.Code != http.StatusCreated || madeJSON.Name != "Nginx Prod" || madeJSON.Filename != "nginx.conf" || len(madeJSON.Tags) != 2 {
		t.Fatalf("create JSON: %d %+v", createdJSON.Code, madeJSON)
	}

	// PATCH metadata (name, filename, tags).
	patched := callAPI(t, app, apiCall{
		method: http.MethodPatch, path: apiPrefix + "resources/" + madeJSON.ID, token: token,
		body:   strings.NewReader(`{"name":"Nginx Edge","filename":"edge.conf","tags":["edge"]}`),
		header: map[string]string{"Content-Type": "application/json"},
	})
	var updated apiResource
	decodeJSON(t, patched, &updated)
	if patched.Code != http.StatusOK || updated.Name != "Nginx Edge" || updated.Filename != "edge.conf" || len(updated.Tags) != 1 || updated.Tags[0] != "edge" {
		t.Fatalf("patch resource: %d %+v", patched.Code, updated)
	}

	// Share links: create (permanent + timed), list, fetch raw content, revoke.
	createdLink := callAPI(t, app, apiCall{
		method: http.MethodPost, path: apiPrefix + "resources/" + madeJSON.ID + "/links", token: token,
		body:   strings.NewReader(`{"name":"node-1","expires_in":"7d","max_uses":5}`),
		header: map[string]string{"Content-Type": "application/json"},
	})
	var link apiLink
	decodeJSON(t, createdLink, &link)
	if createdLink.Code != http.StatusCreated || link.ID == "" || !strings.HasPrefix(link.URL, "https://cfg.test/d/") || link.Name != "node-1" || link.MaxUses != 5 || link.ExpiresAt == nil {
		t.Fatalf("create link: %d %+v", createdLink.Code, link)
	}
	// Permanent share link (empty body).
	permLinkRes := callAPI(t, app, apiCall{method: http.MethodPost, path: apiPrefix + "resources/" + madeJSON.ID + "/links", token: token})
	var permLink apiLink
	decodeJSON(t, permLinkRes, &permLink)
	if permLinkRes.Code != http.StatusCreated || permLink.ExpiresAt != nil || permLink.URL == "" {
		t.Fatalf("permanent link: %d %+v", permLinkRes.Code, permLink)
	}
	linksListRes := callAPI(t, app, apiCall{method: http.MethodGet, path: apiPrefix + "resources/" + madeJSON.ID + "/links", token: reader})
	var linksList struct{ Links []apiLink }
	decodeJSON(t, linksListRes, &linksList)
	if linksListRes.Code != http.StatusOK || len(linksList.Links) != 2 {
		t.Fatalf("list links: %d %+v", linksListRes.Code, linksList)
	}
	delivered := httptest.NewRecorder()
	app.handler.ServeHTTP(delivered, httptest.NewRequest(http.MethodGet, link.URL, nil))
	if delivered.Code != http.StatusOK || delivered.Body.String() != "listen 80;\n" {
		t.Fatalf("share link fetch: %d %q", delivered.Code, delivered.Body.String())
	}
	if revoked := callAPI(t, app, apiCall{method: http.MethodDelete, path: apiPrefix + "resources/" + madeJSON.ID + "/links/" + link.ID, token: token}); revoked.Code != http.StatusNoContent {
		t.Fatalf("revoke link: %d %s", revoked.Code, revoked.Body.String())
	}
	afterRevoke := httptest.NewRecorder()
	app.handler.ServeHTTP(afterRevoke, httptest.NewRequest(http.MethodGet, link.URL, nil))
	if afterRevoke.Code == http.StatusOK {
		t.Fatalf("revoked link still served content: %d", afterRevoke.Code)
	}

	// DELETE moves resource to trash and revokes remaining links.
	deleted := callAPI(t, app, apiCall{method: http.MethodDelete, path: apiPrefix + "resources/" + madeJSON.ID, token: token})
	var delResult struct {
		ID           string `json:"id"`
		Trashed      bool   `json:"trashed"`
		RevokedLinks int    `json:"revoked_links"`
	}
	decodeJSON(t, deleted, &delResult)
	if deleted.Code != http.StatusOK || !delResult.Trashed || delResult.RevokedLinks != 1 {
		t.Fatalf("delete resource: %d %+v", deleted.Code, delResult)
	}
	if gone := callAPI(t, app, apiCall{method: http.MethodGet, path: apiPrefix + "resources/" + madeJSON.ID, token: token}); gone.Code != http.StatusNotFound {
		t.Fatalf("trashed resource still reachable via API: %d", gone.Code)
	}

	// Create resource with multipart/form-data (file upload).
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	_ = writer.WriteField("name", "app log")
	_ = writer.WriteField("filename", "app.log")
	part, _ := writer.CreateFormFile("content", "app.log")
	_, _ = part.Write([]byte("line 1\nline 2\n"))
	_ = writer.Close()
	created := callAPI(t, app, apiCall{method: http.MethodPost, path: apiPrefix + "resources", token: token, body: &form,
		header: map[string]string{"Content-Type": writer.FormDataContentType()}})
	var made apiResource
	decodeJSON(t, created, &made)
	if created.Code != http.StatusCreated || made.Name != "app log" || made.Filename != "app.log" || made.URL != "https://cfg.test/resources/"+made.ID {
		t.Fatalf("create multipart: %d %+v", created.Code, made)
	}

	if out := callAPI(t, app, apiCall{method: http.MethodDelete, path: apiPrefix + "token", token: token}); out.Code != http.StatusNoContent {
		t.Fatalf("revoke self token: %d", out.Code)
	}
	if after := callAPI(t, app, apiCall{method: http.MethodGet, path: apiPrefix + "me", token: token}); after.Code != http.StatusUnauthorized {
		t.Fatalf("a revoked token still works: %d", after.Code)
	}
}

func TestQuickSharesThroughTheAPI(t *testing.T) {
	db, user, _ := testDatabase(t)
	app := newTestApp(db)
	token := issueToken(t, db, user, store.TokenScopeWrite)

	// JSON quick share.
	createdJSON := callAPI(t, app, apiCall{
		method: http.MethodPost, path: apiPrefix + "quick-shares", token: token,
		body:   strings.NewReader(`{"filename":"hello.txt","ttl":"1h","content":"hello json\n"}`),
		header: map[string]string{"Content-Type": "application/json"},
	})
	var madeJSON struct {
		apiResource
		ShareURL string `json:"share_url"`
	}
	decodeJSON(t, createdJSON, &madeJSON)
	if createdJSON.Code != http.StatusCreated || !strings.HasPrefix(madeJSON.ShareURL, "https://cfg.test/d/") {
		t.Fatalf("json quick share: %d %+v", createdJSON.Code, madeJSON)
	}

	share := func(ttl string, content string) *httptest.ResponseRecorder {
		var form bytes.Buffer
		writer := multipart.NewWriter(&form)
		_ = writer.WriteField("filename", "app.log")
		if ttl != "" {
			_ = writer.WriteField("ttl", ttl)
		}
		part, _ := writer.CreateFormFile("content", "content")
		_, _ = part.Write([]byte(content))
		_ = writer.Close()
		return callAPI(t, app, apiCall{method: http.MethodPost, path: apiPrefix + "quick-shares", token: token, body: &form,
			header: map[string]string{"Content-Type": writer.FormDataContentType()}})
	}
	if got := share("2h", "x\n"); got.Code != http.StatusBadRequest || !strings.Contains(got.Body.String(), "invalid_ttl") {
		t.Fatalf("an unknown lifetime: %d %s", got.Code, got.Body.String())
	}
	created := share("1d", "line one\n")
	var made struct {
		apiResource
		ShareURL string `json:"share_url"`
	}
	decodeJSON(t, created, &made)
	if created.Code != http.StatusCreated || made.ExpiresAt == nil || made.Editable || !strings.HasPrefix(made.ShareURL, "https://cfg.test/d/") ||
		made.ExpiresAt.Sub(time.Now()) < 23*time.Hour {
		t.Fatalf("created: %d %+v", created.Code, made)
	}
	delivered := httptest.NewRecorder()
	app.handler.ServeHTTP(delivered, httptest.NewRequest(http.MethodGet, made.ShareURL, nil))
	if delivered.Code != http.StatusOK || delivered.Body.String() != "line one\n" {
		t.Fatalf("the link: %d %q", delivered.Code, delivered.Body.String())
	}
	base := apiPrefix + "resources/" + made.ID
	if got := callAPI(t, app, apiCall{method: http.MethodGet, path: base + "/content", token: token}); got.Code != http.StatusOK || got.Body.String() != "line one\n" {
		t.Fatalf("read: %d", got.Code)
	}
	saved := callAPI(t, app, apiCall{method: http.MethodPut, path: base + "/content", token: token,
		body: strings.NewReader("changed\n")})
	if saved.Code != http.StatusBadRequest || !strings.Contains(saved.Body.String(), "refused") {
		t.Fatalf("a save to a quick share: %d %s", saved.Code, saved.Body.String())
	}

	app.cfg.AnonymousEnabled = false
	if got := share("", "x\n"); got.Code != http.StatusNotFound {
		t.Fatalf("quick shares where they are not offered: %d", got.Code)
	}
}

func TestTheAPIOnlyTakesBearerTokens(t *testing.T) {
	db, user, _ := testDatabase(t)
	client := newVersionClient(t, db, user)
	token := issueToken(t, db, user, store.TokenScopeWrite)

	if cookie := client.do(http.MethodGet, apiPrefix+"me", nil); cookie.Code != http.StatusUnauthorized {
		t.Fatalf("a session cookie called the API: %d", cookie.Code)
	}
	for _, call := range []apiCall{
		{method: http.MethodGet, path: apiPrefix + "me?token=" + token},
		{method: http.MethodGet, path: apiPrefix + "me?access_token=" + token},
		{method: http.MethodGet, path: apiPrefix + "me", header: map[string]string{"Authorization": token}},
		{method: http.MethodGet, path: apiPrefix + "me", header: map[string]string{"Authorization": "Basic " + token}},
		{method: http.MethodGet, path: apiPrefix + "me", token: "pmt_nonsense"},
	} {
		if response := callAPI(t, client.app, call); response.Code != http.StatusUnauthorized || response.Header().Get("WWW-Authenticate") == "" {
			t.Fatalf("%+v answered %d", call, response.Code)
		}
	}
	preflight := callAPI(t, client.app, apiCall{method: http.MethodOptions, path: apiPrefix + "me",
		header: map[string]string{"Origin": "https://evil.example", "Access-Control-Request-Method": "GET", "Access-Control-Request-Headers": "authorization"}})
	if preflight.Code == http.StatusOK || preflight.Code == http.StatusNoContent {
		t.Fatalf("a preflight was let through: %d", preflight.Code)
	}
}

func TestAnotherAccountsTokenReachesNothing(t *testing.T) {
	db, owner, resource := testDatabase(t)
	ctx := context.Background()
	if err := db.UpdateResource(ctx, owner.ID, resource.ID, "Owner Secret Name", "owner-secret.conf", []byte("OWNER-SECRET\n"), "utf-8"); err != nil {
		t.Fatal(err)
	}
	ownerLink, err := db.CreateShare(ctx, owner.ID, resource.ID, "owner link", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	intruder, err := db.UpsertUser(ctx, "200", "mallory", "", "")
	if err != nil {
		t.Fatal(err)
	}
	app := newTestApp(db)
	token := issueToken(t, db, intruder, store.TokenScopeWrite)

	base := apiPrefix + "resources/" + resource.ID
	for _, call := range []apiCall{
		{method: http.MethodGet, path: base},
		{method: http.MethodPatch, path: base, body: strings.NewReader(`{"name":"pwned"}`), header: map[string]string{"Content-Type": "application/json"}},
		{method: http.MethodDelete, path: base},
		{method: http.MethodGet, path: base + "/content"},
		{method: http.MethodPut, path: base + "/content", body: strings.NewReader("MALLORY\n")},
		{method: http.MethodGet, path: base + "/versions"},
		{method: http.MethodGet, path: base + "/links"},
		{method: http.MethodPost, path: base + "/links", body: strings.NewReader(`{"name":"evil"}`), header: map[string]string{"Content-Type": "application/json"}},
		{method: http.MethodDelete, path: base + "/links/" + ownerLink.ID},
		{method: http.MethodGet, path: apiPrefix + "resources?ref=" + resource.ID},
		{method: http.MethodGet, path: apiPrefix + "resources?ref=" + resource.ID[:8]},
		{method: http.MethodGet, path: apiPrefix + "resources?ref=owner-secret.conf"},
		{method: http.MethodGet, path: apiPrefix + "resources?ref=Owner+Secret+Name"},
		{method: http.MethodGet, path: apiPrefix + "resources?q=secret"},
		{method: http.MethodGet, path: apiPrefix + "resources"},
	} {
		call.token = token
		response := callAPI(t, app, call)
		for _, secret := range []string{"OWNER-SECRET", "Owner Secret Name", "owner-secret", resource.ID, ownerLink.Token} {
			if strings.Contains(response.Body.String(), secret) {
				t.Errorf("%s %s answered %d with %q", call.method, call.path, response.Code, secret)
			}
		}
		if (call.method == http.MethodPut || call.method == http.MethodPatch || call.method == http.MethodDelete || call.method == http.MethodPost) && response.Code != http.StatusNotFound {
			t.Errorf("%s %s on another account's resource answered %d", call.method, call.path, response.Code)
		}
	}
	current, err := db.ResourceForOwner(ctx, owner.ID, resource.ID)
	if err != nil || current.Version != 2 || current.Name != "Owner Secret Name" {
		t.Fatalf("the owner's resource changed: %+v %v", current, err)
	}
}

func TestRevokingADeviceFromTheAccountPage(t *testing.T) {
	db, user, _ := testDatabase(t)
	client := newVersionClient(t, db, user)
	token := issueToken(t, db, user, store.TokenScopeRead)
	tokens, err := db.ListAPITokens(context.Background(), user.ID, time.Now().UTC())
	if err != nil || len(tokens) != 1 {
		t.Fatalf("tokens: %v %v", tokens, err)
	}
	dialog := client.partsPage("/account/devices?revoke=" + tokens[0].ID)
	if !strings.Contains(dialog, `data-part="dialog"`) || !strings.Contains(dialog, `name="token" value="`+tokens[0].ID+`"`) {
		t.Fatal("the revoke link opens its dialog")
	}
	if revoked := client.do(http.MethodPost, "/account/tokens", url.Values{"action": {"revoke"}, "token": {tokens[0].ID}}); revoked.Code != http.StatusSeeOther {
		t.Fatalf("revoke: %d", revoked.Code)
	}
	if after := callAPI(t, client.app, apiCall{method: http.MethodGet, path: apiPrefix + "me", token: token}); after.Code != http.StatusUnauthorized {
		t.Fatalf("a revoked token still works: %d", after.Code)
	}
	// Another account's token id revokes nothing.
	other, err := db.UpsertUser(context.Background(), "300", "other", "", "")
	if err != nil {
		t.Fatal(err)
	}
	foreign := issueToken(t, db, other, store.TokenScopeRead)
	theirs, _ := db.ListAPITokens(context.Background(), other.ID, time.Now().UTC())
	client.do(http.MethodPost, "/account/tokens", url.Values{"action": {"revoke"}, "token": {theirs[0].ID}})
	if still := callAPI(t, client.app, apiCall{method: http.MethodGet, path: apiPrefix + "me", token: foreign}); still.Code != http.StatusOK {
		t.Fatalf("one account revoked another's token: %d", still.Code)
	}
}

func TestAPIAuthorizationMatrix(t *testing.T) {
	db, owner, resource := testDatabase(t)
	ctx := context.Background()
	other, err := db.UpsertUser(ctx, "200", "mallory", "", "")
	if err != nil {
		t.Fatal(err)
	}
	client := newVersionClient(t, db, owner)
	app := client.app
	reader := issueToken(t, db, owner, store.TokenScopeRead)
	writer := issueToken(t, db, owner, store.TokenScopeWrite)
	foreign := issueToken(t, db, other, store.TokenScopeWrite)
	revoked := issueToken(t, db, owner, store.TokenScopeWrite)
	if gone := callAPI(t, app, apiCall{method: http.MethodDelete, path: apiPrefix + "token", token: revoked}); gone.Code != http.StatusNoContent {
		t.Fatal("could not revoke")
	}

	if _, err := db.CreateKeyring(ctx, owner.ID, store.Keyring{KDF: store.KeyringKDF, Iterations: 600000, Salt: bytes.Repeat([]byte{1}, 16),
		WrappedByPassword: bytes.Repeat([]byte{2}, 60), WrappedByRecovery: bytes.Repeat([]byte{3}, 60)}, time.Now()); err != nil {
		t.Fatal(err)
	}

	type endpoint struct {
		name, method, path string
		write              bool
		body               func() (io.Reader, map[string]string)
	}
	multipartBody := func() (io.Reader, map[string]string) {
		var form bytes.Buffer
		writer := multipart.NewWriter(&form)
		part, _ := writer.CreateFormFile("content", "x")
		_, _ = part.Write([]byte("new\n"))
		_ = writer.Close()
		return &form, map[string]string{"Content-Type": writer.FormDataContentType()}
	}
	base := apiPrefix + "resources/" + resource.ID
	endpoints := []endpoint{
		{name: "me", method: http.MethodGet, path: apiPrefix + "me"},
		{name: "keyring", method: http.MethodGet, path: apiPrefix + "keyring"},
		{name: "list", method: http.MethodGet, path: apiPrefix + "resources"},
		{name: "resolve", method: http.MethodGet, path: apiPrefix + "resources?ref=" + resource.ID},
		{name: "get", method: http.MethodGet, path: base},
		{name: "patch", method: http.MethodPatch, path: base, write: true, body: func() (io.Reader, map[string]string) {
			return strings.NewReader(`{"name":"Example"}`), map[string]string{"Content-Type": "application/json"}
		}},
		{name: "read", method: http.MethodGet, path: base + "/content"},
		{name: "versions", method: http.MethodGet, path: base + "/versions"},
		{name: "links", method: http.MethodGet, path: base + "/links"},
		{name: "create-link", method: http.MethodPost, path: base + "/links", write: true, body: func() (io.Reader, map[string]string) {
			return strings.NewReader(`{"name":"matrix"}`), map[string]string{"Content-Type": "application/json"}
		}},
		{name: "save", method: http.MethodPut, path: base + "/content", write: true, body: func() (io.Reader, map[string]string) {
			return strings.NewReader("saved " + time.Now().String() + "\n"), nil
		}},
		{name: "create", method: http.MethodPost, path: apiPrefix + "resources", write: true, body: multipartBody},
		{name: "quick-share", method: http.MethodPost, path: apiPrefix + "quick-shares", write: true, body: multipartBody},
	}
	call := func(e endpoint, token string, cookie bool) *httptest.ResponseRecorder {
		var body io.Reader
		var header map[string]string
		if e.body != nil {
			body, header = e.body()
		}
		if !cookie {
			return callAPI(t, app, apiCall{method: e.method, path: e.path, token: token, body: body, header: header})
		}
		request := httptest.NewRequest(e.method, "https://cfg.test"+e.path, body)
		for key, value := range header {
			request.Header.Set(key, value)
		}
		request.AddCookie(&http.Cookie{Name: sessionCookie, Value: client.session})
		request.AddCookie(&http.Cookie{Name: csrfCookie, Value: client.csrf})
		response := httptest.NewRecorder()
		app.handler.ServeHTTP(response, request)
		return response
	}

	for _, e := range endpoints {
		for label, token := range map[string]string{"none": "", "garbage": "pmt_" + strings.Repeat("A", 43), "revoked": revoked} {
			if got := call(e, token, false); got.Code != http.StatusUnauthorized {
				t.Errorf("%s with %s: %d", e.name, label, got.Code)
			}
		}
		if got := call(e, "", true); got.Code != http.StatusUnauthorized {
			t.Errorf("%s with a session cookie: %d", e.name, got.Code)
		}
		got := call(e, reader, false)
		if e.write && got.Code != http.StatusForbidden {
			t.Errorf("%s with a read token: %d", e.name, got.Code)
		}
		if !e.write && got.Code != http.StatusOK {
			t.Errorf("%s with a read token: %d", e.name, got.Code)
		}
		if got := call(e, writer, false); got.Code != http.StatusOK && got.Code != http.StatusCreated {
			t.Errorf("%s with the owner's write token: %d %s", e.name, got.Code, got.Body.String())
		}
		got = call(e, foreign, false)
		switch e.name {
		case "me", "create", "quick-share":
			if got.Code != http.StatusOK && got.Code != http.StatusCreated || strings.Contains(got.Body.String(), "alice") {
				t.Errorf("%s with another account's token: %d %s", e.name, got.Code, got.Body.String())
			}
		case "list", "resolve":
			if got.Code != http.StatusOK || strings.Contains(got.Body.String(), resource.ID) {
				t.Errorf("%s with another account's token: %d %s", e.name, got.Code, got.Body.String())
			}
		default:
			if got.Code != http.StatusNotFound {
				t.Errorf("%s with another account's token: %d", e.name, got.Code)
			}
		}
	}

	if theirs, _ := db.APIResources(ctx, owner.ID, "", ""); len(theirs) != 3 {
		t.Fatalf("the owner has %d resources, want their own three", len(theirs))
	}

	// A token opens no web page and changes nothing through a form.
	for _, page := range []struct{ method, path string }{
		{http.MethodGet, "/account"}, {http.MethodGet, "/resources/"}, {http.MethodGet, "/resources/" + resource.ID},
		{http.MethodPost, "/account/delete"}, {http.MethodPost, "/account/tokens"},
		{http.MethodPost, "/resources/" + resource.ID}, {http.MethodPost, "/resources/" + resource.ID + "/share"},
	} {
		got := callAPI(t, app, apiCall{method: page.method, path: page.path, token: writer})
		if got.Code != http.StatusSeeOther || !strings.HasPrefix(got.Header().Get("Location"), "/login") {
			t.Errorf("%s %s with a token: %d %q", page.method, page.path, got.Code, got.Header().Get("Location"))
		}
	}
	if _, err := db.Account(ctx, owner.ID); err != nil {
		t.Fatal("the account went away")
	}

	// An unknown endpoint says nothing before the caller is known.
	if got := callAPI(t, app, apiCall{method: http.MethodGet, path: apiPrefix + "admin/users"}); got.Code != http.StatusUnauthorized {
		t.Fatalf("an unknown endpoint without a token: %d", got.Code)
	}
	if got := callAPI(t, app, apiCall{method: http.MethodGet, path: apiPrefix + "admin/users", token: writer}); got.Code != http.StatusNotFound {
		t.Fatalf("an unknown endpoint with a token: %d", got.Code)
	}
}
