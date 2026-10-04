package web

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"plainmote/internal/store"
)

func fakeSealed(magic string, mark byte, n int) []byte {
	return append([]byte(magic), bytes.Repeat([]byte{mark}, n)...)
}

// sealedPost sends a multipart form the way sealed.js does: text fields and
// file parts, with the session and its CSRF token.
func (c *versionClient) sealedPost(target string, fields map[string]string, files map[string][]byte, header ...string) *httptest.ResponseRecorder {
	c.t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("csrf", c.csrf)
	for k, v := range fields {
		_ = writer.WriteField(k, v)
	}
	for k, v := range files {
		part, _ := writer.CreateFormFile(k, "blob")
		_, _ = part.Write(v)
	}
	_ = writer.Close()
	request := httptest.NewRequest(http.MethodPost, "https://cfg.test"+target, &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Accept-Language", "en")
	for i := 0; i+1 < len(header); i += 2 {
		request.Header.Set(header[i], header[i+1])
	}
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: c.session})
	request.AddCookie(&http.Cookie{Name: csrfCookie, Value: c.csrf})
	response := httptest.NewRecorder()
	c.app.handler.ServeHTTP(response, request)
	return response
}

var b64 = base64.RawURLEncoding.EncodeToString

// The sealed endpoints, end to end: create, save, link, deliver, and what a
// stranger, another site or a missing token gets.
func TestSealedResourceThroughThePage(t *testing.T) {
	db, user, _ := testDatabase(t)
	client := newVersionClient(t, db, user)
	ctx := context.Background()
	id := uuid.NewString()
	content, meta := fakeSealed("PMr1", 1, 64), fakeSealed("PMm1", 1, 40)

	fields := map[string]string{"id": id, "meta": b64(meta), "sealed_key": b64(bytes.Repeat([]byte{2}, 60))}
	if got := client.sealedPost("/resources/sealed", fields, map[string][]byte{"content": content}, "Origin", "https://evil.test"); got.Code != http.StatusForbidden {
		t.Fatalf("from another site: %d", got.Code)
	}
	created := client.sealedPost("/resources/sealed", fields, map[string][]byte{"content": content})
	if created.Code != http.StatusCreated || !strings.Contains(created.Body.String(), `"/resources/`+id+`?created=1"`) {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	if again := client.sealedPost("/resources/sealed", fields, map[string][]byte{"content": content}); again.Code != http.StatusBadRequest {
		t.Fatalf("the same id again: %d", again.Code)
	}

	// The page shows it locked, with nothing of its content or name.
	page := client.do(http.MethodGet, "/resources/"+id, nil).Body.String()
	if !strings.Contains(page, "data-sealed") || strings.Contains(page, `name="content"`) {
		t.Fatal("an encrypted resource's page")
	}

	saved := client.sealedPost("/resources/"+id+"/sealed", map[string]string{"action": "save", "base_version": "1", "meta": b64(fakeSealed("PMm1", 3, 40))},
		map[string][]byte{"content": fakeSealed("PMr1", 3, 64)})
	if saved.Code != http.StatusOK || !strings.Contains(saved.Body.String(), "saved=2") {
		t.Fatalf("save: %d %s", saved.Code, saved.Body.String())
	}
	stale := client.sealedPost("/resources/"+id+"/sealed", map[string]string{"action": "save", "base_version": "1", "meta": b64(meta)},
		map[string][]byte{"content": content})
	if stale.Code != http.StatusConflict || !strings.Contains(stale.Body.String(), `"current_version":2`) {
		t.Fatalf("a stale save: %d %s", stale.Code, stale.Body.String())
	}

	// Its history pages hold only ciphertext and what opens it in the
	// browser - never a name.
	for _, path := range []string{"/resources/" + id + "/versions", "/resources/" + id + "/versions/1", "/resources/" + id + "/versions/compare?from=1&to=2"} {
		got := client.do(http.MethodGet, path, nil)
		body := got.Body.String()
		if got.Code != http.StatusOK || !strings.Contains(body, "data-sealed-page") || !strings.Contains(body, assetPath("sealed-view.js")) {
			t.Fatalf("%s: %d", path, got.Code)
		}
	}
	if compare := client.do(http.MethodGet, "/resources/"+id+"/versions/compare?from=1&to=2", nil).Body.String(); !strings.Contains(compare, "data-sealed-compare") {
		t.Fatal("the comparison is not left to the browser")
	}

	// A plain link is refused; a link with keys is made under its own id.
	plain := client.do(http.MethodPost, "/resources/"+id+"/share", map[string][]string{"action": {"create"}})
	if plain.Code != http.StatusSeeOther || !strings.Contains(plain.Header().Get("Location"), "error=") {
		t.Fatalf("a plain link: %d %q", plain.Code, plain.Header().Get("Location"))
	}
	linkID := uuid.NewString()
	sealedLink := client.do(http.MethodPost, "/resources/"+id+"/share", map[string][]string{
		"action": {"create"}, "link_id": {linkID}, "sealed_key": {b64(bytes.Repeat([]byte{7}, 60))}, "owner_key": {b64(bytes.Repeat([]byte{8}, 60))},
	})
	if sealedLink.Code != http.StatusSeeOther || strings.Contains(sealedLink.Header().Get("Location"), "error=") {
		t.Fatalf("a sealed link: %d %q", sealedLink.Code, sealedLink.Header().Get("Location"))
	}
	links, _ := db.ListShares(ctx, user.ID, id, time.Now().UTC())
	if len(links) != 1 || links[0].ID != linkID {
		t.Fatalf("links: %+v", links)
	}
	address := "https://cfg.test" + shareAddress(links[0].Token, "file")

	// A browser gets the decryption page and spends nothing; anything else
	// gets the bundle, which is ciphertext.
	browser := httptest.NewRequest(http.MethodGet, address, nil)
	browser.Header.Set("Accept", "text/html,application/xhtml+xml")
	shell := httptest.NewRecorder()
	client.app.handler.ServeHTTP(shell, browser)
	if shell.Code != http.StatusOK || !strings.Contains(shell.Body.String(), "data-decrypt") {
		t.Fatalf("the browser's page: %d", shell.Code)
	}
	raw := httptest.NewRecorder()
	client.app.handler.ServeHTTP(raw, httptest.NewRequest(http.MethodGet, address, nil))
	got, _ := io.ReadAll(raw.Body)
	want, _ := store.SealedBundle(id, bytes.Repeat([]byte{7}, 60), fakeSealed("PMm1", 3, 40))
	want = append(want, fakeSealed("PMr1", 3, 64)...)
	if raw.Code != http.StatusOK || !bytes.Equal(got, want) || raw.Header().Get("Content-Length") != itoa(len(want)) {
		t.Fatalf("the bundle: %d %q", raw.Code, got)
	}
	if after, _ := db.ListShares(ctx, user.ID, id, time.Now().UTC()); after[0].UsedCount != 1 {
		t.Fatalf("uses: %d", after[0].UsedCount)
	}

	// The index names it - encrypted - to its owner and to nobody else.
	index := client.do(http.MethodGet, "/resources/sealed", nil)
	var listed struct {
		Resources []struct{ ID, SealedMeta string } `json:"resources"`
	}
	if err := json.Unmarshal(index.Body.Bytes(), &listed); err != nil || len(listed.Resources) != 1 || listed.Resources[0].ID != id {
		t.Fatalf("index: %s", index.Body.String())
	}
	other, _ := db.UpsertUser(ctx, "200", "bob", "Bob", "")
	stranger := newVersionClient(t, db, other)
	if theirs := stranger.do(http.MethodGet, "/resources/sealed", nil); strings.Contains(theirs.Body.String(), id) {
		t.Fatal("another account's index lists it")
	}
	if got := stranger.sealedPost("/resources/"+id+"/sealed", map[string]string{"action": "save", "base_version": "2", "meta": b64(meta)},
		map[string][]byte{"content": content}); got.Code != http.StatusNotFound {
		t.Fatalf("another account's save: %d", got.Code)
	}
}

// Turning encryption on and off through the page carries every version.
func TestSealAndUnsealThroughThePage(t *testing.T) {
	db, user, resource := testDatabase(t)
	client := newVersionClient(t, db, user)
	ctx := context.Background()
	if _, err := db.SaveResource(ctx, user.ID, resource.ID, store.ResourceEdit{Name: "Example", Filename: "example.conf", Content: []byte("answer=43\n")}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateShare(ctx, user.ID, resource.ID, "", time.Hour, 0); err != nil {
		t.Fatal(err)
	}
	seal := client.sealedPost("/resources/"+resource.ID+"/sealed", map[string]string{
		"action": "seal", "base_version": "2", "meta": b64(fakeSealed("PMm1", 1, 40)), "sealed_key": b64(bytes.Repeat([]byte{2}, 60)),
		"version_1_meta": b64(fakeSealed("PMm1", 2, 40)),
	}, map[string][]byte{"content": fakeSealed("PMr1", 1, 64), "version_1_content": fakeSealed("PMr1", 2, 64)})
	if seal.Code != http.StatusOK || !strings.Contains(seal.Body.String(), `"revoked":1`) {
		t.Fatalf("seal: %d %s", seal.Code, seal.Body.String())
	}
	if sealed, _ := db.ResourceForOwner(ctx, user.ID, resource.ID); !sealed.Sealed() || sealed.Name != "" {
		t.Fatalf("sealed: %+v", sealed)
	}
	unseal := client.sealedPost("/resources/"+resource.ID+"/sealed", map[string]string{
		"action": "unseal", "base_version": "2", "name": "Example", "filename": "example.conf", "version_1_filename": "example.conf",
	}, map[string][]byte{"content": []byte("answer=43\n"), "version_1_content": []byte("answer=42\n")})
	if unseal.Code != http.StatusOK {
		t.Fatalf("unseal: %d %s", unseal.Code, unseal.Body.String())
	}
	plain, _ := db.ResourceForOwner(ctx, user.ID, resource.ID)
	body, _ := db.ReadContent(ctx, plain)
	if plain.Sealed() || plain.Name != "Example" || string(body) != "answer=43\n" {
		t.Fatalf("unsealed: %+v %q", plain, body)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

// The command line's side of encryption: the keyring as it is kept, and
// encrypted resources created, listed, read and saved as ciphertext - each
// account's own, and only with a write token for what writes.
func TestSealedResourcesThroughTheAPI(t *testing.T) {
	db, user, _ := testDatabase(t)
	app := newVersionClient(t, db, user).app
	ctx := context.Background()
	reader := issueToken(t, db, user, store.TokenScopeRead)
	writer := issueToken(t, db, user, store.TokenScopeWrite)
	other, _ := db.UpsertUser(ctx, "200", "bob", "Bob", "")
	foreign := issueToken(t, db, other, store.TokenScopeWrite)

	if got := callAPI(t, app, apiCall{method: http.MethodGet, path: apiPrefix + "keyring", token: reader}); got.Code != http.StatusNotFound {
		t.Fatalf("no keyring yet: %d", got.Code)
	}
	salt, wrapped := bytes.Repeat([]byte{1}, 16), bytes.Repeat([]byte{2}, 60)
	if _, err := db.CreateKeyring(ctx, user.ID, store.Keyring{KDF: store.KeyringKDF, Iterations: 600000, Salt: salt,
		WrappedByPassword: wrapped, WrappedByRecovery: bytes.Repeat([]byte{3}, 60)}, time.Now()); err != nil {
		t.Fatal(err)
	}
	ring := callAPI(t, app, apiCall{method: http.MethodGet, path: apiPrefix + "keyring", token: reader})
	var view struct {
		UserID            string `json:"user_id"`
		Salt              string `json:"salt"`
		WrappedByPassword string `json:"wrapped_by_password"`
	}
	_ = json.Unmarshal(ring.Body.Bytes(), &view)
	if ring.Code != http.StatusOK || view.UserID != user.ID || view.Salt != b64(salt) || view.WrappedByPassword != b64(wrapped) ||
		ring.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("keyring: %d %s", ring.Code, ring.Body.String())
	}
	if got := callAPI(t, app, apiCall{method: http.MethodGet, path: apiPrefix + "keyring", token: foreign}); got.Code != http.StatusNotFound || strings.Contains(got.Body.String(), b64(salt)) {
		t.Fatalf("another account's keyring: %d %s", got.Code, got.Body.String())
	}

	create := func(token string, values map[string]string, content []byte) *httptest.ResponseRecorder {
		var form bytes.Buffer
		w := multipart.NewWriter(&form)
		for k, v := range values {
			_ = w.WriteField(k, v)
		}
		part, _ := w.CreateFormFile("content", "blob")
		_, _ = part.Write(content)
		_ = w.Close()
		return callAPI(t, app, apiCall{method: http.MethodPost, path: apiPrefix + "resources", token: token, body: &form,
			header: map[string]string{"Content-Type": w.FormDataContentType()}})
	}
	id := uuid.NewString()
	content, meta := fakeSealed("PMr1", 1, 64), fakeSealed("PMm1", 1, 40)
	values := map[string]string{"id": id, "sealed_key": b64(bytes.Repeat([]byte{4}, 60)), "sealed_meta": b64(meta)}
	if got := create(reader, values, content); got.Code != http.StatusForbidden {
		t.Fatalf("create with a read token: %d", got.Code)
	}
	if got := create(writer, map[string]string{"id": "not-an-id", "sealed_key": values["sealed_key"], "sealed_meta": values["sealed_meta"]}, content); got.Code != http.StatusNotFound {
		t.Fatalf("create under a malformed id: %d %s", got.Code, got.Body.String())
	}
	if got := create(writer, values, []byte("plain text\n")); got.Code != http.StatusBadRequest {
		t.Fatalf("create with content that is not ciphertext: %d", got.Code)
	}
	created := create(writer, values, content)
	var resource apiResource
	if err := json.Unmarshal(created.Body.Bytes(), &resource); err != nil || created.Code != http.StatusCreated || resource.ID != id ||
		resource.Name != "" || !resource.Encrypted || resource.SealedMeta != b64(meta) || resource.Editable {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	if got := create(foreign, values, content); got.Code != http.StatusBadRequest {
		t.Fatalf("another account creating under the same id: %d", got.Code)
	}

	// Listed with what opens it, read as ciphertext.
	list := callAPI(t, app, apiCall{method: http.MethodGet, path: apiPrefix + "resources", token: reader})
	if !strings.Contains(list.Body.String(), `"sealed_key":"`+values["sealed_key"]+`"`) {
		t.Fatalf("list: %s", list.Body.String())
	}
	body := apiPrefix + "resources/" + id + "/content"
	if got := callAPI(t, app, apiCall{method: http.MethodGet, path: body, token: reader}); got.Code != http.StatusOK || !bytes.Equal(got.Body.Bytes(), content) {
		t.Fatalf("read: %d", got.Code)
	}
	if got := callAPI(t, app, apiCall{method: http.MethodGet, path: body, token: foreign}); got.Code != http.StatusNotFound {
		t.Fatalf("another account's read: %d", got.Code)
	}

	save := func(token, match, metaHeader string, content []byte) *httptest.ResponseRecorder {
		header := map[string]string{"If-Match": match}
		if metaHeader != "" {
			header["X-PlainMote-Sealed-Meta"] = metaHeader
		}
		return callAPI(t, app, apiCall{method: http.MethodPut, path: body, token: token, body: bytes.NewReader(content), header: header})
	}
	next, nextMeta := fakeSealed("PMr1", 5, 64), b64(fakeSealed("PMm1", 5, 40))
	for label, got := range map[string]*httptest.ResponseRecorder{
		"without its metadata":  save(writer, `"v1"`, "", next),
		"with plaintext":        save(writer, `"v1"`, nextMeta, []byte("SECRET=1\n")),
		"with metadata garbled": save(writer, `"v1"`, "!!", next),
	} {
		if got.Code != http.StatusBadRequest {
			t.Errorf("a save %s: %d %s", label, got.Code, got.Body.String())
		}
	}
	if got := save(reader, `"v1"`, nextMeta, next); got.Code != http.StatusForbidden {
		t.Fatalf("a save with a read token: %d", got.Code)
	}
	if got := save(foreign, "*", nextMeta, next); got.Code != http.StatusNotFound {
		t.Fatalf("another account's save: %d", got.Code)
	}
	if got := save(writer, `"v1"`, nextMeta, next); got.Code != http.StatusOK || !strings.Contains(got.Body.String(), `"version":2`) {
		t.Fatalf("save: %d %s", got.Code, got.Body.String())
	}
	if got := save(writer, `"v1"`, nextMeta, next); got.Code != http.StatusPreconditionFailed || !strings.Contains(got.Body.String(), `"current_version":2`) {
		t.Fatalf("a stale save: %d %s", got.Code, got.Body.String())
	}
	stored, _ := db.ResourceForOwner(ctx, user.ID, id)
	if got, _ := db.ReadContent(ctx, stored); !bytes.Equal(got, next) || b64(stored.SealedMeta) != nextMeta || stored.Version != 2 {
		t.Fatalf("stored: v%d %q", stored.Version, got)
	}
}
