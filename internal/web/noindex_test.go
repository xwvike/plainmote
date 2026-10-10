package web

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"regexp"
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
	app := newTestApp(db)
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

	// The home page is the one thing meant to be found, and robots.txt says so
	// too. Everything else stays out.
	if !strings.Contains(robots.Body.String(), "Allow: /$") {
		t.Fatalf("robots.txt must let the home page through, got %q", robots.Body.String())
	}
	if tag := get("/", false).Header().Get("X-Robots-Tag"); tag != "" {
		t.Fatalf("the home page must be indexable, got %q", tag)
	}

	for _, tc := range []struct {
		path     string
		signedIn bool
	}{
		{"/robots.txt", false},
		{"/login", false},
		{"/resources/", true},
		{"/logs", true},
		{"/resources/" + resource.ID, true},
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

// TestPagesCannotBeFramed covers the pages that carry buttons worth tricking
// someone into pressing. SameSite=Lax already keeps a cross-site frame signed
// out; these headers close the rest.
// The pages that say what the service is are meant to be found, and so is what
// a crawler needs to render them. robots.txt, the sitemap and the response
// headers must all agree on which those are.
func TestPublicPagesAreIndexable(t *testing.T) {
	app := &App{cfg: Config{PublicURL: "https://plainmote.link", AnonymousEnabled: true, ContactEmail: "ops@example.com", SessionTTL: time.Hour, MaxContent: 1 << 20}}
	app.templates = app.templateSet()
	app.handler = app.routes()
	get := func(path string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		app.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://plainmote.link"+path, nil))
		return response
	}

	robots := get("/robots.txt").Body.String()
	for _, line := range []string{"Allow: /$", "Allow: /about$", "Allow: /privacy$", "Allow: /terms$", "Allow: /contact$", "Allow: /sitemap.xml$", "Allow: /llms.txt$", "Allow: /static/", "Disallow: /", "Sitemap: https://plainmote.link/sitemap.xml"} {
		if !strings.Contains(robots, line+"\n") {
			t.Errorf("robots.txt is missing %q:\n%s", line, robots)
		}
	}

	sitemap := get("/sitemap.xml")
	if sitemap.Code != http.StatusOK || !strings.HasPrefix(sitemap.Header().Get("Content-Type"), "application/xml") {
		t.Fatalf("sitemap: %d %q", sitemap.Code, sitemap.Header().Get("Content-Type"))
	}
	var parsed struct {
		URLs []struct {
			Loc string `xml:"loc"`
		} `xml:"url"`
	}
	if err := xml.Unmarshal(sitemap.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("sitemap is not valid XML: %v", err)
	}
	var locs []string
	for _, u := range parsed.URLs {
		locs = append(locs, u.Loc)
	}
	want := []string{
		"https://plainmote.link/", "https://plainmote.link/about", "https://plainmote.link/privacy", "https://plainmote.link/terms", "https://plainmote.link/contact",
		"https://plainmote.link/developers/cli", "https://plainmote.link/developers/api", "https://plainmote.link/developers/curl", "https://plainmote.link/developers/ai",
		"https://plainmote.link/zh-cn/", "https://plainmote.link/zh-tw/", "https://plainmote.link/ja/", "https://plainmote.link/fr/", "https://plainmote.link/de/",
		"https://plainmote.link/zh-cn/about", "https://plainmote.link/zh-cn/privacy", "https://plainmote.link/zh-cn/terms", "https://plainmote.link/zh-cn/contact",
		"https://plainmote.link/zh-cn/developers/cli", "https://plainmote.link/zh-cn/developers/api", "https://plainmote.link/zh-cn/developers/curl", "https://plainmote.link/zh-cn/developers/ai",
		"https://plainmote.link/zh-tw/developers/cli", "https://plainmote.link/zh-tw/developers/api", "https://plainmote.link/zh-tw/developers/curl", "https://plainmote.link/zh-tw/developers/ai",
		"https://plainmote.link/ja/developers/cli", "https://plainmote.link/ja/developers/api", "https://plainmote.link/ja/developers/curl", "https://plainmote.link/ja/developers/ai",
		"https://plainmote.link/fr/developers/cli", "https://plainmote.link/fr/developers/api", "https://plainmote.link/fr/developers/curl", "https://plainmote.link/fr/developers/ai",
		"https://plainmote.link/de/developers/cli", "https://plainmote.link/de/developers/api", "https://plainmote.link/de/developers/curl", "https://plainmote.link/de/developers/ai",
	}
	if strings.Join(locs, " ") != strings.Join(want, " ") {
		t.Fatalf("sitemap lists %v, want %v", locs, want)
	}

	for _, page := range []string{"/about", "/privacy", "/terms", "/contact"} {
		response := get(page)
		if tag := response.Header().Get("X-Robots-Tag"); tag != "" {
			t.Errorf("%s carries X-Robots-Tag %q", page, tag)
		}
		body := response.Body.String()
		if strings.Contains(body, "noindex") {
			t.Errorf("%s carries a noindex meta tag", page)
		}
		if !strings.Contains(body, `<link rel="canonical" href="https://plainmote.link`+page+`">`) || !strings.Contains(body, `<meta name="description" content="`) {
			t.Errorf("%s lacks its canonical address or description", page)
		}
	}
	for _, asset := range []string{assetPath("style.css"), assetPath("logo.png")} {
		if tag := get(asset).Header().Get("X-Robots-Tag"); tag != "" {
			t.Errorf("%s carries X-Robots-Tag %q", asset, tag)
		}
	}
	if tag := get("/login").Header().Get("X-Robots-Tag"); !strings.Contains(tag, "noindex") {
		t.Errorf("/login must stay out of the index, got %q", tag)
	}

	home := get("/").Body.String()
	ld := regexp.MustCompile(`<script type="application/ld\+json">(.*?)</script>`).FindStringSubmatch(home)
	if ld == nil {
		t.Fatal("the home page has no structured data")
	}
	var site map[string]string
	if err := json.Unmarshal([]byte(ld[1]), &site); err != nil || site["@type"] != "WebSite" || site["name"] != "PlainMote" || site["url"] != "https://plainmote.link/" {
		t.Fatalf("structured data: %v %v", site, err)
	}
}

// With nothing public there is nothing to list.
func TestNoSitemapWithoutPublicPages(t *testing.T) {
	app := &App{cfg: Config{PublicURL: "https://plainmote.link"}}
	app.templates = app.templateSet()
	app.handler = app.routes()
	response := httptest.NewRecorder()
	app.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://plainmote.link/sitemap.xml", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("sitemap without public pages: %d", response.Code)
	}
	robots := httptest.NewRecorder()
	app.handler.ServeHTTP(robots, httptest.NewRequest(http.MethodGet, "https://plainmote.link/robots.txt", nil))
	if body := robots.Body.String(); body != "User-agent: *\nDisallow: /\n" {
		t.Fatalf("robots.txt without public pages: %q", body)
	}
}

