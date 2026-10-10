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
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"plainmote/internal/store"
)

func sampleEnvelope() []byte {
	envelope := append([]byte("PMe1"), 0)
	envelope = append(envelope, bytes.Repeat([]byte{2}, 12)...)
	return append(envelope, bytes.Repeat([]byte{3}, 48)...)
}

// giveKeyring sets a master password on the account, as keyring.js would:
// the service only ever sees it wrapped.
func giveKeyring(t *testing.T, db *store.Store, userID string) {
	t.Helper()
	if _, err := db.CreateKeyring(context.Background(), userID, store.Keyring{KDF: store.KeyringKDF, Iterations: 600000,
		Salt: bytes.Repeat([]byte{1}, 16), WrappedByPassword: bytes.Repeat([]byte{2}, 60), WrappedByRecovery: bytes.Repeat([]byte{3}, 60)}, time.Now()); err != nil {
		t.Fatal(err)
	}
}

// sealedPasteFields is a quick share as e2ee-share.js posts it, with stand-in
// ciphertext of the right shape.
func sealedPasteFields() map[string]string {
	return map[string]string{
		"ttl": "1h", "id": uuid.NewString(), "link_id": uuid.NewString(), "meta": b64(fakeSealed("PMm1", 1, 40)),
		"sealed_key": b64(bytes.Repeat([]byte{4}, 60)), "link_key": b64(bytes.Repeat([]byte{5}, 60)), "owner_key": b64(bytes.Repeat([]byte{6}, 60)),
	}
}

func (c accountClient) postSealedPaste(csrf string, fields map[string]string, content []byte) *httptest.ResponseRecorder {
	c.t.Helper()
	body := new(bytes.Buffer)
	form := multipart.NewWriter(body)
	_ = form.WriteField("csrf", csrf)
	for key, value := range fields {
		_ = form.WriteField(key, value)
	}
	part, _ := form.CreateFormFile("content", "blob")
	_, _ = part.Write(content)
	_ = form.Close()
	request := httptest.NewRequest(http.MethodPost, "https://cfg.test"+pasteSealedPath, body)
	request.Header.Set("Content-Type", form.FormDataContentType())
	request.Header.Set("Origin", "https://cfg.test")
	request.Header.Set("Accept-Language", "en")
	if c.session != "" {
		request.AddCookie(&http.Cookie{Name: sessionCookie, Value: c.session})
		request.AddCookie(&http.Cookie{Name: csrfCookie, Value: c.csrf})
	}
	response := httptest.NewRecorder()
	c.app.handler.ServeHTTP(response, request)
	return response
}

// The home box's encryption switch: offered only with a master password,
// off until a quick share has been sent encrypted, and from then on starting
// the way the last one was sent. The account page has no setting for it.
func TestE2EESwitchInTheHomeBox(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	client := signedIn(t, db, user)

	page := client.do(http.MethodGet, "/", nil).Body.String()
	if strings.Contains(page, "data-e2ee") || strings.Contains(page, "data-keylock") || strings.Contains(page, `role="switch"`) {
		t.Fatal("without a master password the home box shows no encryption at all")
	}
	if account := client.do(http.MethodGet, accountSecurityPath, nil).Body.String(); strings.Contains(account, "/account/e2ee") || !strings.Contains(account, `data-keyring-open="setup"`) {
		t.Fatal("the account page offers a master password and no quick share setting")
	}

	giveKeyring(t, db, user.ID)
	page = client.do(http.MethodGet, "/", nil).Body.String()
	for _, want := range []string{"data-e2ee", `data-endpoint="/paste/sealed"`, `name="encrypt" value="on">`, `data-pin-pick hidden`, "data-e2ee-unlock", assetPath("e2ee-share.js"), "data-keylock"} {
		if !strings.Contains(page, want) {
			t.Errorf("the home page with a master password is missing %q", want)
		}
	}
	if strings.Contains(page, `name="passphrase"`) || strings.Contains(page, `name="pin"`) || strings.Contains(page, `name="password"`) {
		t.Error("neither the code nor the master password may ever be a form field")
	}

	// Sent encrypted: the box starts encrypted.
	if got := client.postSealedPaste(client.csrf, sealedPasteFields(), fakeSealed("PMr1", 1, 64)); got.Code != http.StatusCreated {
		t.Fatalf("sealed paste: %d %s", got.Code, got.Body.String())
	}
	if page := client.do(http.MethodGet, "/", nil).Body.String(); !strings.Contains(page, `name="encrypt" value="on" checked>`) || strings.Contains(page, "data-pin-pick hidden") {
		t.Fatal("the switch does not start where it was left")
	}
	// Sent in the clear from the box: it starts off again.
	request := httptest.NewRequest(http.MethodPost, "https://cfg.test/paste", strings.NewReader(url.Values{"content": {"hello"}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "text/html")
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: client.session})
	response := httptest.NewRecorder()
	client.app.handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("plain paste: %d", response.Code)
	}
	if on, _ := db.E2EEEnabled(ctx, user.ID); on {
		t.Fatal("a quick share sent in the clear leaves the switch on")
	}
}

