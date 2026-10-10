package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

func TestSessionCookiesAreOutOfScriptsReach(t *testing.T) {
	app := &App{cfg: Config{PublicURL: "https://cfg.test"}}
	response := httptest.NewRecorder()
	app.setSessionCookies(response, httptest.NewRequest(http.MethodGet, "https://cfg.test/", nil), "s", "c", time.Now().Add(time.Hour))
	cleared := httptest.NewRecorder()
	app.clearSessionCookies(cleared)
	for _, cookie := range append(response.Result().Cookies(), cleared.Result().Cookies()...) {
		if !cookie.HttpOnly {
			t.Fatalf("cookie %s is open to scripts", cookie.Name)
		}
	}
}

// A sign-out whose form token no longer matches does not leave the browser
// signed in with every form refused: it asks once more, with a new token.
func TestSignOutWithAStaleTokenAsksAgain(t *testing.T) {
	db, user, _ := testDatabase(t)
	client := signedIn(t, db, user)
	logout := func(cookieCSRF, formCSRF string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "https://cfg.test/logout", strings.NewReader(url.Values{"csrf": {formCSRF}}.Encode()))
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.AddCookie(&http.Cookie{Name: sessionCookie, Value: client.session})
		request.AddCookie(&http.Cookie{Name: csrfCookie, Value: cookieCSRF})
		response := httptest.NewRecorder()
		client.app.handler.ServeHTTP(response, request)
		return response
	}
	stillSignedIn := func() bool {
		_, _, err := db.SessionUser(context.Background(), client.session)
		return err == nil
	}

	asked := logout("replaced-in-the-browser", "replaced-in-the-browser")
	if asked.Code != http.StatusForbidden || !stillSignedIn() {
		t.Fatalf("a stale token signed out (%d) or the session is gone", asked.Code)
	}
	var renewed string
	for _, cookie := range asked.Result().Cookies() {
		if cookie.Name == csrfCookie {
			renewed = cookie.Value
			if !cookie.HttpOnly {
				t.Fatal("the renewed CSRF cookie is open to scripts")
			}
		}
	}
	match := regexp.MustCompile(`action="/logout">\s*<input type="hidden" name="csrf" value="([^"]+)"`).FindStringSubmatch(asked.Body.String())
	if renewed == "" || match == nil || match[1] != renewed || renewed == client.csrf {
		t.Fatalf("no new token to confirm with: cookie %q, page %v", renewed, match)
	}
	if !strings.Contains(asked.Body.String(), "退出请求未能通过验证") && !strings.Contains(asked.Body.String(), "could not be verified") {
		t.Fatalf("the page does not say why it asks: %s", asked.Body.String())
	}
	// The new token signs out; the one it replaced no longer could.
	if _, sessionID, err := db.SessionUser(context.Background(), client.session); err != nil || db.SessionCSRF(context.Background(), sessionID, client.csrf) {
		t.Fatalf("the replaced token is still the session's: %v", err)
	}
	if response := logout(renewed, renewed); response.Code != http.StatusSeeOther || stillSignedIn() {
		t.Fatalf("confirming with the new token: %d, still signed in %v", response.Code, stillSignedIn())
	}
}

func TestSignOutWithItsTokenEndsTheSession(t *testing.T) {
	db, user, _ := testDatabase(t)
	client := signedIn(t, db, user)
	response := client.do(http.MethodPost, "/logout", url.Values{"csrf": {client.csrf}})
	if response.Code != http.StatusSeeOther {
		t.Fatalf("sign-out: %d", response.Code)
	}
	if _, _, err := db.SessionUser(context.Background(), client.session); err == nil {
		t.Fatal("the session outlived its sign-out")
	}
}
