package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	upstreamclient "plainmote/internal/upstream"
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
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if fail {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
		_, _ = w.Write([]byte(body))
	}))
	defer upstream.Close()

	resource, err := db.CreateResource(ctx, user.ID, "远程规则", "remote.yaml", []byte("ignored"), upstream.URL+"/rules.yaml")
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

	app := &App{db: db, upstream: upstreamclient.New(true, 4<<20), cfg: Config{}}

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

func TestRemoteExecutableContentCannotRunOnTheAdminOrigin(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<script>document.location='/logout'</script>"))
	}))
	defer upstream.Close()

	resource, err := db.CreateResource(ctx, user.ID, "remote html", "page.html", nil, upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	share, err := db.CreateShare(ctx, user.ID, resource.ID, "test", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{db: db, upstream: upstreamclient.New(true, 4<<20)}
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
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("remote\n"))
	}))
	defer upstream.Close()

	if err := db.UpdateResource(ctx, user.ID, resource.ID, resource.Name, resource.Filename, []byte("still here\n"), upstream.URL); err != nil {
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
	if err := db.UpdateResource(ctx, user.ID, resource.ID, stored.Name, stored.Filename, nil, ""); err == nil {
		t.Fatal("expected an error when clearing the link without content")
	}
	if err := db.UpdateResource(ctx, user.ID, resource.ID, stored.Name, stored.Filename, []byte("back to local\n"), ""); err != nil {
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
