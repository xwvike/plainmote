package web

import (
	"fmt"
	"html/template"
	"io"
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
		"formatTime": func(value time.Time) string {
			if value.IsZero() {
				return "-"
			}
			return value.Local().Format("2006-01-02 15:04:05")
		},
		"formatMinute": func(value time.Time) string {
			if value.IsZero() {
				return "-"
			}
			return value.Local().Format("2006-01-02 15:04")
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
		"remainText": remainText,
		// untilText says how long is left in the words someone would use when
		// passing the link on. The absolute time is next to it; this is the
		// part that makes it mean something without doing arithmetic.
		"untilText": func(at time.Time) string {
			left := time.Until(at).Round(time.Minute)
			if left < time.Minute {
				return "不到 1 分钟后"
			}
			return fmt.Sprintf("约 %d 分钟后", int(left/time.Minute))
		},
	}).ParseFS(webAssets, "templates/*.html"))
}

// remainText renders how long a share has left in the terms a person thinks
// in, rather than as an absolute timestamp.
func remainText(value *time.Time) string {
	if value == nil {
		return "不过期"
	}
	left := time.Until(*value)
	if left <= 0 {
		return "已过期"
	}
	switch {
	case left >= 24*time.Hour:
		days := int(left / (24 * time.Hour))
		hours := int((left % (24 * time.Hour)) / time.Hour)
		if hours == 0 {
			return fmt.Sprintf("剩 %d 天", days)
		}
		return fmt.Sprintf("剩 %d 天 %d 小时", days, hours)
	case left >= time.Hour:
		minutes := int((left % time.Hour) / time.Minute)
		if minutes == 0 {
			return fmt.Sprintf("剩 %d 小时", int(left/time.Hour))
		}
		return fmt.Sprintf("剩 %d 小时 %d 分钟", int(left/time.Hour), minutes)
	case left >= time.Minute:
		return fmt.Sprintf("剩 %d 分钟", int(left/time.Minute))
	default:
		return "剩不到 1 分钟"
	}
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

func (a *App) renderTemplate(w http.ResponseWriter, status int, name string, data pageData) {
	// A deployment-wide fact, set at the one place every page goes through
	// rather than at each of the handlers that build a pageData. The top bar
	// names the home page after it, and getting that from only some of them
	// would leave the navigation disagreeing with itself.
	data.Anonymous = a.cfg.AnonymousEnabled
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	if err := a.templates.ExecuteTemplate(w, name, data); err != nil {
		fmt.Fprintf(w, "template error: %v", err)
	}
}

func (a *App) renderError(w http.ResponseWriter, status int, err error) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
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
