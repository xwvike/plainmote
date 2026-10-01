package web

import (
	"bytes"
	"compress/gzip"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestStaticCompressionNegotiation(t *testing.T) {
	app := staticApp(t)
	const bundle = "/static/vendor/codemirror.js"
	raw := staticAssets["vendor/codemirror.js"].body

	get := func(accept string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, bundle, nil)
		request.Header.Set("Accept-Encoding", accept)
		recorder := httptest.NewRecorder()
		app.Handler().ServeHTTP(recorder, request)
		return recorder
	}

	compressed := get("gzip, deflate, br")
	if got := compressed.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("content encoding %q, want gzip", got)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed.Body.Bytes()))
	if err != nil {
		t.Fatalf("response is not gzip: %v", err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("decompress: %v", err)
	}
	if !bytes.Equal(decoded, raw) {
		t.Error("the compressed asset does not round-trip")
	}

	for _, accept := range []string{
		"gzip;q=0, identity",
		"gzip;q=0;foo=bar",
		"gzip; foo=bar; Q = 0",
		"gzip;q=invalid",
	} {
		if got := get(accept).Header().Get("Content-Encoding"); got != "" {
			t.Errorf("Accept-Encoding %q returned %q, want identity", accept, got)
		}
	}
	if got := get("br, *;q=0.5").Header().Get("Content-Encoding"); got != "gzip" {
		t.Errorf("a positive wildcard returned %q, want gzip", got)
	}
}

func TestStaticCacheRevalidation(t *testing.T) {
	app := staticApp(t)
	first := httptest.NewRecorder()
	app.Handler().ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/static/editor.js", nil))
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("static response has no ETag")
	}
	if got := first.Header().Get("Cache-Control"); got != "public, no-cache" {
		t.Fatalf("Cache-Control %q, want public, no-cache", got)
	}

	request := httptest.NewRequest(http.MethodGet, "/static/editor.js", nil)
	request.Header.Set("If-None-Match", etag)
	revalidated := httptest.NewRecorder()
	app.Handler().ServeHTTP(revalidated, request)
	if revalidated.Code != http.StatusNotModified || revalidated.Body.Len() != 0 {
		t.Fatalf("revalidation returned status %d and %d bytes", revalidated.Code, revalidated.Body.Len())
	}
}

func staticApp(t *testing.T) *App {
	t.Helper()
	app := &App{cfg: Config{PublicURL: "https://cfg.test"}}
	app.templates = app.templateSet()
	app.handler = app.routes()
	return app
}

// TestStaticAssetsServed fixes the content type of every file under /static/.
// A wrong type here breaks the page quietly: a stylesheet served as text/plain
// is ignored by the browser and the logo would download instead of render.
func TestStaticAssetsServed(t *testing.T) {
	app := staticApp(t)
	for name, asset := range staticAssets {
		recorder := httptest.NewRecorder()
		app.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/static/"+name, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: status %d, want 200", name, recorder.Code)
		}
		if got := recorder.Header().Get("Content-Type"); got != asset.contentType {
			t.Errorf("%s: content type %q, want %q", name, got, asset.contentType)
		}
		if recorder.Body.Len() == 0 {
			t.Errorf("%s: empty body", name)
		}
	}
}

// TestStaticRejectsUnlisted keeps the handler on its allow list. Serving the
// embedded tree by path would expose whatever lands in the directory next.
// TestTemplatesReferenceServedAssets catches a script or stylesheet added to
// a page but not to the embedded list, which would quietly answer 404.
func TestTemplatesReferenceServedAssets(t *testing.T) {
	reference := regexp.MustCompile(`/static/([A-Za-z0-9_./-]+\.(?:js|css|png|ico))`)
	templates, err := fs.Glob(webAssets, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range templates {
		body, err := fs.ReadFile(webAssets, name)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range reference.FindAllSubmatch(body, -1) {
			if _, ok := staticAssets[string(match[1])]; !ok {
				t.Errorf("%s references /static/%s, which is not served", name, match[1])
			}
		}
	}
}

func TestStaticRejectsUnlisted(t *testing.T) {
	app := staticApp(t)
	for _, path := range []string{"/static/", "/static/missing.css", "/static/logo.png/extra", "/static/templates/login.html"} {
		recorder := httptest.NewRecorder()
		app.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, recorder.Code)
		}
	}
}

// TestStaticRejectsTraversal pins what happens to a dot-dot address. ServeMux
// cleans the path before routing, so the request never reaches handleStatic;
// what matters is that no template source comes back either way.
func TestStaticRejectsTraversal(t *testing.T) {
	app := staticApp(t)
	for _, path := range []string{"/static/../templates/login.html", "/static/..%2ftemplates%2flogin.html"} {
		recorder := httptest.NewRecorder()
		app.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code == http.StatusOK {
			t.Errorf("%s: status 200, want a refusal", path)
		}
		if strings.Contains(recorder.Body.String(), "{{define") {
			t.Errorf("%s: response leaked template source", path)
		}
	}
}

// TestLoginPageShowsLogo covers the one page a signed-out visitor sees: the
// brand mark and the favicon both have to resolve to a served asset.
func TestLoginPageShowsLogo(t *testing.T) {
	app := staticApp(t)
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/login", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status %d, want 200", recorder.Code)
	}
	page := recorder.Body.String()
	for _, want := range []string{
		`class="signin-mark" src="/static/logo.png"`,
		`rel="icon" href="/favicon.ico" sizes="48x48"`,
		`rel="icon" href="/static/icon-192.png" type="image/png" sizes="192x192"`,
		`<h1 class="signin-name">PlainMote</h1>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("login page is missing %q", want)
		}
	}
}

// The site's icon is where search engines and browsers look for it without
// being told, in a size a search result accepts, and robots.txt lets them in.
func TestFaviconIsServedAndCrawlable(t *testing.T) {
	app := &App{cfg: Config{PublicURL: "https://cfg.test", AnonymousEnabled: true, ContactEmail: "ops@example.com"}}
	app.templates = app.templateSet()
	app.handler = app.routes()
	get := func(path string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		app.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://cfg.test"+path, nil))
		return response
	}
	icon := get("/favicon.ico")
	if icon.Code != http.StatusOK || icon.Header().Get("Content-Type") != "image/x-icon" || icon.Header().Get("X-Robots-Tag") != "" {
		t.Fatalf("favicon: %d %q %q", icon.Code, icon.Header().Get("Content-Type"), icon.Header().Get("X-Robots-Tag"))
	}
	if body := icon.Body.Bytes(); len(body) < 6 || body[2] != 1 || body[4] != 3 {
		t.Fatal("favicon.ico is not an icon with three sizes")
	}
	if robots := get("/robots.txt").Body.String(); !strings.Contains(robots, "Allow: /favicon.ico$") {
		t.Fatal("robots.txt keeps crawlers from the icon")
	}
	if got := get("/static/icon-192.png"); got.Code != http.StatusOK || got.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("192px icon: %d", got.Code)
	}
}
