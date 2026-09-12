package web

import (
	"crypto/subtle"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	sessionCookie = "plainmote_session"
	csrfCookie    = "plainmote_csrf"
	stateCookie   = "plainmote_oauth_state"
	nextCookie    = "plainmote_oauth_next"
)

func (a *App) baseURL(r *http.Request) string {
	if a.cfg.PublicURL != "" {
		return strings.TrimRight(a.cfg.PublicURL, "/")
	}
	// TLS is terminated at the proxy in both documented deployments, so r.TLS
	// is nil even though the visitor is on https. Setting public_url is still
	// the reliable answer; this only keeps the fallback from being wrong.
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	} else if a.behindTrustedProxy(r) && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (a *App) currentUser(r *http.Request) (User, string, bool) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil || cookie.Value == "" {
		return User{}, "", false
	}
	user, sessionID, err := a.db.SessionUser(r.Context(), cookie.Value)
	if err != nil {
		return User{}, "", false
	}
	return user, sessionID, true
}

func (a *App) requireUser(w http.ResponseWriter, r *http.Request) (User, string, bool) {
	user, sessionID, ok := a.currentUser(r)
	if ok {
		return user, sessionID, true
	}
	http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
	return User{}, "", false
}

func (a *App) checkCSRF(r *http.Request, sessionID string) bool {
	if sessionID == "" {
		return false
	}
	value := strings.TrimSpace(r.FormValue("csrf"))
	cookie, err := r.Cookie(csrfCookie)
	if err != nil || value == "" || cookie.Value == "" || subtle.ConstantTimeCompare([]byte(value), []byte(cookie.Value)) != 1 {
		return false
	}
	return a.db.SessionCSRF(r.Context(), sessionID, value)
}

func (a *App) setSessionCookies(w http.ResponseWriter, r *http.Request, session, csrf string, expires time.Time) {
	secure := strings.HasPrefix(strings.ToLower(a.baseURL(r)), "https://")
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: session, Path: "/", Expires: expires, MaxAge: int(time.Until(expires).Seconds()), HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode})
	http.SetCookie(w, &http.Cookie{Name: csrfCookie, Value: csrf, Path: "/", Expires: expires, MaxAge: int(time.Until(expires).Seconds()), Secure: secure, SameSite: http.SameSiteLaxMode})
}

func (a *App) clearSessionCookies(w http.ResponseWriter) {
	for _, name := range []string{sessionCookie, csrfCookie, stateCookie} {
		http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: name != csrfCookie, SameSite: http.SameSiteLaxMode})
	}
}

func safeNext(value string) string {
	if strings.HasPrefix(value, "/") && !strings.HasPrefix(value, "//") {
		return value
	}
	return ""
}
