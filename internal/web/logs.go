package web

import (
	"net/http"
	"strings"

	"plainmote/internal/store"
)

// accessOutcomes are the values the public handler records; the filter offers
// exactly these so a typo cannot silently return nothing.
var accessOutcomes = store.AccessOutcomes

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
	logs, err := a.db.ListAccess(r.Context(), user.ID, resourceID, outcome, 200)
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	a.renderTemplate(w, http.StatusOK, "logs.html", pageData{
		User: user, CSRF: csrfValue(r), Active: "logs", BaseURL: a.baseURL(r), SignedIn: true,
		AccessLogs: logs, Resources: resources,
		LogResource: resourceID, LogOutcome: outcome, Outcomes: accessOutcomes,
	})
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
