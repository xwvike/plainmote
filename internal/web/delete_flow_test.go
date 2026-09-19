package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"plainmote/internal/store"
)

// TestDeleteFlowThroughRouter walks deletion the way a person does: open the
// confirm dialog, press it, land back on the dashboard with the resource and
// its addresses gone - and with the log of what was served still there.
func TestDeleteFlowThroughRouter(t *testing.T) {
	db, user, resource := testDatabase(t)
	ctx := context.Background()
	app := newTestApp(db, user.GitHubID)
	session, csrf, _, err := db.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	do := func(method, target string, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		var request *http.Request
		if form == nil {
			request = httptest.NewRequest(method, "https://cfg.test"+target, nil)
		} else {
			request = httptest.NewRequest(method, "https://cfg.test"+target, strings.NewReader(form.Encode()))
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
		request.AddCookie(&http.Cookie{Name: csrfCookie, Value: csrf})
		response := httptest.NewRecorder()
		app.handler.ServeHTTP(response, request)
		return response
	}

	share, err := db.CreateShare(ctx, user.ID, resource.ID, "临时", time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	address := shareAddress(share.Token, resource.Filename)
	served := httptest.NewRecorder()
	app.handler.ServeHTTP(served, httptest.NewRequest(http.MethodGet, "https://cfg.test"+address, nil))
	if served.Code != http.StatusOK {
		t.Fatalf("the link must work before the delete, got %d", served.Code)
	}

	// The confirm step names what goes and says the log stays.
	confirm := do(http.MethodGet, "/resources/"+resource.ID+"?delete=1", nil)
	if confirm.Code != http.StatusOK || !strings.Contains(confirm.Body.String(), "确认删除") {
		t.Fatalf("delete confirm: status %d body %q", confirm.Code, confirm.Body.String())
	}

	// Deleting is a POST, so it needs the CSRF token like every other write.
	forged := do(http.MethodPost, "/resources/"+resource.ID, url.Values{"action": {"delete"}})
	if forged.Code != http.StatusForbidden {
		t.Fatalf("a delete without a csrf token must be refused, got %d", forged.Code)
	}

	deleted := do(http.MethodPost, "/resources/"+resource.ID, url.Values{"action": {"delete"}, "csrf": {csrf}})
	if deleted.Code != http.StatusSeeOther || deleted.Header().Get("Location") != "/resources/" {
		t.Fatalf("delete must land on the dashboard, got %d %q", deleted.Code, deleted.Header().Get("Location"))
	}

	if gone := do(http.MethodGet, "/resources/"+resource.ID, nil); gone.Code != http.StatusNotFound {
		t.Fatalf("the resource page must be gone, got %d", gone.Code)
	}
	after := httptest.NewRecorder()
	app.handler.ServeHTTP(after, httptest.NewRequest(http.MethodGet, "https://cfg.test"+address, nil))
	if after.Code == http.StatusOK {
		t.Fatal("the address must stop resolving once the resource is deleted")
	}

	logs, err := db.ListAccess(ctx, user.ID, "", store.OutcomeSuccess, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) == 0 || logs[0].ResourceName != resource.Name {
		t.Fatalf("the delivery must still be in the log under its name, got %+v", logs)
	}
}
