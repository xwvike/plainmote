package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
	for name, contentType := range staticAssets {
		recorder := httptest.NewRecorder()
		app.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/static/"+name, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: status %d, want 200", name, recorder.Code)
		}
		if got := recorder.Header().Get("Content-Type"); got != contentType {
			t.Errorf("%s: content type %q, want %q", name, got, contentType)
		}
		if recorder.Body.Len() == 0 {
			t.Errorf("%s: empty body", name)
		}
	}
}

// TestStaticRejectsUnlisted keeps the handler on its allow list. Serving the
// embedded tree by path would expose whatever lands in the directory next.
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
		`rel="icon" href="/static/logo.png"`,
		`<h1 class="signin-name">PlainMote</h1>`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("login page is missing %q", want)
		}
	}
}
