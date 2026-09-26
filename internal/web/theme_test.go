package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The switch is a link: it sets a cookie and goes back where it came from,
// and every page after that carries the choice on <html>.
func TestThemeSwitchIsRememberedAndStamped(t *testing.T) {
	app := &App{cfg: Config{PublicURL: "https://cfg.test", AnonymousEnabled: true}}
	app.templates = app.templateSet()
	app.handler = app.routes()

	get := func(target string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "https://cfg.test"+target, nil)
		for _, cookie := range cookies {
			request.AddCookie(cookie)
		}
		response := httptest.NewRecorder()
		app.handler.ServeHTTP(response, request)
		return response
	}

	switched := get("/theme?to=dark&next=%2Fprivacy")
	if switched.Code != http.StatusSeeOther || switched.Header().Get("Location") != "/privacy" {
		t.Fatalf("switch: %d to %q", switched.Code, switched.Header().Get("Location"))
	}
	cookies := switched.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != themeCookie || cookies[0].Value != "dark" || !cookies[0].HttpOnly {
		t.Fatalf("switch cookie: %+v", cookies)
	}
	if away := get("/theme?to=light&next=%2F%2Fevil.example"); away.Header().Get("Location") != "/" {
		t.Fatalf("an outside next must not be followed: %q", away.Header().Get("Location"))
	}
	if bad := get("/theme?to=sepia"); bad.Code != http.StatusBadRequest {
		t.Fatalf("an unknown theme is refused, got %d", bad.Code)
	}

	if page := get("/", cookies[0]).Body.String(); !strings.Contains(page, `data-theme="dark"`) {
		t.Fatal("the chosen theme is not stamped on the page")
	}
	page := get("/").Body.String()
	if strings.Contains(page, "data-theme=") {
		t.Fatal("with no choice the page follows the system")
	}
	for _, want := range []string{`class="theme-to-dark" href="/theme?next=%2F&amp;to=dark"`, `class="theme-to-light" href="/theme?next=%2F&amp;to=light"`} {
		if !strings.Contains(page, want) {
			t.Errorf("the page is missing %q", want)
		}
	}
}
