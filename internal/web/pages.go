package web

import (
	"fmt"
	"html/template"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"plainmote/internal/store"
)

var editorEncodingOptions = []encodingOption{
	{Value: "utf-8", Label: "UTF-8"},
	{Value: "utf-8bom", Label: "UTF-8 with BOM"},
	{Value: "utf-16le", Label: "UTF-16 LE"},
	{Value: "utf-16le-bom", Label: "UTF-16 LE with BOM"},
	{Value: "utf-16be", Label: "UTF-16 BE"},
	{Value: "utf-16be-bom", Label: "UTF-16 BE with BOM"},
	{Value: "utf-32le", Label: "UTF-32 LE"},
	{Value: "utf-32le-bom", Label: "UTF-32 LE with BOM"},
	{Value: "utf-32be", Label: "UTF-32 BE"},
	{Value: "utf-32be-bom", Label: "UTF-32 BE with BOM"},
	{Value: "gb18030", Label: "GB18030"},
	{Value: "gbk", Label: "GBK / GB2312"},
	{Value: "big5", Label: "Big5"},
	{Value: "shift_jis", Label: "Shift JIS"},
	{Value: "euc-jp", Label: "EUC-JP"},
	{Value: "iso-2022-jp", Label: "ISO-2022-JP"},
	{Value: "euc-kr", Label: "EUC-KR"},
	{Value: "windows-874", Label: "Windows 874"},
	{Value: "windows-1250", Label: "Windows 1250"},
	{Value: "windows-1251", Label: "Windows 1251"},
	{Value: "windows-1252", Label: "Windows 1252"},
	{Value: "windows-1253", Label: "Windows 1253"},
	{Value: "windows-1254", Label: "Windows 1254"},
	{Value: "windows-1255", Label: "Windows 1255"},
	{Value: "windows-1256", Label: "Windows 1256"},
	{Value: "windows-1257", Label: "Windows 1257"},
	{Value: "windows-1258", Label: "Windows 1258"},
	{Value: "iso-8859-2", Label: "ISO-8859-2"},
	{Value: "iso-8859-3", Label: "ISO-8859-3"},
	{Value: "iso-8859-4", Label: "ISO-8859-4"},
	{Value: "iso-8859-5", Label: "ISO-8859-5"},
	{Value: "iso-8859-6", Label: "ISO-8859-6"},
	{Value: "iso-8859-7", Label: "ISO-8859-7"},
	{Value: "iso-8859-8", Label: "ISO-8859-8"},
	{Value: "iso-8859-9", Label: "ISO-8859-9"},
	{Value: "iso-8859-10", Label: "ISO-8859-10"},
	{Value: "iso-8859-13", Label: "ISO-8859-13"},
	{Value: "iso-8859-14", Label: "ISO-8859-14"},
	{Value: "iso-8859-15", Label: "ISO-8859-15"},
	{Value: "koi8-r", Label: "KOI8-R"},
	{Value: "koi8-u", Label: "KOI8-U"},
	{Value: "ibm866", Label: "IBM866"},
	{Value: "macintosh", Label: "Mac Roman"},
	{Value: "mac-cyrillic", Label: "Mac Cyrillic"},
}

func (a *App) templateSet() *template.Template {
	return template.Must(template.New("pages").Funcs(template.FuncMap{
		"tr":              translate,
		"openGraphLocale": openGraphLocale,
		"formatTime": func(value time.Time) template.HTML {
			return localTimeMarkup(value, false)
		},
		"formatMinute": func(value time.Time) template.HTML {
			return localTimeMarkup(value, true)
		},
		// Month and day only, for a list where the year is this one.
		"formatDay": func(value time.Time) template.HTML {
			if value.IsZero() {
				return "-"
			}
			value = value.UTC()
			return template.HTML(fmt.Sprintf(`<time datetime="%s" data-local-time="day">%s</time>`,
				value.Format(time.RFC3339), value.Format("01-02")))
		},
		"bytesText": bytesText,
		"hasPrefix": strings.HasPrefix,
		"originHost": func(value string) string {
			parsed, err := url.Parse(value)
			if err != nil || parsed.Host == "" {
				return value
			}
			return parsed.Host
		},
		"outcomeClass": func(outcome string) string {
			// "invalid" and "missing_token" are no longer written - an
			// unowned refusal has no reader - but rows from before still are.
			switch outcome {
			case store.OutcomeSuccess:
				return "on"
			case store.OutcomeExpired, store.OutcomeExhausted:
				return "wa"
			case store.OutcomeRevoked, store.OutcomeUpstreamError, "invalid", "missing_token":
				return "no"
			default:
				return "off"
			}
		},
		"outcomeText":        accessOutcomeText,
		"outcomeDescription": accessOutcomeDescription,
		"remainText":         remainText,
		"leftPercent":        leftPercent,
		"gaugeStyle":         gaugeStyle,
		"endStyle":           endStyle,
		"addressParts":       addressParts,
		"untilText":          untilText,
		"lifetimeText":       lifetimeText,
		"lifetimeShort":      lifetimeShort,
		"lifetimes":          func() []ttlChoice { return lifetimeChoices },
		"pageSummary":        pageSummary,
		"countText":          countText,
		"deleteWarning":      deleteWarning,
		"legalDuration":      legalDuration,
		"legalBytes":         legalBytes,
	}).ParseFS(webAssets, "templates/*.html"))
}

