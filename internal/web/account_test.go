package web

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"plainmote/internal/store"
)

type accountClient struct {
	t       *testing.T
	app     *App
	session string
	csrf    string
}

func signedIn(t *testing.T, db *store.Store, user User) accountClient {
	t.Helper()
	session, csrf, _, err := db.CreateSession(context.Background(), user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return accountClient{t: t, app: newTestApp(db, user.GitHubID), session: session, csrf: csrf}
}

func (c accountClient) do(method, target string, form url.Values) *httptest.ResponseRecorder {
	c.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	request := httptest.NewRequest(method, "https://cfg.test"+target, body)
	if form != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: c.session})
	request.AddCookie(&http.Cookie{Name: csrfCookie, Value: c.csrf})
	response := httptest.NewRecorder()
	c.app.handler.ServeHTTP(response, request)
	return response
}

func TestAccountPageIsReachableFromTheTopBar(t *testing.T) {
	db, user, _ := testDatabase(t)
	client := signedIn(t, db, user)
	if page := client.do(http.MethodGet, "/resources/", nil).Body.String(); !strings.Contains(page, `class="who" href="/account"`) {
		t.Fatal("the username in the top bar does not lead to the account page")
	}
	page := client.do(http.MethodGet, "/account", nil)
	body := page.Body.String()
	if page.Code != http.StatusOK || !strings.Contains(body, `action="/account/export"`) || !strings.Contains(body, `action="/account/delete"`) {
		t.Fatalf("account page: %d %s", page.Code, body)
	}
}

func TestExportHoldsTheWholeAccount(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	share, err := db.CreateShare(ctx, user.ID, resource.ID, "for bob", time.Hour, 3)
	if err != nil {
		t.Fatal(err)
	}
	note, err := db.CreateResource(ctx, user.ID, "Notes", "", []byte("remember this\n"), "utf-8", "")
	if err != nil {
		t.Fatal(err)
	}
	remote, err := db.CreateResource(ctx, user.ID, "Upstream", "remote.txt", nil, "", "https://example.com/remote.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordAccess(ctx, store.AccessEvent{
		OwnerID: user.ID, ResourceID: resource.ID, LinkID: share.ID, Outcome: store.OutcomeSuccess, Status: 200,
	}, store.RequestMeta{
		Method: "GET", RemoteIP: "203.0.113.9", UserAgent: "curl/8",
		Path:    "/d/" + share.Token + "/example.conf",
		Query:   "token=" + url.QueryEscape(share.Token),
		Referer: "https://cfg.test/d/" + share.Token + "/example.conf?grant=" + url.QueryEscape(share.Token),
	}); err != nil {
		t.Fatal(err)
	}
	client := signedIn(t, db, user)

	if forged := client.do(http.MethodPost, "/account/export", url.Values{}); forged.Code != http.StatusForbidden {
		t.Fatalf("an export without the csrf token must be refused, got %d", forged.Code)
	}
	response := client.do(http.MethodPost, "/account/export", url.Values{"csrf": {client.csrf}})
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/zip" ||
		!strings.HasPrefix(response.Header().Get("Content-Disposition"), `attachment; filename="plainmote-alice-`) {
		t.Fatalf("export response: %d %v", response.Code, response.Header())
	}

	archive, err := zip.NewReader(bytes.NewReader(response.Body.Bytes()), int64(response.Body.Len()))
	if err != nil {
		t.Fatalf("the export is not a readable archive: %v", err)
	}
	files := map[string][]byte{}
	for _, file := range archive.File {
		reader, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		files[file.Name], _ = io.ReadAll(reader)
		reader.Close()
	}

	if account := string(files["account.txt"]); !strings.Contains(account, "GitHub username: alice") || !strings.Contains(account, "Display name: Alice") {
		t.Fatalf("account.txt: %s", account)
	}
	if _, ok := files["account.json"]; ok {
		t.Fatal("the export still contains account.json")
	}
	if _, ok := files["resources.json"]; ok {
		t.Fatal("the export still contains resources.json")
	}
	if body := string(files["files/"+resource.ID+"/example.conf"]); body != "answer=42\n" {
		t.Fatalf("the named file body is %q", body)
	}
	if body := string(files["files/"+note.ID+"/Notes.txt"]); body != "remember this\n" {
		t.Fatalf("the editor resource did not receive a .txt filename: %q", body)
	}
	if shortcut := string(files["files/"+remote.ID+"/remote.txt.url"]); !strings.Contains(shortcut, "URL=https://example.com/remote.txt") {
		t.Fatalf("the remote resource shortcut is %q", shortcut)
	}
	var logs []exportAccessLog
	if err := json.Unmarshal(files["access_logs.json"], &logs); err != nil || len(logs) != 1 || logs[0].RemoteIP != "203.0.113.9" {
		t.Fatalf("access_logs.json: %v %s", err, files["access_logs.json"])
	}

	// A share address is a working credential; an archive people keep and
	// forward must not carry any.
	for name, content := range files {
		if bytes.Contains(content, []byte(share.Token)) {
			t.Fatalf("%s contains a share token", name)
		}
	}
}

