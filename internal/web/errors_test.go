package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"plainmote/internal/store"
)

// TestOnlyRefusalsReachThePerson sends an error nobody marked - what a lost
// database connection looks like - through each of the three ways a failure
// is answered: a page, the command line's API and the admin interface. None
// of them may repeat its text; each says the service failed. A refusal about
// the request itself still comes through in full.
func TestOnlyRefusalsReachThePerson(t *testing.T) {
	leak := "failed to connect to host=postgres user=plainmote database=plainmote"
	raw := fmt.Errorf("update share: %w", errors.New(leak))
	app := staticApp(t)

	text, status := app.writeErrorText("test", raw)
	if status != http.StatusInternalServerError || strings.Contains(text, "postgres") {
		t.Fatalf("a page was told %d %q", status, text)
	}
	if text, status := app.writeErrorText("test", store.ErrTakenDown); status != http.StatusBadRequest || text != store.ErrTakenDown.Error() {
		t.Fatalf("a refusal on a page: %d %q", status, text)
	}

	api := httptest.NewRecorder()
	app.apiRefused(api, "test", raw)
	if api.Code != http.StatusInternalServerError || strings.Contains(api.Body.String(), "postgres") {
		t.Fatalf("the API was told %d %s", api.Code, api.Body.String())
	}
	quota := httptest.NewRecorder()
	app.apiRefused(quota, "test", &store.QuotaError{Limit: 1, Usage: 1})
	if quota.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("a quota through the API: %d", quota.Code)
	}

	// An error shaped like one of the admin interface's codes is still the
	// service failing unless the store refused with it.
	for _, err := range []error{raw, errors.New("timeout")} {
		admin := httptest.NewRecorder()
		adminRequest{app: app, w: admin, r: httptest.NewRequest(http.MethodPost, "/_admin/v1/x", nil)}.failWith(err)
		if admin.Code != http.StatusInternalServerError || strings.Contains(admin.Body.String(), "postgres") || strings.Contains(admin.Body.String(), "timeout") {
			t.Fatalf("the admin interface was told %d %s", admin.Code, admin.Body.String())
		}
	}
	conflict := httptest.NewRecorder()
	adminRequest{app: app, w: conflict, r: httptest.NewRequest(http.MethodPost, "/_admin/v1/x", nil)}.failWith(store.ErrPlanInUse)
	if conflict.Code != http.StatusConflict || !strings.Contains(conflict.Body.String(), "plan_in_use") {
		t.Fatalf("an admin conflict: %d %s", conflict.Code, conflict.Body.String())
	}
}
