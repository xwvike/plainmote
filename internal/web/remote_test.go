package web

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestRemoteResourceForwardsLive covers the whole point of a remote resource:
// nothing is stored, every public request re-fetches, and the upstream's
// safe content types are passed through unchanged.
func TestRemoteResourceForwardsLive(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	hits := 0
	body := "rules:\n  - MATCH,DIRECT\n"
	fail := false
	source := upstreamFunc(func(_ context.Context, _ string) ([]byte, string, error) {
		hits++
		if fail {
			return nil, "", errors.New("boom")
		}
		return []byte(body), "text/yaml; charset=utf-8", nil
	})

	resource, err := db.CreateResource(ctx, user.ID, "远程规则", "remote.yaml", []byte("ignored"), "", "https://upstream.example/rules.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if resource.ContentKey != "" || resource.ContentSize != 0 || resource.ContentType != "" {
		t.Fatalf("a remote resource must store no body or type, got key=%q size=%d type=%q", resource.ContentKey, resource.ContentSize, resource.ContentType)
	}
	longLived, err := db.CreateShare(ctx, user.ID, resource.ID, "长期", 0, 0)
	if err != nil {
		t.Fatal(err)
	}

	app := &App{db: db, upstream: source, cfg: Config{}}

	fetch := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "https://cfg.test/d/"+longLived.Token+"/"+resource.Filename, nil)
		response := httptest.NewRecorder()
		app.handlePublic(response, request)
		return response
	}

	first := fetch()
	if first.Code != http.StatusOK || first.Body.String() != body {
		t.Fatalf("forward failed: %d %q", first.Code, first.Body.String())
	}
	if got := first.Header().Get("Content-Type"); got != "text/yaml; charset=utf-8" {
		t.Fatalf("upstream content type not passed through: %q", got)
	}

	// A second request must hit the upstream again: nothing is cached.
	body = "rules:\n  - MATCH,PROXY\n"
	second := fetch()
	if second.Body.String() != body {
		t.Fatalf("second request served a cached body: %q", second.Body.String())
	}
	if hits != 2 {
		t.Fatalf("expected one upstream request per public request, got %d", hits)
	}

	// Upstream failure is hard: 502, no fallback to anything stored.
	fail = true
	third := fetch()
	if third.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 on upstream failure, got %d", third.Code)
	}
	logs, err := db.ListAccess(ctx, user.ID, resource.ID, "upstream_error", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != 1 || logs[0].Status != http.StatusBadGateway {
		t.Fatalf("upstream failure was not recorded: %+v", logs)
	}
	allLogs, err := db.ListAccess(ctx, user.ID, resource.ID, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(allLogs) != 3 || allLogs[0].Outcome != "upstream_error" {
		t.Fatalf("each public request must produce one final access result: %+v", allLogs)
	}
}

func TestRemotePreviewUsesUpstreamCharset(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	resource, err := db.CreateResource(ctx, user.ID, "Legacy remote", "remote.txt", nil, "", "https://upstream.example/legacy.txt")
	if err != nil {
		t.Fatal(err)
	}
	app := newTestApp(db)
	app.upstream = upstreamFunc(func(_ context.Context, _ string) ([]byte, string, error) {
		return []byte{'c', 'a', 'f', 0xe9, '\n'}, "text/plain; charset=windows-1252", nil
	})
	session, csrf, _, err := db.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "https://cfg.test/resources/"+resource.ID, nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	request.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrf})
	response := httptest.NewRecorder()
	app.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "café") {
		t.Fatalf("remote legacy preview: %d %q", response.Code, response.Body.String())
	}
}

func TestRemoteExecutableContentCannotRunOnTheAdminOrigin(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	resource, err := db.CreateResource(ctx, user.ID, "remote html", "page.html", nil, "", "https://upstream.example/page.html")
	if err != nil {
		t.Fatal(err)
	}
	share, err := db.CreateShare(ctx, user.ID, resource.ID, "test", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{db: db, upstream: upstreamFunc(func(_ context.Context, _ string) ([]byte, string, error) {
		return []byte("<script>document.location='/logout'</script>"), "text/html; charset=utf-8", nil
	})}
	request := httptest.NewRequest(http.MethodGet, "https://cfg.test/d/"+share.Token+"/page.html", nil)
	response := httptest.NewRecorder()
	app.handlePublic(response, request)

	if got := response.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("executable upstream type was served as %q", got)
	}
	if got := response.Header().Get("Content-Security-Policy"); got == "" {
		t.Fatal("delivery response has no Content-Security-Policy sandbox")
	}
}

// TestSwitchingBetweenLocalAndRemote checks the store-level transition in both
// directions. The current page keeps each editor focused on its source type,
// but the resource model itself does not make that choice permanent.
func TestSwitchingBetweenLocalAndRemote(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	if err := db.UpdateResource(ctx, user.ID, resource.ID, resource.Name, resource.Filename, []byte("still here\n"), "", "https://upstream.example/resource"); err != nil {
		t.Fatal(err)
	}
	stored, err := db.ResourceForOwner(ctx, user.ID, resource.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Remote() || stored.ContentKey != "" || stored.ContentType != "" {
		t.Fatalf("turning remote must drop the stored body and type: %+v", stored)
	}

	// Clearing the link requires content again, and restores a local resource.
	if err := db.UpdateResource(ctx, user.ID, resource.ID, stored.Name, stored.Filename, nil, "", ""); err == nil {
		t.Fatal("expected an error when clearing the link without content")
	}
	if err := db.UpdateResource(ctx, user.ID, resource.ID, stored.Name, stored.Filename, []byte("back to local\n"), "", ""); err != nil {
		t.Fatal(err)
	}
	stored, err = db.ResourceForOwner(ctx, user.ID, resource.ID)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := db.ReadContent(ctx, stored)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Remote() || string(restored) != "back to local\n" || stored.ContentType != "text/plain; charset=utf-8" {
		t.Fatalf("clearing the link must restore a local resource: %+v / %q", stored, restored)
	}
}
