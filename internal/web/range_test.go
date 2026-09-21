package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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

func TestOwnerPreviewServesByteRanges(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	resource, err := db.CreateResource(ctx, user.ID, "Audio", "sample.mp3", []byte("0123456789"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	app := newTestApp(db, user.GitHubID)

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
}
