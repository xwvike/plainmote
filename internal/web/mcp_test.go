package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"plainmote/internal/store"
)

// mcpToken is an access token for user, issued as the authorization server
// issues one, for the resource given.
func mcpToken(t *testing.T, db *store.Store, user User, scope, resource string) store.OAuthTokens {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	client, err := db.RegisterOAuthClient(ctx, "Test App", "", []string{"http://127.0.0.1:33418/callback"}, "", now)
	if err != nil {
		t.Fatal(err)
	}
	verifier, challenge := testPKCE()
	code, err := db.CreateOAuthCode(ctx, store.OAuthAuthorization{UserID: user.ID, Client: client, Scope: scope,
		Resource: resource, RedirectURI: client.RedirectURIs[0], CodeChallenge: challenge}, now)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := db.ExchangeOAuthCode(ctx, store.OAuthExchange{Code: code, ClientID: client.ID,
		RedirectURI: client.RedirectURIs[0], Verifier: verifier, Resource: resource}, now)
	if err != nil {
		t.Fatal(err)
	}
	return tokens
}

// mcpRequest is one POST to the MCP endpoint. A modern request carries its
// version in _meta and mirrors it, the method and the name into headers.
type mcpRequest struct {
	token   string
	version string // empty: a legacy request with no header
	modern  bool
	method  string
	params  map[string]any
	header  map[string]string
	id      any
}

