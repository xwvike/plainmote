package web

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"plainmote/internal/store"
	"plainmote/internal/upstream"
)

// TestResourcePageStaysPostable is the guarantee the whole editor rests on: the
// page a browser without JavaScript receives must still be a textarea in a form.
// The editor only ever upgrades that markup, so if this changes, saving breaks
// for every client that does not run the module.
func TestResourcePageStaysPostable(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	app := &App{db: db, upstream: upstream.New(4 << 20), cfg: Config{
		MaxContent: 4 << 20, PublicURL: "https://cfg.test",
		AllowedIDs: map[string]bool{user.GitHubID: true},
	}}
	app.templates = app.templateSet()
	app.handler = app.routes()
	session, csrf, _, err := db.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "https://cfg.test/resources/"+resource.ID, nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	request.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrf})
	recorder := httptest.NewRecorder()
	app.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", recorder.Code)
	}
	page := recorder.Body.String()

	if !strings.Contains(page, `<textarea name="content"`) {
		t.Error("the content textarea must be in the served markup, not built by script")
	}
	if !strings.Contains(page, `data-editor data-filename="example.conf" data-content-type="text/plain; charset=utf-8"`) {
		t.Error("the textarea must carry the source metadata used by the browser editor")
	}
	if !strings.Contains(page, `aria-label="Resource content"`) {
		t.Error("the editor source must provide an accessible name")
	}
	if !strings.Contains(page, `data-encoding-select aria-label="File Encoding"`) ||
		!strings.Contains(page, `data-reopen-encoding`) ||
		!strings.Contains(page, `>Reopen with Encoding</button>`) ||
		!strings.Contains(page, `<span>End of Line Sequence</span>`) {
		t.Error("the editor must expose its file encoding, reopen, and end-of-line controls")
	}
	if !strings.Contains(page, `<script type="module" src="/static/editor.js">`) {
		t.Error("the editor module must be linked")
	}
	// cm-source is what editor.js adds once it is running. Rendering it here
	// would hide the textarea in browsers where the module never runs.
	if strings.Contains(page, "cm-source") {
		t.Error("the served markup must never hide the textarea itself")
	}

	createPage := func(path string) string {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "https://cfg.test"+path, nil)
		request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
		request.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrf})
		recorder := httptest.NewRecorder()
		app.handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: status %d, want 200", path, recorder.Code)
		}
		return recorder.Body.String()
	}
	if uploadPage := createPage("/resources/new"); !strings.Contains(uploadPage, `/static/editor.js`) || !strings.Contains(uploadPage, `/static/upload.js`) {
		t.Error("the upload form must load the editor")
	} else {
		if !strings.Contains(uploadPage, `data-upload data-max-bytes="4194304"`) {
			t.Error("the file input must expose the upload hook and configured size limit")
		}
		if !strings.Contains(uploadPage, `data-upload-status role="status" aria-live="polite"`) {
			t.Error("the upload form must include an accessible preview status")
		}
	}
	if remotePage := createPage("/resources/new?kind=remote"); strings.Contains(remotePage, `/static/editor.js`) || strings.Contains(remotePage, `/static/upload.js`) {
		t.Error("the remote resource form must not load the editor")
	}
	opaque, err := db.CreateResource(ctx, user.ID, "Image", "image.png", []byte("not text"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	if opaquePage := createPage("/resources/" + opaque.ID); strings.Contains(opaquePage, `/static/editor.js`) {
		t.Error("a non-text resource page must not load the editor")
	} else if !strings.Contains(opaquePage, `/static/upload.js`) || !strings.Contains(opaquePage, `class="media-stage" data-current-content`) {
		t.Error("a non-text resource page must load the lightweight upload preview")
	}
	legacyText := "名称: 上海节点\n说明: 中文配置文件\n"
	legacyBytes, legacyEncoding, err := store.EncodeText(legacyText, "gb18030")
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := db.CreateResource(ctx, user.ID, "Legacy text", "legacy.yaml", legacyBytes, legacyEncoding, "")
	if err != nil {
		t.Fatal(err)
	}
	if legacy.ContentEncoding != "gb18030" || !legacy.Editable() {
		t.Fatalf("legacy text must retain its source encoding: %+v", legacy)
	}
	legacyPage := createPage("/resources/" + legacy.ID)
	if !strings.Contains(legacyPage, legacyText) || !strings.Contains(legacyPage, `data-encoding="gb18030"`) {
		t.Error("legacy text must be decoded into the editor with its source encoding")
	}
}

func TestEveryEditorEncodingIsSupportedByTheStore(t *testing.T) {
	for _, option := range editorEncodingOptions {
		encoded, canonical, err := store.EncodeText("plain ASCII\n", option.Value)
		if err != nil {
			t.Errorf("%s cannot be encoded: %v", option.Value, err)
			continue
		}
		decoded, detected, err := store.DecodeText(encoded, canonical)
		if err != nil || detected != option.Value || decoded != "plain ASCII\n" {
			t.Errorf("%s round-trip: detected=%q decoded=%q err=%v", option.Value, detected, decoded, err)
		}
	}
}

func TestLegacyTextKeepsBytesUntilEdited(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	originalText := "名称: 上海节点\n说明: 中文配置文件\n"
	original, encodingName, err := store.EncodeText(originalText, "gb18030")
	if err != nil {
		t.Fatal(err)
	}
	resource, err := db.CreateResource(ctx, user.ID, "Legacy", "legacy.yaml", original, encodingName, "")
	if err != nil {
		t.Fatal(err)
	}
	originalKey := resource.ContentKey

	app := newTestApp(db, user.GitHubID)
	session, csrf, _, err := db.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	post := func(values url.Values) *httptest.ResponseRecorder {
		t.Helper()
		values.Set("csrf", csrf)
		request := httptest.NewRequest(http.MethodPost, "https://cfg.test/resources/"+resource.ID, strings.NewReader(values.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
		request.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrf})
		response := httptest.NewRecorder()
		app.Handler().ServeHTTP(response, request)
		return response
	}

	// This is what enhanced browsers submit when only metadata changed: no
	// content field, so the object must remain byte-for-byte untouched.
	metadataOnly := post(url.Values{
		"name":             {"Renamed"},
		"filename":         {"legacy.yaml"},
		"content_encoding": {"windows-1252"},
	})
	if metadataOnly.Code != http.StatusSeeOther {
		t.Fatalf("metadata update: %d %s", metadataOnly.Code, metadataOnly.Body.String())
	}
	stored, err := db.ResourceForOwner(ctx, user.ID, resource.ID)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := db.ReadContent(ctx, stored)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ContentKey != originalKey || stored.ContentEncoding != encodingName || !bytes.Equal(unchanged, original) {
		t.Fatalf("metadata update changed source format: key %q -> %q, encoding %q -> %q, bytes %x -> %x", originalKey, stored.ContentKey, encodingName, stored.ContentEncoding, original, unchanged)
	}

	editedText := originalText + "状态: 可用\n"
	editedResponse := post(url.Values{
		"name":             {"Renamed"},
		"filename":         {"legacy.yaml"},
		"content":          {editedText},
		"content_encoding": {encodingName},
	})
	if editedResponse.Code != http.StatusSeeOther {
		t.Fatalf("content update: %d %s", editedResponse.Code, editedResponse.Body.String())
	}
	stored, err = db.ResourceForOwner(ctx, user.ID, resource.ID)
	if err != nil {
		t.Fatal(err)
	}
	want, _, err := store.EncodeText(editedText, encodingName)
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.ReadContent(ctx, stored)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ContentEncoding != encodingName || !bytes.Equal(got, want) {
		t.Fatalf("edited legacy text was not saved as %s: resource=%+v bytes=%x want=%x", encodingName, stored, got, want)
	}

	share, err := db.CreateShare(ctx, user.ID, resource.ID, "encoding", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	delivered := httptest.NewRecorder()
	app.Handler().ServeHTTP(delivered, httptest.NewRequest(http.MethodGet, "https://cfg.test/d/"+url.PathEscape(share.Token)+"/legacy.yaml", nil))
	if delivered.Code != http.StatusOK || !bytes.Equal(delivered.Body.Bytes(), want) {
		t.Fatalf("legacy share changed bytes: %d %x", delivered.Code, delivered.Body.Bytes())
	}
	if gotType := delivered.Header().Get("Content-Type"); gotType != "application/yaml; charset=gb18030" {
		t.Fatalf("legacy share content type = %q", gotType)
	}
}

func TestEditorSavesSelectedEncodingAndLineEnding(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	const text = "name: 示例\nstatus: enabled\n"
	resource, err := db.CreateResource(ctx, user.ID, "Config", "config.yaml", []byte(text), "utf-8", "")
	if err != nil {
		t.Fatal(err)
	}
	originalKey := resource.ContentKey

	app := newTestApp(db, user.GitHubID)
	session, csrf, _, err := db.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	values := url.Values{
		"csrf":             {csrf},
		"name":             {resource.Name},
		"filename":         {resource.Filename},
		"content":          {text},
		"content_encoding": {"utf-16le-bom"},
		"content_eol":      {"crlf"},
	}
	request := httptest.NewRequest(http.MethodPost, "https://cfg.test/resources/"+resource.ID, strings.NewReader(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	request.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrf})
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("save: %d %s", response.Code, response.Body.String())
	}

	stored, err := db.ResourceForOwner(ctx, user.ID, resource.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := db.ReadContent(ctx, stored)
	if err != nil {
		t.Fatal(err)
	}
	want, encodingName, err := store.EncodeText(store.ApplyEOL(text, store.EOLCRLF), "utf-16le-bom")
	if err != nil {
		t.Fatal(err)
	}
	if stored.ContentKey == originalKey {
		t.Fatal("changing encoding and line endings must replace the stored object")
	}
	if stored.ContentEncoding != encodingName || !bytes.Equal(got, want) {
		t.Fatalf("saved format = encoding:%q bytes:%x, want encoding:%q bytes:%x", stored.ContentEncoding, got, encodingName, want)
	}
	decoded, _, err := store.DecodeText(got, stored.ContentEncoding)
	if err != nil {
		t.Fatal(err)
	}
	if decoded != "name: 示例\r\nstatus: enabled\r\n" {
		t.Fatalf("decoded content = %q", decoded)
	}
}
