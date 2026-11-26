package web

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"plainmote/internal/store"
)

// accessOutcomes are the values the public handler records; the filter offers
// these plus the grouped "failed" shorthand (all non-success outcomes).
var accessOutcomes = store.AccessOutcomes

const (
	defaultLogPageSize = 30
	outcomeGroupFailed = "failed"
)

var logPageSizes = []int{20, 30, 50, 100}

type accessLogDay struct {
	DateKey   string
	DateLabel string
	Items     []AccessLog
}

type logLinkChoice struct {
	ID    string
	Label string
	Ended bool
}

func accessOutcomeText(locale, outcome string) string {
	switch outcome {
	case store.OutcomeSuccess:
		return translate(locale, "outcome_success")
	case outcomeGroupFailed:
		return translate(locale, "outcome_group_failed")
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

// handleLogs retires the standalone global access-log page: a bookmark or
// deep link with ?resource={id} lands on that file's access logs, and a bare
// /logs visit returns to the resource browser.
func (a *App) handleLogs(w http.ResponseWriter, r *http.Request) {
	user, _, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	q := r.URL.Query()
	if resourceID := strings.TrimSpace(q.Get("resource")); resourceID != "" {
		if _, err := a.db.ResourceForOwner(r.Context(), user.ID, resourceID); err == nil {
			target := resourceLogsURL(resourceID, strings.TrimSpace(q.Get("link")), strings.TrimSpace(q.Get("outcome")), modalModeFromRequest(r), defaultLogPageSize, 1)
			http.Redirect(w, r, target, http.StatusSeeOther)
			return
		}
	}
	http.Redirect(w, r, "/resources/", http.StatusSeeOther)
}

func (a *App) handleResourceLogs(w http.ResponseWriter, r *http.Request, user User, resourceID string) {
	if r.Method != http.MethodGet {
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	resource, err := a.db.ResourceForOwner(r.Context(), user.ID, resourceID)
	if errors.Is(err, store.ErrNotFound) {
		writePlainError(w, http.StatusNotFound, "resource not found")
		return
	}
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}

	query := r.URL.Query()
	modalMode := modalModeFromRequest(r)
	linkID := strings.TrimSpace(query.Get("link"))
	outcome := strings.TrimSpace(query.Get("outcome"))
	if outcome != "" && outcome != outcomeGroupFailed && !containsString(accessOutcomes, outcome) {
		outcome = ""
	}
	size := normalizeLogPageSize(query.Get("size"))
	page, err := strconv.Atoi(strings.TrimSpace(query.Get("page")))
	if err != nil || page < 1 {
		page = 1
	}

	now := time.Now().UTC()
	shares, ended, err := a.listShares(r.Context(), user.ID, resource.ID, now)
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	lang := requestLanguage(r)
	linkChoices := make([]logLinkChoice, 0, len(shares)+len(ended))
	validLink := linkID == ""
	for _, s := range shares {
		label := s.Name
		if label == "" {
			label = translate(lang.Locale, "untitled_share")
		}
		linkChoices = append(linkChoices, logLinkChoice{ID: s.ID, Label: label})
		if s.ID == linkID {
			validLink = true
		}
	}
	for _, s := range ended {
		label := s.Name
		if label == "" {
			label = translate(lang.Locale, "untitled_share")
		}
		linkChoices = append(linkChoices, logLinkChoice{ID: s.ID, Label: label, Ended: true})
		if s.ID == linkID {
			validLink = true
		}
	}
	if !validLink {
		linkID = ""
	}

	allCount, err := a.db.CountAccessByResource(r.Context(), user.ID, resource.ID)
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	logs, total, err := a.db.ListAccessBy(r.Context(), user.ID, store.AccessLogFilter{
		ResourceID: resource.ID,
		LinkID:     linkID,
		Outcome:    outcome,
	}, size, (page-1)*size)
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	if last := lastPage(total, size); page > last {
		http.Redirect(w, r, resourceLogsURL(resource.ID, linkID, outcome, modalMode, size, last), http.StatusSeeOther)
		return
	}
	pager := buildPager(page, size, total, logPageSizes, func(number int) string {
		return resourceLogsURL(resource.ID, linkID, outcome, modalMode, size, number)
	})

	data := a.basePage(r, user)
	data.Active = "resources"
	data.ModalMode = modalMode
	data.Resource = resource
	data.AccessLogs = logs
	data.AccessLogDays = groupAccessLogsByDay(logs)
	data.AccessLogCount = allCount
	data.LogLinks = linkChoices
	data.LogLink = linkID
	data.LogOutcome = outcome
	data.Outcomes = accessOutcomes
	data.Pager = pager
	if resource.FolderID != "" {
		if crumbs, crumbErr := a.db.FolderBreadcrumbs(r.Context(), user.ID, resource.FolderID); crumbErr == nil {
			data.CurrentFolderID = resource.FolderID
			data.FolderCrumbs = crumbs
		}
	}
	a.renderTemplate(w, r, http.StatusOK, "logs.html", data)
}

func groupAccessLogsByDay(logs []AccessLog) []accessLogDay {
	days := make([]accessLogDay, 0)
	for _, item := range logs {
		key := item.OccurredAt.UTC().Format("2006-01-02")
		label := item.OccurredAt.UTC().Format("2006/01/02")
		if len(days) == 0 || days[len(days)-1].DateKey != key {
			days = append(days, accessLogDay{DateKey: key, DateLabel: label, Items: []AccessLog{item}})
		} else {
			days[len(days)-1].Items = append(days[len(days)-1].Items, item)
		}
	}
	return days
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

func resourceLogsURL(resourceID, linkID, outcome, modalMode string, size, page int) string {
	values := url.Values{}
	if linkID != "" {
		values.Set("link", linkID)
	}
	if outcome != "" {
		values.Set("outcome", outcome)
	}
	if modalMode != "" {
		values.Set("modal", modalMode)
	}
	if size != defaultLogPageSize {
		values.Set("size", strconv.Itoa(size))
	}
	if page > 1 {
		values.Set("page", strconv.Itoa(page))
	}
	base := "/resources/" + resourceID + "/logs"
	if len(values) == 0 {
		return base
	}
	return base + "?" + values.Encode()
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
