package web

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// curl is the other way in: `cmd | curl -F 'content=<-' .../paste` has to work,
// answer with the address alone, and keep the bytes it was sent.
func TestPasteFromATerminal(t *testing.T) {
	db, _, _ := testDatabase(t)
	app := newTestApp(db)

	send := func(body io.Reader, contentType, accept string, headers ...string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(http.MethodPost, "https://cfg.test/paste", body)
		r.Header.Set("Content-Type", contentType)
		r.Header.Set("Accept", accept)
		for i := 0; i+1 < len(headers); i += 2 {
			r.Header.Set(headers[i], headers[i+1])
		}
		w := httptest.NewRecorder()
		app.handler.ServeHTTP(w, r)
		return w
	}
	multipartBody := func(fields map[string]string, file, fileName string) (io.Reader, string) {
		var buffer bytes.Buffer
		form := multipart.NewWriter(&buffer)
		for name, value := range fields {
			_ = form.WriteField(name, value)
		}
		if fileName != "" {
			part, _ := form.CreateFormFile("content", fileName)
			_, _ = io.WriteString(part, file)
		}
		_ = form.Close()
		return &buffer, form.FormDataContentType()
	}
	fetch := func(address string) string {
		t.Helper()
		w := httptest.NewRecorder()
		app.handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, strings.TrimSpace(address), nil))
		if w.Code != http.StatusOK {
			t.Fatalf("fetching %q: %d", address, w.Code)
		}
		return w.Body.String()
	}

	// What `curl -F 'content=<-'` sends: a field, CRLF and tabs intact.
	log := "line 1\r\n\tindented\r\ntrailing   \n"
	body, contentType := multipartBody(map[string]string{"content": log}, "", "")
	piped := send(body, contentType, "*/*")
	if piped.Code != http.StatusCreated || !strings.HasPrefix(piped.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("piped paste: %d %q", piped.Code, piped.Body.String())
	}
	address := piped.Body.String()
	if !strings.HasPrefix(address, "https://cfg.test/d/") || !strings.HasSuffix(address, "\n") || strings.Count(address, "\n") != 1 {
		t.Fatalf("a terminal must get the address on one line, got %q", address)
	}
	if got := fetch(address); got != log {
		t.Fatalf("the paste must come back byte for byte: %q", got)
	}

	// What `curl -F content=@app.log -F ttl=30` sends: the file as a part.
	body, contentType = multipartBody(map[string]string{"ttl": "30"}, "from a file\n", "app.log")
	uploaded := send(body, contentType, "*/*")
	if uploaded.Code != http.StatusCreated || !strings.HasSuffix(strings.TrimSpace(uploaded.Body.String()), "/app.log") {
		t.Fatalf("file part: %d %q", uploaded.Code, uploaded.Body.String())
	}
	if got := fetch(uploaded.Body.String()); got != "from a file\n" {
		t.Fatalf("file part content: %q", got)
	}

	// What `curl --data-urlencode content=...` sends.
	encoded := send(strings.NewReader(url.Values{"content": {"a\r\nb"}}.Encode()), "application/x-www-form-urlencoded", "*/*")
	if encoded.Code != http.StatusCreated || fetch(encoded.Body.String()) != "a\r\nb" {
		t.Fatalf("urlencoded paste: %d %q", encoded.Code, encoded.Body.String())
	}

	// Refusals are a line of text too, not a page.
	body, contentType = multipartBody(map[string]string{"content": ""}, "", "")
	empty := send(body, contentType, "*/*")
	if empty.Code != http.StatusBadRequest || strings.Contains(empty.Body.String(), "<html") || !strings.Contains(empty.Body.String(), "empty") {
		t.Fatalf("an empty paste from a terminal: %d %q", empty.Code, empty.Body.String())
	}

	// The page's form is unchanged: a redirect, and the textarea's CRLF undone.
	form := send(strings.NewReader(url.Values{"content": {"x\r\ny"}}.Encode()), "application/x-www-form-urlencoded", "text/html,*/*;q=0.8")
	if form.Code != http.StatusSeeOther || !strings.HasPrefix(form.Header().Get("Location"), pasteResultPrefix) {
		t.Fatalf("the page's form must still redirect: %d", form.Code)
	}
	result := getPasteResult(t, app, form)
	if got := fetch(findDeliveryAddress(t, result.Body.String())); got != "x\ny" {
		t.Fatalf("a textarea paste must lose the CRLF the browser added: %q", got)
	}

	// Another site still cannot drive a visitor's browser into it.
	body, contentType = multipartBody(map[string]string{"content": "x"}, "", "")
	if forged := send(body, contentType, "*/*", "Origin", "https://evil.test"); forged.Code != http.StatusForbidden {
		t.Fatalf("a cross-site post must be refused, got %d", forged.Code)
	}
}

// `curl --data-binary @app.log` labels its body a form but sends the file as
// it is; with no content field it is the content, byte for byte, and its
// options ride in the query string. A text/plain body is the same.
func TestPasteRawBodyFromATerminal(t *testing.T) {
	db, _, _ := testDatabase(t)
	app := newTestApp(db)
	send := func(method, target, contentType, accept, body string) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, "https://cfg.test"+target, strings.NewReader(body))
		if contentType != "" {
			r.Header.Set("Content-Type", contentType)
		}
		r.Header.Set("Accept", accept)
		w := httptest.NewRecorder()
		app.handler.ServeHTTP(w, r)
		return w
	}
	fetch := func(address string) string {
		t.Helper()
		w := httptest.NewRecorder()
		app.handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, strings.TrimSpace(address), nil))
		return w.Body.String()
	}

	// Looks like a form to a parser, but has no content field: it is a file.
	raw := "a=b&c=d\r\n%zz\ttab\n"
	binary := send(http.MethodPost, "/paste?ttl=30&filename=app.log", "application/x-www-form-urlencoded", "*/*", raw)
	if binary.Code != http.StatusCreated || !strings.HasSuffix(strings.TrimSpace(binary.Body.String()), "/app.log") {
		t.Fatalf("--data-binary paste: %d %q", binary.Code, binary.Body.String())
	}
	if got := fetch(binary.Body.String()); got != raw {
		t.Fatalf("--data-binary must be kept byte for byte, got %q", got)
	}

	plain := send(http.MethodPost, "/paste", "text/plain", "*/*", "plain body\r\n")
	if plain.Code != http.StatusCreated || fetch(plain.Body.String()) != "plain body\r\n" {
		t.Fatalf("text/plain paste: %d %q", plain.Code, plain.Body.String())
	}

	// Opened in a terminal the endpoint explains itself; in a browser it is
	// the home page.
	usage := send(http.MethodGet, "/paste", "", "*/*", "")
	if usage.Code != http.StatusOK || !strings.Contains(usage.Body.String(), "curl -F 'content=<-' https://cfg.test/paste") ||
		!strings.Contains(usage.Body.String(), "(default 10m)") || !strings.Contains(usage.Body.String(), "At most 4 MiB") {
		t.Fatalf("usage: %d %q", usage.Code, usage.Body.String())
	}
	browser := send(http.MethodGet, "/paste", "", "text/html,*/*;q=0.8", "")
	if browser.Code != http.StatusSeeOther || browser.Header().Get("Location") != "/" {
		t.Fatalf("a browser opening /paste must land on the home page: %d %q", browser.Code, browser.Header().Get("Location"))
	}
	if put := send(http.MethodPut, "/paste", "text/plain", "*/*", "x"); put.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT must not be a second way in past the rate limit on POST, got %d", put.Code)
	}
}
