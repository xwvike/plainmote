package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"testing"
	"time"
)

// TestDeploymentTopologies pins how the service behaves behind the two ways it
// is meant to run: a reverse proxy on the same host or Docker network, and a
// Cloudflare tunnel pointed at loopback. In both the public path is passed
// through untouched, and the audit log has to name the visitor rather than the
// proxy standing in front.
func TestDeploymentTopologies(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	resource, err := db.CreateResource(ctx, user.ID, "clash", "clash.yaml", []byte("port: 7890\n"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	const visitor = "203.0.113.44"

	fetch := func(publicURL string, mutate func(*http.Request)) (int, string, string) {
		t.Helper()
		share, err := db.CreateShare(ctx, user.ID, resource.ID, "s", time.Hour, 0)
		if err != nil {
			t.Fatal(err)
		}
		app := &App{db: db, cfg: Config{
			MaxContent: 4 << 20,
			PublicURL:  publicURL,
			TrustedProxies: []netip.Prefix{
				netip.MustParsePrefix("127.0.0.1/32"),
				netip.MustParsePrefix("172.18.0.0/16"),
			},
		}}
		app.templates = app.templateSet()
		app.handler = app.routes()

		request := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8964/d/"+url.PathEscape(share.Token)+"/"+resource.Filename, nil)
		mutate(request)
		response := httptest.NewRecorder()
		app.handler.ServeHTTP(response, request)

		logs, err := db.ListAccess(ctx, user.ID, "", "", 1)
		if err != nil || len(logs) == 0 {
			t.Fatalf("no access log written: %v", err)
		}
		return response.Code, logs[0].RemoteIP, app.baseURL(request) + resource.Filename
	}

	// nginx (or any reverse proxy) terminating TLS in front of the container.
	nginx := func(r *http.Request) {
		r.RemoteAddr = "172.18.0.5:41234"
		r.Host = "config.example.com"
		r.Header.Set("X-Forwarded-For", visitor)
		r.Header.Set("X-Forwarded-Proto", "https")
	}
	// cloudflared on loopback; Cloudflare adds CF-Connecting-IP.
	tunnel := func(r *http.Request) {
		r.RemoteAddr = "127.0.0.1:54321"
		r.Host = "config.example.com"
		r.Header.Set("CF-Connecting-IP", visitor)
		r.Header.Set("X-Forwarded-For", visitor)
		r.Header.Set("X-Forwarded-Proto", "https")
	}

	for _, tc := range []struct {
		name   string
		setup  func(*http.Request)
		public string
	}{
		{"nginx, public_url 未配", nginx, ""},
		{"nginx, public_url 已配", nginx, "https://config.example.com"},
		{"tunnel, public_url 未配", tunnel, ""},
		{"tunnel, public_url 已配", tunnel, "https://config.example.com"},
	} {
		status, ip, link := fetch(tc.public, tc.setup)
		if status != http.StatusOK {
			t.Errorf("%s: the configured path must resolve, got %d", tc.name, status)
		}
		if ip != visitor {
			t.Errorf("%s: access log recorded %q, want the visitor %q", tc.name, ip, visitor)
		}
		if want := "https://config.example.com" + resource.Filename; link != want {
			t.Errorf("%s: share link is %q, want %q", tc.name, link, want)
		}
	}
}

// A private peer is not automatically a trusted proxy. Trust is an explicit
// deployment setting, even when Docker puts both services on one network.
func TestPrivatePeerIsNotAutomaticallyTrusted(t *testing.T) {
	app := &App{}
	request := httptest.NewRequest(http.MethodGet, "http://cfg.test/", nil)
	request.RemoteAddr = "172.18.0.8:33333"
	request.Header.Set("X-Forwarded-For", "203.0.113.44")
	request.Header.Set("X-Forwarded-Proto", "https")
	if got := app.clientIP(request); got != "172.18.0.8" {
		t.Fatalf("unconfigured private peer was trusted: %q", got)
	}
	if got := app.baseURL(request); got != "http://cfg.test" {
		t.Fatalf("unconfigured private peer changed the scheme: %q", got)
	}
}

// A directly exposed instance must not let a caller name its own IP.
func TestForwardedHeadersAreIgnoredWithoutAProxy(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	resource, err := db.CreateResource(ctx, user.ID, "clash", "direct.yaml", []byte("a: 1\n"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	share, err := db.CreateShare(ctx, user.ID, resource.ID, "s", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{db: db, cfg: Config{MaxContent: 4 << 20}}
	app.templates = app.templateSet()
	app.handler = app.routes()

	request := httptest.NewRequest(http.MethodGet, "http://cfg.test/d/"+url.PathEscape(share.Token)+"/"+resource.Filename, nil)
	request.RemoteAddr = "198.51.100.7:33333" // a public peer: no proxy in front
	request.Header.Set("X-Forwarded-For", "1.2.3.4")
	request.Header.Set("CF-Connecting-IP", "5.6.7.8")
	request.Header.Set("X-Forwarded-Proto", "https")
	app.handler.ServeHTTP(httptest.NewRecorder(), request)

	logs, err := db.ListAccess(ctx, user.ID, "", "", 1)
	if err != nil || len(logs) == 0 {
		t.Fatalf("no access log: %v", err)
	}
	if logs[0].RemoteIP != "198.51.100.7" {
		t.Fatalf("a spoofed header was believed: logged %q", logs[0].RemoteIP)
	}
	if got := app.baseURL(request); got != "http://cfg.test" {
		t.Fatalf("a spoofed X-Forwarded-Proto changed the scheme: %q", got)
	}
}

// Mounting under a sub-path is not supported: the stored path is matched
// exactly, so a prefix the proxy does not strip simply does not resolve.
func TestSubPathMountIsNotSupported(t *testing.T) {
	db, user, _ := testDatabase(t)
	ctx := context.Background()
	resource, err := db.CreateResource(ctx, user.ID, "clash", "sub.yaml", []byte("a: 1\n"), "", "")
	if err != nil {
		t.Fatal(err)
	}
	share, err := db.CreateShare(ctx, user.ID, resource.ID, "s", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{db: db, cfg: Config{MaxContent: 4 << 20}}
	app.templates = app.templateSet()
	app.handler = app.routes()

	response := httptest.NewRecorder()
	app.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet,
		"http://cfg.test/veil/d/"+url.PathEscape(share.Token)+"/"+resource.Filename, nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("an unstripped prefix should not resolve, got %d", response.Code)
	}
}