// leftPercent is how much of a link's current terms is still to run, 0 to
// 100: what the sand gauge draws. A link that never expires is full.
func leftPercent(start time.Time, end *time.Time) int {
	if end == nil {
		return 100
	}
	total := end.Sub(start)
	left := time.Until(*end)
	switch {
	case left <= 0 || total <= 0:
		return 0
	case left >= total:
		return 100
	}
	return int(math.Ceil(float64(left) / float64(total) * 100))
}

// gaugeStyle is what the sand gauge needs to draw itself and keep draining
// with no script: the share of the terms left when the page was rendered, and
// the seconds until none is. Both are numbers this function formats, which is
// what makes handing them to the template as CSS safe.
func gaugeStyle(start time.Time, end *time.Time) template.CSS {
	if end == nil {
		return "--left: 1"
	}
	total, left := end.Sub(start), time.Until(*end)
	if left <= 0 || total <= 0 {
		return "--left: 0"
	}
	share := math.Min(float64(left)/float64(total), 1)
	return template.CSS(fmt.Sprintf("--left: %.4f; --secs: %ds", share, int(math.Ceil(left.Seconds()))))
}

// endStyle gives a live row the seconds until its link expires, for the
// stylesheet to turn it into an ended one at that moment. Nothing for a link
// that never expires.
func endStyle(end *time.Time) template.CSS {
	if end == nil {
		return ""
	}
	left := time.Until(*end)
	if left < 0 {
		left = 0
	}
	return template.CSS(fmt.Sprintf("--end: %ds", int(math.Ceil(left.Seconds()))))
}

// addressParts splits a share address around its token - the path segment
// after /d/ - so the page can set the credential apart from what every
// address shares. Anything else comes back whole, in the first part.
func addressParts(address string) [3]string {
	at := strings.Index(address, "/d/")
	if at < 0 {
		return [3]string{address}
	}
	head, rest := address[:at+3], address[at+3:]
	if slash := strings.IndexByte(rest, '/'); slash >= 0 {
		return [3]string{head, rest[:slash], rest[slash:]}
	}
	return [3]string{head, rest, ""}
}

// remainText renders how long a share has left in the terms a person thinks
// in, rather than as an absolute timestamp.
func remainText(locale string, value *time.Time) string {
	if value == nil {
		return translate(locale, "remain_never")
	}
	return remainingText(locale, time.Until(*value))
}

func remainingText(locale string, left time.Duration) string {
	if left <= 0 {
		return translate(locale, "remain_expired")
	}
	switch {
	case left >= 24*time.Hour:
		days := int(left / (24 * time.Hour))
		hours := int((left % (24 * time.Hour)) / time.Hour)
		if days == 1 {
			switch hours {
			case 0:
				return translate(locale, "remain_day")
			case 1:
				return translate(locale, "remain_day_hour")
			default:
				return fmt.Sprintf(translate(locale, "remain_day_hours"), hours)
			}
		}
		if hours == 0 {
			return fmt.Sprintf(translate(locale, "remain_days"), days)
		}
		if hours == 1 {
			return fmt.Sprintf(translate(locale, "remain_days_hour"), days)
		}
		return fmt.Sprintf(translate(locale, "remain_days_hours"), days, hours)
	case left >= time.Hour:
		hours := int(left / time.Hour)
		minutes := int((left % time.Hour) / time.Minute)
		if hours == 1 {
			switch minutes {
			case 0:
				return translate(locale, "remain_hour")
			case 1:
				return translate(locale, "remain_hour_minute")
			default:
				return fmt.Sprintf(translate(locale, "remain_hour_minutes"), minutes)
			}
		}
		if minutes == 0 {
			return fmt.Sprintf(translate(locale, "remain_hours"), hours)
		}
		if minutes == 1 {
			return fmt.Sprintf(translate(locale, "remain_hours_minute"), hours)
		}
		return fmt.Sprintf(translate(locale, "remain_hours_minutes"), hours, minutes)
	case left >= time.Minute:
		minutes := int(left / time.Minute)
		if minutes == 1 {
			return translate(locale, "remain_minute")
		}
		return fmt.Sprintf(translate(locale, "remain_minutes"), minutes)
	default:
		return translate(locale, "remain_less_minute")
	}
}

