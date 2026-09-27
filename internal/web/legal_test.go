package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func legalApp(cfg Config) *App {
	if cfg.PublicURL == "" {
		cfg.PublicURL = "https://cfg.test"
	}
	if cfg.SessionTTL == 0 {
		cfg.SessionTTL = 720 * time.Hour
	}
	if cfg.MaxContent == 0 {
		cfg.MaxContent = 4 << 20
	}
	return New(cfg, nil, nil, nil)
}

func getLegal(app *App, path, language string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, "https://cfg.test"+path, nil)
	if language != "" {
		r.Header.Set("Accept-Language", language)
	}
	w := httptest.NewRecorder()
	app.Handler().ServeHTTP(w, r)
	return w
}

func TestLegalPagesNeedAContactAddress(t *testing.T) {
	app := legalApp(Config{})
	for _, page := range legalPages {
		if got := getLegal(app, "/"+page, "").Code; got != http.StatusNotFound {
			t.Errorf("/%s without a contact address: %d, want 404", page, got)
		}
	}
	if login := getLegal(app, "/login", "").Body.String(); strings.Contains(login, `href="/privacy"`) {
		t.Fatal("the footer links to pages that do not exist")
	}
}

func TestLegalPagesRenderInChineseOrEnglish(t *testing.T) {
	app := legalApp(Config{ContactEmail: "ops@example.com", Operator: "Example Ops", LogRetention: 720 * time.Hour})
	titles := map[string][2]string{
		"about":   {"<h1>关于</h1>", "<h1>About</h1>"},
		"privacy": {"<h1>隐私政策</h1>", "<h1>Privacy Policy</h1>"},
		"terms":   {"<h1>服务条款</h1>", "<h1>Terms of Service</h1>"},
		"contact": {"<h1>联系我们</h1>", "<h1>Contact us</h1>"},
	}
	for _, page := range legalPages {
		for index, language := range []string{"zh-TW,zh;q=0.9", "ja-JP"} {
			response := getLegal(app, "/"+page, language)
			body := response.Body.String()
			if response.Code != http.StatusOK || strings.Contains(body, "template error") {
				t.Fatalf("/%s (%s): %d %s", page, language, response.Code, body)
			}
			if !strings.Contains(body, titles[page][index]) {
				t.Errorf("/%s (%s) is not in the expected language", page, language)
			}
			if !strings.Contains(body, `aria-current="page">`) {
				t.Errorf("/%s does not mark itself in the footer", page)
			}
		}
	}
	for _, page := range []string{"privacy", "terms", "contact"} {
		if body := getLegal(app, "/"+page, "").Body.String(); !strings.Contains(body, `href="mailto:ops@example.com"`) {
			t.Errorf("/%s does not give the contact address", page)
		}
	}
	if body := getLegal(app, "/about", "").Body.String(); !strings.Contains(body, "PlainMote is operated by Example Ops.") {
		t.Error("the about page does not name the operator")
	}
	// The site is named by the configured public origin, never by the host a
	// request happened to arrive on.
	live := legalApp(Config{ContactEmail: "ops@example.com", Operator: "example", PublicURL: "https://plainmote.link"})
	if body := getLegal(live, "/privacy", "zh-CN").Body.String(); !strings.Contains(body, "PlainMote（plainmote.link，以下简称“本服务”）由 example（以下简称“我们”）运营。") {
		t.Error("the privacy policy does not introduce the service by its public domain and operator")
	}
	if login := getLegal(app, "/login", "").Body.String(); !strings.Contains(login, `href="/privacy"`) || !strings.Contains(login, `<a href="/llms.txt">llms.txt</a>`) {
		t.Error("the sign-in page footer lacks the privacy policy or llms.txt")
	}
}

