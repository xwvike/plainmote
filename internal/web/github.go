package web

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

func (a *App) handleGitHubLogin(w http.ResponseWriter, r *http.Request) {
	if a.github == nil || !a.github.Configured() {
		a.renderError(w, http.StatusServiceUnavailable, errors.New("GitHub OAuth is not configured"))
		return
	}
	state, err := a.github.NewState()
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	secure := strings.HasPrefix(strings.ToLower(a.baseURL(r)), "https://")
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: state, Path: "/auth/github", MaxAge: 600, HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode})
	if next := safeNext(r.URL.Query().Get("next")); next != "" {
		http.SetCookie(w, &http.Cookie{Name: nextCookie, Value: next, Path: "/auth/github", MaxAge: 600, HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode})
	}
	redirectURI := a.baseURL(r) + "/auth/github/callback"
	http.Redirect(w, r, a.github.AuthorizationURL(redirectURI, state), http.StatusFound)
}

func (a *App) handleGitHubCallback(w http.ResponseWriter, r *http.Request) {
	stateCookieValue, err := r.Cookie(stateCookie)
	if err != nil || stateCookieValue.Value == "" || r.URL.Query().Get("state") == "" || subtle.ConstantTimeCompare([]byte(stateCookieValue.Value), []byte(r.URL.Query().Get("state"))) != 1 {
		a.renderError(w, http.StatusBadRequest, errors.New("invalid OAuth state"))
		return
	}
	if oauthErr := r.URL.Query().Get("error"); oauthErr != "" {
		a.renderError(w, http.StatusUnauthorized, fmt.Errorf("GitHub OAuth failed: %s", oauthErr))
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	if code == "" {
		a.renderError(w, http.StatusBadRequest, errors.New("OAuth code is missing"))
		return
	}
	if a.github == nil || !a.github.Configured() {
		a.renderError(w, http.StatusServiceUnavailable, errors.New("GitHub OAuth is not configured"))
		return
	}
	profile, err := a.github.Authenticate(r.Context(), code, a.baseURL(r)+"/auth/github/callback")
	if err != nil {
		a.renderError(w, http.StatusBadGateway, err)
		return
	}
	if !a.cfg.AllowedIDs[profile.ID] {
		a.renderError(w, http.StatusForbidden, errors.New("this GitHub account is not allowed"))
		return
	}
	user, err := a.db.UpsertUser(r.Context(), profile.ID, profile.Login, profile.Name, profile.AvatarURL)
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	session, csrf, expires, err := a.db.CreateSession(r.Context(), user.ID, a.cfg.SessionTTL)
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	a.setSessionCookies(w, r, session, csrf, expires)
	a.clearOAuthStateCookie(w)
	next := "/"
	if nextValue, err := r.Cookie(nextCookie); err == nil {
		if candidate := safeNext(nextValue.Value); candidate != "" {
			next = candidate
		}
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (a *App) clearOAuthStateCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: "", Path: "/auth/github", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	http.SetCookie(w, &http.Cookie{Name: nextCookie, Value: "", Path: "/auth/github", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}
