package web

import (
	"net/http"
	"net/url"
	"strings"
	"time"
)

const themeCookie = "plainmote_theme"

// themeView is the three-state theme switch (auto / light / dark). With no
// choice made the page follows the system, which the server cannot see, so the
// stylesheet picks the next stop that immediately changes what is on screen.
// A manual choice is stored in a cookie and stamped on <html>; "auto" clears
// the cookie to return to system tracking - no script involved.
type themeView struct {
	Current  string // "light", "dark", or "" to follow the system
	DarkURL  string
	LightURL string
	AutoURL  string
}

func requestTheme(r *http.Request) themeView {
	view := themeView{}
	if cookie, err := r.Cookie(themeCookie); err == nil && (cookie.Value == "light" || cookie.Value == "dark") {
		view.Current = cookie.Value
	}
	next := r.URL.RequestURI()
	view.DarkURL = themePath("dark", next)
	view.LightURL = themePath("light", next)
	view.AutoURL = themePath("auto", next)
	return view
}

func themePath(theme, next string) string {
	values := url.Values{"to": {theme}}
	if safe := safeNext(next); safe != "" {
		values.Set("next", safe)
	}
	return "/theme?" + values.Encode()
}

func (a *App) handleTheme(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	theme := strings.TrimSpace(r.URL.Query().Get("to"))
	if theme != "light" && theme != "dark" && theme != "auto" {
		writePlainError(w, http.StatusBadRequest, "unknown theme")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	cookie := &http.Cookie{
		Name: themeCookie, Value: theme, Path: "/", MaxAge: int((365 * 24 * time.Hour).Seconds()),
		HttpOnly: true, Secure: strings.HasPrefix(strings.ToLower(a.baseURL(r)), "https://"), SameSite: http.SameSiteLaxMode,
	}
	if theme == "auto" {
		cookie.Value = ""
		cookie.MaxAge = -1
		cookie.Expires = time.Unix(0, 0)
	}
	http.SetCookie(w, cookie)
	next := safeNext(r.URL.Query().Get("next"))
	if next == "" {
		next = "/"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}
