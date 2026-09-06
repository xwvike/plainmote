package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"plainmote/internal/upstream"
)

// TestShareFlowThroughRouter walks the whole share feature the way a person
// does: press 分享, get a dialog with a working link in it, change the terms,
// then revoke.
func TestShareFlowThroughRouter(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	app := &App{db: db, upstream: upstream.New(true, 4<<20), cfg: Config{
		MaxContent: 4 << 20, PublicURL: "https://cfg.test",
		AllowedIDs: map[string]bool{user.GitHubID: true},
	}}
	app.templates = app.templateSet()
	app.handler = app.routes()
	session, csrf, _, err := db.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	do := func(method, target string, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		var request *http.Request
		if form == nil {
			request = httptest.NewRequest(method, "https://cfg.test"+target, nil)
		} else {
			form.Set("csrf", csrf)
			request = httptest.NewRequest(method, "https://cfg.test"+target, strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
		request.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrf})
		response := httptest.NewRecorder()
		app.handler.ServeHTTP(response, request)
		return response
	}

	// Pressing 分享 must land on the resource page with the dialog open.
	created := do(http.MethodPost, "/resources/"+resource.ID+"/share", url.Values{"action": {"create"}})
	if created.Code != http.StatusSeeOther {
		t.Fatalf("share create: expected a redirect, got %d", created.Code)
	}
	location := created.Header().Get("Location")
	if !strings.Contains(location, "share=") {
		t.Fatalf("share create must reopen the dialog on the new link, got %q", location)
	}

	page := do(http.MethodGet, location, nil)
	body := page.Body.String()
	if page.Code != http.StatusOK {
		t.Fatalf("resource page: %d", page.Code)
	}
	for _, want := range []string{"分享「", "分享链接", "存活时长", "使用次数"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dialog is missing %q", want)
		}
	}

	shares, err := db.ListShares(ctx, user.ID, resource.ID, time.Now().UTC())
	if err != nil || len(shares) != 1 {
		t.Fatalf("expected exactly one share: %d %v", len(shares), err)
	}
	share := shares[0]
	// A new share is single-use by default: a link that gets forwarded or
	// sits in a chat log should not keep working.
	if share.MaxUses != 1 {
		t.Fatalf("a new share should be single-use, got max_uses=%d", share.MaxUses)
	}
	if share.ExpiresAt == nil {
		t.Fatal("a new share should expire")
	}
	if !strings.Contains(body, share.Token) {
		t.Fatal("the dialog must show a link that already works")
	}

	// That link must actually open the resource.
	public := httptest.NewRecorder()
	app.handler.ServeHTTP(public, httptest.NewRequest(http.MethodGet, "https://cfg.test/d/"+url.PathEscape(share.Token)+"/"+resource.Filename, nil))
	if public.Code != http.StatusOK || public.Body.String() != "answer=42\n" {
		t.Fatalf("share link did not serve the resource: %d %q", public.Code, public.Body.String())
	}

	// Changing the terms keeps the address.
	updated := do(http.MethodPost, "/resources/"+resource.ID+"/share",
		url.Values{"action": {"update"}, "share_id": {share.ID}, "name": {"同事 A"}, "ttl": {"1h"}, "uses": {"1"}})
	if updated.Code != http.StatusSeeOther {
		t.Fatalf("share update: %d %s", updated.Code, updated.Body.String())
	}
	after, err := db.ListShares(ctx, user.ID, resource.ID, time.Now().UTC())
	if err != nil || len(after) != 1 {
		t.Fatalf("expected one share after the update: %d %v", len(after), err)
	}
	if after[0].Token != share.Token || after[0].Name != "同事 A" || after[0].MaxUses != 1 {
		t.Fatalf("update did not apply cleanly: %+v", after[0])
	}
	// The link had already been opened once; applying new terms restarts the
	// budget rather than killing it on the spot.
	if after[0].UsedCount != 0 {
		t.Fatalf("applying new terms must restart the use count, got %d", after[0].UsedCount)
	}

	// The other use-count presets apply the same way.
	for _, want := range []int{2, 10, 0} {
		if got := do(http.MethodPost, "/resources/"+resource.ID+"/share",
			url.Values{"action": {"update"}, "share_id": {share.ID}, "ttl": {"24h"}, "uses": {strconv.Itoa(want)}}); got.Code != http.StatusSeeOther {
			t.Fatalf("uses=%d: %d", want, got.Code)
		}
		live, err := db.ListShares(ctx, user.ID, resource.ID, time.Now().UTC())
		if err != nil || len(live) != 1 {
			t.Fatalf("uses=%d: expected one share, got %d %v", want, len(live), err)
		}
		if live[0].MaxUses != want {
			t.Fatalf("uses=%d not applied, got %d", want, live[0].MaxUses)
		}
	}

	// An unreadable count is reported rather than silently widening the share.
	if bad := do(http.MethodPost, "/resources/"+resource.ID+"/share",
		url.Values{"action": {"update"}, "share_id": {share.ID}, "ttl": {"24h"}, "uses": {"once"}}); !strings.Contains(bad.Header().Get("Location"), "error=") {
		t.Fatalf("an unreadable use count must be reported, got %q", bad.Header().Get("Location"))
	}

	// A custom duration that cannot be parsed comes back as an error, not a panic.
	bad := do(http.MethodPost, "/resources/"+resource.ID+"/share",
		url.Values{"action": {"update"}, "share_id": {share.ID}, "ttl": {"custom"}, "ttl_custom": {"soon"}})
	if !strings.Contains(bad.Header().Get("Location"), "error=") {
		t.Fatalf("a bad custom duration must be reported, got %q", bad.Header().Get("Location"))
	}

	// Revoking kills it for whoever already holds the link.
	if revoked := do(http.MethodPost, "/resources/"+resource.ID+"/share",
		url.Values{"action": {"revoke"}, "share_id": {share.ID}}); revoked.Code != http.StatusSeeOther {
		t.Fatalf("share revoke: %d", revoked.Code)
	}
	gone := httptest.NewRecorder()
	app.handler.ServeHTTP(gone, httptest.NewRequest(http.MethodGet, "https://cfg.test/d/"+url.PathEscape(share.Token)+"/"+resource.Filename, nil))
	if gone.Code != http.StatusUnauthorized {
		t.Fatalf("a revoked link must stop working, got %d", gone.Code)
	}

	// 查看分享 opens the dialog without minting anything.
	before, err := db.ListShares(ctx, user.ID, resource.ID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	listed := do(http.MethodGet, "/resources/"+resource.ID+"?shares=1", nil)
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), "分享「") {
		t.Fatalf("查看分享 should open the dialog: %d", listed.Code)
	}
	after2, err := db.ListShares(ctx, user.ID, resource.ID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(after2) != len(before) {
		t.Fatalf("merely viewing the list must not create a share: %d -> %d", len(before), len(after2))
	}

	// A share can be made permanent.
	permanent, err := db.CreateShare(ctx, user.ID, resource.ID, "长期", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	page2 := do(http.MethodGet, "/resources/"+resource.ID+"?share="+permanent.ID, nil).Body.String()
	if !strings.Contains(page2, "永不过期") {
		t.Fatal("the dialog must offer a never-expiring option")
	}

	// Another account must not be able to touch these links.
	other, err := db.UpsertUser(ctx, "999", "mallory", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RevokeLink(ctx, other.ID, resource.ID, permanent.ID); err == nil {
		t.Fatal("a link must not be revocable by another account")
	}
}

// TestParseShareDuration pins the formats the custom box accepts, including
// days, which Go's own parser does not understand.
func TestParseShareDuration(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{"30s", 30 * time.Second},
		{"15m", 15 * time.Minute},
		{"90m", 90 * time.Minute},
		{"2h", 2 * time.Hour},
		{"1h30m", 90 * time.Minute},
		{"7d", 7 * 24 * time.Hour},
		{"30d", 30 * 24 * time.Hour},
		{"0.5d", 12 * time.Hour},
		{" 2H ", 2 * time.Hour},
	} {
		got, err := parseShareDuration(tc.in)
		if err != nil || got != tc.want {
			t.Fatalf("%q: got %v %v, want %v", tc.in, got, err, tc.want)
		}
	}
	for _, bad := range []string{"", "soon", "2 hours", "d", "7days", "-1h"} {
		if got, err := parseShareDuration(bad); err == nil && got > 0 {
			t.Fatalf("%q should not parse into a usable duration, got %v", bad, got)
		}
	}
}

// TestServedContentTypeIsDetected proves the derived type reaches the wire,
// and that renaming the path re-derives it on the next save.
func TestServedContentTypeIsDetected(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	app := newTestApp(db, user.GitHubID)

	serve := func(path, token string) string {
		t.Helper()
		response := httptest.NewRecorder()
		app.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://cfg.test/d/"+url.PathEscape(token)+"/"+path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s: status %d", path, response.Code)
		}
		return response.Header().Get("Content-Type")
	}

	for _, tc := range []struct{ path, body, want string }{
		{"clash.yaml", "port: 7890\n", "application/yaml"},
		{"box.json", "{\"a\":1}", "application/json"},
		{"plain", "{\"a\":1}", "application/json"},
		{"nginx.conf", "location / {}\n", "text/plain; charset=utf-8"},
	} {
		resource, err := db.CreateResource(ctx, user.ID, "r", tc.path, []byte(tc.body), "")
		if err != nil {
			t.Fatal(err)
		}
		share, err := db.CreateShare(ctx, user.ID, resource.ID, "s", time.Hour, 0)
		if err != nil {
			t.Fatal(err)
		}
		if got := serve(tc.path, share.Token); got != tc.want {
			t.Errorf("%s served as %q, want %q", tc.path, got, tc.want)
		}
	}

	// Renaming a resource re-derives the type from the new path.
	resource, err := db.CreateResource(ctx, user.ID, "r", "renamed.txt", []byte("{\"a\":1}"), "")
	if err != nil {
		t.Fatal(err)
	}
	share, err := db.CreateShare(ctx, user.ID, resource.ID, "s", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := serve("renamed.txt", share.Token); got != "text/plain; charset=utf-8" {
		t.Fatalf("before rename: %q", got)
	}
	if err := db.UpdateResource(ctx, user.ID, resource.ID, "r", "renamed.json", []byte("{\"a\":1}"), ""); err != nil {
		t.Fatal(err)
	}
	if got := serve("renamed.json", share.Token); got != "application/json" {
		t.Fatalf("after rename: %q, want application/json", got)
	}
}

// The two ways into the dialog are two different screens: 分享 is about the
// link it just made, 查看分享 is about managing what exists.
func TestShareDialogModesAreSeparate(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	app := newTestApp(db, user.GitHubID)
	session, csrf, _, err := db.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	older, err := db.CreateShare(ctx, user.ID, resource.ID, "老链接", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := db.CreateShare(ctx, user.ID, resource.ID, "新链接", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}

	page := func(query string) string {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "https://cfg.test/resources/"+resource.ID+query, nil)
		request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
		request.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrf})
		response := httptest.NewRecorder()
		app.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s: %d", query, response.Code)
		}
		return response.Body.String()
	}

	// Focused on one link: that link, and nothing about the others.
	created := page("?share=" + fresh.ID)
	if !strings.Contains(created, fresh.Token) {
		t.Fatal("the focused dialog must show its own link")
	}
	for _, unwanted := range []string{older.Token, "老链接", "进行中的分享", "全部撤销", "新建分享"} {
		if strings.Contains(created, unwanted) {
			t.Errorf("the create dialog should not mention %q", unwanted)
		}
	}

	// The list: everything live, with the bulk actions.
	listed := page("?shares=1")
	for _, wanted := range []string{older.Token, fresh.Token, "进行中的分享", "全部撤销", "新建分享"} {
		if !strings.Contains(listed, wanted) {
			t.Errorf("the list dialog should include %q", wanted)
		}
	}
	if strings.Contains(listed, "存活时长") {
		t.Error("the list should not carry the per-link settings form")
	}
}
