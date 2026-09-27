package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func localizedApp() *App {
	app := &App{cfg: Config{PublicURL: "https://plainmote.link", AnonymousEnabled: true, ContactEmail: "ops@example.com", SourceURL: "https://example.com/src"}}
	app.templates = app.templateSet()
	app.handler = app.routes()
	return app
}

func crawl(app *App, path string) *httptest.ResponseRecorder {
	// A crawler: no language preference, no cookie.
	response := httptest.NewRecorder()
	app.handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "https://plainmote.link"+path, nil))
	return response
}

// Each language has an address that shows it to a crawler, which sends no
// language preference, and every version names all the others.
func TestPublicPagesHaveAnAddressPerLanguage(t *testing.T) {
	app := localizedApp()
	for _, c := range []struct{ path, lang, title, canonical string }{
		{"/", "en", "<title>Share text as an expiring link · PlainMote</title>", "https://plainmote.link/"},
		{"/zh-cn/", "zh-CN", "<title>在线分享文本，生成限时链接 · PlainMote</title>", "https://plainmote.link/zh-cn/"},
		{"/zh-tw/", "zh-TW", "<title>線上分享文字，產生限時連結 · PlainMote</title>", "https://plainmote.link/zh-tw/"},
		{"/ja/", "ja", "<title>テキストを期限付きリンクで共有 · PlainMote</title>", "https://plainmote.link/ja/"},
		{"/fr/", "fr", "<title>Partager du texte via un lien temporaire · PlainMote</title>", "https://plainmote.link/fr/"},
		{"/de/", "de", "<title>Text über einen ablaufenden Link teilen · PlainMote</title>", "https://plainmote.link/de/"},
	} {
		response := crawl(app, c.path)
		body := response.Body.String()
		if response.Code != http.StatusOK || response.Header().Get("X-Robots-Tag") != "" {
			t.Errorf("%s: %d %q", c.path, response.Code, response.Header().Get("X-Robots-Tag"))
			continue
		}
		for _, want := range []string{
			`<html lang="` + c.lang + `"`, c.title,
			`<link rel="canonical" href="` + c.canonical + `">`,
			`<link rel="alternate" hreflang="en" href="https://plainmote.link/">`,
			`<link rel="alternate" hreflang="zh-CN" href="https://plainmote.link/zh-cn/">`,
			`<link rel="alternate" hreflang="de" href="https://plainmote.link/de/">`,
			`<link rel="alternate" hreflang="x-default" href="https://plainmote.link/">`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s is missing %q", c.path, want)
			}
		}
	}

	// A language's home keeps its language in the home links, and a Chinese
	// page points the footer at the Chinese legal texts.
	zhTW := crawl(app, "/zh-tw/").Body.String()
	for _, want := range []string{`<a href="/zh-tw/">`, `href="/zh-cn/about"`, `href="/zh-cn/privacy"`} {
		if !strings.Contains(zhTW, want) {
			t.Errorf("/zh-tw/ is missing %q", want)
		}
	}
	if ja := crawl(app, "/ja/").Body.String(); !strings.Contains(ja, `href="/about"`) {
		t.Error("a non-Chinese page links the English legal texts")
	}
}

// The legal texts exist in Chinese at /zh-cn/ and in English at the root;
// nothing else under a language prefix exists.
func TestLegalPagesInChinese(t *testing.T) {
	app := localizedApp()
	page := crawl(app, "/zh-cn/privacy")
	body := page.Body.String()
	for _, want := range []string{
		`<html lang="zh-CN"`, "隐私政策",
		`<link rel="canonical" href="https://plainmote.link/zh-cn/privacy">`,
		`<link rel="alternate" hreflang="zh" href="https://plainmote.link/zh-cn/privacy">`,
		`<link rel="alternate" hreflang="en" href="https://plainmote.link/privacy">`,
	} {
		if page.Code != http.StatusOK || !strings.Contains(body, want) {
			t.Errorf("/zh-cn/privacy is missing %q (%d)", want, page.Code)
		}
	}
	if english := crawl(app, "/privacy").Body.String(); !strings.Contains(english, `<html lang="en"`) || !strings.Contains(english, "Privacy Policy") {
		t.Error("a crawler reads the English policy at /privacy")
	}
	for _, missing := range []string{"/ja/privacy", "/zh-tw/about", "/zh-cn/resources/", "/fr/x"} {
		if got := crawl(app, missing).Code; got != http.StatusNotFound {
			t.Errorf("%s: %d, want 404", missing, got)
		}
	}
}

// The language switch on a language's own address leads to English, which
// follows the browser again; robots.txt lets crawlers in, and the sitemap
// pairs each address with its versions.
func TestLanguageAddressesAreDiscoverable(t *testing.T) {
	app := localizedApp()
	request := httptest.NewRequest(http.MethodGet, "https://plainmote.link/ja/", nil)
	request.Header.Set("Accept-Language", "zh-CN")
	response := httptest.NewRecorder()
	app.handler.ServeHTTP(response, request)
	body := response.Body.String()
	if !strings.Contains(body, `<html lang="ja"`) || !strings.Contains(body, `href="/language?lang=en&amp;next=%2F"`) {
		t.Error("/ja/ shows Japanese whatever the browser asks for, and its switch leads to English")
	}
	robots := crawl(app, "/robots.txt").Body.String()
	for _, want := range []string{"Allow: /zh-cn/$", "Allow: /de/$", "Allow: /zh-cn/terms$"} {
		if !strings.Contains(robots, want) {
			t.Errorf("robots.txt is missing %q", want)
		}
	}
	sitemap := crawl(app, "/sitemap.xml").Body.String()
	if !strings.Contains(sitemap, `<loc>https://plainmote.link/fr/</loc><xhtml:link rel="alternate" hreflang="en" href="https://plainmote.link/"/>`) {
		t.Error("the sitemap does not pair addresses with their language versions")
	}
}