// Only a signed-in account with a master password and the setting on, with
// its CSRF token, can post an encrypted quick share - and only one of the
// expected shape. Its owner then finds it with its link to show again, and
// can keep it as an encrypted resource.
func TestSealedPasteEndpoint(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	client := signedIn(t, db, user)
	content := fakeSealed("PMr1", 1, 64)

	anonymous := accountClient{t: t, app: client.app}
	if got := anonymous.postSealedPaste("", sealedPasteFields(), content); got.Code != http.StatusUnauthorized {
		t.Fatalf("signed out: %d", got.Code)
	}
	if err := db.SetE2EE(ctx, user.ID, true); err != nil {
		t.Fatal(err)
	}
	if got := client.postSealedPaste(client.csrf, sealedPasteFields(), content); got.Code != http.StatusForbidden {
		t.Fatalf("no master password: %d", got.Code)
	}
	giveKeyring(t, db, user.ID)
	if got := client.postSealedPaste("wrong", sealedPasteFields(), content); got.Code != http.StatusForbidden {
		t.Fatalf("bad csrf: %d", got.Code)
	}
	if got := client.postSealedPaste(client.csrf, sealedPasteFields(), []byte("port: 7890\n")); got.Code != http.StatusBadRequest {
		t.Fatalf("plain text: %d", got.Code)
	}
	short := sealedPasteFields()
	short["link_key"] = b64(bytes.Repeat([]byte{5}, 59))
	if got := client.postSealedPaste(client.csrf, short, content); got.Code != http.StatusBadRequest {
		t.Fatalf("a malformed link key: %d", got.Code)
	}

	fields := sealedPasteFields()
	created := client.postSealedPaste(client.csrf, fields, content)
	if created.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	var answer struct{ Result string }
	if err := json.Unmarshal(created.Body.Bytes(), &answer); err != nil || answer.Result != pasteResultPrefix+fields["id"] {
		t.Fatalf("answer %q %v", created.Body.String(), err)
	}
	if again := client.postSealedPaste(client.csrf, fields, content); again.Code != http.StatusBadRequest {
		t.Fatalf("the same ids again: %d", again.Code)
	}

	// The result page marks it and says the link can be shown again.
	result := client.do(http.MethodGet, answer.Result, nil).Body.String()
	for _, want := range []string{"data-e2ee-result", "data-e2ee-lost", "unlock on the resource page", assetPath("e2ee-share.js"), "/resources/" + fields["id"]} {
		if !strings.Contains(result, want) {
			t.Errorf("the result page is missing %q", want)
		}
	}

	// Its page: read only until kept, its link made whole in the browser,
	// its content opened there.
	page := client.do(http.MethodGet, "/resources/"+fields["id"], nil).Body.String()
	for _, want := range []string{"data-sealed data-read-only", `data-sealed-link="` + fields["link_id"] + `"`, `data-owner-key="` + fields["owner_key"] + `"`,
		`name="action" value="keep"`, assetPath("sealed.js")} {
		if !strings.Contains(page, want) {
			t.Errorf("the quick share's page is missing %q", want)
		}
	}
	if strings.Contains(page, "data-sealed-file") || strings.Contains(page, `class="savebar"`) {
		t.Error("a quick share not yet kept cannot be edited")
	}

	// Delivered as a bundle under its link's own key.
	links, _ := db.ListShares(ctx, user.ID, fields["id"], time.Now().UTC())
	if len(links) != 1 || links[0].ID != fields["link_id"] {
		t.Fatalf("links: %+v", links)
	}
	raw := httptest.NewRecorder()
	client.app.handler.ServeHTTP(raw, httptest.NewRequest(http.MethodGet, "https://cfg.test"+shareAddress(links[0].Token, "file"), nil))
	want, _ := store.SealedBundle(fields["id"], bytes.Repeat([]byte{5}, 60), fakeSealed("PMm1", 1, 40))
	if got, _ := io.ReadAll(raw.Body); raw.Code != http.StatusOK || !bytes.Equal(got, append(want, content...)) {
		t.Fatalf("delivered: %d %q", raw.Code, got)
	}

	// Another account sees none of it.
	other, _ := db.UpsertUser(ctx, "200", "bob", "Bob", "")
	stranger := signedIn(t, db, other)
	if got := stranger.do(http.MethodGet, "/resources/"+fields["id"], nil); got.Code != http.StatusNotFound {
		t.Fatalf("another account's view: %d", got.Code)
	}
	if got := stranger.do(http.MethodGet, answer.Result, nil).Body.String(); strings.Contains(got, fields["link_id"]) || strings.Contains(got, "/resources/"+fields["id"]) {
		t.Fatal("another account's view of the result page")
	}

	// Kept: an encrypted resource, its link among its links.
	if got := client.do(http.MethodPost, "/resources/"+fields["id"], url.Values{"csrf": {client.csrf}, "action": {"keep"}}); got.Code != http.StatusSeeOther {
		t.Fatalf("keep: %d", got.Code)
	}
	kept := client.do(http.MethodGet, "/resources/"+fields["id"], nil).Body.String()
	if strings.Contains(kept, "data-read-only") || !strings.Contains(kept, "data-sealed-file") || !strings.Contains(kept, `data-sealed-link="`+fields["link_id"]+`"`) {
		t.Fatal("the kept quick share is not an encrypted resource with its link")
	}

	// One opened by a code says so to its owner's page.
	coded := sealedPasteFields()
	coded["link_key"], coded["owner_key"] = b64(bytes.Repeat([]byte{5}, 76)), b64(bytes.Repeat([]byte{6}, 32))
	if got := client.postSealedPaste(client.csrf, coded, content); got.Code != http.StatusCreated {
		t.Fatalf("a code link: %d %s", got.Code, got.Body.String())
	}
	if page := client.do(http.MethodGet, "/resources/"+coded["id"], nil).Body.String(); !strings.Contains(page, "data-code-link") {
		t.Fatal("a code link is not marked")
	}
}

