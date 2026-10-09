package web

import (
	"net/http"
)

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := a.currentUser(r); ok {
		http.Redirect(w, r, dashboardPath, http.StatusSeeOther)
		return
	}
	data := pageData{LoginOptions: a.loginOptions(safeNext(r.URL.Query().Get("next"))), RegistrationMode: a.cfg.RegistrationMode}
	if r.URL.Query().Get("deleted") == "1" {
		data.Notice = translate(requestLanguage(r).Locale, "account_deleted")
	}
	if r.URL.Query().Get("error") != "" {
		data.Error = r.URL.Query().Get("error")
	}
	a.renderTemplate(w, r, http.StatusOK, "login.html", data)
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
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
