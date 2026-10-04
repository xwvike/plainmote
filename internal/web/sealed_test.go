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
