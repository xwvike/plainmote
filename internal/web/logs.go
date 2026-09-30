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

func accessOutcomeText(locale, outcome string) string {
	switch outcome {
	case store.OutcomeSuccess:
		return translate(locale, "outcome_success")
	case store.OutcomeExpired:
		return translate(locale, "outcome_expired")
	case store.OutcomeExhausted:
		return translate(locale, "outcome_exhausted")
	case store.OutcomeRevoked:
		return translate(locale, "outcome_revoked")
	case store.OutcomeUpstreamError:
		return translate(locale, "outcome_upstream_error")
	case store.OutcomeTakenDown:
		return translate(locale, "outcome_taken_down")
	case store.OutcomeSuspended:
		return translate(locale, "outcome_suspended")
	case "invalid":
		return translate(locale, "outcome_invalid")
	case "missing_token":
		return translate(locale, "outcome_missing_token")
	default:
		return translate(locale, "outcome_unknown")
	}
}

func accessOutcomeDescription(locale, outcome string) string {
	switch outcome {
	case store.OutcomeSuccess:
		return translate(locale, "outcome_desc_success")
	case store.OutcomeExpired:
		return translate(locale, "outcome_desc_expired")
	case store.OutcomeExhausted:
		return translate(locale, "outcome_desc_exhausted")
	case store.OutcomeRevoked:
		return translate(locale, "outcome_desc_revoked")
	case store.OutcomeUpstreamError:
		return translate(locale, "outcome_desc_upstream_error")
	case store.OutcomeTakenDown:
		return translate(locale, "outcome_desc_taken_down")
	case store.OutcomeSuspended:
		return translate(locale, "outcome_desc_suspended")
	case "invalid", "missing_token":
		return translate(locale, "outcome_desc_invalid")
	default:
		return translate(locale, "outcome_desc_unknown")
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
	a.renderTemplate(w, r, http.StatusOK, "logs.html", pageData{
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
