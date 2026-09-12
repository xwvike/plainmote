package web

import (
	"net/http"
	"net/url"
)

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := a.currentUser(r); ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	data := pageData{LoginURL: "/auth/github", RegistrationMode: a.cfg.RegistrationMode}
	if next := safeNext(r.URL.Query().Get("next")); next != "" {
		data.LoginURL += "?next=" + url.QueryEscape(next)
	}
	if r.URL.Query().Get("error") != "" {
		data.Error = r.URL.Query().Get("error")
	}
	a.renderTemplate(w, http.StatusOK, "login.html", data)
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	_, sessionID, ok := a.currentUser(r)
	if ok {
		if !a.checkCSRF(r, sessionID) {
			writePlainError(w, http.StatusForbidden, "invalid csrf token")
			return
		}
		_ = a.db.DeleteSession(r.Context(), sessionID)
	}
	a.clearSessionCookies(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
