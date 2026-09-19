package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func postPaste(t *testing.T, app *App, form url.Values, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "https://cfg.test/paste", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		request.Header.Set(k, v)
	}
	response := httptest.NewRecorder()
	app.handler.ServeHTTP(response, request)
	return response
}

// TestAnyoneCanPasteAndGetALink is the front door working: no account, no
// cookie, one box, and an address that delivers what was typed.
func TestAnyoneCanPasteAndGetALink(t *testing.T) {
	db, _, _ := testDatabase(t)
	app := newTestApp(db)

	home := httptest.NewRecorder()
	app.handler.ServeHTTP(home, httptest.NewRequest(http.MethodGet, "https://cfg.test/", nil))
	if home.Code != http.StatusOK {
		t.Fatalf("the home page must open without a session, got %d", home.Code)
	}
	if body := home.Body.String(); !strings.Contains(body, `action="/paste"`) || !strings.Contains(body, "登录") {
		t.Fatal("the home page must offer the box and a way to sign in")
	}
	// The meta tag and the response header have to agree, or the one page meant
	// to be found de-indexes itself from inside the document.
	if strings.Contains(home.Body.String(), `name="robots"`) {
		t.Fatal("the home page must not carry a noindex meta tag")
	}

	response := postPaste(t, app, url.Values{
		"content":  {"port: 7890\n"},
		"filename": {"notes.txt"},
		"ttl":      {"5"},
	}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("paste: %d %s", response.Code, response.Body.String())
	}
	page := response.Body.String()
	address := findDeliveryAddress(t, page)

	fetched := httptest.NewRecorder()
	app.handler.ServeHTTP(fetched, httptest.NewRequest(http.MethodGet, "https://cfg.test"+address, nil))
	if fetched.Code != http.StatusOK || fetched.Body.String() != "port: 7890\n" {
		t.Fatalf("the address must deliver the paste, got %d %q", fetched.Code, fetched.Body.String())
	}
	// Whatever it is called, it comes back as text.
	if ct := fetched.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("a paste must be delivered as text, got %q", ct)
	}
}

// TestPasteRefusalKeepsWhatWasTyped is the same lesson the resource screen
// taught: a refusal that comes back empty has thrown the visitor's work away,
// and here they have nowhere else to get it from.
func TestPasteRefusalKeepsWhatWasTyped(t *testing.T) {
	db, _, _ := testDatabase(t)
	app := newTestApp(db)

	const typed = "port: 7890\nmode: rule\n"
	response := postPaste(t, app, url.Values{
		"content":  {typed},
		"filename": {"bad/name.txt"},
		"ttl":      {"5"},
	}, nil)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("this filename must be refused, got %d", response.Code)
	}
	page := response.Body.String()
	if !strings.Contains(page, "port: 7890") {
		t.Fatal("a refused paste must come back with what was typed")
	}
	if !strings.Contains(page, "bad/name.txt") {
		t.Fatal("a refused paste must come back with the filename too")
	}
	if !strings.Contains(page, `value="5" selected`) {
		t.Fatal("a refused paste must keep the chosen lifetime")
	}
}

// TestPasteLifetimeCannotBeChosenFreely keeps the page from being a way around
// the ceiling the store holds.
func TestPasteLifetimeCannotBeChosenFreely(t *testing.T) {
	db, _, _ := testDatabase(t)
	app := newTestApp(db)

	// A value that is not on the menu falls back to the default rather than
	// being taken at face value or costing the visitor their paste.
	response := postPaste(t, app, url.Values{"content": {"x"}, "ttl": {"1440"}}, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("paste: %d %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "1 分钟后") {
		t.Fatalf("an unknown lifetime must become the default, got %s", response.Body.String())
	}
}

// TestPasteRefusesADrivenCrossSitePost is the stateless stand-in for CSRF on an
// endpoint with no session to hang a token off.
func TestPasteRefusesADrivenCrossSitePost(t *testing.T) {
	db, _, _ := testDatabase(t)
	app := newTestApp(db)

	foreign := postPaste(t, app, url.Values{"content": {"x"}}, map[string]string{"Origin": "https://evil.test"})
	if foreign.Code != http.StatusForbidden {
		t.Fatalf("a cross-site post must be refused, got %d", foreign.Code)
	}
	own := postPaste(t, app, url.Values{"content": {"x"}}, map[string]string{"Origin": "https://cfg.test"})
	if own.Code != http.StatusOK {
		t.Fatalf("the page's own form must work, got %d", own.Code)
	}
	// No Origin at all is curl, not an attack, and is let through.
	bare := postPaste(t, app, url.Values{"content": {"x"}}, nil)
	if bare.Code != http.StatusOK {
		t.Fatalf("a request with no Origin must be allowed, got %d", bare.Code)
	}
}

// TestSignedInVisitorIsToldThePasteIsStillAnonymous closes the misreading this
// page invites: the box does the same thing whoever is looking at it.
func TestSignedInVisitorIsToldThePasteIsStillAnonymous(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	app := newTestApp(db, user.GitHubID)
	session, csrf, _, err := db.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "https://cfg.test/", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	request.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrf})
	response := httptest.NewRecorder()
	app.handler.ServeHTTP(response, request)

	page := response.Body.String()
	if !strings.Contains(page, "匿名的") {
		t.Fatal("a signed-in visitor must be told the box is still anonymous")
	}
	if !strings.Contains(page, `href="/resources/"`) {
		t.Fatal("a signed-in visitor must have a way to their own resources")
	}
}

func findDeliveryAddress(t *testing.T, page string) string {
	t.Helper()
	const marker = "https://cfg.test/d/"
	start := strings.Index(page, marker)
	if start < 0 {
		t.Fatalf("no delivery address on the page: %s", page)
	}
	rest := page[start+len("https://cfg.test"):]
	if end := strings.IndexAny(rest, `"<`); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

// TestAnonymousCanBeTurnedOff is what makes this deployable by someone who
// does not want an open write endpoint at all. The switch has to remove the
// endpoint rather than leave it there refusing, and the page has to still be a
// page.
func TestAnonymousCanBeTurnedOff(t *testing.T) {
	db, user, _ := testDatabase(t)
	app := &App{db: db, cfg: Config{PublicURL: "https://cfg.test", AllowedIDs: map[string]bool{user.GitHubID: true}}}
	app.templates = app.templateSet()
	app.handler = app.routes()

	home := httptest.NewRecorder()
	app.handler.ServeHTTP(home, httptest.NewRequest(http.MethodGet, "https://cfg.test/", nil))
	if home.Code != http.StatusOK {
		t.Fatalf("the home page must still open, got %d", home.Code)
	}
	page := home.Body.String()
	if strings.Contains(page, `action="/paste"`) {
		t.Fatal("no box when the deployment does not take pastes")
	}
	if !strings.Contains(page, "没有开放匿名分享") || !strings.Contains(page, "可撤销的链接") {
		t.Fatal("the page must still say what this service is and how to get in")
	}
	// The parts of the page that describe the box have to go with it, or the
	// tab and the navigation advertise something this deployment does not do.
	if strings.Contains(page, "<title>PlainMote - 粘贴内容") {
		t.Fatal("the title must not promise a box that is not there")
	}
	if strings.Contains(page, ">快速分享<") {
		t.Fatal("the navigation must not name a feature that is off")
	}

	if posted := postPaste(t, app, url.Values{"content": {"x"}}, nil); posted.Code != http.StatusNotFound {
		t.Fatalf("the endpoint must not exist, got %d", posted.Code)
	}
}
