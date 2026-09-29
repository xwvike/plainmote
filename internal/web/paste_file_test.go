package web

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestPasteAFileFromThePage is the home page's form with a file chosen: the
// file wins over the box, keeps its bytes and its name, and comes back as a
// download unless it is text. With no file chosen the browser still sends an
// empty file field, and the box is what counts.
func TestPasteAFileFromThePage(t *testing.T) {
	db, _, _ := testDatabase(t)
	app := newTestApp(db)

	submit := func(box string, file []byte, name string) *httptest.ResponseRecorder {
		t.Helper()
		var buffer bytes.Buffer
		form := multipart.NewWriter(&buffer)
		_ = form.WriteField("content", box)
		_ = form.WriteField("filename", "")
		_ = form.WriteField("ttl", "10")
		part, _ := form.CreateFormFile("file", name)
		_, _ = part.Write(file)
		_ = form.Close()
		r := httptest.NewRequest(http.MethodPost, "https://cfg.test/paste", &buffer)
		r.Header.Set("Content-Type", form.FormDataContentType())
		r.Header.Set("Accept", "text/html,*/*;q=0.8")
		w := httptest.NewRecorder()
		app.handler.ServeHTTP(w, r)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("page submission: %d %s", w.Code, w.Body.String())
		}
		result := httptest.NewRecorder()
		app.handler.ServeHTTP(result, httptest.NewRequest(http.MethodGet, "https://cfg.test"+w.Header().Get("Location"), nil))
		address := strings.SplitN(strings.SplitN(result.Body.String(), `data-copy="`, 2)[1], `"`, 2)[0]
		delivered := httptest.NewRecorder()
		app.handler.ServeHTTP(delivered, httptest.NewRequest(http.MethodGet, address, nil))
		if delivered.Code != http.StatusOK {
			t.Fatalf("fetching %q: %d", address, delivered.Code)
		}
		return delivered
	}

	image := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0xff, 0x00}, 32)...)
	got := submit("ignored", image, `C:\Users\me\photo.png`)
	if !bytes.Equal(got.Body.Bytes(), image) || got.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("a file must come back as the same opaque bytes, got %q", got.Header().Get("Content-Type"))
	}
	if disposition := got.Header().Get("Content-Disposition"); !strings.HasPrefix(disposition, "attachment;") || !strings.Contains(disposition, `"photo.png"`) {
		t.Fatalf("a file must be a download under its own name, got %q", disposition)
	}

	// A text file from the page keeps its CRLF: only the box's are undone.
	text := submit("", []byte("a\r\nb\r\n"), "app.log")
	if text.Body.String() != "a\r\nb\r\n" || !strings.HasPrefix(text.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("a text file must be served as sent, got %q %q", text.Body.String(), text.Header().Get("Content-Type"))
	}

	box := submit("from\r\nthe box", nil, "")
	if box.Body.String() != "from\nthe box" {
		t.Fatalf("with no file chosen the box counts, got %q", box.Body.String())
	}
}
