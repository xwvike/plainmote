package web

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"plainmote/internal/auth"
	"plainmote/internal/store"
)

// oauthPrefix is where each provider's sign-in starts, /auth/<provider>, and
// comes back, /auth/<provider>/callback.
const oauthPrefix = "/auth/"

// linkCookie marks a flow started from the account page to add a sign-in
// method rather than to sign in. Only that page's form sets it, behind its
// CSRF check.
const linkCookie = "plainmote_oauth_link"

// oauthProvider is one way to sign in, as the handlers see it.
type oauthProvider struct {
	name         string
	label        string
	authorize    func(redirectURI string, flow auth.Flow) string
	authenticate func(ctx context.Context, code, redirectURI string, flow auth.Flow) (auth.Profile, error)
}

// providers are the configured ways to sign in, Google first.
func (a *App) providers() []oauthProvider {
	var list []oauthProvider
	if a.google.Configured() {
		list = append(list, oauthProvider{
			name: store.ProviderGoogle, label: "Google",
			authorize: a.google.AuthorizationURL, authenticate: a.google.Authenticate,
		})
	}
	if a.github != nil && a.github.Configured() {
		list = append(list, oauthProvider{
			name: store.ProviderGitHub, label: "GitHub",
			authorize: func(redirectURI string, flow auth.Flow) string {
				return a.github.AuthorizationURL(redirectURI, flow.State)
			},
			authenticate: func(ctx context.Context, code, redirectURI string, _ auth.Flow) (auth.Profile, error) {
				return a.github.Authenticate(ctx, code, redirectURI)
			},
		})
	}
	return list
}

func (a *App) provider(name string) (oauthProvider, bool) {
	for _, p := range a.providers() {
		if p.name == name {
			return p, true
		}
	}
	return oauthProvider{}, false
}

// providerLabel names a provider whether or not it is still configured: an
// account keeps a sign-in method the deployment has since turned off.
func providerLabel(name string) string {
	switch name {
	case store.ProviderGitHub:
		return "GitHub"
	case store.ProviderGoogle:
		return "Google"
	}
	return name
}

// loginOption is one sign-in button.
type loginOption struct {
	Provider string
	Label    string
	URL      string
}

func (a *App) loginOptions(next string) []loginOption {
	var options []loginOption
	for _, p := range a.providers() {
		address := oauthPrefix + p.name
		if next != "" {
			address += "?next=" + url.QueryEscape(next)
		}
		options = append(options, loginOption{Provider: p.name, Label: p.label, URL: address})
	}
	return options
}

func (a *App) handleOAuth(w http.ResponseWriter, r *http.Request) {
	name, callback := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, oauthPrefix), "/callback")
	p, ok := a.provider(name)
	if !ok {
		writePlainError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if callback {
		a.finishOAuth(w, r, p)
		return
	}
	a.beginOAuth(w, r, p, safeNext(r.URL.Query().Get("next")), false)
}

func (a *App) beginOAuth(w http.ResponseWriter, r *http.Request, p oauthProvider, next string, link bool) {
	flow, err := auth.NewFlow()
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	path := oauthPrefix + p.name
	secure := strings.HasPrefix(strings.ToLower(a.baseURL(r)), "https://")
	set := func(name, value string) {
		maxAge := 600
		if value == "" {
			maxAge = -1
		}
		http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: path, MaxAge: maxAge, HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode})
	}
	set(stateCookie, flow.Encode())
	set(nextCookie, next)
	if link {
		set(linkCookie, "1")
	} else {
		set(linkCookie, "")
	}
	http.Redirect(w, r, p.authorize(a.baseURL(r)+path+"/callback", flow), http.StatusFound)
}

