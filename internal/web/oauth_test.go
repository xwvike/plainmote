package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"plainmote/internal/auth"
	"plainmote/internal/store"
)

// fakeProvider answers every callback with one provider account, as Google
// would after a consent.
func fakeProvider(name, subject string) oauthProvider {
	return oauthProvider{
		name: name, label: providerLabel(name),
		authorize: func(string, auth.Flow) string { return "https://provider.test/authorize" },
		authenticate: func(context.Context, string, string, auth.Flow) (auth.Profile, error) {
			return auth.Profile{ID: subject, Login: "login-" + subject, Name: "Name " + subject}, nil
		},
	}
}

// callback finishes a flow at the callback with the browser's cookies.
func callback(app *App, p oauthProvider, state string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "https://cfg.test/auth/"+p.name+"/callback?code=c&state="+url.QueryEscape(state), nil)
	for _, cookie := range cookies {
		request.AddCookie(cookie)
	}
	response := httptest.NewRecorder()
	app.finishOAuth(response, request, p)
	return response
}

func flowCookie(t *testing.T) (*http.Cookie, string) {
	t.Helper()
	flow, err := auth.NewFlow()
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: stateCookie, Value: flow.Encode()}, flow.State
}

func sessionOf(response *httptest.ResponseRecorder) string {
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == sessionCookie {
			return cookie.Value
		}
	}
	return ""
}

func sessionCookies(t *testing.T, db *store.Store, userID string) []*http.Cookie {
	t.Helper()
	session, csrf, _, err := db.CreateSession(context.Background(), userID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return []*http.Cookie{{Name: sessionCookie, Value: session}, {Name: csrfCookie, Value: csrf}}
}

func identityOwnerID(t *testing.T, db *store.Store, provider, subject string) string {
	t.Helper()
	user, err := db.IdentityUser(context.Background(), provider, subject)
	if err != nil {
		return ""
	}
	return user.ID
}

func TestOAuthCallbackNeedsItsOwnState(t *testing.T) {
	db, _, _ := testDatabase(t)
	app := newTestApp(db)
	app.cfg.RegistrationMode = auth.RegistrationOpen
	p := fakeProvider(store.ProviderGoogle, "g-1")
	cookie, state := flowCookie(t)

	for name, response := range map[string]*httptest.ResponseRecorder{
		"no cookie":       callback(app, p, state),
		"another state":   callback(app, p, "forged", cookie),
		"no state":        callback(app, p, "", cookie),
		"malformed flow":  callback(app, p, state, &http.Cookie{Name: stateCookie, Value: state}),
		"provider refuse": callbackWithError(app, p, state, cookie),
	} {
		if response.Code < 400 || sessionOf(response) != "" {
			t.Fatalf("%s: %d, session %q", name, response.Code, sessionOf(response))
		}
	}
	if identityOwnerID(t, db, store.ProviderGoogle, "g-1") != "" {
		t.Fatal("a refused callback made an account")
	}
	if response := callback(app, p, state, cookie); response.Code != http.StatusSeeOther || sessionOf(response) == "" {
		t.Fatalf("a valid callback: %d", response.Code)
	}
}

func callbackWithError(app *App, p oauthProvider, state string, cookie *http.Cookie) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, "https://cfg.test/auth/"+p.name+"/callback?error=access_denied&state="+url.QueryEscape(state), nil)
	request.AddCookie(cookie)
	response := httptest.NewRecorder()
	app.finishOAuth(response, request, p)
	return response
}

func TestClosedRegistrationAdmitsOnlyExistingIdentities(t *testing.T) {
	db, alice, _ := testDatabase(t)
	app := newTestApp(db)
	app.cfg.SessionTTL = time.Hour
	ctx := context.Background()
	if err := db.LinkIdentity(ctx, alice.ID, store.Identity{Provider: store.ProviderGoogle, Subject: "alice"}); err != nil {
		t.Fatal(err)
	}

	cookie, state := flowCookie(t)
	if response := callback(app, fakeProvider(store.ProviderGoogle, "newcomer"), state, cookie); response.Code != http.StatusForbidden || sessionOf(response) != "" {
		t.Fatalf("a new Google account while closed: %d", response.Code)
	}
	if identityOwnerID(t, db, store.ProviderGoogle, "newcomer") != "" {
		t.Fatal("closed registration made an account")
	}

	cookie, state = flowCookie(t)
	response := callback(app, fakeProvider(store.ProviderGoogle, "alice"), state, cookie)
	session := sessionOf(response)
	if response.Code != http.StatusSeeOther || session == "" {
		t.Fatalf("alice's linked Google account while closed: %d", response.Code)
	}
	request := httptest.NewRequest(http.MethodGet, "https://cfg.test/", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	if got, _, ok := app.currentUser(request); !ok || got.ID != alice.ID {
		t.Fatalf("the session belongs to %+v, not alice", got)
	}
}

// Linking starts only from the account page's form, and only for the account
// signed in there.
func TestLinkingStartsFromTheAccountPage(t *testing.T) {
	db, alice, _ := testDatabase(t)
	client := signedIn(t, db, alice)
	client.app.google = auth.NewGoogle("client-id", "client-secret")

	forged := client.do(http.MethodPost, accountIdentitiesPath, url.Values{"action": {"link"}, "provider": {"google"}})
	if forged.Code != http.StatusForbidden {
		t.Fatalf("a link without the CSRF token: %d", forged.Code)
	}
	for _, cookie := range forged.Result().Cookies() {
		if cookie.Name == linkCookie && cookie.Value != "" {
			t.Fatal("a forged link request set the link cookie")
		}
	}

	started := client.do(http.MethodPost, accountIdentitiesPath, url.Values{"csrf": {client.csrf}, "action": {"link"}, "provider": {"google"}})
	if started.Code != http.StatusFound || !strings.HasPrefix(started.Header().Get("Location"), "https://accounts.google.com/") {
		t.Fatalf("starting a link: %d %s", started.Code, started.Header().Get("Location"))
	}
	var link *http.Cookie
	for _, cookie := range started.Result().Cookies() {
		if cookie.Name == linkCookie {
			link = cookie
		}
	}
	if link == nil || link.Value != alice.ID || !link.HttpOnly || link.Path != "/auth/google" {
		t.Fatalf("link cookie %+v", link)
	}

	// A plain sign-in started afterwards in the same browser is a sign-in.
	request := httptest.NewRequest(http.MethodGet, "https://cfg.test/auth/google", nil)
	response := httptest.NewRecorder()
	client.app.handler.ServeHTTP(response, request)
	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == linkCookie && cookie.Value != "" {
			t.Fatal("a sign-in kept the link cookie of an earlier link")
		}
	}
}