func untilText(locale string, at time.Time) string {
	left := time.Until(at)
	// In the largest unit that still says something: minutes within the hour,
	// hours within the day, days beyond. Rounded, since it says "about".
	switch {
	case left >= 36*time.Hour:
		return fmt.Sprintf(translate(locale, "until_days"), int(left.Round(24*time.Hour)/(24*time.Hour)))
	case left >= 23*time.Hour+30*time.Minute:
		return translate(locale, "until_one_day")
	case left >= 90*time.Minute:
		return fmt.Sprintf(translate(locale, "until_hours"), int(left.Round(time.Hour)/time.Hour))
	case left >= 59*time.Minute+30*time.Second:
		return translate(locale, "until_one_hour")
	}
	left = left.Round(time.Minute)
	if left < time.Minute {
		return translate(locale, "until_less_minute")
	}
	if left < 2*time.Minute {
		return translate(locale, "until_one_minute")
	}
	return fmt.Sprintf(translate(locale, "until_minutes"), int(left/time.Minute))
}

// lifetimeText says a lifetime in words, for a scale's titles, the share
// settings, and whoever reads them aloud; lifetimeShort is the label under a
// stop on the quick share scale.
func lifetimeText(locale, value string) string {
	d, _ := parsePasteTTL(value)
	switch {
	case d >= 24*time.Hour && d%(24*time.Hour) == 0:
		if days := int(d / (24 * time.Hour)); days != 1 {
			return fmt.Sprintf(translate(locale, "days"), days)
		}
		return translate(locale, "one_day")
	case d >= time.Hour && d%time.Hour == 0:
		if hours := int(d / time.Hour); hours != 1 {
			return fmt.Sprintf(translate(locale, "hours"), hours)
		}
		return translate(locale, "one_hour")
	}
	if minutes := int(d / time.Minute); minutes != 1 {
		return fmt.Sprintf(translate(locale, "minutes"), minutes)
	}
	return translate(locale, "one_minute")
}

func lifetimeShort(locale, value string) string {
	d, _ := parsePasteTTL(value)
	switch {
	case d >= 24*time.Hour && d%(24*time.Hour) == 0:
		days := int(d / (24 * time.Hour))
		if days == 1 {
			return fmt.Sprintf(translate(locale, "ttl_day"), days)
		}
		return fmt.Sprintf(translate(locale, "ttl_days"), days)
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf(translate(locale, "ttl_hour"), int(d/time.Hour))
	}
	return fmt.Sprintf(translate(locale, "ttl_min"), int(d/time.Minute))
}

func pageSummary(locale string, from, to, total int, unitKey string) string {
	if total == 1 {
		unitKey = singularUnitKey(unitKey)
	}
	return fmt.Sprintf(translate(locale, "page_summary"), from, to, total, translate(locale, unitKey))
}

func countText(locale string, count int, unitKey string) string {
	key := unitKey + "_count"
	if count == 1 {
		key = singularUnitKey(unitKey) + "_count"
		if value := translate(locale, key); value != key {
			return value
		}
	}
	return fmt.Sprintf(translate(locale, key), count)
}

func singularUnitKey(unitKey string) string {
	switch unitKey {
	case "items":
		return "item"
	case "entries":
		return "entry"
	case "shares":
		return "share"
	default:
		return strings.TrimSuffix(unitKey, "s")
	}
}

func localTimeMarkup(value time.Time, minute bool) template.HTML {
	if value.IsZero() {
		return "-"
	}
	value = value.UTC()
	fallbackLayout := "2006-01-02 15:04:05"
	precision := "second"
	if minute {
		fallbackLayout = "2006-01-02 15:04"
		precision = "minute"
	}
	markup := fmt.Sprintf(`<time datetime="%s" data-local-time="%s">%s UTC</time>`,
		value.Format(time.RFC3339), precision, value.Format(fallbackLayout))
	return template.HTML(markup)
}

func deleteWarning(locale string, shares int) string {
	if shares == 0 {
		return translate(locale, "delete_warning")
	}
	if shares == 1 {
		return translate(locale, "delete_warning_share")
	}
	return fmt.Sprintf(translate(locale, "delete_warning_shares"), shares)
}

