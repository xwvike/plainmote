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
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"plainmote/internal/store"
)

// issueToken signs a command line in the way the browser would approve it.
func issueToken(t *testing.T, db *store.Store, user User, scope string) string {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	deviceCode, userCode, grant, err := db.CreateDeviceGrant(ctx, store.DeviceRequest{Scope: scope, DeviceName: "test-box", DeviceOS: "linux/amd64"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.DecideDeviceGrant(ctx, user.ID, grant.ID, userCode, true, now); err != nil {
		t.Fatal(err)
	}
	exchange, err := db.ExchangeDeviceCode(ctx, deviceCode, now)
	if err != nil || exchange.State != store.DeviceApproved {
		t.Fatalf("issue token: %+v %v", exchange, err)
	}
	return exchange.Token
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

// TestDeviceSignInThroughTheBrowser follows a command line's sign-in over
// HTTP: the code asked for, typed into the device page, checked, approved,
// and exchanged for a token that calls the API.
func TestDeviceSignInThroughTheBrowser(t *testing.T) {
	db, user, _ := testDatabase(t)
	client := newVersionClient(t, db, user)
	app := client.app

	if bad := callAPI(t, app, apiCall{method: http.MethodPost, path: apiDeviceCode, body: strings.NewReader(`scope=write`),
		header: map[string]string{"Content-Type": "application/x-www-form-urlencoded"}}); bad.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("a form post is not a sign-in request: %d", bad.Code)
	}
	if bad := callAPI(t, app, apiCall{method: http.MethodPost, path: apiDeviceCode, body: strings.NewReader(`{"scope":"admin"}`),
		header: map[string]string{"Content-Type": "application/json"}}); bad.Code != http.StatusBadRequest {
		t.Fatalf("an unknown scope is refused: %d", bad.Code)
	}
	started := callAPI(t, app, apiCall{method: http.MethodPost, path: apiDeviceCode,
		body:   strings.NewReader(`{"scope":"write","device":"mira-mbp","os":"darwin/arm64","version":"abc123"}`),
		header: map[string]string{"Content-Type": "application/json"}})
	var start struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
	}
	decodeJSON(t, started, &start)
	if started.Code != http.StatusOK || start.DeviceCode == "" || !regexp.MustCompile(`^[A-Z2-9]{4}-[A-Z2-9]{4}$`).MatchString(start.UserCode) ||
		start.VerificationURI != "https://cfg.test/cli/device" || start.Interval != store.DevicePollInterval || start.ExpiresIn < 590 {
		t.Fatalf("sign-in start: %d %+v", started.Code, start)
	}

	poll := func() (int, map[string]any) {
		response := callAPI(t, app, apiCall{method: http.MethodPost, path: apiDeviceToken,
			body:   strings.NewReader(`{"device_code":"` + start.DeviceCode + `"}`),
			header: map[string]string{"Content-Type": "application/json"}})
		var body map[string]any
		decodeJSON(t, response, &body)
		return response.Code, body
	}
	if code, body := poll(); code != http.StatusBadRequest || body["error"] != store.DevicePending {
		t.Fatalf("before approval: %d %v", code, body)
	}

	// The page ignores a code in its address: it has to be typed.
	page := client.page(deviceVerifyPath + "?code=" + start.UserCode)
	input := regexp.MustCompile(`<input type="text" name="code"[^>]*>`).FindString(page)
	if input == "" || strings.Contains(input, "value=") {
		t.Fatalf("the device page filled in a code from its address: %q", input)
	}
	if forged := client.do(http.MethodPost, deviceVerifyPath, nil); forged.Code != http.StatusForbidden {
		t.Fatalf("a post without a token is refused: %d", forged.Code)
	}
	confirm := client.do(http.MethodPost, deviceVerifyPath, url.Values{"action": {"lookup"}, "code": {strings.ToLower(start.UserCode)}})
	body := confirm.Body.String()
	for _, want := range []string{start.UserCode, "mira-mbp", "darwin/arm64", "abc123", "192.0.2.1", "alice", `value="approve"`, `value="deny"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("the confirmation is missing %q: %d", want, confirm.Code)
		}
	}
	grantID := regexp.MustCompile(`name="grant" value="([^"]+)"`).FindStringSubmatch(body)
	if grantID == nil {
		t.Fatal("the confirmation carries no grant")
	}
	done := client.do(http.MethodPost, deviceVerifyPath, url.Values{"action": {"approve"}, "code": {start.UserCode}, "grant": {grantID[1]}})
	if done.Code != http.StatusOK || !strings.Contains(done.Body.String(), "Allowed mira-mbp") {
		t.Fatalf("approval: %d", done.Code)
	}

	code, issued := poll()
	token, _ := issued["access_token"].(string)
	if code != http.StatusOK || !strings.HasPrefix(token, store.TokenPrefix) || issued["login"] != "alice" || issued["scope"] != "write" {
		t.Fatalf("after approval: %d %v", code, issued)
	}
	if code, body := poll(); code != http.StatusBadRequest || body["error"] != store.DeviceExpired {
		t.Fatalf("a device code yields one token: %d %v", code, body)
	}
	me := callAPI(t, app, apiCall{method: http.MethodGet, path: apiPrefix + "me", token: token})
	if me.Code != http.StatusOK || !strings.Contains(me.Body.String(), `"login":"alice"`) {
		t.Fatalf("the token signs in: %d %s", me.Code, me.Body.String())
	}
	if account := client.page("/account"); !strings.Contains(account, "mira-mbp") || !strings.Contains(account, "darwin/arm64") {
		t.Fatal("the account page lists the device")
	}
}

