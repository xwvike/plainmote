package web

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"plainmote/internal/store"
)

type adminClient struct {
	t       *testing.T
	app     *App
	private ed25519.PrivateKey
}

func newAdminClient(t *testing.T, db *store.Store) *adminClient {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	app := newTestApp(db)
	app.cfg.AdminKeys = []ed25519.PublicKey{public}
	app.cfg.AdminOrigins = []string{"http://localhost:5173"}
	app.cfg.Version, app.cfg.Revision = "abc123", "abc123def"
	app.admin = newAdminGate(app.cfg.AdminKeys, app.cfg.AdminOrigins)
	app.handler = app.routes()
	return &adminClient{t: t, app: app, private: private}
}

// signed builds a request the way docs/admin-api.md describes.
func (c *adminClient) signed(method, target string, body []byte, at time.Time, nonce string) *http.Request {
	request := httptest.NewRequest(method, "https://cfg.test"+target, bytes.NewReader(body))
	timestamp := strconv.FormatInt(at.Unix(), 10)
	if nonce == "" {
		raw := make([]byte, 18)
		_, _ = rand.Read(raw)
		nonce = base64.RawURLEncoding.EncodeToString(raw)
	}
	signature := ed25519.Sign(c.private, []byte(AdminSigningString(method, request.URL.RequestURI(), timestamp, nonce, body)))
	request.Header.Set("X-PlainMote-Key", AdminKeyID(c.private.Public().(ed25519.PublicKey)))
	request.Header.Set("X-PlainMote-Timestamp", timestamp)
	request.Header.Set("X-PlainMote-Nonce", nonce)
	request.Header.Set("X-PlainMote-Signature", base64.RawURLEncoding.EncodeToString(signature))
	return request
}

func (c *adminClient) serve(request *http.Request) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	c.app.handler.ServeHTTP(response, request)
	return response
}

func (c *adminClient) call(method, target string, body any) (int, map[string]any, string) {
	c.t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	response := c.serve(c.signed(method, target, raw, time.Now(), ""))
	var decoded map[string]any
	_ = json.Unmarshal(response.Body.Bytes(), &decoded)
	return response.Code, decoded, response.Body.String()
}

func TestAdminInterfaceIsAbsentWithoutKeys(t *testing.T) {
	db, _, _ := testDatabase(t)
	app := newTestApp(db)
	response := httptest.NewRecorder()
	app.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://cfg.test/_admin/v1/overview", nil))
	if response.Code != http.StatusNotFound || strings.Contains(response.Body.String(), "{") {
		t.Fatalf("with no key configured the admin interface does not exist: %d %q", response.Code, response.Body.String())
	}
}

// TestAdminRefusalsLookLikeNothing: every way of failing to authenticate is
// answered as a path that does not exist, and a captured request cannot be
// replayed.
func TestAdminRefusalsLookLikeNothing(t *testing.T) {
	db, _, _ := testDatabase(t)
	client := newAdminClient(t, db)
	now := time.Now()

	if response := client.serve(httptest.NewRequest(http.MethodGet, "https://cfg.test/_admin/v1/overview", nil)); response.Code != http.StatusNotFound {
		t.Fatalf("unsigned: %d", response.Code)
	}
	stale := client.signed(http.MethodGet, "/_admin/v1/overview", nil, now.Add(-10*time.Minute), "")
	if response := client.serve(stale); response.Code != http.StatusNotFound {
		t.Fatalf("stale timestamp: %d", response.Code)
	}
	tampered := client.signed(http.MethodGet, "/_admin/v1/overview", nil, now, "")
	tampered.URL.RawQuery = "extra=1"
	if response := client.serve(tampered); response.Code != http.StatusNotFound {
		t.Fatalf("a request changed after signing: %d", response.Code)
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	stranger := &adminClient{t: t, app: client.app, private: other}
	if response := client.serve(stranger.signed(http.MethodGet, "/_admin/v1/overview", nil, now, "")); response.Code != http.StatusNotFound {
		t.Fatalf("unknown key: %d", response.Code)
	}

	nonce := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{7}, 18))
	if response := client.serve(client.signed(http.MethodGet, "/_admin/v1/overview", nil, now, nonce)); response.Code != http.StatusOK {
		t.Fatalf("a good request: %d %s", response.Code, response.Body.String())
	}
	if response := client.serve(client.signed(http.MethodGet, "/_admin/v1/overview", nil, now, nonce)); response.Code != http.StatusNotFound {
		t.Fatalf("a replayed nonce: %d", response.Code)
	}
}