// The policy states the periods and limits this deployment enforces, so a
// changed setting cannot leave the text promising something else.
func TestPrivacyPolicyStatesTheConfiguredFacts(t *testing.T) {
	kept := getLegal(legalApp(Config{ContactEmail: "ops@example.com", LogRetention: 168 * time.Hour, SessionTTL: 36 * time.Hour}), "/privacy", "").Body.String()
	for _, want := range []string{"deleted automatically after 7 days", "remains valid for 36 hours", "up to 2 times every 24 hours", "1.5 Data export records", "1.6 Cookies"} {
		if !strings.Contains(kept, want) {
			t.Errorf("privacy policy is missing %q", want)
		}
	}
	if strings.Contains(kept, "1.5 Quick shares") {
		t.Error("quick shares are described on a deployment that does not take them")
	}

	forever := getLegal(legalApp(Config{ContactEmail: "ops@example.com", AnonymousEnabled: true, PublicURL: "http://cfg.test"}), "/privacy", "zh-CN").Body.String()
	for _, want := range []string{"访问记录：不自动删除", "临时分享", "链接最长在 30 分钟后失效", "每个账号每 24 小时最多可导出 2 次", "（六）数据导出记录", "（七）Cookie"} {
		if !strings.Contains(forever, want) {
			t.Errorf("privacy policy is missing %q", want)
		}
	}

	terms := getLegal(legalApp(Config{ContactEmail: "ops@example.com", AnonymousEnabled: true}), "/terms", "").Body.String()
	for _, want := range []string{"may not exceed 4 MiB", "may not exceed 128 KiB"} {
		if !strings.Contains(terms, want) {
			t.Errorf("terms are missing %q", want)
		}
	}
}

func TestPrivacyPolicyNamesR2OnlyWhenConfigured(t *testing.T) {
	r2 := getLegal(legalApp(Config{ContactEmail: "ops@example.com", BlobEndpoint: "https://0123abcd.r2.cloudflarestorage.com"}), "/privacy", "zh-CN").Body.String()
	if !strings.Contains(r2, "资源正文存放于 Cloudflare R2 对象存储") || !strings.Contains(r2, "<strong>Cloudflare：</strong>") {
		t.Error("an R2 deployment does not name R2 as the storage provider")
	}
	// What the policy states is what data goes where and why; how the service
	// is built does not belong in it.
	article := r2[strings.Index(r2, `<article class="doc">`):strings.Index(r2, "</article>")]
	for _, detail := range []string{"AES", "SHA-256", "256", "read:user", "X-Forwarded-For", "CF-Connecting-IP", "plainmote_", "[redacted]", "fonts.googleapis.com", "哈希"} {
		if strings.Contains(article, detail) {
			t.Errorf("the privacy policy exposes an implementation detail: %q", detail)
		}
	}
	minio := getLegal(legalApp(Config{ContactEmail: "ops@example.com", BlobEndpoint: "http://minio:9000"}), "/privacy", "").Body.String()
	if strings.Contains(minio, "R2") {
		t.Error("the policy names R2 on a deployment that does not use it")
	}
}

func TestLegalDuration(t *testing.T) {
	for _, tc := range []struct {
		d      time.Duration
		zh, en string
	}{
		{720 * time.Hour, "30 天", "30 days"},
		{24 * time.Hour, "24 小时", "24 hours"},
		{48 * time.Hour, "2 天", "2 days"},
		{36 * time.Hour, "36 小时", "36 hours"},
		{90 * time.Minute, "90 分钟", "90 minutes"},
		{time.Minute, "1 分钟", "1 minute"},
	} {
		if got := legalDuration(true, tc.d); got != tc.zh {
			t.Errorf("legalDuration(zh, %s) = %q, want %q", tc.d, got, tc.zh)
		}
		if got := legalDuration(false, tc.d); got != tc.en {
			t.Errorf("legalDuration(en, %s) = %q, want %q", tc.d, got, tc.en)
		}
	}
}

func TestLegalMethods(t *testing.T) {
	app := legalApp(Config{ContactEmail: "ops@example.com"})
	r := httptest.NewRequest(http.MethodPost, "https://cfg.test/privacy", nil)
	w := httptest.NewRecorder()
	app.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /privacy: %d", w.Code)
	}
}