func TestDevicePageLocksAfterWrongCodes(t *testing.T) {
	db, user, _ := testDatabase(t)
	client := newVersionClient(t, db, user)
	for i := 0; i < 5; i++ {
		if wrong := client.do(http.MethodPost, deviceVerifyPath, url.Values{"action": {"lookup"}, "code": {"AAAA-AAAA"}}); wrong.Code != http.StatusBadRequest {
			t.Fatalf("attempt %d: %d", i, wrong.Code)
		}
	}
	// Locked now, even for a code that would have been right.
	_, userCode, _, err := db.CreateDeviceGrant(context.Background(), store.DeviceRequest{Scope: store.TokenScopeRead}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	locked := client.do(http.MethodPost, deviceVerifyPath, url.Values{"action": {"lookup"}, "code": {userCode}})
	if locked.Code != http.StatusTooManyRequests || !strings.Contains(locked.Body.String(), "15 minutes") {
		t.Fatalf("too many wrong codes lock the page: %d", locked.Code)
	}
}

// TestTheResourceAPI covers what the command line does with a token: list,
// resolve, read, save with and without the version it started from, create,
// and sign out - and what a read-only token and a stale save are told.
func TestTheResourceAPI(t *testing.T) {
	db, user, resource := testDatabase(t)
	app := newTestApp(db, user.GitHubID)
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

	put := func(token, match, body string) *httptest.ResponseRecorder {
		header := map[string]string{"Content-Type": "application/octet-stream"}
		if match != "" {
			header["If-Match"] = match
		}
		return callAPI(t, app, apiCall{method: http.MethodPut, path: base + "/content", token: token, body: strings.NewReader(body), header: header})
	}
	if denied := put(reader, `"v1"`, "answer=0\n"); denied.Code != http.StatusForbidden || !strings.Contains(denied.Body.String(), "read_only") {
		t.Fatalf("a read-only token saves nothing: %d", denied.Code)
	}
	if missing := put(token, "", "answer=0\n"); missing.Code != http.StatusPreconditionRequired {
		t.Fatalf("a save without its version is refused: %d", missing.Code)
	}
	saved := put(token, `"v1"`, "answer=43\n")
	if saved.Code != http.StatusOK || !strings.Contains(saved.Body.String(), `"version":2`) || !strings.Contains(saved.Body.String(), `"new_version":true`) {
		t.Fatalf("save: %d %s", saved.Code, saved.Body.String())
	}
	stale := put(token, `"v1"`, "answer=44\n")
	var conflict apiError
	decodeJSON(t, stale, &conflict)
	if stale.Code != http.StatusPreconditionFailed || conflict.Error != "conflict" || conflict.CurrentVersion != 2 {
		t.Fatalf("a stale save: %d %+v", stale.Code, conflict)
	}
	if forced := put(token, "*", "answer=44\n"); forced.Code != http.StatusOK || !strings.Contains(forced.Body.String(), `"version":3`) {
		t.Fatalf("saving over on purpose: %d %s", forced.Code, forced.Body.String())
	}
	if current, _ := db.ResourceForOwner(context.Background(), user.ID, resource.ID); current.Version != 3 {
		t.Fatalf("the resource is at v%d", current.Version)
	}

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
		t.Fatalf("create: %d %+v", created.Code, made)
	}

	remote, err := db.CreateResource(context.Background(), user.ID, "remote", "", nil, "", "https://example.com/config")
	if err != nil {
		t.Fatal(err)
	}
	if ref := callAPI(t, app, apiCall{method: http.MethodGet, path: apiPrefix + "resources/" + remote.ID + "/content", token: token}); ref.Code != http.StatusConflict {
		t.Fatalf("a reference has no content to read: %d", ref.Code)
	}

	if out := callAPI(t, app, apiCall{method: http.MethodDelete, path: apiPrefix + "token", token: token}); out.Code != http.StatusNoContent {
		t.Fatalf("sign out: %d", out.Code)
	}
	if after := callAPI(t, app, apiCall{method: http.MethodGet, path: apiPrefix + "me", token: token}); after.Code != http.StatusUnauthorized {
		t.Fatalf("a signed-out token still works: %d", after.Code)
	}
}

