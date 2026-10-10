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
	a.renderTemplate(w, r, http.StatusOK, "login.html", data)
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	user, sessionID, ok := a.currentUser(r)
	if ok {
		if !a.checkCSRF(r, sessionID) {
			a.confirmLogout(w, r, user, sessionID)
			return
		}
		_ = a.db.DeleteSession(r.Context(), sessionID)
	}
	a.clearSessionCookies(w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// confirmLogout answers a sign-out whose form token did not match: a stale
// page, or a token changed in the browser. Signing out still needs this
// site's own form, so another site cannot sign anyone out, but the session
// is given a new token and a page to confirm with, rather than a refusal
// that leaves it signed in with every form broken.
func (a *App) confirmLogout(w http.ResponseWriter, r *http.Request, user User, sessionID string) {
	csrf, expires, err := a.db.RenewSessionCSRF(r.Context(), sessionID)
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	a.setCSRFCookie(w, r, csrf, expires)
	data := a.basePage(r, user)
	data.CSRF = csrf
	a.renderTemplate(w, r, http.StatusForbidden, "logout.html", data)
}
