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

// uploadResource posts one file to the new-resource screen the way the browser
// does, with the filename field already carrying the file's own name - which
// is what editor.js fills in on selection.
func uploadResource(t *testing.T, app *App, session, csrf, name, filename, body string) *httptest.ResponseRecorder {
	t.Helper()
	var payload bytes.Buffer
	writer := multipart.NewWriter(&payload)
	_ = writer.WriteField("csrf", csrf)
	_ = writer.WriteField("kind", "upload")
	_ = writer.WriteField("name", name)
	_ = writer.WriteField("filename", filename)
	_ = writer.WriteField("content_encoding", "utf-8")
	part, err := writer.CreateFormFile("upload", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "https://cfg.test/resources/new", &payload)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	request.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrf})
	response := httptest.NewRecorder()
	app.handler.ServeHTTP(response, request)
	return response
}

// TestRejectedUploadKeepsItsBody is the loss this screen must never cause. A
// browser cannot refill a file input, so if the re-rendered page does not also
// carry the bytes back, everything the upload brought is gone and the only way
// out is to find the file again.
func TestRejectedUploadKeepsItsBody(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	app := newTestApp(db)
	session, csrf, _, err := db.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	const body = "port: 7890\nmode: rule\n"
	// A name the filename rule refuses, so the save fails after the file has
	// already been read.
	response := uploadResource(t, app, session, csrf, "配置", "配置/子目录.yaml", body)
	if response.Code == http.StatusSeeOther {
		t.Fatal("this filename must not be accepted")
	}
	page := response.Body.String()
	if !strings.Contains(page, "port: 7890") {
		t.Fatal("a refused upload must hand its body back to the editor, or the user loses it")
	}
	if !strings.Contains(page, "配置") {
		t.Fatal("the refused page must keep what was typed")
	}
}

// TestUploadedNameSurvivesTheRoundTrip pins the case that started this: the
// browser hands over a file, editor.js puts its name in the field, and saving
// has to work - in whatever script the name happens to be written in.
func TestUploadedNameSurvivesTheRoundTrip(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	app := newTestApp(db)
	session, csrf, _, err := db.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	for _, filename := range []string{
		"机场配置.yaml",
		"設定ファイル.yaml",
		"إعدادات.yaml",
		"настройки.yaml",
		"설정.yaml",
		"my config (1).yaml",
	} {
		response := uploadResource(t, app, session, csrf, filename, filename, "port: 7890\n")
		if response.Code != http.StatusSeeOther {
			t.Errorf("%q must be accepted, got %d", filename, response.Code)
			continue
		}
		resources, _, err := db.ListResources(ctx, user.ID, filename, 10, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(resources) != 1 || resources[0].Filename != filename {
			t.Errorf("%q must be stored exactly as given, got %+v", filename, resources)
		}
	}
}

// TestRefusedUpdateKeepsTheEditInProgress is the same loss on the edit screen.
// The stored object is still there, so nothing is destroyed - but re-reading it
// puts the old version in front of the user in place of the work they were
// doing, which reads as though the edit never happened.
func TestRefusedUpdateKeepsTheEditInProgress(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	app := newTestApp(db)
	session, csrf, _, err := db.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	form := url.Values{
		"csrf":             {csrf},
		"name":             {resource.Name},
		"filename":         {"bad/name.yaml"},
		"content":          {"answer=43\n"},
		"content_encoding": {"utf-8"},
		"content_eol":      {"lf"},
	}
	request := httptest.NewRequest(http.MethodPost, "https://cfg.test/resources/"+resource.ID, strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	request.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrf})
	response := httptest.NewRecorder()
	app.handler.ServeHTTP(response, request)

	if response.Code == http.StatusSeeOther {
		t.Fatal("this filename must not be accepted")
	}
	page := response.Body.String()
	if !strings.Contains(page, "answer=43") {
		t.Fatal("the refused page must show the edit in progress, not the stored version")
	}
	if strings.Contains(page, "answer=42") {
		t.Fatal("the stored version must not replace what was being edited")
	}
	stored, err := db.ReadContent(ctx, resource)
	if err != nil || string(stored) != "answer=42\n" {
		t.Fatalf("a refused save must not have written anything, got %q: %v", stored, err)
	}
}
