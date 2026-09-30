package web

import (
	"bytes"
	"context"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// pngBytes is a real one-pixel PNG, so detection and the preview both see
// something a browser would actually accept.
var pngBytes = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0a, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
	0x42, 0x60, 0x82,
}

// TestUploadedBinarySurvivesTheRoundTrip is the reason binary content is not
// shown in a textarea: a form round-trip rewrites every byte it cannot
// represent, so the file would come back corrupted.
func TestUploadedBinarySurvivesTheRoundTrip(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	app := newTestApp(db, user.GitHubID)
	session, csrf, _, err := db.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	signedIn := func(request *http.Request) *httptest.ResponseRecorder {
		t.Helper()
		request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
		request.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrf})
		response := httptest.NewRecorder()
		app.Handler().ServeHTTP(response, request)
		return response
	}

	// Upload the file the way the form does.
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	_ = form.WriteField("csrf", csrf)
	_ = form.WriteField("kind", "upload")
	_ = form.WriteField("name", "站标")
	_ = form.WriteField("filename", "logo.png")
	part, err := form.CreateFormFile("upload", "logo.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(pngBytes); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	upload := httptest.NewRequest(http.MethodPost, "https://cfg.test/resources/new", &body)
	upload.Header.Set("Content-Type", form.FormDataContentType())
	created := signedIn(upload)
	if created.Code != http.StatusSeeOther {
		t.Fatalf("upload: %d %s", created.Code, created.Body.String())
	}

	list, _, err := db.ListResources(ctx, user.ID, "logo.png", 10, 0)
	if err != nil || len(list) != 1 {
		t.Fatalf("expected the uploaded resource: %d %v", len(list), err)
	}
	stored, err := db.ResourceForOwner(ctx, user.ID, list[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	storedBytes, err := db.ReadContent(ctx, stored)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(storedBytes, pngBytes) {
		t.Fatalf("upload changed the bytes: %d in, %d stored", len(pngBytes), len(storedBytes))
	}
	if stored.ContentSize != int64(len(pngBytes)) {
		t.Fatalf("recorded size %d, want %d", stored.ContentSize, len(pngBytes))
	}
	if stored.ContentType != "image/png" {
		t.Fatalf("stored as %q, want image/png", stored.ContentType)
	}
	if stored.Editable() {
		t.Fatal("a PNG must not be offered to the text editor")
	}

	// The page shows a preview instead of a textarea, so there is no way to
	// submit mangled bytes back.
	page := signedIn(httptest.NewRequest(http.MethodGet, "https://cfg.test/resources/"+stored.ID, nil)).Body.String()
	if strings.Contains(page, `<textarea name="content"`) {
		t.Fatal("a binary resource must not render an editable textarea")
	}
	if !strings.Contains(page, `src="/resources/`+stored.ID+`/raw"`) {
		t.Fatal("expected an inline preview of the image")
	}
	if !strings.Contains(page, `class="resource-media resource-image"`) || !strings.Contains(page, `/static/upload.js`) {
		t.Fatal("the image page must use the media preview without loading the editor")
	}

	// Saving the form again, with no file chosen, must leave the bytes alone.
	resave := url.Values{"csrf": {csrf}, "name": {"站标"}, "filename": {"logo.png"}}
	request := httptest.NewRequest(http.MethodPost, "https://cfg.test/resources/"+stored.ID, strings.NewReader(resave.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if got := signedIn(request); got.Code != http.StatusSeeOther {
		t.Fatalf("resave: %d %s", got.Code, got.Body.String()[:200])
	}
	after, err := db.ResourceForOwner(ctx, user.ID, stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterBytes, err := db.ReadContent(ctx, after)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterBytes, pngBytes) {
		t.Fatalf("saving the page corrupted the file: %d bytes left of %d", len(afterBytes), len(pngBytes))
	}
	if after.ContentKey != stored.ContentKey {
		t.Fatalf("metadata-only save replaced the object: %q -> %q", stored.ContentKey, after.ContentKey)
	}

	// The preview endpoint belongs to the owner and serves an inert type.
	raw := signedIn(httptest.NewRequest(http.MethodGet, "https://cfg.test/resources/"+stored.ID+"/raw", nil))
	if raw.Code != http.StatusOK || !bytes.Equal(raw.Body.Bytes(), pngBytes) {
		t.Fatalf("raw preview: %d, %d bytes", raw.Code, raw.Body.Len())
	}
	if raw.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("the preview must be served with nosniff")
	}
	anonymous := httptest.NewRecorder()
	app.Handler().ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, "https://cfg.test/resources/"+stored.ID+"/raw", nil))
	if anonymous.Code == http.StatusOK {
		t.Fatal("the preview must not be readable without signing in")
	}

	// And it is delivered publicly with the right type.
	share, err := db.CreateShare(ctx, user.ID, stored.ID, "s", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	delivered := httptest.NewRecorder()
	app.Handler().ServeHTTP(delivered, httptest.NewRequest(http.MethodGet,
		"https://cfg.test/d/"+url.PathEscape(share.Token)+"/logo.png", nil))
	if delivered.Code != http.StatusOK || !bytes.Equal(delivered.Body.Bytes(), pngBytes) {
		t.Fatalf("public delivery: %d, %d bytes", delivered.Code, delivered.Body.Len())
	}
	if got := delivered.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("served as %q", got)
	}
}

func TestSavedVideoLoadsMetadataAndFirstFrame(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	video, err := db.CreateResource(ctx, user.ID, "Clip", "clip.mp4", []byte("video\x00fixture"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if video.ContentType != "video/mp4" {
		t.Fatalf("video stored as %q", video.ContentType)
	}
	session, _, _, err := db.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "https://cfg.test/resources/"+video.ID, nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	response := httptest.NewRecorder()
	newTestApp(db, user.GitHubID).Handler().ServeHTTP(response, request)
	page := response.Body.String()
	if response.Code != http.StatusOK {
		t.Fatalf("video page: %d %q", response.Code, page)
	}
	if !strings.Contains(page, `preload="metadata"`) || !strings.Contains(page, `data-prime-video`) {
		t.Fatal("saved video must load its duration and prime the first frame")
	}
}
