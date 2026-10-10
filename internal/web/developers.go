package web

import (
	"net/http"
	"net/url"
	"slices"
	"strings"

	"plainmote/internal/store"
)

const developersPath = "/developers"

// developerPages are the pages for someone who would rather type, in the
// order the side navigation lists them: the command line, the HTTP API,
// quick shares with curl where the deployment takes them, and what is there
// for AI assistants where llms.txt is served.
func (a *App) developerPages() []string {
	pages := []string{"cli", "api"}
	if a.cfg.AnonymousEnabled {
		pages = append(pages, "curl")
	}
	if a.hasPublicPages() {
		pages = append(pages, "ai")
	}
	return pages
}

// handleDevelopers serves /developers/<page>; /developers itself leads to
// the first. The pages are found by search where the deployment has public
// pages at all, in every language at an address of its own.
func (a *App) handleDevelopers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	prefix := ""
	if route, ok := routeLocale(r); ok {
		prefix = route.prefix
	}
	page := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, prefix), developersPath)
	if page == "" || page == "/" {
		http.Redirect(w, r, prefix+developersPath+"/cli", http.StatusFound)
		return
	}
	page = strings.TrimPrefix(page, "/")
	if !slices.Contains(a.developerPages(), page) {
		writePlainError(w, http.StatusNotFound, "not found")
		return
	}
	path := developersPath + "/" + page
	data := pageData{Active: "developers", Section: "dev-" + page, SignInURL: "/login", BaseURL: a.baseURL(r), Canonical: prefix + path}
	data.Indexable = a.indexable(data.Canonical)
	data.Alternates = localeAlternates(data.BaseURL, path)
	if user, _, ok := a.currentUser(r); ok {
		data.User, data.SignedIn, data.CSRF = user, true, csrfValue(r)
	}
	locale := requestLanguage(r).Locale
	switch page {
	case "cli":
		w.Header().Add("Vary", "User-Agent")
		w.Header().Add("Vary", "Sec-CH-UA-Platform")
		data.CLIBinaries = a.cliBinaries()
		data.CLIVersion = a.cfg.Version
		data.CLIPlatform = clientPlatform(r)
		data.CLILang = cliLanguage(locale)
	case "api":
		data.APIDocsURL = apiDocsURL(a.cfg.SourceURL, locale)
	case "curl":
		data.PasteTTL = pasteDefaultTTL
		data.MaxPaste = store.AnonymousMaxBytes
	}
	a.renderTemplate(w, r, http.StatusOK, "developers.html", data)
}

// apiDocsURL is the API reference in the source, where the source is on
// GitHub and so its address is known; elsewhere the page goes without.
func apiDocsURL(source, locale string) string {
	parsed, err := url.Parse(source)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "github.com" {
		return ""
	}
	file := "api.md"
	if strings.HasPrefix(locale, "zh") {
		file = "api.zh-CN.md"
	}
	return strings.TrimRight(source, "/") + "/blob/main/docs/" + file
}