func (q mcpRequest) send(app *App) (*httptest.ResponseRecorder, map[string]any) {
	params := map[string]any{}
	for key, value := range q.params {
		params[key] = value
	}
	if q.modern {
		params["_meta"] = map[string]any{
			mcpMetaVersion: q.version, mcpMetaCaps: map[string]any{},
			"io.modelcontextprotocol/clientInfo": map[string]any{"name": "test", "version": "1"},
		}
	}
	id := q.id
	if id == nil {
		id = 1
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": q.method, "params": params})
	request := httptest.NewRequest(http.MethodPost, "https://cfg.test"+mcpPath, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	if q.token != "" {
		request.Header.Set("Authorization", "Bearer "+q.token)
	}
	if q.version != "" {
		request.Header.Set("MCP-Protocol-Version", q.version)
	}
	if q.modern {
		request.Header.Set("Mcp-Method", q.method)
		if name, ok := q.params["name"].(string); ok {
			request.Header.Set("Mcp-Name", name)
		}
	}
	for key, value := range q.header {
		if value == "" {
			request.Header.Del(key)
		} else {
			request.Header.Set(key, value)
		}
	}
	response := httptest.NewRecorder()
	app.handler.ServeHTTP(response, request)
	var answer map[string]any
	_ = json.Unmarshal(response.Body.Bytes(), &answer)
	return response, answer
}

func rpcCode(answer map[string]any) int {
	failure, _ := answer["error"].(map[string]any)
	code, _ := failure["code"].(float64)
	return int(code)
}

// toolCall calls a tool the current way and returns its result.
type toolAnswer struct {
	structured map[string]any
	texts      []string
	isError    bool
}

func toolCall(t *testing.T, app *App, token, name string, args map[string]any) toolAnswer {
	t.Helper()
	response, answer := mcpRequest{token: token, version: mcpModernVersion, modern: true, method: "tools/call",
		params: map[string]any{"name": name, "arguments": args}}.send(app)
	if response.Code != http.StatusOK || answer["result"] == nil {
		t.Fatalf("%s: %d %s", name, response.Code, response.Body)
	}
	result := answer["result"].(map[string]any)
	if result["resultType"] != "complete" {
		t.Fatalf("%s: result type %v", name, result["resultType"])
	}
	var out toolAnswer
	out.isError, _ = result["isError"].(bool)
	out.structured, _ = result["structuredContent"].(map[string]any)
	for _, item := range result["content"].([]any) {
		out.texts = append(out.texts, item.(map[string]any)["text"].(string))
	}
	return out
}

func TestMCPAcceptsOnlyItsOwnAccessTokens(t *testing.T) {
	db, alice, _ := testDatabase(t)
	app := newTestApp(db)
	ctx := context.Background()
	list := mcpRequest{version: mcpModernVersion, modern: true, method: "tools/list"}

	response, _ := list.send(app)
	challenge := response.Header().Get("WWW-Authenticate")
	if response.Code != http.StatusUnauthorized || !strings.Contains(challenge, `resource_metadata="https://cfg.test/.well-known/oauth-protected-resource/mcp"`) ||
		!strings.Contains(challenge, `scope="read"`) || strings.Contains(challenge, "invalid_token") {
		t.Fatalf("no token: %d %q", response.Code, challenge)
	}

	cli := issueToken(t, db, alice, store.TokenScopeRead)
	tokens := mcpToken(t, db, alice, "read", testMCPResource)
	elsewhere := mcpToken(t, db, alice, "read", "https://other.test/mcp")
	for name, token := range map[string]string{
		"a command-line token":           cli,
		"a refresh token":                tokens.RefreshToken,
		"a token for another server":     elsewhere.AccessToken,
		"an access token with a char on": tokens.AccessToken + "x",
	} {
		list.token = token
		response, _ := list.send(app)
		if response.Code != http.StatusUnauthorized || !strings.Contains(response.Header().Get("WWW-Authenticate"), `error="invalid_token"`) {
			t.Errorf("%s: %d", name, response.Code)
		}
	}

	list.token = tokens.AccessToken
	if response, answer := list.send(app); response.Code != http.StatusOK || len(answer["result"].(map[string]any)["tools"].([]any)) != len(mcpTools) {
		t.Fatalf("a good token: %d %s", response.Code, response.Body)
	}
	// The MCP token is no key to the command line's API either.
	request := httptest.NewRequest(http.MethodGet, "https://cfg.test/api/v1/me", nil)
	request.Header.Set("Authorization", "Bearer "+tokens.AccessToken)
	recorder := httptest.NewRecorder()
	app.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("the MCP token opened /api/v1: %d", recorder.Code)
	}

	grants, err := db.ListOAuthGrants(ctx, alice.ID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	for _, grant := range grants {
		if err := db.RevokeOAuthGrant(ctx, alice.ID, grant.ID); err != nil {
			t.Fatal(err)
		}
	}
	if response, _ := list.send(app); response.Code != http.StatusUnauthorized {
		t.Fatalf("a revoked grant still works: %d", response.Code)
	}
}

func TestMCPTransport(t *testing.T) {
	db, alice, _ := testDatabase(t)
	app := newTestApp(db)
	token := mcpToken(t, db, alice, "read", testMCPResource).AccessToken
	modern := func(method string, params map[string]any) mcpRequest {
		return mcpRequest{token: token, version: mcpModernVersion, modern: true, method: method, params: params}
	}

	response, answer := modern("server/discover", nil).send(app)
	result, _ := answer["result"].(map[string]any)
	if response.Code != http.StatusOK || result["resultType"] != "complete" || result["cacheScope"] != "public" ||
		result["supportedVersions"].([]any)[0] != mcpModernVersion || result["capabilities"].(map[string]any)["tools"] == nil {
		t.Fatalf("discover: %d %s", response.Code, response.Body)
	}
	if info := result["_meta"].(map[string]any)[mcpMetaServer].(map[string]any); info["name"] != "plainmote" {
		t.Fatalf("server info %v", info)
	}

	for name, c := range map[string]struct {
		request mcpRequest
		status  int
		code    int
	}{
		"Mcp-Method differs":    {mcpRequest{token: token, version: mcpModernVersion, modern: true, method: "tools/list", header: map[string]string{"Mcp-Method": "tools/call"}}, 400, rpcHeaderMismatch},
		"version header gone":   {mcpRequest{token: token, version: mcpModernVersion, modern: true, method: "tools/list", header: map[string]string{"MCP-Protocol-Version": ""}}, 400, rpcHeaderMismatch},
		"Mcp-Name missing":      {mcpRequest{token: token, version: mcpModernVersion, modern: true, method: "tools/call", params: map[string]any{"name": "list_resources"}, header: map[string]string{"Mcp-Name": ""}}, 400, rpcHeaderMismatch},
		"Mcp-Name differs":      {mcpRequest{token: token, version: mcpModernVersion, modern: true, method: "tools/call", params: map[string]any{"name": "list_resources"}, header: map[string]string{"Mcp-Name": "read_resource"}}, 400, rpcHeaderMismatch},
		"unknown version":       {mcpRequest{token: token, version: "2099-01-01", modern: true, method: "tools/list"}, 400, rpcUnsupportedVersion},
		"unknown method":        {modern("resources/list", nil), 404, rpcMethodNotFound},
		"modern header no meta": {mcpRequest{token: token, version: mcpModernVersion, method: "tools/list"}, 400, rpcInvalidParams},
		"legacy unknown header": {mcpRequest{token: token, version: "2024-01-01", method: "tools/list"}, 400, rpcUnsupportedVersion},
		"unknown tool":          {modern("tools/call", map[string]any{"name": "delete_everything"}), 200, rpcInvalidParams},
	} {
		response, answer := c.request.send(app)
		if response.Code != c.status || rpcCode(answer) != c.code {
			t.Errorf("%s: %d %s", name, response.Code, response.Body)
		}
	}
	if _, answer := (mcpRequest{token: token, version: "2099-01-01", modern: true, method: "tools/list"}).send(app); answer["error"].(map[string]any)["data"].(map[string]any)["supported"] == nil {
		t.Fatal("an unsupported version is not told which are")
	}
	// A name that is not plain ASCII travels in base64.
	encoded := mcpRequest{token: token, version: mcpModernVersion, modern: true, method: "tools/call",
		params: map[string]any{"name": "list_resources"}, header: map[string]string{"Mcp-Name": "=?base64?" + base64.StdEncoding.EncodeToString([]byte("list_resources")) + "?="}}
	if response, _ := encoded.send(app); response.Code != http.StatusOK {
		t.Fatalf("a base64 Mcp-Name: %d %s", response.Code, response.Body)
	}

	// The earlier revision: initialize, answered without a session.
	response, answer = mcpRequest{token: token, method: "initialize", params: map[string]any{
		"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "old", "version": "1"},
	}}.send(app)
	result, _ = answer["result"].(map[string]any)
	if response.Code != http.StatusOK || result["protocolVersion"] != "2025-06-18" || response.Header().Get("Mcp-Session-Id") != "" || result["resultType"] != nil {
		t.Fatalf("initialize: %d %s", response.Code, response.Body)
	}
	if _, answer := (mcpRequest{token: token, method: "initialize", params: map[string]any{"protocolVersion": "2024-11-05"}}).send(app); answer["result"].(map[string]any)["protocolVersion"] != mcpLegacyVersion {
		t.Fatal("an old version is not offered the one this server speaks")
	}
	if response, answer := (mcpRequest{token: token, version: mcpLegacyVersion, method: "tools/list"}).send(app); response.Code != http.StatusOK || len(answer["result"].(map[string]any)["tools"].([]any)) != len(mcpTools) {
		t.Fatalf("legacy tools/list: %d %s", response.Code, response.Body)
	}
	if response, answer := (mcpRequest{token: token, version: mcpLegacyVersion, method: "tools/call", params: map[string]any{"name": "list_resources"}}).send(app); response.Code != http.StatusOK || answer["result"].(map[string]any)["structuredContent"] == nil {
		t.Fatalf("legacy tools/call: %d %s", response.Code, response.Body)
	}

	post := func(body string, header map[string]string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "https://cfg.test"+mcpPath, strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		for key, value := range header {
			request.Header.Set(key, value)
		}
		recorder := httptest.NewRecorder()
		app.handler.ServeHTTP(recorder, request)
		return recorder
	}
	if got := post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil); got.Code != http.StatusAccepted || got.Body.Len() != 0 {
		t.Fatalf("a notification: %d", got.Code)
	}
	if got := post(`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, nil); got.Code != http.StatusBadRequest {
		t.Fatalf("a batch: %d", got.Code)
	}
	if got := post(`{"jsonrpc":"2.0","id":null,"method":"ping"}`, nil); got.Code != http.StatusBadRequest {
		t.Fatalf("a null id: %d", got.Code)
	}
	if got := post(`{nope`, nil); got.Code != http.StatusBadRequest {
		t.Fatalf("not JSON: %d", got.Code)
	}
	if got := post(`{"jsonrpc":"2.0","id":1,"method":"ping"}`, map[string]string{"Origin": "https://evil.test"}); got.Code != http.StatusForbidden {
		t.Fatalf("another site's page: %d", got.Code)
	}
	if got := post(`{"jsonrpc":"2.0","id":1,"method":"ping"}`, map[string]string{"Origin": "http://localhost:6274"}); got.Code != http.StatusOK ||
		got.Header().Get("Access-Control-Allow-Origin") != "http://localhost:6274" {
		t.Fatalf("a local inspector: %d", got.Code)
	}
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		request := httptest.NewRequest(method, "https://cfg.test"+mcpPath, nil)
		request.Header.Set("Authorization", "Bearer "+token)
		recorder := httptest.NewRecorder()
		app.handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: %d", method, recorder.Code)
		}
	}
}

func TestMCPReadTools(t *testing.T) {
	db, alice, example := testDatabase(t)
	app := newTestApp(db)
	ctx := context.Background()
	token := mcpToken(t, db, alice, "read", testMCPResource).AccessToken

	big := strings.Repeat("aé", 200_000) // 600 000 bytes; two-byte runes straddle every boundary
	long, err := db.CreateResource(ctx, alice.ID, "Long log", "long.txt", []byte(big), "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateResource(ctx, alice.ID, "Logo", "logo.png", append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...), ""); err != nil {
		t.Fatal(err)
	}
	sealedID := uuid.NewString()
	if _, err := db.CreateSealedResource(ctx, alice.ID, sealedID, store.SealedPart{
		Content: append([]byte("PMr1"), bytes.Repeat([]byte("S"), 48)...), Meta: append([]byte("PMm1"), bytes.Repeat([]byte("M"), 40)...),
	}, bytes.Repeat([]byte{9}, 60)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveResource(ctx, alice.ID, example.ID, store.ResourceEdit{Name: example.Name, Filename: example.Filename,
		Content: []byte("answer=43\n"), BaseVersion: 1}); err != nil {
		t.Fatal(err)
	}

	listed := toolCall(t, app, token, "list_resources", nil)
	items := listed.structured["resources"].([]any)
	if listed.isError || len(items) != 4 || listed.structured["total"].(float64) != 4 {
		t.Fatalf("list: %v", listed.structured)
	}
	for _, raw := range items {
		item := raw.(map[string]any)
		if item["id"] == sealedID {
			if item["name"] != mcpEncryptedName || item["encrypted"] != true || item["filename"] != nil || item["type"] != nil {
				t.Fatalf("an encrypted resource says more than that it exists: %v", item)
			}
		}
	}
	if found := toolCall(t, app, token, "list_resources", map[string]any{"query": "long"}); len(found.structured["resources"].([]any)) != 1 {
		t.Fatalf("query: %v", found.structured)
	}
	if bad := toolCall(t, app, token, "list_resources", map[string]any{"cursor": "nope"}); !bad.isError {
		t.Fatal("a made-up cursor was taken")
	}

	// By id, by page address, by share link, by name: the same resource.
	share, err := db.CreateShare(ctx, alice.ID, example.ID, "", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{example.ID, "https://cfg.test/resources/" + example.ID, "https://cfg.test" + shareAddress(share.Token, "example.conf"), "example.conf"} {
		read := toolCall(t, app, token, "read_resource", map[string]any{"resource": ref})
		if read.isError || read.structured["text"] != "answer=43\n" || read.structured["version"].(float64) != 2 {
			t.Fatalf("read %s: %v %v", ref, read.texts, read.structured)
		}
	}
	if used, err := db.ListShares(ctx, alice.ID, example.ID, time.Now().UTC()); err != nil || used[0].UsedCount != 0 {
		t.Fatalf("reading by a share link used it up: %v %v", used, err)
	}
	earlier := toolCall(t, app, token, "read_resource", map[string]any{"resource": example.ID, "version": 1})
	if earlier.isError || earlier.structured["text"] != "answer=42\n" || earlier.structured["current_version"].(float64) != 2 {
		t.Fatalf("version 1: %v", earlier.structured)
	}
	if missing := toolCall(t, app, token, "read_resource", map[string]any{"resource": example.ID, "version": 9}); !missing.isError {
		t.Fatal("a version that does not exist was read")
	}
	versions := toolCall(t, app, token, "list_versions", map[string]any{"resource": example.ID})
	if list := versions.structured["versions"].([]any); len(list) != 2 || list[0].(map[string]any)["current"] != true || list[1].(map[string]any)["version"].(float64) != 1 {
		t.Fatalf("versions: %v", versions.structured)
	}

	// Long text in parts, cut between characters, put back together whole.
	var joined strings.Builder
	offset := 0.0
	for parts := 0; ; parts++ {
		if parts > len(big)/mcpReadChunk+1 {
			t.Fatal("reading never ends")
		}
		part := toolCall(t, app, token, "read_resource", map[string]any{"resource": long.ID, "offset": offset})
		text := part.structured["text"].(string)
		if part.isError || len(text) > mcpReadChunk || !strings.HasPrefix(big[int(offset):], text) || part.texts[1] != text {
			t.Fatalf("part at %v: %v", offset, part.texts[0])
		}
		joined.WriteString(text)
		next, more := part.structured["next_offset"].(float64)
		if !more {
			break
		}
		offset = next
	}
	if joined.String() != big {
		t.Fatal("the parts do not add up to the text")
	}
	if past := toolCall(t, app, token, "read_resource", map[string]any{"resource": long.ID, "offset": len(big) + 1}); !past.isError {
		t.Fatal("an offset past the end was read")
	}

	if logo := toolCall(t, app, token, "read_resource", map[string]any{"resource": "logo.png"}); logo.isError || logo.structured["binary"] != true || logo.structured["text"] != nil {
		t.Fatalf("binary: %v", logo.structured)
	}
	sealed := toolCall(t, app, token, "read_resource", map[string]any{"resource": sealedID})
	if !sealed.isError || sealed.structured != nil || strings.Contains(strings.Join(sealed.texts, ""), "SSSS") {
		t.Fatalf("an encrypted resource was read: %v", sealed)
	}
	if history := toolCall(t, app, token, "list_versions", map[string]any{"resource": sealedID}); history.isError || history.structured["name"] != mcpEncryptedName {
		t.Fatalf("encrypted history: %v", history.structured)
	}
	if bad := toolCall(t, app, token, "read_resource", map[string]any{"resource": example.ID, "offset": "zero"}); !bad.isError {
		t.Fatal("an offset that is not a number was taken")
	}
}

// One account's token, pointed at another account's resources in every way
// a tool can be pointed, finds nothing - in the words it would use for a
// resource that does not exist.
func TestMCPToolsStayWithinTheAccount(t *testing.T) {
	db, alice, example := testDatabase(t)
	app := newTestApp(db)
	ctx := context.Background()
	bob, err := db.UpsertUser(ctx, "200", "bob", "Bob", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateResource(ctx, bob.ID, "Bob's notes", "bob.txt", []byte("bob\n"), ""); err != nil {
		t.Fatal(err)
	}
	share, err := db.CreateShare(ctx, alice.ID, example.ID, "", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	token := mcpToken(t, db, bob, "read", testMCPResource).AccessToken

	listed := toolCall(t, app, token, "list_resources", nil)
	if items := listed.structured["resources"].([]any); len(items) != 1 || items[0].(map[string]any)["name"] != "Bob's notes" {
		t.Fatalf("bob sees %v", items)
	}
	ghost := uuid.NewString()
	for _, c := range []struct{ alices, nobodys string }{
		{example.ID, ghost},
		{"https://cfg.test/resources/" + example.ID, "https://cfg.test/resources/" + ghost},
		{"https://cfg.test" + shareAddress(share.Token, "x"), "https://cfg.test" + shareAddress(strings.Repeat("A", len(share.Token)), "x")},
		{"example.conf", "nothing.conf"},
		{example.ID[:8], "ffffffff"},
	} {
		for _, tool := range []string{"read_resource", "list_versions"} {
			alices := toolCall(t, app, token, tool, map[string]any{"resource": c.alices})
			nobodys := toolCall(t, app, token, tool, map[string]any{"resource": c.nobodys})
			if !alices.isError || alices.structured != nil ||
				strings.ReplaceAll(alices.texts[0], c.alices, "REF") != strings.ReplaceAll(nobodys.texts[0], c.nobodys, "REF") {
				t.Fatalf("%s %s: %q vs %q", tool, c.alices, alices.texts, nobodys.texts)
			}
		}
	}
	if used, err := db.ListShares(ctx, alice.ID, example.ID, time.Now().UTC()); err != nil || used[0].UsedCount != 0 {
		t.Fatalf("bob's lookup used alice's link: %v %v", used, err)
	}
}

// TestMCPServerPage fixes the developer page on the MCP server: the address
// to add, set up for the apps it names, every tool the server offers, and the
// page's earlier address leading to it.
func TestMCPServerPage(t *testing.T) {
	app := staticApp(t)
	get := func(path string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Accept", "text/html")
		response := httptest.NewRecorder()
		app.handler.ServeHTTP(response, request)
		return response
	}
	page := get("/developers/mcp")
	body := html.UnescapeString(page.Body.String())
	if page.Code != http.StatusOK {
		t.Fatalf("/developers/mcp: %d", page.Code)
	}
	for _, want := range []string{
		`data-copy="https://cfg.test/mcp"`,
		"claude mcp add --transport http --scope user plainmote https://cfg.test/mcp",
		"codex mcp add plainmote --url https://cfg.test/mcp",
		`code --add-mcp '{"name":"plainmote","type":"http","url":"https://cfg.test/mcp"}'`,
		`"context_servers": {`, `"type": "remote",`, "opencode mcp auth plainmote",
		`"serverUrl": "https://cfg.test/mcp"`,
		`href="https://cursor.com/en/install-mcp?name=plainmote&config=eyJ1cmwiOiJodHRwczovL2NmZy50ZXN0L21jcCJ9"`,
		`href="/developers/mcp" class="on" aria-current="page"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page lacks %s", want)
		}
	}
	for _, tool := range mcpTools {
		if !strings.Contains(body, `<td class="mono">`+tool.Name+`</td>`) {
			t.Errorf("the page does not list %s", tool.Name)
		}
	}
	// Locale prefixes exist only where there are public pages.
	app.cfg.AnonymousEnabled = true
	for from, to := range map[string]string{"/developers/ai": "/developers/mcp", "/zh-cn/developers/ai": "/zh-cn/developers/mcp"} {
		if moved := get(from); moved.Code != http.StatusMovedPermanently || moved.Header().Get("Location") != to {
			t.Fatalf("%s: %d %q", from, moved.Code, moved.Header().Get("Location"))
		}
	}
}