// TestTheAPIOnlyTakesBearerTokens: a session cookie, a token in the address
// and a token without its scheme all open nothing.
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

// TestAnotherAccountsTokenReachesNothing is the isolation test for the API: a
// token of one account, aimed at another's resources by id, prefix, name and
// filename, finds nothing and changes nothing.
func TestAnotherAccountsTokenReachesNothing(t *testing.T) {
	db, owner, resource := testDatabase(t)
	ctx := context.Background()
	if err := db.UpdateResource(ctx, owner.ID, resource.ID, "Owner Secret Name", "owner-secret.conf", []byte("OWNER-SECRET\n"), "utf-8", ""); err != nil {
		t.Fatal(err)
	}
	intruder, err := db.UpsertUser(ctx, "200", "mallory", "", "")
	if err != nil {
		t.Fatal(err)
	}
	app := newTestApp(db, owner.GitHubID, intruder.GitHubID)
	token := issueToken(t, db, intruder, store.TokenScopeWrite)

	base := apiPrefix + "resources/" + resource.ID
	for _, call := range []apiCall{
		{method: http.MethodGet, path: base},
		{method: http.MethodGet, path: base + "/content"},
		{method: http.MethodPut, path: base + "/content", body: strings.NewReader("MALLORY\n"), header: map[string]string{"If-Match": "*"}},
		{method: http.MethodGet, path: apiPrefix + "resources?ref=" + resource.ID},
		{method: http.MethodGet, path: apiPrefix + "resources?ref=" + resource.ID[:8]},
		{method: http.MethodGet, path: apiPrefix + "resources?ref=owner-secret.conf"},
		{method: http.MethodGet, path: apiPrefix + "resources?ref=Owner+Secret+Name"},
		{method: http.MethodGet, path: apiPrefix + "resources?q=secret"},
		{method: http.MethodGet, path: apiPrefix + "resources"},
	} {
		call.token = token
		response := callAPI(t, app, call)
		for _, secret := range []string{"OWNER-SECRET", "Owner Secret Name", "owner-secret", resource.ID} {
			if strings.Contains(response.Body.String(), secret) {
				t.Errorf("%s %s answered %d with %q", call.method, call.path, response.Code, secret)
			}
		}
		if call.method == http.MethodPut && response.Code != http.StatusNotFound {
			t.Errorf("a save on another account's resource answered %d", response.Code)
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
	dialog := client.partsPage("/account?revoke=" + tokens[0].ID)
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

// TestInstallPageAndScripts serves the builds a directory holds: the page for
// a browser, the script for curl, and sums that agree everywhere.
func TestInstallPageAndScripts(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"plainmote-linux-amd64": "linux build", "plainmote-darwin-arm64": "mac build", "plainmote-windows-amd64.exe": "windows build",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	app := staticApp(t)
	app.cfg.CLIDir = dir
	app.cfg.Version = "abc123def456"
	get := func(path, accept string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		if accept != "" {
			request.Header.Set("Accept", accept)
		}
		response := httptest.NewRecorder()
		app.handler.ServeHTTP(response, request)
		return response
	}
	linuxSum, _, _ := fileSHA256(filepath.Join(dir, "plainmote-linux-amd64"))
	windowsSum, _, _ := fileSHA256(filepath.Join(dir, "plainmote-windows-amd64.exe"))

	// The page leads with the command for the system it is opened on, and
	// folds the other one and the downloads away.
	curl, irm := "curl -fsSL https://cfg.test/cli/en | sh", "irm https://cfg.test/cli/en.ps1 | iex"
	pageFor := func(agent, platform string) string {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/cli", nil)
		request.Header.Set("Accept", "text/html,application/xhtml+xml")
		request.Header.Set("User-Agent", agent)
		if platform != "" {
			request.Header.Set("Sec-CH-UA-Platform", platform)
		}
		response := httptest.NewRecorder()
		app.handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || strings.Contains(response.Body.String(), linuxSum) || !strings.Contains(response.Body.String(), "/cli/checksums.txt") {
			t.Fatalf("the install page for %q: %d", agent, response.Code)
		}
		return response.Body.String()
	}
	before := func(page, command string) bool {
		at, fold := strings.Index(page, command), strings.Index(page, "<details")
		return at >= 0 && at < fold
	}
	mac := pageFor("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15", "")
	if !before(mac, curl) || before(mac, irm) || !strings.Contains(mac, irm) || !strings.Contains(mac, "· macOS") {
		t.Fatal("on macOS the page leads with the shell command")
	}
	windows := pageFor("Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/130.0", `"Windows"`)
	if !before(windows, irm) || before(windows, curl) || !strings.Contains(windows, curl) {
		t.Fatal("on Windows the page leads with PowerShell")
	}
	if hinted := pageFor("Mozilla/5.0 (X11; Linux x86_64) Chrome/130.0", `"Windows"`); !before(hinted, irm) {
		t.Fatal("the platform hint wins over the user agent")
	}
	phone := pageFor("Mozilla/5.0 (Linux; Android 14; Pixel 8)", "")
	if !before(phone, curl) || !before(phone, irm) {
		t.Fatal("where the system cannot be told, both commands are shown")
	}

	// A command copied from a page installs a command line in the page's
	// language; the plain address leaves the language to the system.
	chinese := httptest.NewRequest(http.MethodGet, "/cli", nil)
	chinese.Header.Set("Accept", "text/html")
	chinese.Header.Set("Accept-Language", "zh-CN")
	chinese.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh)")
	zhPage := httptest.NewRecorder()
	app.handler.ServeHTTP(zhPage, chinese)
	if !strings.Contains(zhPage.Body.String(), "curl -fsSL https://cfg.test/cli/zh | sh") {
		t.Fatal("a Chinese page offers the Chinese install command")
	}
	for path, want := range map[string]string{
		"/cli/zh": `"$dir/plainmote" config language zh`, "/cli/en": `"$dir/plainmote" config language en`,
		"/cli/zh.ps1": "& $target config language zh", "/cli/en.ps1": "& $target config language en",
	} {
		if script := get(path, ""); script.Code != http.StatusOK || !strings.Contains(script.Body.String(), want) {
			t.Fatalf("%s: %d %s", path, script.Code, script.Body.String())
		}
	}
	if plain := get("/cli", "*/*"); strings.Contains(plain.Body.String(), "config language") {
		t.Fatal("the plain script sets no language")
	}
	// Languages it does not speak get the nearest one it does.
	for path, want := range map[string]string{
		"/cli/ja": "config language en", "/cli/fr.ps1": "config language en",
		"/cli/zh-TW": "config language zh", "/cli/zh-Hant-TW": "config language zh",
	} {
		if script := get(path, ""); script.Code != http.StatusOK || !strings.Contains(script.Body.String(), want) {
			t.Fatalf("%s: %d", path, script.Code)
		}
	}
	script := get("/cli", "*/*")
	body := script.Body.String()
	if script.Code != http.StatusOK || !strings.HasPrefix(body, "#!/bin/sh") || !strings.Contains(body, "linux-amd64) sum='"+linuxSum+"'") ||
		strings.Contains(body, windowsSum) || !strings.Contains(body, "base='https://cfg.test'") {
		t.Fatalf("the install script: %d %s", script.Code, body)
	}
	if ps := get("/cli.ps1", ""); !strings.Contains(ps.Body.String(), "'windows-amd64' = '"+windowsSum+"'") {
		t.Fatalf("the PowerShell script: %s", ps.Body.String())
	}
	if sums := get("/cli/checksums.txt", ""); !strings.Contains(sums.Body.String(), linuxSum+"  plainmote-linux-amd64") {
		t.Fatalf("checksums: %s", sums.Body.String())
	}
	download := get("/cli/download/linux-amd64", "")
	if download.Code != http.StatusOK || download.Body.String() != "linux build" || !strings.Contains(download.Header().Get("Content-Disposition"), `filename="plainmote"`) {
		t.Fatalf("download: %d %v", download.Code, download.Header())
	}
	for _, path := range []string{"/cli/download/linux-arm64", "/cli/download/linux-amd64/x", "/cli/download/linux", "/cli/nothing"} {
		if missing := get(path, ""); missing.Code != http.StatusNotFound {
			t.Fatalf("%s answered %d", path, missing.Code)
		}
	}
	// A path that climbs out is cleaned by the router before it gets here.
	if climbed := get("/cli/download/../../etc/passwd", ""); climbed.Code == http.StatusOK || strings.Contains(climbed.Body.String(), "build") {
		t.Fatalf("a climbing path answered %d", climbed.Code)
	}

	// Without a configured address the script takes the request's host, and
	// a host that could break out of the script's quotes gets no script.
	hostApp := staticApp(t)
	hostApp.cfg.PublicURL = ""
	hostApp.cfg.CLIDir = dir
	quoted := httptest.NewRequest(http.MethodGet, "/cli", nil)
	quoted.Host = "evil';touch${IFS}pwned;'"
	refused := httptest.NewRecorder()
	hostApp.handler.ServeHTTP(refused, quoted)
	if refused.Code != http.StatusBadRequest || strings.Contains(refused.Body.String(), "touch") {
		t.Fatalf("a quoting host got a script: %d %s", refused.Code, refused.Body.String())
	}

	empty := staticApp(t)
	empty.cfg.CLIDir = t.TempDir()
	request := httptest.NewRequest(http.MethodGet, "/cli", nil)
	response := httptest.NewRecorder()
	empty.handler.ServeHTTP(response, request)
	if !strings.Contains(response.Body.String(), `fail "this server does not offer the command line"`) {
		t.Fatalf("a server without builds says so: %s", response.Body.String())
	}
}

// TestAPIAuthorizationMatrix asks every endpoint with every kind of caller
// and checks the answer is the one that caller should get: nothing without
// a valid token, reading only with a read token, the owner's resource only
// for the owner. A token also opens no web page, and a session no endpoint.
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
		{name: "list", method: http.MethodGet, path: apiPrefix + "resources"},
		{name: "resolve", method: http.MethodGet, path: apiPrefix + "resources?ref=" + resource.ID},
		{name: "get", method: http.MethodGet, path: base},
		{name: "read", method: http.MethodGet, path: base + "/content"},
		{name: "save", method: http.MethodPut, path: base + "/content", write: true, body: func() (io.Reader, map[string]string) {
			return strings.NewReader("saved " + time.Now().String() + "\n"), map[string]string{"If-Match": "*"}
		}},
		{name: "create", method: http.MethodPost, path: apiPrefix + "resources", write: true, body: multipartBody},
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
		// No valid credential: refused alike, whatever the endpoint.
		for label, token := range map[string]string{"none": "", "garbage": "pmt_" + strings.Repeat("A", 43), "revoked": revoked} {
			if got := call(e, token, false); got.Code != http.StatusUnauthorized {
				t.Errorf("%s with %s: %d", e.name, label, got.Code)
			}
		}
		if got := call(e, "", true); got.Code != http.StatusUnauthorized {
			t.Errorf("%s with a session cookie: %d", e.name, got.Code)
		}
		// A read token reads, and writes nothing.
		got := call(e, reader, false)
		if e.write && got.Code != http.StatusForbidden {
			t.Errorf("%s with a read token: %d", e.name, got.Code)
		}
		if !e.write && got.Code != http.StatusOK {
			t.Errorf("%s with a read token: %d", e.name, got.Code)
		}
		// The owner's write token does everything here.
		if got := call(e, writer, false); got.Code != http.StatusOK && got.Code != http.StatusCreated {
			t.Errorf("%s with the owner's write token: %d %s", e.name, got.Code, got.Body.String())
		}
		// Another account's token: its own data only.
		got = call(e, foreign, false)
		switch e.name {
		case "me", "create":
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

	// What other accounts' tokens created stays theirs.
	if theirs, _ := db.APIResources(ctx, owner.ID, ""); len(theirs) != 2 {
		t.Fatalf("the owner has %d resources, want their own two", len(theirs))
	}

	// A token opens no web page and changes nothing through a form.
	for _, page := range []struct{ method, path string }{
		{http.MethodGet, "/account"}, {http.MethodGet, "/resources/"}, {http.MethodGet, "/resources/" + resource.ID},
		{http.MethodGet, deviceVerifyPath}, {http.MethodPost, "/account/delete"}, {http.MethodPost, "/account/tokens"},
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