func TestAdminAnswersItsOwnOriginsOnly(t *testing.T) {
	db, _, _ := testDatabase(t)
	client := newAdminClient(t, db)
	preflight := httptest.NewRequest(http.MethodOptions, "https://cfg.test/_admin/v1/overview", nil)
	preflight.Header.Set("Origin", "http://localhost:5173")
	preflight.Header.Set("Access-Control-Request-Method", "GET")
	response := client.serve(preflight)
	if response.Code != http.StatusNoContent || response.Header().Get("Access-Control-Allow-Origin") != "http://localhost:5173" ||
		!strings.Contains(response.Header().Get("Access-Control-Allow-Headers"), "X-PlainMote-Signature") {
		t.Fatalf("an allowed origin gets its preflight: %d %v", response.Code, response.Header())
	}
	preflight.Header.Set("Origin", "https://evil.example")
	if response := client.serve(preflight); response.Code != http.StatusNotFound || response.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("any other origin gets nothing: %d", response.Code)
	}
}

func TestAdminOverviewCarriesNoContent(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	client := newAdminClient(t, db)
	link, err := db.CreateShare(ctx, user.ID, resource.ID, "web-01", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	status, overview, body := client.call(http.MethodGet, "/_admin/v1/overview", nil)
	if status != http.StatusOK || overview["version"] != "abc123" {
		t.Fatalf("overview: %d %s", status, body)
	}
	users := overview["users"].(map[string]any)
	resources := overview["resources"].(map[string]any)
	if users["total"].(float64) != 1 || resources["total"].(float64) != 1 || resources["current_bytes"].(float64) != 10 {
		t.Fatalf("overview counts: %s", body)
	}
	for _, target := range []string{"/_admin/v1/overview", "/_admin/v1/users", "/_admin/v1/users/" + user.ID, "/_admin/v1/resources", "/_admin/v1/resources/" + resource.ID} {
		_, _, body := client.call(http.MethodGet, target, nil)
		if strings.Contains(body, "answer=42") || strings.Contains(body, link.Token) {
			t.Fatalf("%s leaked content or a token: %s", target, body)
		}
	}
	remote, err := db.CreateResource(ctx, user.ID, "upstream", "sub.yaml", nil, "", "https://example.com/private/path?key=secret")
	if err != nil {
		t.Fatal(err)
	}
	_, detail, body := client.call(http.MethodGet, "/_admin/v1/resources/"+remote.ID, nil)
	if detail["origin_host"] != "example.com" || strings.Contains(body, "secret") || strings.Contains(body, "/private/") {
		t.Fatalf("a reference is named by its host alone: %s", body)
	}
}

func TestAdminSuspension(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	client := newAdminClient(t, db)
	link, err := db.CreateShare(ctx, user.ID, resource.ID, "web-01", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	session, _, _, err := db.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	if status, _, _ := client.call(http.MethodPost, "/_admin/v1/users/"+user.ID+"/suspend", map[string]string{}); status != http.StatusBadRequest {
		t.Fatalf("a change without a reason is refused: %d", status)
	}
	status, detail, body := client.call(http.MethodPost, "/_admin/v1/users/"+user.ID+"/suspend", map[string]string{"reason": "spam"})
	if status != http.StatusOK || detail["status"] != "suspended" || detail["suspended_reason"] != "spam" {
		t.Fatalf("suspend: %d %s", status, body)
	}
	if _, _, err := db.SessionUser(ctx, session); err == nil {
		t.Fatal("a suspended account's sessions end")
	}
	if _, _, _, err := db.CreateSession(ctx, user.ID, time.Hour); !errors.Is(err, store.ErrSuspended) {
		t.Fatalf("a suspended account cannot sign in: %v", err)
	}
	fetch := httptest.NewRecorder()
	client.app.handler.ServeHTTP(fetch, httptest.NewRequest(http.MethodGet, "https://cfg.test/d/"+link.Token+"/example.conf", nil))
	if fetch.Code != http.StatusUnauthorized {
		t.Fatalf("a suspended account's links deliver nothing: %d", fetch.Code)
	}
	logs, _ := db.ListAccess(ctx, user.ID, resource.ID, "", 5)
	if len(logs) != 1 || logs[0].Outcome != store.OutcomeSuspended {
		t.Fatalf("the refusal is recorded as suspended: %+v", logs)
	}

	signIn := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "https://cfg.test/auth/github/callback", nil)
	request.Header.Set("Accept-Language", "en")
	client.app.renderSuspended(signIn, request, user.GitHubID)
	if signIn.Code != http.StatusForbidden || !strings.Contains(signIn.Body.String(), "This account has been suspended. Reason: spam") {
		t.Fatalf("the sign-in page says why: %d", signIn.Code)
	}

	if status, _, _ := client.call(http.MethodPost, "/_admin/v1/users/"+user.ID+"/suspend", map[string]string{"reason": "again"}); status != http.StatusConflict {
		t.Fatalf("suspending twice conflicts: %d", status)
	}
	if status, detail, _ := client.call(http.MethodPost, "/_admin/v1/users/"+user.ID+"/unsuspend", map[string]string{"reason": "appeal"}); status != http.StatusOK || detail["status"] != "active" {
		t.Fatalf("unsuspend: %d", status)
	}
	again := httptest.NewRecorder()
	client.app.handler.ServeHTTP(again, httptest.NewRequest(http.MethodGet, "https://cfg.test/d/"+link.Token+"/example.conf", nil))
	if again.Code != http.StatusOK {
		t.Fatalf("links work again: %d", again.Code)
	}
	_, audit, body := client.call(http.MethodGet, "/_admin/v1/audit?target="+user.ID, nil)
	if audit["total"].(float64) != 2 || !strings.Contains(body, `"user.suspend"`) || !strings.Contains(body, `"reason":"spam"`) {
		t.Fatalf("both changes are in the audit: %s", body)
	}
}

func TestAdminTakedownAndLookup(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	client := newAdminClient(t, db)
	link, err := db.CreateShare(ctx, user.ID, resource.ID, "web-01", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}

	status, found, body := client.call(http.MethodPost, "/_admin/v1/lookup", map[string]string{"link": "https://plainmote.link/d/" + link.Token + "/example.conf"})
	if status != http.StatusOK || found["resource"].(map[string]any)["id"] != resource.ID || strings.Contains(body, link.Token) {
		t.Fatalf("a reported address finds its resource, and the token is not echoed: %d %s", status, body)
	}

	status, detail, _ := client.call(http.MethodPost, "/_admin/v1/resources/"+resource.ID+"/takedown", map[string]string{"reason": "malware"})
	if status != http.StatusOK || detail["status"] != "taken_down" {
		t.Fatalf("takedown: %d", status)
	}
	fetch := httptest.NewRecorder()
	client.app.handler.ServeHTTP(fetch, httptest.NewRequest(http.MethodGet, "https://cfg.test/d/"+link.Token+"/example.conf", nil))
	if fetch.Code != http.StatusUnauthorized {
		t.Fatalf("a taken-down resource delivers nothing: %d", fetch.Code)
	}
	logs, _ := db.ListAccess(ctx, user.ID, resource.ID, "", 5)
	if len(logs) != 1 || logs[0].Outcome != store.OutcomeTakenDown {
		t.Fatalf("the refusal is recorded as taken down: %+v", logs)
	}
	if _, err := db.CreateShare(ctx, user.ID, resource.ID, "new", time.Hour, 0); !errors.Is(err, store.ErrTakenDown) {
		t.Fatalf("no new links for a taken-down resource: %v", err)
	}
	owner := newVersionClient(t, db, user)
	page := owner.page("/resources/" + resource.ID)
	if !strings.Contains(page, "taken down by the operator") || !strings.Contains(page, "malware") || strings.Contains(page, "data-share-create") {
		t.Fatal("the owner sees the takedown and its reason, and no way to make a link")
	}

	if status, detail, _ := client.call(http.MethodPost, "/_admin/v1/resources/"+resource.ID+"/restore", map[string]string{"reason": "cleared"}); status != http.StatusOK || detail["status"] != "active" {
		t.Fatalf("restore: %d", status)
	}
	if status, _, _ := client.call(http.MethodPost, "/_admin/v1/links/"+link.ID+"/revoke", map[string]string{"reason": "leaked"}); status != http.StatusOK {
		t.Fatalf("revoke link: %d", status)
	}
	if status, _, _ := client.call(http.MethodDelete, "/_admin/v1/resources/"+resource.ID, map[string]string{"reason": "gone"}); status != http.StatusOK {
		t.Fatalf("delete: %d", status)
	}
	if _, err := db.ResourceForOwner(ctx, user.ID, resource.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("the resource is deleted")
	}
	_, audit, _ := client.call(http.MethodGet, "/_admin/v1/audit", nil)
	if audit["total"].(float64) != 4 {
		t.Fatalf("takedown, restore, revoke and delete are all recorded: %v", audit)
	}
}

func TestAdminTakedownOfAQuickShareDeletesIt(t *testing.T) {
	db, _, _ := testDatabase(t)
	ctx := context.Background()
	client := newAdminClient(t, db)
	paste, _, err := db.CreateAnonymousPaste(ctx, "note.txt", []byte("hello\n"), time.Hour, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	status, result, body := client.call(http.MethodPost, "/_admin/v1/resources/"+paste.ID+"/takedown", map[string]string{"reason": "abuse"})
	if status != http.StatusOK || result["deleted"] != true {
		t.Fatalf("a quick share has no owner to notify, so it is deleted: %d %s", status, body)
	}
	if status, _, _ := client.call(http.MethodGet, "/_admin/v1/resources/"+paste.ID, nil); status != http.StatusNotFound {
		t.Fatalf("it is gone: %d", status)
	}
}

func TestAdminPlans(t *testing.T) {
	db, user, _ := testDatabase(t)
	client := newAdminClient(t, db)
	status, plan, body := client.call(http.MethodPost, "/_admin/v1/plans", map[string]any{"name": "friends", "max_resources": 100, "max_storage": 1 << 30, "reason": "for friends"})
	if status != http.StatusCreated {
		t.Fatalf("create plan: %d %s", status, body)
	}
	planID := plan["id"].(string)
	status, detail, body := client.call(http.MethodPost, "/_admin/v1/users/"+user.ID+"/plans", map[string]any{"plan_id": planID, "reason": "friend"})
	if status != http.StatusOK || detail["storage"].(map[string]any)["limit_bytes"].(float64) != float64(100<<20+1<<30) {
		t.Fatalf("a granted plan adds to the limit: %d %s", status, body)
	}
	_, plans, _ := client.call(http.MethodGet, "/_admin/v1/plans", nil)
	var defaultID string
	for _, item := range plans["items"].([]any) {
		if entry := item.(map[string]any); entry["default"] == true {
			defaultID = entry["id"].(string)
		}
		if strings.Contains(item.(map[string]any)["name"].(string), "anonymous") {
			t.Fatal("the anonymous fuse is not a plan to grant")
		}
	}
	if status, _, _ := client.call(http.MethodDelete, "/_admin/v1/users/"+user.ID+"/plans/"+defaultID, map[string]string{"reason": "no"}); status != http.StatusConflict {
		t.Fatalf("the default plan cannot be taken back: %d", status)
	}
	if status, _, _ := client.call(http.MethodDelete, "/_admin/v1/users/"+user.ID+"/plans/"+planID, map[string]string{"reason": "ended"}); status != http.StatusOK {
		t.Fatalf("a granted plan can: %d", status)
	}
}
