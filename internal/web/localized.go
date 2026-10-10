package web

import (
	"context"
	"net/http"
	"strings"
)

// Search engines index one language per address, and a crawler sends no
// language preference, so an address that follows the browser is English to
// every search engine. The pages meant to be found therefore also exist at an
// address per language - /zh-cn/, /ja/, ... - that shows that language to
// everyone, with rel=alternate hreflang links tying the versions together.
// English stays at the unprefixed address, which keeps following the browser
// for people and is what a crawler already sees there.
//
// Only the public pages have these addresses. Everything behind a sign-in
// follows the browser as before.

// publicLocales are the languages with an address of their own, in the order
// they are listed as alternates.
var publicLocales = []struct{ prefix, locale string }{
	{"/zh-cn", "zh-CN"},
	{"/zh-tw", "zh-TW"},
	{"/ja", "ja"},
	{"/fr", "fr"},
	{"/de", "de"},
}

// The legal pages are written in Chinese and English only. The Chinese text
// has its own address under /zh-cn; every other language reads the English
// one at the unprefixed address.
const legalChinesePrefix = "/zh-cn"

type localeKey struct{}

// localeRoute is the language an address fixes, and the English address of
// the same page, which the language switch leads to.
type localeRoute struct {
	locale  string
	prefix  string
	english string
}

func withLocale(r *http.Request, route localeRoute) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), localeKey{}, route))
}

func routeLocale(r *http.Request) (localeRoute, bool) {
	route, ok := r.Context().Value(localeKey{}).(localeRoute)
	return route, ok
}

// alternate is one rel=alternate hreflang link.
type alternate struct {
	Hreflang string
	URL      string
}

// localeAlternates are a page written in every language - the home page, the
// developer page - at each of its addresses, and x-default, the address for
// a reader none of them matches.
func localeAlternates(base, path string) []alternate {
	links := []alternate{{"en", base + path}}
	for _, public := range publicLocales {
		links = append(links, alternate{public.locale, base + public.prefix + path})
	}
	return append(links, alternate{"x-default", base + path})
}

// legalAlternates are a legal page in the two languages it is written in.
func legalAlternates(base, page string) []alternate {
	return []alternate{
		{"en", base + "/" + page},
		{"zh", base + legalChinesePrefix + "/" + page},
		{"x-default", base + "/" + page},
	}
}

// handleLocalized serves /<language>/, /<language>/developers/<page> and,
// for Chinese, /zh-cn/<legal page>. Anything else under a language prefix does
// not exist.
func (a *App) handleLocalized(w http.ResponseWriter, r *http.Request) {
	for _, public := range publicLocales {
		rest, ok := strings.CutPrefix(r.URL.Path, public.prefix)
		if !ok || !strings.HasPrefix(rest, "/") {
			continue
		}
		route := localeRoute{locale: public.locale, prefix: public.prefix, english: rest}
		switch {
		case rest == "/" && a.cfg.AnonymousEnabled:
			a.serveHome(w, withLocale(r, route))
			return
		case (rest == developersPath || strings.HasPrefix(rest, developersPath+"/")) && a.hasPublicPages():
			a.handleDevelopers(w, withLocale(r, route))
			return
		case public.prefix == legalChinesePrefix && a.cfg.ContactEmail != "" && isLegalPage(strings.TrimPrefix(rest, "/")):
			a.serveLegal(w, withLocale(r, route), strings.TrimPrefix(rest, "/"))
			return
		}
		break
	}
	writePlainError(w, http.StatusNotFound, "not found")
}

func isLegalPage(page string) bool {
	for _, legal := range legalPages {
		if legal == page {
			return true
		}
	}
	return false
}

// pageAlternates are the language versions of an indexable path.
func (a *App) pageAlternates(base, path string) []alternate {
	for _, public := range publicLocales {
		if rest, ok := strings.CutPrefix(path, public.prefix); ok && strings.HasPrefix(rest, "/") {
			path = rest
			break
		}
	}
	if page := strings.Trim(path, "/"); isLegalPage(page) {
		return legalAlternates(base, page)
	}
	return localeAlternates(base, path)
}

// localizedPaths are the language addresses of the pages meant to be found,
// for robots.txt, the sitemap and the index header.
func (a *App) localizedPaths() []string {
	var paths []string
	if a.cfg.AnonymousEnabled {
		for _, public := range publicLocales {
			paths = append(paths, public.prefix+"/")
		}
	}
	if a.cfg.ContactEmail != "" {
		for _, page := range legalPages {
			paths = append(paths, legalChinesePrefix+"/"+page)
		}
	}
	for _, public := range publicLocales {
		for _, page := range a.developerPages() {
			paths = append(paths, public.prefix+developersPath+"/"+page)
		}
	}
	return paths
}
