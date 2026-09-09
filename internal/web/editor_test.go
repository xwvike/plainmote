package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"plainmote/internal/upstream"
)

// TestResourcePageStaysPostable is the guarantee the whole editor rests on: the
// page a browser without JavaScript receives must still be a textarea in a form.
// The editor only ever upgrades that markup, so if this changes, saving breaks
// for every client that does not run the module.
func TestResourcePageStaysPostable(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	app := &App{db: db, upstream: upstream.New(true, 4<<20), cfg: Config{
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
	if !strings.Contains(page, `aria-label="资源内容"`) {
		t.Error("the editor source must provide an accessible name")
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
	if uploadPage := createPage("/resources/new"); !strings.Contains(uploadPage, `/static/editor.js`) {
		t.Error("the upload form must load the editor")
	}
	if remotePage := createPage("/resources/new?kind=remote"); strings.Contains(remotePage, `/static/editor.js`) {
		t.Error("the remote resource form must not load the editor")
	}
	opaque, err := db.CreateResource(ctx, user.ID, "Image", "image.png", []byte("not text"), "")
	if err != nil {
		t.Fatal(err)
	}
	if opaquePage := createPage("/resources/" + opaque.ID); strings.Contains(opaquePage, `/static/editor.js`) {
		t.Error("a non-text resource page must not load the editor")
	}
}
