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

	"plainmote/internal/store"
)

func sampleEnvelope() []byte {
	envelope := append([]byte("PMe1"), 0)
	envelope = append(envelope, bytes.Repeat([]byte{2}, 12)...)
	return append(envelope, bytes.Repeat([]byte{3}, 48)...)
}

func (c accountClient) postEnvelope(csrf string, envelope []byte) *httptest.ResponseRecorder {
	c.t.Helper()
	body := new(bytes.Buffer)
	form := multipart.NewWriter(body)
	_ = form.WriteField("csrf", csrf)
	_ = form.WriteField("ttl", "5")
	part, _ := form.CreateFormFile("envelope", "envelope")
	_, _ = part.Write(envelope)
	_ = form.Close()
	request := httptest.NewRequest(http.MethodPost, "https://cfg.test"+pasteEncryptedPath, body)
	request.Header.Set("Content-Type", form.FormDataContentType())
	request.Header.Set("Origin", "https://cfg.test")
	if c.session != "" {
		request.AddCookie(&http.Cookie{Name: sessionCookie, Value: c.session})
		request.AddCookie(&http.Cookie{Name: csrfCookie, Value: c.csrf})
	}
	response := httptest.NewRecorder()
	c.app.handler.ServeHTTP(response, request)
	return response
}

// The setting is off until the account turns it on, and only then does the
// home page encrypt - with the button disabled until the script runs.
func TestE2EESettingChangesTheHomePage(t *testing.T) {
	db, user, _ := testDatabase(t)
	client := signedIn(t, db, user)

	if page := client.do(http.MethodGet, "/", nil).Body.String(); strings.Contains(page, "data-e2ee") {
		t.Fatal("encryption must be off by default")
	}
	account := client.do(http.MethodGet, accountPath, nil).Body.String()
	for _, want := range []string{`action="/account/e2ee"`, `name="e2ee" value="on"`, "curl"} {
		if !strings.Contains(account, want) {
			t.Errorf("the account page is missing %q", want)
		}
	}
	if got := client.do(http.MethodPost, accountE2EEPath, url.Values{"csrf": {"wrong"}, "e2ee": {"on"}}); got.Code != http.StatusForbidden {
		t.Fatalf("a toggle without the CSRF token must be refused, got %d", got.Code)
	}
	if got := client.do(http.MethodPost, accountE2EEPath, url.Values{"csrf": {client.csrf}, "e2ee": {"on"}}); got.Code != http.StatusSeeOther {
		t.Fatalf("toggle: %d", got.Code)
	}
	page := client.do(http.MethodGet, "/", nil).Body.String()
	for _, want := range []string{"data-e2ee", `data-endpoint="/paste/encrypted"`, `data-passphrase`, `class="pri end" disabled`, "/static/e2ee-share.js"} {
		if !strings.Contains(page, want) {
			t.Errorf("the encrypting home page is missing %q", want)
		}
	}
	if strings.Contains(page, `name="passphrase"`) {
		t.Error("the passphrase must never be a form field")
	}
}

// Only a signed-in account with the setting on, with its CSRF token, can post
// ciphertext - and only ciphertext.
func TestEncryptedPasteEndpoint(t *testing.T) {
	db, user, _ := testDatabase(t)
	client := signedIn(t, db, user)

	anonymous := accountClient{t: t, app: client.app}
	if got := anonymous.postEnvelope("", sampleEnvelope()); got.Code != http.StatusUnauthorized {
		t.Fatalf("signed out: %d", got.Code)
	}
	if got := client.postEnvelope(client.csrf, sampleEnvelope()); got.Code != http.StatusForbidden {
		t.Fatalf("setting off: %d", got.Code)
	}
	if err := db.SetE2EE(context.Background(), user.ID, true); err != nil {
		t.Fatal(err)
	}
	if got := client.postEnvelope("wrong", sampleEnvelope()); got.Code != http.StatusForbidden {
		t.Fatalf("bad csrf: %d", got.Code)
	}
	if got := client.postEnvelope(client.csrf, []byte("port: 7890\n")); got.Code != http.StatusBadRequest {
		t.Fatalf("plain text posted as an envelope: %d", got.Code)
	}
	created := client.postEnvelope(client.csrf, sampleEnvelope())
	if created.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	var answer struct{ Result string }
	if err := json.Unmarshal(created.Body.Bytes(), &answer); err != nil || !strings.HasPrefix(answer.Result, pasteResultPrefix) {
		t.Fatalf("answer %q %v", created.Body.String(), err)
	}

	// The result page marks it and offers no save.
	result := client.do(http.MethodGet, answer.Result, nil).Body.String()
	for _, want := range []string{"data-e2ee-result", "data-e2ee-lost", "/static/e2ee-share.js", "Encrypted shares cannot be saved as resources"} {
		if !strings.Contains(result, want) {
			t.Errorf("the result page is missing %q", want)
		}
	}
	if strings.Contains(result, `action="/paste/save"`) {
		t.Error("an encrypted share must not be offered as a resource")
	}
}

// With the setting on, the box's own form never stores text in the clear -
// that is what reaches the server when the page's script did not run.
func TestE2EEAccountRefusesPlaintextFromTheBox(t *testing.T) {
	db, user, _ := testDatabase(t)
	client := signedIn(t, db, user)
	if err := db.SetE2EE(context.Background(), user.ID, true); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "https://cfg.test/paste", strings.NewReader(url.Values{"content": {"secret"}}.Encode()))
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
	app := newTestApp(db, user.GitHubID)
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
	for _, want := range []string{"data-decrypt", "/static/e2ee-open.js", "?raw=1"} {
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
	logs, _ := db.ListAccess(ctx, store.AnonymousUserID, "", "", 10)
	if len(logs) != 1 {
		t.Fatalf("only the fetch for the bytes is a use, found %d log rows", len(logs))
	}
}
