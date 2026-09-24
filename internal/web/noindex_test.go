package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestNothingHereIsIndexable covers both halves. robots.txt only helps against
// a crawler that asks first; the header travels with every response, including
// a delivery address someone else published.
func TestNothingHereIsIndexable(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	app := newTestApp(db, user.GitHubID)
	share, err := db.CreateShare(ctx, user.ID, resource.ID, "s", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	session, csrf, _, err := db.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	get := func(path string, signedIn bool) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "https://cfg.test"+path, nil)
		if signedIn {
			request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
			request.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrf})
		}
		response := httptest.NewRecorder()
		app.handler.ServeHTTP(response, request)
		return response
	}

	robots := get("/robots.txt", false)
	if robots.Code != http.StatusOK {
		t.Fatalf("robots.txt must be served, got %d", robots.Code)
	}
	if body := robots.Body.String(); !strings.Contains(body, "User-agent: *") || !strings.Contains(body, "Disallow: /") {
		t.Fatalf("robots.txt must keep crawlers out entirely, got %q", body)
	}

	// The home page is the one thing meant to be found, and robots.txt says so
	// too. Everything else stays out.
	if !strings.Contains(robots.Body.String(), "Allow: /$") {
		t.Fatalf("robots.txt must let the home page through, got %q", robots.Body.String())
	}
	if tag := get("/", false).Header().Get("X-Robots-Tag"); tag != "" {
		t.Fatalf("the home page must be indexable, got %q", tag)
	}

	for _, tc := range []struct {
		path     string
		signedIn bool
	}{
		{"/robots.txt", false},
		{"/login", false},
		{"/resources/", true},
		{"/logs", true},
		{"/resources/" + resource.ID, true},
		{"/static/style.css", false},
		// The one that matters most: the address is the secret, and it is the
		// only path a crawler can reach without a session.
		{shareAddress(share.Token, resource.Filename), false},
		{"/d/nonsense/x.yaml", false},
	} {
		response := get(tc.path, tc.signedIn)
		tag := response.Header().Get("X-Robots-Tag")
		if !strings.Contains(tag, "noindex") || !strings.Contains(tag, "noarchive") {
			t.Errorf("%s: X-Robots-Tag is %q", tc.path, tag)
		}
	}

	// A delivered resource carries it too, not only the pages around it.
	delivered := get(shareAddress(share.Token, resource.Filename), false)
	if delivered.Code != http.StatusOK {
		t.Fatalf("the share must still deliver, got %d", delivered.Code)
	}
}

// TestPagesCannotBeFramed covers the pages that carry buttons worth tricking
// someone into pressing. SameSite=Lax already keeps a cross-site frame signed
// out; these headers close the rest.
func TestPagesCannotBeFramed(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	app := newTestApp(db, user.GitHubID)
	session, csrf, _, err := db.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/login", "/resources/", "/resources/" + resource.ID, "/resources/" + resource.ID + "?delete=1", "/logs"} {
		request := httptest.NewRequest(http.MethodGet, "https://cfg.test"+path, nil)
		request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
		request.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrf})
		response := httptest.NewRecorder()
		app.handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			continue
		}
		h := response.Header()
		if h.Get("X-Frame-Options") != "DENY" || !strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Errorf("%s can be framed: X-Frame-Options=%q CSP=%q", path, h.Get("X-Frame-Options"), h.Get("Content-Security-Policy"))
		}
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "same-origin" {
			t.Errorf("%s is missing nosniff or the referrer policy", path)
		}
	}
}

// With scripts off the pages load noscript.css, which removes controls that
// would do nothing: the copy buttons and the editor's encoding controls.
func TestNoScriptStylesheetIsLinkedAndServed(t *testing.T) {
	app := &App{cfg: Config{PublicURL: "https://cfg.test", AnonymousEnabled: true}}
	app.templates = app.templateSet()
	app.handler = app.routes()
	get := func(target string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		app.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://cfg.test"+target, nil))
		return response
	}
	if !strings.Contains(get("/").Body.String(), `<noscript><link rel="stylesheet" href="/static/noscript.css"></noscript>`) {
		t.Fatal("pages do not load the no-script stylesheet")
	}
	css := get("/static/noscript.css")
	if css.Code != http.StatusOK || !strings.Contains(css.Header().Get("Content-Type"), "text/css") {
		t.Fatalf("noscript.css: %d %q", css.Code, css.Header().Get("Content-Type"))
	}
	for _, rule := range []string{
		"[data-copy] { display: none !important; }",
		"[data-text-controls] { display: none !important; }",
		"[data-upload], [data-upload-status] { display: none !important; }",
	} {
		if !strings.Contains(css.Body.String(), rule) {
			t.Errorf("noscript.css is missing %q", rule)
		}
	}
}
