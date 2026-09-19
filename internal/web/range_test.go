package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestParseByteRange(t *testing.T) {
	for _, tc := range []struct {
		header string
		size   int64
		want   *byteRange
	}{
		{"", 10, nil},
		{"items=0-2", 10, nil},
		{"bytes=0-3", 10, &byteRange{0, 3}},
		{"bytes=4-", 10, &byteRange{4, 9}},
		{"bytes=-3", 10, &byteRange{7, 9}},
		{"bytes=7-99", 10, &byteRange{7, 9}},
	} {
		got, err := parseByteRange(tc.header, tc.size)
		if err != nil {
			t.Errorf("%q: %v", tc.header, err)
			continue
		}
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("%q: got %v, want %v", tc.header, got, tc.want)
		}
	}
	for _, header := range []string{"bytes=", "bytes=10-", "bytes=4-2", "bytes=-0", "bytes=0-1,4-5"} {
		if _, err := parseByteRange(header, 10); err == nil {
			t.Errorf("%q should be refused", header)
		}
	}
}

func TestLocalResourcesServeByteRanges(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	resource, err := db.CreateResource(ctx, user.ID, "Audio", "sample.mp3", []byte("0123456789"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	share, err := db.CreateShare(ctx, user.ID, resource.ID, "range", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	app := newTestApp(db, user.GitHubID)

	request := httptest.NewRequest(http.MethodGet, "https://cfg.test/d/"+url.PathEscape(share.Token)+"/sample.mp3", nil)
	request.Header.Set("Range", "bytes=2-5")
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusPartialContent || response.Body.String() != "2345" {
		t.Fatalf("public range: status %d body %q", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Content-Range"); got != "bytes 2-5/10" {
		t.Fatalf("Content-Range = %q", got)
	}
	if got := response.Header().Get("Content-Length"); got != "4" {
		t.Fatalf("Content-Length = %q", got)
	}
	if got := response.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("Accept-Ranges = %q", got)
	}
	if got := response.Header().Get("Content-Security-Policy"); got != deliveredContentSecurityPolicy {
		t.Fatalf("Content-Security-Policy = %q", got)
	}

	session, csrf, _, err := db.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	rawRequest := httptest.NewRequest(http.MethodGet, "https://cfg.test/resources/"+resource.ID+"/raw", nil)
	rawRequest.Header.Set("Range", "bytes=-3")
	rawRequest.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	rawRequest.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrf})
	raw := httptest.NewRecorder()
	app.Handler().ServeHTTP(raw, rawRequest)
	if raw.Code != http.StatusPartialContent || raw.Body.String() != "789" {
		t.Fatalf("owner range: status %d body %q", raw.Code, raw.Body.String())
	}
	if got := raw.Header().Get("Content-Range"); got != "bytes 7-9/10" {
		t.Fatalf("owner Content-Range = %q", got)
	}

	badRequest := httptest.NewRequest(http.MethodGet, "https://cfg.test/d/"+url.PathEscape(share.Token)+"/sample.mp3", nil)
	badRequest.Header.Set("Range", "bytes=99-")
	bad := httptest.NewRecorder()
	app.Handler().ServeHTTP(bad, badRequest)
	if bad.Code != http.StatusRequestedRangeNotSatisfiable || bad.Header().Get("Content-Range") != "bytes */10" {
		t.Fatalf("invalid range: status %d Content-Range %q", bad.Code, bad.Header().Get("Content-Range"))
	}
}

func TestSingleUseShareGrantFinishesBrowserRangeRequests(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	share, err := db.CreateShare(ctx, user.ID, resource.ID, "single media view", time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	app := newTestApp(db, user.GitHubID)
	target := "https://cfg.test/d/" + url.PathEscape(share.Token) + "/example.conf"

	first := httptest.NewRecorder()
	app.Handler().ServeHTTP(first, httptest.NewRequest(http.MethodGet, target, nil))
	if first.Code != http.StatusOK {
		t.Fatalf("first request: %d %q", first.Code, first.Body.String())
	}
	var grant *http.Cookie
	for _, cookie := range first.Result().Cookies() {
		if cookie.Name == deliveryCookie {
			grant = cookie
			break
		}
	}
	if grant == nil {
		t.Fatal("counted delivery did not issue a continuation cookie")
	}
	if grant.Path != deliveryPrefix+share.Token || !grant.HttpOnly || !grant.Secure {
		t.Fatalf("unexpected delivery cookie: %+v", grant)
	}

	continuedRequest := httptest.NewRequest(http.MethodGet, target, nil)
	continuedRequest.Header.Set("Range", "bytes=2-5")
	continuedRequest.AddCookie(grant)
	continued := httptest.NewRecorder()
	app.Handler().ServeHTTP(continued, continuedRequest)
	if continued.Code != http.StatusPartialContent || continued.Body.String() != "swer" {
		t.Fatalf("continued range: %d %q", continued.Code, continued.Body.String())
	}

	withoutGrant := httptest.NewRecorder()
	app.Handler().ServeHTTP(withoutGrant, httptest.NewRequest(http.MethodGet, target, nil))
	if withoutGrant.Code != http.StatusUnauthorized {
		t.Fatalf("used-up link without its grant: got %d", withoutGrant.Code)
	}

	if err := db.RevokeLink(ctx, user.ID, resource.ID, share.ID); err != nil {
		t.Fatal(err)
	}
	revokedRequest := httptest.NewRequest(http.MethodGet, target, nil)
	revokedRequest.AddCookie(grant)
	revoked := httptest.NewRecorder()
	app.Handler().ServeHTTP(revoked, revokedRequest)
	if revoked.Code != http.StatusUnauthorized {
		t.Fatalf("revoked link resumed with an old grant: got %d", revoked.Code)
	}
}
