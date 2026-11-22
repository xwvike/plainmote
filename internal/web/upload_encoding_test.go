package web

import (
	"bytes"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"plainmote/internal/store"
)

func readPreparedResourceForm(w http.ResponseWriter, r *http.Request, maxBytes int64) (resourceForm, error) {
	cleanup, err := prepareResourcePost(w, r, maxBytes)
	defer cleanup()
	if err != nil {
		return resourceForm{}, err
	}
	return readResourceForm(r, maxBytes)
}

func TestEditorContentUsesSelectedEncoding(t *testing.T) {
	values := url.Values{
		"content":          {"name: café\n"},
		"content_encoding": {"windows-1252"},
	}
	request := httptest.NewRequest(http.MethodPost, "/resources/new", bytes.NewBufferString(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	form, err := readPreparedResourceForm(httptest.NewRecorder(), request, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{'n', 'a', 'm', 'e', ':', ' ', 'c', 'a', 'f', 0xe9, '\n'}
	if form.Uploaded || form.ContentEncoding != "windows-1252" || !bytes.Equal(form.Content, want) {
		t.Fatalf("encoded form = uploaded:%v encoding:%q content:%x", form.Uploaded, form.ContentEncoding, form.Content)
	}
}

func TestEditorRejectsCharactersTheSelectedEncodingCannotRepresent(t *testing.T) {
	values := url.Values{
		"content":          {"emoji: 😀\n"},
		"content_encoding": {"gbk"},
	}
	request := httptest.NewRequest(http.MethodPost, "/resources/new", bytes.NewBufferString(values.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, err := readPreparedResourceForm(httptest.NewRecorder(), request, 1<<20); err == nil {
		t.Fatal("GBK save must reject a character it cannot represent")
	}
}

func TestUploadedBytesAreNotReencoded(t *testing.T) {
	original, encodingName, err := store.EncodeText("名称: 上海节点\n", "gb18030")
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("content", "this preview must not win")
	_ = writer.WriteField("content_encoding", encodingName)
	part, err := writer.CreateFormFile("upload", "legacy.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(original); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/resources/new", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	form, err := readPreparedResourceForm(httptest.NewRecorder(), request, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !form.Uploaded || form.ContentEncoding != "gb18030" || !bytes.Equal(form.Content, original) {
		t.Fatalf("upload changed bytes: uploaded:%v encoding:%q got:%x want:%x", form.Uploaded, form.ContentEncoding, form.Content, original)
	}
}

// TestChunkedUploadIsBoundedToo covers the request the length check never saw:
// chunked transfer declares no length, so the old guard let the whole body
// through to ParseMultipartForm, which spills to disk without a ceiling.
func TestChunkedUploadIsBoundedToo(t *testing.T) {
	const limit = 1 << 20
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("upload", "big.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(bytes.Repeat([]byte("a"), 4*limit)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/resources/new", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.ContentLength = -1 // what a chunked request looks like to the handler
	request.TransferEncoding = []string{"chunked"}

	_, err = readPreparedResourceForm(httptest.NewRecorder(), request, limit)
	// Refusing it is not the point - the old code refused it too, after the
	// multipart parser had already written all of it to disk. The point is
	// that reading stopped at the limit, which is what MaxBytesError reports.
	var tooLarge *http.MaxBytesError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("reading must stop at the limit, got %v", err)
	}
}

// TestResourcePostEnforcesLimitBeforeCSRF verifies both create and edit reject
// an oversized multipart upload before CSRF parsing can buffer it unboundedly.
func TestResourcePostEnforcesLimitBeforeCSRF(t *testing.T) {
	db, user, resource := testDatabase(t)
	client := signedIn(t, db, user)
	client.app.cfg.MaxContent = 4096

	for _, target := range []string{"/resources/new", "/resources/" + resource.ID} {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		_ = writer.WriteField("csrf", "invalid-token")
		part, err := writer.CreateFormFile("upload", "big.bin")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(bytes.Repeat([]byte("x"), 128*1024)); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "https://cfg.test"+target, &body)
		req.Header.Set("Content-Type", writer.FormDataContentType())
		req.ContentLength = -1
		req.TransferEncoding = []string{"chunked"}
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: client.session})
		req.AddCookie(&http.Cookie{Name: csrfCookie, Value: client.csrf})
		rec := httptest.NewRecorder()
		client.app.handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("%s oversize with bad csrf: got %d, want 413", target, rec.Code)
		}
	}
}
