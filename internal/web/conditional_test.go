package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestSharesAnswerConditionalRequests is a program polling a link: it keeps
// the ETag, asks again with it, and gets an empty 304 until the resource is
// saved. Every answer, 304 included, is a counted and recorded use, and a link
// that has ended is refused rather than told "not modified".
func TestSharesAnswerConditionalRequests(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	app := newTestApp(db, user.GitHubID)
	link, err := db.CreateShare(ctx, user.ID, resource.ID, "agent", time.Hour, 5)
	if err != nil {
		t.Fatal(err)
	}
	address := "https://cfg.test" + shareAddress(link.Token, resource.Filename)
	fetch := func(method string, header map[string]string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(method, address, nil)
		request.Header.Set("User-Agent", "curl/8.7.1")
		for name, value := range header {
			request.Header.Set(name, value)
		}
		response := httptest.NewRecorder()
		app.handler.ServeHTTP(response, request)
		return response
	}

	first := fetch(http.MethodGet, nil)
	etag, modified := first.Header().Get("ETag"), first.Header().Get("Last-Modified")
	if first.Code != http.StatusOK || etag == "" || modified == "" {
		t.Fatalf("a delivery must carry validators: %d %q %q", first.Code, etag, modified)
	}
	if got := first.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("validators must not open delivery to caches, got %q", got)
	}

	same := fetch(http.MethodGet, map[string]string{"If-None-Match": `"other", W/` + etag})
	if same.Code != http.StatusNotModified || same.Body.Len() != 0 || same.Header().Get("ETag") != etag {
		t.Fatalf("an unchanged resource must be a bare 304 with its tag: %d %d", same.Code, same.Body.Len())
	}
	if since := fetch(http.MethodGet, map[string]string{"If-Modified-Since": modified}); since.Code != http.StatusNotModified {
		t.Fatalf("If-Modified-Since at the saved time must be a 304, got %d", since.Code)
	}

	head := fetch(http.MethodHead, nil)
	if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Length") != "10" {
		t.Fatalf("HEAD must describe the content without sending it: %d %d %q",
			head.Code, head.Body.Len(), head.Header().Get("Content-Length"))
	}

	if err := db.UpdateResource(ctx, user.ID, resource.ID, resource.Name, resource.Filename, []byte("answer=43\n"), "", ""); err != nil {
		t.Fatal(err)
	}
	changed := fetch(http.MethodGet, map[string]string{"If-None-Match": etag})
	if changed.Code != http.StatusOK || changed.Body.String() != "answer=43\n" || changed.Header().Get("ETag") == etag {
		t.Fatalf("a saved change must be delivered under a new tag: %d %q", changed.Code, changed.Body.String())
	}

	// Five uses: GET, two 304s, HEAD, GET. The sixth is refused, validators
	// or not.
	if spent := fetch(http.MethodGet, map[string]string{"If-None-Match": changed.Header().Get("ETag")}); spent.Code != http.StatusUnauthorized {
		t.Fatalf("a used-up link must be refused, not answered 304; got %d", spent.Code)
	}
	logs, err := db.ListAccess(ctx, user.ID, "", "", 50)
	if err != nil {
		t.Fatal(err)
	}
	notModified := 0
	for _, entry := range logs {
		if entry.Status == http.StatusNotModified {
			notModified++
		}
	}
	if notModified != 2 {
		t.Fatalf("both 304s must be in the access history, found %d", notModified)
	}
}