func TestDeleteAccountThroughTheRouter(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	share, err := db.CreateShare(ctx, user.ID, resource.ID, "", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	client := signedIn(t, db, user)

	if forged := client.do(http.MethodPost, "/account/delete", url.Values{"confirm": {"alice"}}); forged.Code != http.StatusForbidden {
		t.Fatalf("a delete without the csrf token must be refused, got %d", forged.Code)
	}
	mismatch := client.do(http.MethodPost, "/account/delete", url.Values{"csrf": {client.csrf}, "confirm": {"alic"}})
	if mismatch.Code != http.StatusBadRequest || !strings.Contains(mismatch.Body.String(), "The username does not match.") {
		t.Fatalf("a wrong confirmation must be refused on the page: %d", mismatch.Code)
	}
	if _, err := db.Account(ctx, user.ID); err != nil {
		t.Fatal("a refused confirmation deleted the account")
	}

	// The typed username only opens the confirmation; nothing is deleted yet.
	confirm := client.do(http.MethodPost, "/account/delete", url.Values{"csrf": {client.csrf}, "confirm": {"ALICE"}})
	if confirm.Code != http.StatusOK || !strings.Contains(confirm.Body.String(), `<dialog class="dlg" open>`) ||
		!strings.Contains(confirm.Body.String(), `name="final" value="1"`) {
		t.Fatalf("the username must open a confirmation dialog: %d", confirm.Code)
	}
	if _, err := db.Account(ctx, user.ID); err != nil {
		t.Fatal("the first step deleted the account")
	}
	if skipped := client.do(http.MethodPost, "/account/delete", url.Values{"csrf": {client.csrf}, "confirm": {"bob"}, "final": {"1"}}); skipped.Code != http.StatusBadRequest {
		t.Fatalf("the final step must still check the username, got %d", skipped.Code)
	}

	deleted := client.do(http.MethodPost, "/account/delete", url.Values{"csrf": {client.csrf}, "confirm": {"ALICE"}, "final": {"1"}})
	if deleted.Code != http.StatusSeeOther || deleted.Header().Get("Location") != "/login?deleted=1" {
		t.Fatalf("delete: %d %q", deleted.Code, deleted.Header().Get("Location"))
	}
	cleared := map[string]bool{}
	for _, cookie := range deleted.Result().Cookies() {
		if cookie.MaxAge < 0 {
			cleared[cookie.Name] = true
		}
	}
	if !cleared[sessionCookie] || !cleared[csrfCookie] {
		t.Fatalf("the session cookies must be cleared: %v", deleted.Result().Cookies())
	}
	if _, err := db.Account(ctx, user.ID); err == nil {
		t.Fatal("the account still exists")
	}
	served := httptest.NewRecorder()
	client.app.handler.ServeHTTP(served, httptest.NewRequest(http.MethodGet, "https://cfg.test"+shareAddress(share.Token, resource.Filename), nil))
	if served.Code == http.StatusOK {
		t.Fatal("a link of the deleted account still serves its content")
	}

	login := httptest.NewRecorder()
	client.app.handler.ServeHTTP(login, httptest.NewRequest(http.MethodGet, "https://cfg.test/login?deleted=1", nil))
	if !strings.Contains(login.Body.String(), "The account has been deleted.") {
		t.Fatal("the sign-in page does not confirm the deletion")
	}
}

func TestExportIsLimitedAndThePageSaysSo(t *testing.T) {
	db, user, _ := testDatabase(t)
	client := signedIn(t, db, user)
	for i := 0; i < store.ExportLimit; i++ {
		if response := client.do(http.MethodPost, "/account/export", url.Values{"csrf": {client.csrf}}); response.Code != http.StatusOK {
			t.Fatalf("export %d within the limit: %d", i+1, response.Code)
		}
	}
	refused := client.do(http.MethodPost, "/account/export", url.Values{"csrf": {client.csrf}})
	if refused.Code != http.StatusTooManyRequests || !strings.Contains(refused.Body.String(), "The export limit has been reached.") {
		t.Fatalf("an export over the limit: %d", refused.Code)
	}
	page := client.do(http.MethodGet, "/account", nil).Body.String()
	if !strings.Contains(page, "Up to 2 exports every 24 hours.") || !strings.Contains(page, `<button type="submit" disabled>`) || !strings.Contains(page, "Next export available: ") {
		t.Fatal("the account page does not show that the limit is reached")
	}
}

func TestDeleteConfirmationDoesNotPrefillTheUsername(t *testing.T) {
	db, user, _ := testDatabase(t)
	page := signedIn(t, db, user).do(http.MethodGet, "/account", nil).Body.String()
	start := strings.Index(page, `name="confirm"`)
	field := page[start : start+strings.Index(page[start:], ">")]
	if strings.Contains(field, "alice") || strings.Contains(field, "placeholder") {
		t.Fatalf("the confirmation field gives the answer away: %s", field)
	}
}

func TestUntitledNamesFollowTheReadersLanguage(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	resource, err := db.CreateResource(ctx, user.ID, "", "", []byte("no name"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	share, err := db.CreateShare(ctx, user.ID, resource.ID, "", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	client := signedIn(t, db, user)
	served := httptest.NewRecorder()
	client.app.handler.ServeHTTP(served, httptest.NewRequest(http.MethodGet, "https://cfg.test"+shareAddress(share.Token, ""), nil))

	get := func(target, language string) string {
		request := httptest.NewRequest(http.MethodGet, "https://cfg.test"+target, nil)
		request.Header.Set("Accept-Language", language)
		request.AddCookie(&http.Cookie{Name: sessionCookie, Value: client.session})
		request.AddCookie(&http.Cookie{Name: csrfCookie, Value: client.csrf})
		response := httptest.NewRecorder()
		client.app.handler.ServeHTTP(response, request)
		return response.Body.String()
	}
	for _, tc := range []struct{ language, resource, share string }{
		{"en", "Untitled resource", "Untitled share"},
		{"zh-CN", "未命名资源", "未命名分享"},
	} {
		if page := get("/resources/"+resource.ID, tc.language); !strings.Contains(page, "<h1>"+tc.resource+"</h1>") {
			t.Errorf("%s resource page does not say %q", tc.language, tc.resource)
		}
		if page := get("/resources/", tc.language); !strings.Contains(page, ">"+tc.resource+"</a>") {
			t.Errorf("%s resource list does not say %q", tc.language, tc.resource)
		}
		if page := get("/logs", tc.language); !strings.Contains(page, ">"+tc.share+"</span>") {
			t.Errorf("%s access log does not say %q", tc.language, tc.share)
		}
	}
	// The edit field holds what is stored, so saving the page keeps it unnamed.
	if page := get("/resources/"+resource.ID, "en"); strings.Contains(page, `name="name" value="Untitled resource"`) {
		t.Error("the placeholder leaked into the name field")
	}
}
