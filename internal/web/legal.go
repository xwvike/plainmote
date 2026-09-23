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
const legalUpdated = "2026-09-23"

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
}

func (a *App) handleLegal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	page := strings.TrimPrefix(r.URL.Path, "/")
	data := pageData{Active: page, SignInURL: "/login"}
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
