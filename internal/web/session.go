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
	a.setCSRFCookie(w, r, csrf, expires)
}

// setCSRFCookie keeps the session's CSRF token where the server reads it back
// into each form. No script reads it, so none may write it either: a token
// replaced from the page would refuse every form of the session.
func (a *App) setCSRFCookie(w http.ResponseWriter, r *http.Request, csrf string, expires time.Time) {
	secure := strings.HasPrefix(strings.ToLower(a.baseURL(r)), "https://")
	http.SetCookie(w, &http.Cookie{Name: csrfCookie, Value: csrf, Path: "/", Expires: expires, MaxAge: int(time.Until(expires).Seconds()), HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode})
}

func (a *App) clearSessionCookies(w http.ResponseWriter) {
	for _, name := range []string{sessionCookie, csrfCookie, stateCookie} {
		http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
	}
}

// safeNext accepts a path on this site and nothing else. Checking for a leading
// "/" and refusing "//" is not enough on its own, because browsers repair a URL
// before resolving it: a backslash is read as a slash and tabs and newlines are
// dropped, so "/\evil.com" and "/\t/evil.com" both land on another host. Rather
// than predict that repair, anything it could act on is refused, and what is
// left has to parse as a bare path with no scheme and no host.
func safeNext(value string) string {
	if !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") {
		return ""
	}
	for _, r := range value {
		if r == '\\' || r < 0x20 || r == 0x7f {
			return ""
		}
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "" || parsed.Host != "" || parsed.Opaque != "" {
		return ""
	}
	return value
}