// bytesText accepts both int (len of a slice) and int64 (a configured limit).
func bytesText(value any) string {
	var size int64
	switch typed := value.(type) {
	case int:
		size = int64(typed)
	case int64:
		size = typed
	default:
		return "-"
	}
	switch {
	case size >= 1<<20:
		return fmt.Sprintf("%.2f MiB", float64(size)/(1<<20))
	case size >= 1<<10:
		return fmt.Sprintf("%.2f KiB", float64(size)/(1<<10))
	default:
		return fmt.Sprintf("%d B", size)
	}
}

func (a *App) renderTemplate(w http.ResponseWriter, r *http.Request, status int, name string, data pageData) {
	// A deployment-wide fact, set at the one place every page goes through
	// rather than at each of the handlers that build a pageData. The top bar
	// names the home page after it, and getting that from only some of them
	// would leave the navigation disagreeing with itself.
	data.Anonymous = a.cfg.AnonymousEnabled
	data.LegalLinks = a.cfg.ContactEmail != ""
	data.SourceURL = a.cfg.SourceURL
	data.Language = requestLanguage(r)
	data.Theme = requestTheme(r)
	if route, ok := routeLocale(r); ok {
		data.LocalePrefix = route.prefix
		if strings.HasPrefix(route.locale, "zh") {
			data.LegalPrefix = legalChinesePrefix
		}
	}
	data.Locale = data.Language.Locale
	data.Error = localizePageError(data.Locale, data.Error)
	data.UpstreamError = localizePageError(data.Locale, data.UpstreamError)
	w.Header().Add("Vary", "Accept-Language")
	w.Header().Add("Vary", "Cookie")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// No page here is meant to be framed. The session cookie is SameSite=Lax,
	// so a cross-site frame already loads signed out; this closes the rest,
	// including a same-site host framing the delete and revoke buttons. The
	// policy stops at frame-ancestors because two templates still carry an
	// inline script. same-origin keeps a path such as /paste/<id> - which is
	// what lets a quick share be saved - out of the Referer sent to the font
	// host.
	// The media player arrives here with a stricter policy that already has
	// frame-ancestors. Keep it intact instead of replacing it with the common
	// page policy.
	if w.Header().Get("Content-Security-Policy") == "" {
		w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	}
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// A handler that already chose a stricter policy - the media player and the
	// delivery path send none at all - keeps it.
	if w.Header().Get("Referrer-Policy") == "" {
		w.Header().Set("Referrer-Policy", "same-origin")
	}
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	if err := a.templates.ExecuteTemplate(w, name, data); err != nil {
		// The marker stays on the page so a broken template is visible and the
		// render tests can see it; the detail names template internals.
		fmt.Fprintf(os.Stderr, "template %s: %v\n", name, err)
		_, _ = io.WriteString(w, "template error")
	}
}

// renderError answers a page that could not be built. A 4xx carries a message
// the handler wrote for the person reading it. A 5xx carries whatever failed
// underneath - pgx, S3, a key that no longer decrypts - and that text names
// relations, object keys and hosts, so it goes to stderr behind one sentence.
func (a *App) renderError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if status >= http.StatusInternalServerError {
		if err != nil {
			fmt.Fprintf(os.Stderr, "render %d: %v\n", status, err)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, "internal error\n")
		return
	}
	w.WriteHeader(status)
	if err != nil {
		_, _ = io.WriteString(w, err.Error()+"\n")
	}
}

// serverError keeps the cause in the process log. Anonymous callers reach this
// path, and a pgx or S3 error text says more about the deployment than anyone
// asking for a file needs to know.
func (a *App) serverError(w http.ResponseWriter, what string, err error) {
	fmt.Fprintf(os.Stderr, "%s: %v\n", what, err)
	writePlainError(w, http.StatusInternalServerError, "internal error")
}

func writePlainError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	http.Error(w, message, status)
}

func csrfValue(r *http.Request) string {
	cookie, err := r.Cookie(csrfCookie)
	if err != nil {
		return ""
	}
	return cookie.Value
}

// basePage seeds the fields every signed-in page needs: the chrome state, the
// origin used to build share addresses, and the configured content limit.
func (a *App) basePage(r *http.Request, user User) pageData {
	return pageData{
		User:            user,
		CSRF:            csrfValue(r),
		Active:          "resources",
		BaseURL:         a.baseURL(r),
		SignedIn:        true,
		MaxContent:      a.cfg.MaxContent,
		ContentEncoding: "utf-8",
		ContentEOL:      store.EOLLF,
		TextEncodings:   editorEncodingOptions,
	}
}

func (a *App) basePageWithError(r *http.Request, user User, message string) pageData {
	data := a.basePage(r, user)
	data.Error = message
	return data
}
