package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"plainmote/internal/store"
)

// Search text is bounded before it reaches LIKE, without splitting a
// character as a plain byte slice would do to a Chinese term.
func TestSearchTextIsTrimmedOnCharacterBoundaries(t *testing.T) {
	for _, input := range []string{
		strings.Repeat("a", 200),
		strings.Repeat("配置文件", 40),
		strings.Repeat("clash 订阅 ", 30),
		"短的",
		"",
	} {
		got := truncateBytes(input, maxQueryLength)
		if len(got) > maxQueryLength {
			t.Errorf("%q trimmed to %d bytes, over the %d budget", input[:10], len(got), maxQueryLength)
		}
		if !utf8.ValidString(got) {
			t.Errorf("trimming produced invalid UTF-8 for %q", input[:10])
		}
		if len(input) <= maxQueryLength && got != input {
			t.Errorf("a short term should pass through untouched, got %q", got)
		}
		if !strings.HasPrefix(input, got) {
			t.Errorf("trimming should keep a prefix, got %q", got)
		}
	}
}

func TestDashboardURLKeepsSelectedTags(t *testing.T) {
	tags := resourceTags([]string{"  OPS ", "ops", "", "配置 & 日志"})
	if want := []string{"OPS", "配置 & 日志"}; !slices.Equal(tags, want) {
		t.Fatalf("selected tags = %q, want %q", tags, want)
	}
	address, err := url.Parse(dashboardURL(store.ResourceFilter{
		Query: "nginx", Kind: store.KindResource, Tags: tags,
	}, 20, 2))
	if err != nil {
		t.Fatal(err)
	}
	query := address.Query()
	if !slices.Equal(query["tag"], tags) || query.Get("q") != "nginx" ||
		query.Get("kind") != store.KindResource || query.Get("size") != "20" || query.Get("page") != "2" {
		t.Fatalf("filters and pagination did not round-trip: %v", query)
	}
}

func TestDashboardTagSidebarLocalization(t *testing.T) {
	app := localizedApp()
	for _, tc := range []struct {
		locale, tags, search, empty, clear string
	}{
		{"en", "Tags", "Find tags", "No matching tags", "Clear"},
		{"zh-CN", "标签", "搜索标签", "没有匹配的标签", "清除"},
		{"zh-TW", "標籤", "搜尋標籤", "沒有符合的標籤", "清除"},
		{"ja", "タグ", "タグを検索", "一致するタグはありません", "クリア"},
		{"fr", "Étiquettes", "Filtrer les étiquettes", "Aucune étiquette correspondante", "Effacer"},
		{"de", "Tags", "Tags suchen", "Keine passenden Tags", "Leeren"},
	} {
		t.Run(tc.locale, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "https://cfg.test/resources/?tag=ops", nil)
			request.Header.Set("Accept-Language", tc.locale)
			response := httptest.NewRecorder()
			app.renderTemplate(response, request, http.StatusOK, "dashboard.html", pageData{
				Tags:  []tagChip{{Name: "ops", Count: 1, On: true}},
				Pager: pager{Tags: []string{"ops"}, ClearTagsURL: "/resources/"},
			})
			body := response.Body.String()
			for _, want := range []string{
				`<html lang="` + tc.locale + `"`,
				`class="tag-sidebar" aria-label="` + tc.tags + `"`,
				`placeholder="` + tc.search + `" aria-label="` + tc.search + `"`,
				`data-tag-empty hidden>` + tc.empty + `</p>`,
				`class="txt tag-clear" href="/resources/">` + tc.clear + `</a>`,
				`name="tag" value="ops" checked`,
			} {
				if !strings.Contains(body, want) {
					t.Errorf("missing %q", want)
				}
			}
		})
	}
}
