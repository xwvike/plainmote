package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"plainmote/internal/auth"
	"plainmote/internal/store"
)

// The registration mode admits new accounts; it is not a live access-control
// list. Closing registration must not silently revoke sessions belonging to an
// existing user. Suspension belongs to the user record instead.
func TestExistingSessionDoesNotDependOnRegistrationMode(t *testing.T) {
	db, user, _ := testDatabase(t)
	app := newTestApp(db)
	app.cfg.RegistrationMode = auth.RegistrationClosed

	session, _, _, err := db.CreateSession(context.Background(), user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "https://cfg.test/", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})

	got, _, ok := app.currentUser(request)
	if !ok || got.ID != user.ID {
		t.Fatalf("existing user was rejected after registration closed: ok=%v user=%+v", ok, got)
	}
}

func TestGitHubIdentityAdmissionAppliesOnlyToNewUsers(t *testing.T) {
	db, user, _ := testDatabase(t)
	app := newTestApp(db)
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		mode auth.RegistrationMode
		id   string
		want bool
	}{
		{"existing user while closed", auth.RegistrationClosed, user.GitHubID, true},
		{"new user while closed", auth.RegistrationClosed, "200", false},
		{"new user while open", auth.RegistrationOpen, "200", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app.cfg.RegistrationMode = tc.mode
			got, err := app.identityAdmitted(ctx, store.ProviderGitHub, tc.id)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("identityAdmitted() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLoginExplainsClosedRegistration(t *testing.T) {
	for _, tc := range []struct {
		mode auth.RegistrationMode
		want string
	}{
		{auth.RegistrationClosed, "New account registration is currently closed."},
	} {
		app := &App{cfg: Config{RegistrationMode: tc.mode}}
		app.templates = app.templateSet()
		response := httptest.NewRecorder()
		app.handleLogin(response, httptest.NewRequest(http.MethodGet, "https://cfg.test/login", nil))
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), tc.want) {
			t.Fatalf("login mode %q: status=%d body=%q", tc.mode, response.Code, response.Body.String())
		}
	}
}
