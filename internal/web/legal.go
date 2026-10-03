package web

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"plainmote/internal/store"
)

// legalPages are served at /<name> and rendered by the template "<name>-zh" or
// "<name>-en". The texts exist in those two languages only; every Chinese
// locale gets the Chinese one and everything else the English one.
var legalPages = []string{"about", "privacy", "terms", "contact"}

// legalUpdated is the date printed at the top of the privacy policy and the
// terms. Change it in the same commit as the text.
const legalUpdated = "2026-10-02"

// legalView is what the texts need to be true of this deployment rather than
// of some deployment: every retention period and limit they state is read from
// the configuration that enforces it.
type legalView struct {
	Page          string
	Chinese       bool
	Site          string
	Operator      string
	Email         string
	Updated       string
	R2            bool
	SessionTTL    time.Duration
	LogRetention  time.Duration
	Anonymous     bool
	AnonymousTTL  time.Duration
	AnonymousSize int64
	MaxContent    int64
	ExportLimit   int
	ExportWindow  time.Duration
	SourceURL     string
	// How many earlier versions a resource keeps, and for how long after
	// each was replaced.
	HistoryKeep      int
	HistoryRetention time.Duration
	// How long a command line's sign-in lasts, and how long a sign-in
	// request waits to be confirmed.
	TokenLifetime time.Duration
	DeviceWindow  time.Duration
}

func (a *App) handleLegal(w http.ResponseWriter, r *http.Request) {
	a.serveLegal(w, r, strings.TrimPrefix(r.URL.Path, "/"))
}

// serveLegal is a legal page at /<page> and, in Chinese, at /zh-cn/<page>.
func (a *App) serveLegal(w http.ResponseWriter, r *http.Request, page string) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// These pages say what the service is, so unlike the rest of it they are
	// meant to be found.
	data := pageData{Active: page, SignInURL: "/login", Indexable: true, BaseURL: a.baseURL(r), Canonical: "/" + page}
	if route, ok := routeLocale(r); ok {
		data.Canonical = route.prefix + "/" + page
	}
	data.Alternates = legalAlternates(data.BaseURL, page)
	if user, _, ok := a.currentUser(r); ok {
		data.User, data.SignedIn, data.CSRF = user, true, csrfValue(r)
	}
	site := a.cfg.PublicURL
	if parsed, err := url.Parse(a.cfg.PublicURL); err == nil && parsed.Host != "" {
		site = parsed.Host
	}
	data.Legal = legalView{
		Page:          page,
		Chinese:       strings.HasPrefix(requestLanguage(r).Locale, "zh"),
		Site:          site,
		Operator:      a.cfg.Operator,
		Email:         a.cfg.ContactEmail,
		SourceURL:     a.cfg.SourceURL,
		Updated:       legalUpdated,
		R2:            cloudflareR2(a.cfg.BlobEndpoint),
		SessionTTL:    a.cfg.SessionTTL,
		LogRetention:  a.cfg.LogRetention,
		Anonymous:     a.cfg.AnonymousEnabled,
		AnonymousTTL:  store.AnonymousMaxTTL,
		AnonymousSize: store.AnonymousMaxBytes,
		MaxContent:    a.cfg.MaxContent,
		ExportLimit:   store.ExportLimit,
		ExportWindow:  store.ExportWindow,

		HistoryKeep:      store.HistoryKeep,
		HistoryRetention: store.HistoryRetention,
		TokenLifetime:    store.TokenLifetime,
		DeviceWindow:     store.DeviceGrantLifetime,
	}
	a.renderTemplate(w, r, http.StatusOK, "legal.html", data)
}

// legalDuration states a configured period in the largest whole unit it
// divides into, so 720h reads as 30 days and 90m as 90 minutes. A single day
// stays 24 hours: "every 24 hours" is how a rolling window is said.
func legalDuration(chinese bool, d time.Duration) string {
	value, zh, en := int64(d/time.Minute), "分钟", "minute"
	switch {
	case d > 24*time.Hour && d%(24*time.Hour) == 0:
		value, zh, en = int64(d/(24*time.Hour)), "天", "day"
	case d >= time.Hour && d%time.Hour == 0:
		value, zh, en = int64(d/time.Hour), "小时", "hour"
	}
	if chinese {
		return fmt.Sprintf("%d %s", value, zh)
	}
	if value != 1 {
		en += "s"
	}
	return fmt.Sprintf("%d %s", value, en)
}

// legalBytes states a configured limit as the whole number it was configured
// as: 4 MiB, not the 4.00 MiB the resource pages show next to measured sizes.
func legalBytes(size int64) string {
	switch {
	case size >= 1<<20 && size%(1<<20) == 0:
		return fmt.Sprintf("%d MiB", size>>20)
	case size >= 1<<10 && size%(1<<10) == 0:
		return fmt.Sprintf("%d KiB", size>>10)
	}
	return bytesText(size)
}

// cloudflareR2 reports whether bodies go to Cloudflare R2, so the policy names
// the provider only where it is the one actually configured.
func cloudflareR2(endpoint string) bool {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	return strings.HasSuffix(strings.ToLower(parsed.Hostname()), ".r2.cloudflarestorage.com")
}
