package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"plainmote/internal/auth"
)

// The allowlist admits new accounts; it is not a live access-control list.
// Removing an ID must not silently revoke sessions belonging to an existing
// user. Account suspension, when added, belongs to the user record instead.
func TestExistingSessionDoesNotDependOnRegistrationAllowlist(t *testing.T) {
	db, user, _ := testDatabase(t)
	app := newTestApp(db)
	app.cfg.RegistrationMode = auth.RegistrationAllowlist

	session, _, _, err := db.CreateSession(context.Background(), user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "https://cfg.test/", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})

	got, _, ok := app.currentUser(request)
	if !ok || got.ID != user.ID {
		t.Fatalf("existing user was rejected after leaving the registration allowlist: ok=%v user=%+v", ok, got)
	}
}

func TestGitHubIdentityAdmissionAppliesOnlyToNewUsers(t *testing.T) {
	db, user, _ := testDatabase(t)
	app := newTestApp(db)
	ctx := context.Background()

	for _, tc := range []struct {
		name    string
		mode    auth.RegistrationMode
		allowed map[string]bool
		id      string
		want    bool
	}{
		{"existing user while closed", auth.RegistrationClosed, nil, user.GitHubID, true},
		{"new user while closed", auth.RegistrationClosed, nil, "200", false},
		{"new user while open", auth.RegistrationOpen, nil, "200", true},
		{"allowlisted new user", auth.RegistrationAllowlist, map[string]bool{"200": true}, "200", true},
		{"new user outside allowlist", auth.RegistrationAllowlist, map[string]bool{"300": true}, "200", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app.cfg.RegistrationMode = tc.mode
			app.cfg.AllowedIDs = tc.allowed
			got, err := app.githubIdentityAdmitted(ctx, tc.id)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("githubIdentityAdmitted() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLoginExplainsRestrictedRegistrationModes(t *testing.T) {
	for _, tc := range []struct {
		mode auth.RegistrationMode
		want string
	}{
		{auth.RegistrationAllowlist, "新账号仅限允许名单内的 GitHub 用户"},
		{auth.RegistrationClosed, "当前不开放新账号注册"},
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
