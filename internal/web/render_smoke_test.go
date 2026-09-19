package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"plainmote/internal/store"
	upstreamclient "plainmote/internal/upstream"
)

// TestAllPagesRender drives every signed-in page through the real router and
// fails on any template that does not execute cleanly.
func TestAllPagesRender(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
		_, _ = w.Write([]byte("rules:\n  - MATCH,PROXY\n"))
	}))
	t.Cleanup(upstream.Close)
	if _, err := db.CreateShare(ctx, user.ID, resource.ID, "长期", 0, 0); err != nil {
		t.Fatal(err)
	}
	// Enough to fill a second page at size 10, and no more: this test renders
	// pages, so it has no business spending the account's resource quota.
	for i := 0; i < 12; i++ {
		if _, err := db.CreateResource(ctx, user.ID, fmt.Sprintf("填充资源 %d", i), fmt.Sprintf("fill-%d.yaml", i), []byte("a: 1\n"), "", ""); err != nil {
			t.Fatal(err)
		}
	}
	remote, err := db.CreateResource(ctx, user.ID, "远程规则", "remote.yaml", nil, "", upstream.URL+"/rules.yaml")
	if err != nil {
		t.Fatal(err)
	}
	share, err := db.CreateShare(ctx, user.ID, resource.ID, "冒烟", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	meta := store.RequestMeta{RemoteIP: "203.0.113.9", Host: "cfg.test", Query: "token=[redacted]", Proto: "HTTP/2.0",
		UserAgent: "clash/1.0", XForwardedFor: "203.0.113.9, 172.71.1.1", CFRay: "8c1f-HKG", TLS: true, Method: "GET", Path: resource.Filename}
	for _, token := range []string{share.Token, "bogus"} {
		if _, err := db.ConsumeToken(ctx, token, meta, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}

	app := &App{
		db:       db,
		upstream: upstreamclient.New(true, 4<<20),
		cfg: Config{
			MaxContent: 4 << 20,
			PublicURL:  "https://cfg.test",
			AllowedIDs: map[string]bool{user.GitHubID: true},
		},
	}
	app.templates = app.templateSet()
	app.handler = app.routes()

	session, csrf, _, err := db.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "https://cfg.test"+path, nil)
		request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
		request.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrf})
		response := httptest.NewRecorder()
		app.handler.ServeHTTP(response, request)
		return response
	}

	// The login page is only reachable while signed out.
	anon := httptest.NewRecorder()
	app.handler.ServeHTTP(anon, httptest.NewRequest(http.MethodGet, "https://cfg.test/login?error=denied", nil))
	if anon.Code != http.StatusOK || !strings.Contains(anon.Body.String(), "使用 GitHub 登录") {
		t.Fatalf("login: status %d body %q", anon.Code, anon.Body.String())
	}

	for _, tc := range []struct{ name, path, want string }{
		{"home", "/", "粘贴内容"},
		{"dashboard", "/resources/", "上传文件"},
		{"dashboard page 2", "/resources/?size=10&page=2", `class="pgn on">2<`},
		{"dashboard search", "/resources/?q=" + resource.Name, resource.Filename},
		{"dashboard no match", "/resources/?q=%25_nothing", "没有匹配"},
		{"logs", "/logs", "success"},
		{"new resource, upload", "/resources/new?kind=upload", "选择文件"},
		{"new resource, remote", "/resources/new?kind=remote", "远程地址"},
		{"resource", "/resources/" + resource.ID, "查看分享"},
		{"resource remote", "/resources/" + remote.ID, "远程地址"},
		{"resource delete confirm", "/resources/" + resource.ID + "?delete=1", "确认删除"},
		// The save bar states what is live rather than what saving would do,
		// so a save confirms itself by changing that line.
		{"resource states what is live", "/resources/" + resource.ID, "条分享正在提供"},
		{"unshared resource says so", "/resources/" + remote.ID, "还没有分享"},
		{"logs filtered", "/logs?resource=" + resource.ID + "&outcome=expired", "expired"},
	} {
		response := get(tc.path)
		body := response.Body.String()
		if response.Code != http.StatusOK {
			t.Fatalf("%s: status %d body %q", tc.name, response.Code, body)
		}
		for _, bad := range []string{"template error", "<no value>", "ZgotmplZ"} {
			if strings.Contains(body, bad) {
				t.Fatalf("%s: output contains %q", tc.name, bad)
			}
		}
		if !strings.Contains(body, tc.want) {
			t.Fatalf("%s: output missing %q", tc.name, tc.want)
		}
	}

	// The search filter must treat LIKE wildcards as literals, and an
	// out-of-range page must fall back to the last page instead of 404-ing.
	if body := get("/resources/?q=" + resource.Name).Body.String(); strings.Contains(body, "/configs/fill-1") {
		t.Fatal("search returned resources that do not match the query")
	}
	if response := get("/resources/?size=10&page=99"); response.Code != http.StatusSeeOther {
		t.Fatalf("out-of-range page: expected redirect, got %d", response.Code)
	}
}