func TestLinkCallbackGoesOnlyToTheAccountThatStartedIt(t *testing.T) {
	db, alice, _ := testDatabase(t)
	app := newTestApp(db)
	app.cfg.RegistrationMode = auth.RegistrationOpen
	ctx := context.Background()
	bob, err := db.SignIn(ctx, store.Identity{Provider: store.ProviderGoogle, Subject: "bob"})
	if err != nil {
		t.Fatal(err)
	}
	linkFor := func(userID string) *http.Cookie { return &http.Cookie{Name: linkCookie, Value: userID} }

	// Alice starts a link; bob is signed in when it comes back.
	cookie, state := flowCookie(t)
	response := callback(app, fakeProvider(store.ProviderGitHub, "555"), state, append(sessionCookies(t, db, bob.ID), cookie, linkFor(alice.ID))...)
	if response.Code != http.StatusSeeOther || strings.Contains(response.Header().Get("Location"), "linked=") {
		t.Fatalf("a link started by alice finished for bob: %d %s", response.Code, response.Header().Get("Location"))
	}
	if owner := identityOwnerID(t, db, store.ProviderGitHub, "555"); owner != "" {
		t.Fatalf("the GitHub account was linked to %s", owner)
	}

	// Nobody is signed in when it comes back: no account is made either.
	cookie, state = flowCookie(t)
	response = callback(app, fakeProvider(store.ProviderGitHub, "555"), state, cookie, linkFor(alice.ID))
	if sessionOf(response) != "" || identityOwnerID(t, db, store.ProviderGitHub, "555") != "" {
		t.Fatal("a link without a session signed someone in or made an account")
	}

	// Alice finishes her own link.
	cookie, state = flowCookie(t)
	response = callback(app, fakeProvider(store.ProviderGoogle, "alice"), state, append(sessionCookies(t, db, alice.ID), cookie, linkFor(alice.ID))...)
	if response.Header().Get("Location") != accountSignInPath+"?linked=google" {
		t.Fatalf("alice's link: %d %s", response.Code, response.Header().Get("Location"))
	}
	if identityOwnerID(t, db, store.ProviderGoogle, "alice") != alice.ID {
		t.Fatal("alice's Google account is not hers")
	}

	// Bob's Google account stays bob's, even when alice links it.
	cookie, state = flowCookie(t)
	response = callback(app, fakeProvider(store.ProviderGoogle, "bob"), state, append(sessionCookies(t, db, alice.ID), cookie, linkFor(alice.ID))...)
	if loc := response.Header().Get("Location"); loc != accountSignInPath+"?identity=taken" && loc != accountSignInPath+"?identity=linked" {
		t.Fatalf("linking bob's Google account to alice: %s", loc)
	}
	if identityOwnerID(t, db, store.ProviderGoogle, "bob") != bob.ID {
		t.Fatal("bob's Google account moved")
	}
	// Bob's GitHub-free account cannot take alice's GitHub account.
	cookie, state = flowCookie(t)
	response = callback(app, fakeProvider(store.ProviderGitHub, alice.GitHubID), state, append(sessionCookies(t, db, bob.ID), cookie, linkFor(bob.ID))...)
	if response.Header().Get("Location") != accountSignInPath+"?identity=taken" {
		t.Fatalf("linking alice's GitHub account to bob: %s", response.Header().Get("Location"))
	}
	if identityOwnerID(t, db, store.ProviderGitHub, alice.GitHubID) != alice.ID {
		t.Fatal("alice's GitHub account moved")
	}
}

func TestUnlinkingFromTheAccountPage(t *testing.T) {
	db, alice, _ := testDatabase(t)
	client := signedIn(t, db, alice)
	unlink := func(provider string, csrf string) *httptest.ResponseRecorder {
		return client.do(http.MethodPost, accountIdentitiesPath, url.Values{"csrf": {csrf}, "action": {"unlink"}, "provider": {provider}})
	}
	if response := unlink("github", ""); response.Code != http.StatusForbidden {
		t.Fatalf("an unlink without the CSRF token: %d", response.Code)
	}
	if response := unlink("github", client.csrf); response.Header().Get("Location") != accountSignInPath+"?identity=last" {
		t.Fatalf("unlinking the last method: %s", response.Header().Get("Location"))
	}
	if err := db.LinkIdentity(context.Background(), alice.ID, store.Identity{Provider: store.ProviderGoogle, Subject: "alice"}); err != nil {
		t.Fatal(err)
	}
	if response := unlink("github", client.csrf); response.Header().Get("Location") != accountSignInPath+"?unlinked=github" {
		t.Fatalf("unlinking GitHub: %s", response.Header().Get("Location"))
	}
	if identityOwnerID(t, db, store.ProviderGitHub, alice.GitHubID) != "" {
		t.Fatal("the unlinked GitHub account still reaches alice")
	}
}
