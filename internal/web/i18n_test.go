package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestBrowserLanguage(t *testing.T) {
	tests := []struct {
		header string
		want   string
	}{
		{"", "en"},
		{"en-US,en;q=0.9", "en"},
		{"zh-CN,zh;q=0.9", "zh-CN"},
		{"zh-Hant-HK,zh;q=0.8", "zh-TW"},
		{"ja-JP", "ja"},
		{"fr-CA", "fr"},
		{"de-AT", "de"},
		{"es-MX,fr;q=0.8", "fr"},
		{"es-MX,pt;q=0.8", "en"},
	}
	for _, test := range tests {
		if got := browserLanguage(test.header); got != test.want {
			t.Errorf("browserLanguage(%q) = %q, want %q", test.header, got, test.want)
		}
	}
}

func TestLanguageViewOnlyOffersDetectedLanguageAndEnglish(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "https://cfg.test/resources/?page=2", nil)
	request.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	view := requestLanguage(request)
	if view.Locale != "zh-CN" || view.NativeLabel != "文" || !view.ShowSwitch {
		t.Fatalf("Chinese language view = %#v", view)
	}
	if !strings.Contains(view.NativeURL, "lang=zh-CN") || !strings.Contains(view.EnglishURL, "lang=en") ||
		!strings.Contains(view.EnglishURL, "next=%2Fresources%2F%3Fpage%3D2") {
		t.Fatalf("language links did not preserve the current page: %#v", view)
	}

	request.AddCookie(&http.Cookie{Name: languageCookie, Value: "en"})
	if selected := requestLanguage(request); selected.Locale != "en" || !selected.ShowSwitch {
		t.Fatalf("English selection must retain the Chinese/English switch: %#v", selected)
	}

	unsupported := httptest.NewRequest(http.MethodGet, "https://cfg.test/", nil)
	unsupported.Header.Set("Accept-Language", "es-MX")
	if selected := requestLanguage(unsupported); selected.Locale != "en" || selected.ShowSwitch {
		t.Fatalf("unsupported browser language must get plain English: %#v", selected)
	}
}

func TestLanguageHandlerPersistsAValidSelection(t *testing.T) {
	app := &App{cfg: Config{PublicURL: "https://cfg.test"}}
	request := httptest.NewRequest(http.MethodGet, "https://cfg.test/language?lang=en&next=%2Fresources%2F%3Fpage%3D2", nil)
	request.Header.Set("Accept-Language", "de-DE")
	response := httptest.NewRecorder()
	app.handleLanguage(response, request)
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/resources/?page=2" {
		t.Fatalf("language redirect = %d %q", response.Code, response.Header().Get("Location"))
	}
	if response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("language response may be cached: %q", response.Header().Get("Cache-Control"))
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != languageCookie || cookies[0].Value != "en" ||
		!cookies[0].HttpOnly || !cookies[0].Secure || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatalf("language cookie = %#v", cookies)
	}

	invalid := httptest.NewRequest(http.MethodGet, "https://cfg.test/language?lang=fr&next=https://evil.test", nil)
	invalid.Header.Set("Accept-Language", "de-DE")
	response = httptest.NewRecorder()
	app.handleLanguage(response, invalid)
	if response.Header().Get("Location") != "/" || response.Result().Cookies()[0].Value != "en" {
		t.Fatalf("invalid selection or redirect was accepted: %q %#v", response.Header().Get("Location"), response.Result().Cookies())
	}
}

func TestEveryLocaleHasEveryMessage(t *testing.T) {
	for key, values := range messageTable {
		for index, locale := range []string{"en", "zh-CN", "zh-TW", "ja", "fr", "de"} {
			if strings.TrimSpace(values[index]) == "" {
				t.Errorf("message %q is empty for %s", key, locale)
			}
		}
	}
}