// With the setting on, the box's own form never stores text in the clear -
// that is what reaches the server when the page's script did not run.
func TestE2EEAccountRefusesPlaintextFromTheBox(t *testing.T) {
	db, user, _ := testDatabase(t)
	client := signedIn(t, db, user)
	giveKeyring(t, db, user.ID)
	request := httptest.NewRequest(http.MethodPost, "https://cfg.test/paste", strings.NewReader(url.Values{"content": {"secret"}, "encrypt": {"on"}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "text/html")
	request.Header.Set("Accept-Language", "en")
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: client.session})
	response := httptest.NewRecorder()
	client.app.handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "Enable JavaScript") {
		t.Fatalf("plain form: %d", response.Code)
	}
	quota, err := db.QuotaForUser(context.Background(), store.AnonymousUserID, time.Now().UTC())
	if err != nil || quota.Usage.Resources != 0 {
		t.Fatalf("nothing may have been stored: %d %v", quota.Usage.Resources, err)
	}
}

// A browser opening an encrypted link gets the decryption page, which costs no
// use; the page's fetch for the bytes is the counted request.
func TestEncryptedLinkOpensTheDecryptionPage(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	app := newTestApp(db)
	resource, link, err := db.CreateEncryptedPaste(ctx, user.ID, sampleEnvelope(), 0, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	address := shareAddress(link.Token, deliveryFilename(resource, resource.ContentType))

	open := httptest.NewRequest(http.MethodGet, "https://cfg.test"+address, nil)
	open.Header.Set("Accept", "text/html,application/xhtml+xml")
	page := httptest.NewRecorder()
	app.handler.ServeHTTP(page, open)
	if page.Code != http.StatusOK || page.Header().Get("Content-Security-Policy") != decryptPagePolicy || page.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("decryption page: %d %q", page.Code, page.Header().Get("Content-Security-Policy"))
	}
	body := page.Body.String()
	for _, want := range []string{"data-decrypt", assetPath("e2ee-open.js"), "?raw=1"} {
		if !strings.Contains(body, want) {
			t.Errorf("the decryption page is missing %q", want)
		}
	}
	if strings.Contains(body, "fonts.googleapis.com") || strings.Contains(body, "/theme?") || strings.Contains(body, "/language?") {
		t.Error("the decryption page must load nothing from elsewhere and carry its address nowhere")
	}

	raw := httptest.NewRecorder()
	app.handler.ServeHTTP(raw, httptest.NewRequest(http.MethodGet, "https://cfg.test"+address+"?raw=1", nil))
	got, _ := io.ReadAll(raw.Body)
	if raw.Code != http.StatusOK || !bytes.Equal(got, sampleEnvelope()) || raw.Header().Get("Content-Type") != store.EncryptedContentType {
		t.Fatalf("raw: %d %q", raw.Code, raw.Header().Get("Content-Type"))
	}
	logs, _ := db.ListAccess(ctx, user.ID, "", "", 10)
	if len(logs) != 1 {
		t.Fatalf("only the fetch for the bytes is a use, found %d log rows", len(logs))
	}
}

// The name at the end of an encrypted link says nothing, not even that it is
// encrypted - one encrypted the earlier way or under the master password.
func TestEncryptedLinksEndNeutrally(t *testing.T) {
	for _, contentType := range []string{store.EncryptedContentType, store.SealedContentType} {
		if got := deliveryFilename(store.Resource{ContentType: contentType}, contentType); got != "file" {
			t.Errorf("%s: %q", contentType, got)
		}
	}
}
