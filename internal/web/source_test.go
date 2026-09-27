package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The source link is on every page with a footer, whether or not the legal
// pages are configured, and llms.txt names it too.
func TestSourceLinkIsOnEveryPage(t *testing.T) {
	const source = "https://git.example.com/fork/plainmote"
	for _, cfg := range []Config{
		{PublicURL: "https://cfg.test", AnonymousEnabled: true, SourceURL: source},
		{PublicURL: "https://cfg.test", AnonymousEnabled: true, SourceURL: source, ContactEmail: "ops@example.com"},
	} {
		app := &App{cfg: cfg}
		app.templates = app.templateSet()
		app.handler = app.routes()
		for _, path := range []string{"/", "/login"} {
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "https://cfg.test"+path, nil)
			request.Header.Set("Accept-Language", "zh-CN")
			app.handler.ServeHTTP(response, request)
			if !strings.Contains(response.Body.String(), `<a href="`+source+`" rel="noopener">源代码</a>`) {
				t.Errorf("%s (contact %q) has no source link", path, cfg.ContactEmail)
			}
		}
		if cfg.ContactEmail != "" {
			llms := httptest.NewRecorder()
			app.handler.ServeHTTP(llms, httptest.NewRequest(http.MethodGet, "https://cfg.test/llms.txt", nil))
			if !strings.Contains(llms.Body.String(), "Source code (AGPL-3.0): "+source) {
				t.Error("llms.txt does not name the source")
			}
			about := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "https://cfg.test/about", nil)
			request.Header.Set("Accept-Language", "zh-CN")
			app.handler.ServeHTTP(about, request)
			if !strings.Contains(about.Body.String(), "AGPL-3.0") || !strings.Contains(about.Body.String(), source) {
				t.Error("the about page does not name the license and the source")
			}
		}
	}
}