func (a *App) finishOAuth(w http.ResponseWriter, r *http.Request, p oauthProvider) {
	path := oauthPrefix + p.name
	a.clearOAuthCookies(w, path)
	var flow auth.Flow
	if cookie, err := r.Cookie(stateCookie); err == nil {
		flow, _ = auth.ParseFlow(cookie.Value)
	}
	state := r.URL.Query().Get("state")
	if flow.State == "" || state == "" || subtle.ConstantTimeCompare([]byte(flow.State), []byte(state)) != 1 {
		a.renderError(w, http.StatusBadRequest, errors.New("invalid OAuth state"))
		return
	}
	if oauthErr := r.URL.Query().Get("error"); oauthErr != "" {
		a.renderError(w, http.StatusUnauthorized, fmt.Errorf("%s sign-in failed: %s", p.label, oauthErr))
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" {
		a.renderError(w, http.StatusBadRequest, errors.New("OAuth code is missing"))
		return
	}
	profile, err := p.authenticate(r.Context(), code, a.baseURL(r)+path+"/callback", flow)
	if err != nil {
		a.renderError(w, http.StatusBadGateway, err)
		return
	}
	identity := store.Identity{Provider: p.name, Subject: profile.ID, Login: profile.Login, Name: profile.Name, AvatarURL: profile.AvatarURL}
	if cookie, err := r.Cookie(linkCookie); err == nil && cookie.Value == "1" {
		a.finishLink(w, r, identity)
		return
	}
	admitted, err := a.identityAdmitted(r.Context(), p.name, profile.ID)
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	if !admitted {
		a.renderError(w, http.StatusForbidden, errors.New("registration is closed"))
		return
	}
	user, err := a.db.SignIn(r.Context(), identity)
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	session, csrf, expires, err := a.db.CreateSession(r.Context(), user.ID, a.cfg.SessionTTL)
	if errors.Is(err, store.ErrSuspended) {
		a.renderSuspended(w, r, p.name, profile.ID)
		return
	}
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	a.setSessionCookies(w, r, session, csrf, expires)
	next := "/"
	if nextValue, err := r.Cookie(nextCookie); err == nil {
		if candidate := safeNext(nextValue.Value); candidate != "" {
			next = candidate
		}
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// finishLink adds the identity to the signed-in account. One that already
// signs in to another account stays there: accounts are never merged.
func (a *App) finishLink(w http.ResponseWriter, r *http.Request, identity store.Identity) {
	user, _, ok := a.currentUser(r)
	if !ok {
		http.Redirect(w, r, "/login?next="+url.QueryEscape(accountPath), http.StatusSeeOther)
		return
	}
	err := a.db.LinkIdentity(r.Context(), user.ID, identity)
	switch {
	case err == nil:
		http.Redirect(w, r, accountPath+"?linked="+url.QueryEscape(identity.Provider), http.StatusSeeOther)
	case errors.Is(err, store.ErrIdentityTaken):
		http.Redirect(w, r, accountPath+"?identity=taken", http.StatusSeeOther)
	case errors.Is(err, store.ErrProviderLinked):
		http.Redirect(w, r, accountPath+"?identity=linked", http.StatusSeeOther)
	default:
		a.serverError(w, "link identity", err)
	}
}

// identityAdmitted separates authentication at a provider from local account
// admission. Existing users always pass; only an identity that has no local
// user yet is subject to the current registration mode.
func (a *App) identityAdmitted(ctx context.Context, provider, subject string) (bool, error) {
	_, err := a.db.IdentityUser(ctx, provider, subject)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, store.ErrNotFound):
		return a.cfg.RegistrationMode.AllowsNewUser(), nil
	default:
		return false, err
	}
}

func (a *App) clearOAuthCookies(w http.ResponseWriter, path string) {
	for _, name := range []string{stateCookie, nextCookie, linkCookie} {
		http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: path, MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	}
}

// renderSuspended answers a sign-in by a suspended account with the sign-in
// page and the reason it was given, rather than a bare refusal.
func (a *App) renderSuspended(w http.ResponseWriter, r *http.Request, provider, subject string) {
	locale := requestLanguage(r).Locale
	message := translate(locale, "account_suspended")
	if user, err := a.db.IdentityUser(r.Context(), provider, subject); err == nil && user.SuspendedReason != "" {
		message += user.SuspendedReason
	}
	data := pageData{LoginOptions: a.loginOptions(""), RegistrationMode: a.cfg.RegistrationMode, Error: message}
	a.renderTemplate(w, r, http.StatusForbidden, "login.html", data)
}
