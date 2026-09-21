package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"plainmote/internal/store"
)

// accessOutcomes are the values the public handler records; the filter offers
// exactly these so a typo cannot silently return nothing.
var accessOutcomes = store.AccessOutcomes

const defaultLogPageSize = 20

var logPageSizes = []int{20, 50, 100}

func accessOutcomeText(outcome string) string {
	switch outcome {
	case store.OutcomeSuccess:
		return "成功"
	case store.OutcomeExpired:
		return "已过期"
	case store.OutcomeExhausted:
		return "次数已用完"
	case store.OutcomeRevoked:
		return "已撤销"
	case store.OutcomeUpstreamError:
		return "回源失败"
	case "invalid":
		return "链接无效"
	case "missing_token":
		return "缺少链接"
	default:
		return "未知结果"
	}
}

func accessOutcomeDescription(outcome string) string {
	switch outcome {
	case store.OutcomeSuccess:
		return "资源已成功交付"
	case store.OutcomeExpired:
		return "访问时分享已经过期"
	case store.OutcomeExhausted:
		return "分享的可用次数已经用完"
	case store.OutcomeRevoked:
		return "访问时分享已经撤销"
	case store.OutcomeUpstreamError:
		return "未能从远程地址获取资源"
	case "invalid", "missing_token":
		return "请求没有对应的有效分享"
	default:
		return "访问没有完成"
	}
}

func (a *App) handleLogs(w http.ResponseWriter, r *http.Request) {
	user, _, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	query := r.URL.Query()
	resourceID := strings.TrimSpace(query.Get("resource"))
	outcome := strings.TrimSpace(query.Get("outcome"))
	size := normalizeLogPageSize(query.Get("size"))
	page, err := strconv.Atoi(strings.TrimSpace(query.Get("page")))
	if err != nil || page < 1 {
		page = 1
	}
	if outcome != "" && !containsString(accessOutcomes, outcome) {
		outcome = ""
	}

	resources, _, err := a.db.ListResources(r.Context(), user.ID, "", 500, 0)
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	if resourceID != "" && !ownsResource(resources, resourceID) {
		resourceID = ""
	}
	logs, total, err := a.db.ListAccessPage(r.Context(), user.ID, resourceID, outcome, size, (page-1)*size)
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	if last := lastPage(total, size); page > last {
		http.Redirect(w, r, logsURL(resourceID, outcome, size, last), http.StatusSeeOther)
		return
	}
	pager := buildPager(page, size, total, logPageSizes, func(number int) string {
		return logsURL(resourceID, outcome, size, number)
	})
	a.renderTemplate(w, http.StatusOK, "logs.html", pageData{
		User: user, CSRF: csrfValue(r), Active: "logs", BaseURL: a.baseURL(r), SignedIn: true,
		AccessLogs: logs, Resources: resources,
		LogResource: resourceID, LogOutcome: outcome, Outcomes: accessOutcomes,
		Pager: pager,
	})
}

func normalizeLogPageSize(value string) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return defaultLogPageSize
	}
	for _, allowed := range logPageSizes {
		if parsed == allowed {
			return parsed
		}
	}
	return defaultLogPageSize
}

func logsURL(resourceID, outcome string, size, page int) string {
	values := url.Values{}
	if resourceID != "" {
		values.Set("resource", resourceID)
	}
	if outcome != "" {
		values.Set("outcome", outcome)
	}
	if size != defaultLogPageSize {
		values.Set("size", strconv.Itoa(size))
	}
	if page > 1 {
		values.Set("page", strconv.Itoa(page))
	}
	if len(values) == 0 {
		return "/logs"
	}
	return "/logs?" + values.Encode()
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func ownsResource(resources []Resource, id string) bool {
	for _, resource := range resources {
		if resource.ID == id {
			return true
		}
	}
	return false
}