func TestEveryTranslationKeepsItsPlaceholders(t *testing.T) {
	bracePattern := regexp.MustCompile(`\{[a-z_]+\}`)
	formatPattern := regexp.MustCompile(`%(?:\[[0-9]+\])?[-+#0-9 .]*[a-zA-Z]`)
	signature := func(value string) string {
		parts := bracePattern.FindAllString(value, -1)
		for _, match := range formatPattern.FindAllString(value, -1) {
			parts = append(parts, match[len(match)-1:])
		}
		slices.Sort(parts)
		return strings.Join(parts, "|")
	}
	for key, values := range messageTable {
		want := signature(values[0])
		for index, locale := range []string{"en", "zh-CN", "zh-TW", "ja", "fr", "de"} {
			if got := signature(values[index]); got != want {
				t.Errorf("message %q placeholders for %s = %q, want %q", key, locale, got, want)
			}
		}
	}
}

func TestEveryTemplateMessageExists(t *testing.T) {
	paths, err := fs.Glob(webAssets, "templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`\btr\s+[^\s}]+\s+"([^"]+)"`)
	for _, path := range paths {
		body, err := fs.ReadFile(webAssets, path)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range pattern.FindAllSubmatch(body, -1) {
			key := string(match[1])
			if _, ok := messageTable[key]; !ok {
				t.Errorf("%s uses missing message %q", path, key)
			}
		}
	}
}

func TestDynamicMessagesFormatCleanly(t *testing.T) {
	for _, locale := range []string{"en", "zh-CN", "zh-TW", "ja", "fr", "de"} {
		values := []string{
			pageSummary(locale, 1, 2, 3, "items"),
			pageSummary(locale, 1, 1, 1, "entries"),
			countText(locale, 1, "shares"),
			countText(locale, 2, "shares"),
			deleteWarning(locale, 1),
			deleteWarning(locale, 2),
		}
		for _, duration := range []time.Duration{
			24 * time.Hour, 25 * time.Hour, 26 * time.Hour, 49 * time.Hour, 50 * time.Hour,
			time.Hour, time.Hour + time.Minute, time.Hour + 2*time.Minute,
			2 * time.Hour, 2*time.Hour + time.Minute, 2*time.Hour + 2*time.Minute,
			time.Minute, 2 * time.Minute,
		} {
			values = append(values, remainingText(locale, duration))
		}
		for _, value := range values {
			if strings.Contains(value, "%!") || strings.TrimSpace(value) == "" {
				t.Errorf("invalid dynamic message for %s: %q", locale, value)
			}
		}
	}
}

func TestRemainingTimeUsesNaturalSingularForms(t *testing.T) {
	for _, test := range []struct {
		locale string
		left   time.Duration
		want   string
	}{
		{"en", 24 * time.Hour, "1 day left"},
		{"fr", 25 * time.Hour, "Encore 1 jour et 1 heure"},
		{"de", 49 * time.Hour, "Noch 2 Tage 1 Stunde"},
		{"ja", 61 * time.Minute, "残り1時間1分"},
		{"zh-TW", time.Minute, "剩 1 分鐘"},
	} {
		if got := remainingText(test.locale, test.left); got != test.want {
			t.Errorf("remainingText(%q, %s) = %q, want %q", test.locale, test.left, got, test.want)
		}
	}
	if got := deleteWarning("fr", 1); got != "Cette ressource sera supprimée définitivement et 1 partage actif sera révoqué." {
		t.Fatalf("French singular delete warning = %q", got)
	}
}

func TestPageErrorsAreLocalized(t *testing.T) {
	tests := []struct {
		locale, source, want string
	}{
		{"en", "内容不能为空", "Content cannot be empty."},
		{"zh-TW", "文件名不能包含路径分隔符", "檔名不能包含路徑分隔符。"},
		{"ja", "资源数量已达上限：已有 10 个，上限 10 个。请先删除不再需要的资源。", "現在10件、上限10件"},
		{"fr", `不支持的文本编码 "x-test"`, `Encodage de texte non pris en charge : &#34;x-test&#34;`},
		{"de", "上游内容超过 4 MiB 上限", "4 MiB"},
		{"en", "content is too large（该内容无法在页面中保留，请重新选择文件）", "choose the file again"},
	}
	for _, test := range tests {
		got := localizePageError(test.locale, test.source)
		// localizePageError returns plain text. The quote escaping in this one
		// assertion is therefore normalized before comparing with HTML copy.
		want := strings.ReplaceAll(test.want, "&#34;", `"`)
		if !strings.Contains(got, want) || strings.Contains(got, "%!") {
			t.Errorf("localizePageError(%q, %q) = %q, want it to contain %q", test.locale, test.source, got, want)
		}
	}
	if got := localizePageError("en", "provider-specific failure"); got != "provider-specific failure" {
		t.Fatalf("unknown error was replaced: %q", got)
	}
}

func TestLanguageSwitcherRendering(t *testing.T) {
	app := &App{cfg: Config{AnonymousEnabled: true}}
	app.templates = app.templateSet()

	for _, test := range []struct {
		header, lang, text string
		switcher           bool
	}{
		{"", "en", "Create share link", false},
		{"es-MX", "en", "Create share link", false},
		{"ja-JP", "ja", "共有リンクを作成", true},
		{"zh-TW", "zh-TW", "產生分享連結", true},
	} {
		request := httptest.NewRequest(http.MethodGet, "https://cfg.test/", nil)
		request.Header.Set("Accept-Language", test.header)
		response := httptest.NewRecorder()
		app.renderTemplate(response, request, http.StatusOK, "home.html", pageData{MaxPaste: 1024})
		body := response.Body.String()
		if !strings.Contains(body, `<html lang="`+test.lang+`">`) || !strings.Contains(body, test.text) {
			t.Fatalf("rendered %q page incorrectly: %s", test.header, body)
		}
		if got := strings.Contains(body, `class="lang-switch"`); got != test.switcher {
			t.Fatalf("switcher for %q = %t, want %t", test.header, got, test.switcher)
		}
		vary := strings.Join(response.Header().Values("Vary"), ",")
		if !strings.Contains(vary, "Accept-Language") || !strings.Contains(vary, "Cookie") {
			t.Fatalf("language-dependent page has Vary %q", vary)
		}
	}
}

func TestIndexableHomeHasLocalizedDiscoveryMetadata(t *testing.T) {
	app := &App{cfg: Config{AnonymousEnabled: true}}
	app.templates = app.templateSet()
	request := httptest.NewRequest(http.MethodGet, "https://plainmote.example/", nil)
	request.Header.Set("Accept-Language", "zh-CN")
	response := httptest.NewRecorder()
	app.renderTemplate(response, request, http.StatusOK, "home.html", pageData{
		BaseURL: "https://plainmote.example", Indexable: true, MaxPaste: 1024, Canonical: "/",
	})
	body := response.Body.String()
	for _, expected := range []string{
		`<title>在线分享配置或日志，生成限时链接 · PlainMote</title>`,
		`<h1>把配置、日志或文本内容，转成链接</h1>`,
		`<meta name="description" content="无需登录，在线分享配置、日志和其他文件`,
		`<meta name="keywords" content="配置文件分享, 日志分享`,
		`<h2>分享调试日志</h2>`,
		`<h2>供程序读取的配置</h2>`,
		`<h2>从终端分享</h2>`,
		`curl -F <span>'content=&lt;-'</span> <span>https://plainmote.example/paste</span></code>`,
		`<link rel="canonical" href="https://plainmote.example/">`,
		`<meta property="og:locale" content="zh_CN">`,
		`<meta property="og:url" content="https://plainmote.example/">`,
		`<meta name="twitter:card" content="summary">`,
	} {
		if !strings.Contains(body, expected) {
			t.Errorf("home metadata is missing %q", expected)
		}
	}

	response = httptest.NewRecorder()
	app.renderTemplate(response, request, http.StatusOK, "home.html", pageData{BaseURL: "https://plainmote.example"})
	body = response.Body.String()
	if strings.Contains(body, `rel="canonical"`) || strings.Contains(body, `property="og:`) {
		t.Fatal("a non-indexable paste result exposed homepage discovery metadata")
	}
}