func TestPagesCannotBeFramed(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	app := newTestApp(db)
	session, csrf, _, err := db.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/login", "/resources/", "/resources/" + resource.ID, "/resources/" + resource.ID + "?delete=1", "/logs"} {
		request := httptest.NewRequest(http.MethodGet, "https://cfg.test"+path, nil)
		request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
		request.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrf})
		response := httptest.NewRecorder()
		app.handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			continue
		}
		h := response.Header()
		if h.Get("X-Frame-Options") != "DENY" || !strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Errorf("%s can be framed: X-Frame-Options=%q CSP=%q", path, h.Get("X-Frame-Options"), h.Get("Content-Security-Policy"))
		}
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "same-origin" {
			t.Errorf("%s is missing nosniff or the referrer policy", path)
		}
	}
}

// With scripts off the pages load noscript.css, which removes controls that
// would do nothing: the copy buttons and the editor's encoding controls.
func TestNoScriptStylesheetIsLinkedAndServed(t *testing.T) {
	app := &App{cfg: Config{PublicURL: "https://cfg.test", AnonymousEnabled: true}}
	app.templates = app.templateSet()
	app.handler = app.routes()
	get := func(target string) *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		app.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://cfg.test"+target, nil))
		return response
	}
	if !strings.Contains(get("/").Body.String(), `<noscript><link rel="stylesheet" href="`+assetPath("noscript.css")+`"></noscript>`) {
		t.Fatal("pages do not load the no-script stylesheet")
	}
	css := get(assetPath("noscript.css"))
	if css.Code != http.StatusOK || !strings.Contains(css.Header().Get("Content-Type"), "text/css") {
		t.Fatalf("noscript.css: %d %q", css.Code, css.Header().Get("Content-Type"))
	}
	for _, rule := range []string{
		"[data-copy] { display: none !important; }",
		"[data-upload-key], [data-upload-status] { display: none !important; }",
	} {
		if !strings.Contains(css.Body.String(), rule) {
			t.Errorf("noscript.css is missing %q", rule)
		}
	}
}

// llms.txt describes what this deployment offers, and nothing it does not.
func TestLLMsTextFollowsTheDeployment(t *testing.T) {
	get := func(cfg Config, path string) *httptest.ResponseRecorder {
		cfg.PublicURL, cfg.MaxContent = "https://plainmote.link", 4<<20
		app := &App{cfg: cfg}
		app.templates = app.templateSet()
		app.handler = app.routes()
		response := httptest.NewRecorder()
		app.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://plainmote.link"+path, nil))
		return response
	}
	full := get(Config{AnonymousEnabled: true, ContactEmail: "ops@example.com"}, "/llms.txt")
	body := full.Body.String()
	if full.Code != http.StatusOK || !strings.HasPrefix(body, "# PlainMote\n\n> ") {
		t.Fatalf("llms.txt: %d %q", full.Code, body)
	}
	for _, want := range []string{
		"cmd | curl -F 'content=<-' https://plainmote.link/paste",
		"Default `1h`.", "At most 10 MiB, stored byte for byte", "up to 4 MiB each",
		"`https://plainmote.link/d/<token>/<filename>`",
		"- [Privacy Policy](https://plainmote.link/privacy)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("llms.txt is missing %q", want)
		}
	}
	noBox := get(Config{ContactEmail: "ops@example.com"}, "/llms.txt").Body.String()
	if strings.Contains(noBox, "## Quick share") || strings.Contains(noBox, "/paste") {
		t.Error("llms.txt offers a quick share this deployment does not have")
	}
	if closed := get(Config{}, "/llms.txt"); closed.Code != http.StatusNotFound {
		t.Fatalf("llms.txt on a deployment with nothing public: %d", closed.Code)
	}
}
