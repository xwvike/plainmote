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

	for _, tc := range []struct {
		path     string
		signedIn bool
	}{
		{"/robots.txt", false},
		{"/login", false},
		{"/", true},
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
