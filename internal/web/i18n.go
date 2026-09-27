package web

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/text/language"
)

const languageCookie = "plainmote_language"

type languageView struct {
	Locale       string
	NativeLocale string
	NativeLabel  string
	ShowSwitch   bool
	NativeURL    string
	EnglishURL   string
}

func requestLanguage(r *http.Request) languageView {
	// A page with its language in the address shows that language to everyone,
	// the crawler included; its switch leads to the English address, which
	// goes back to following the browser.
	if forced, ok := r.Context().Value(localeKey{}).(localeRoute); ok {
		view := languageView{Locale: forced.locale}
		if forced.locale != "en" {
			view.NativeLocale = forced.locale
			view.NativeLabel = languageLabels[forced.locale]
			view.ShowSwitch = true
			view.NativeURL = r.URL.Path
			view.EnglishURL = languagePath("en", forced.english)
		}
		return view
	}
	native := browserLanguage(r.Header.Get("Accept-Language"))
	view := languageView{Locale: "en"}
	if native == "" || native == "en" {
		return view
	}
	view.NativeLocale = native
	view.NativeLabel = languageLabels[native]
	view.ShowSwitch = true
	view.Locale = native
	if cookie, err := r.Cookie(languageCookie); err == nil && cookie.Value == "en" {
		view.Locale = "en"
	}
	next := r.URL.RequestURI()
	view.NativeURL = languagePath(native, next)
	view.EnglishURL = languagePath("en", next)
	return view
}

func browserLanguage(header string) string {
	tags, _, err := language.ParseAcceptLanguage(header)
	if err != nil {
		return "en"
	}
	for _, tag := range tags {
		base, _ := tag.Base()
		switch base.String() {
		case "en":
			return "en"
		case "ja":
			return "ja"
		case "fr":
			return "fr"
		case "de":
			return "de"
		case "zh":
			script, _ := tag.Script()
			region, _ := tag.Region()
			if script.String() == "Hant" || region.String() == "TW" || region.String() == "HK" || region.String() == "MO" {
				return "zh-TW"
			}
			return "zh-CN"
		}
	}
	return "en"
}

func languagePath(locale, next string) string {
	values := url.Values{"lang": {locale}}
	if safe := safeNext(next); safe != "" {
		values.Set("next", safe)
	}
	return "/language?" + values.Encode()
}

func (a *App) handleLanguage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	native := browserLanguage(r.Header.Get("Accept-Language"))
	selected := strings.TrimSpace(r.URL.Query().Get("lang"))
	if native == "en" || native == "" || (selected != "en" && selected != native) {
		selected = "en"
	}
	secure := strings.HasPrefix(strings.ToLower(a.baseURL(r)), "https://")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Add("Vary", "Accept-Language")
	http.SetCookie(w, &http.Cookie{
		Name: languageCookie, Value: selected, Path: "/", MaxAge: int((365 * 24 * time.Hour).Seconds()),
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
	})
	next := safeNext(r.URL.Query().Get("next"))
	if next == "" {
		next = "/"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

var languageLabels = map[string]string{
	"zh-CN": "文",
	"zh-TW": "文",
	"ja":    "日",
	"fr":    "FR",
	"de":    "DE",
}

func openGraphLocale(locale string) string {
	switch locale {
	case "zh-CN":
		return "zh_CN"
	case "zh-TW":
		return "zh_TW"
	case "ja":
		return "ja_JP"
	case "fr":
		return "fr_FR"
	case "de":
		return "de_DE"
	default:
		return "en_US"
	}
}

func translate(locale, key string) string {
	if catalog := messageCatalogs[locale]; catalog != nil {
		if value := catalog[key]; value != "" {
			return value
		}
	}
	if value := messageCatalogs["en"][key]; value != "" {
		return value
	}
	return key
}

func writeLocalizedError(w http.ResponseWriter, r *http.Request, status int, key string) {
	w.Header().Add("Vary", "Accept-Language")
	w.Header().Add("Vary", "Cookie")
	writePlainError(w, status, translate(requestLanguage(r).Locale, key))
}
