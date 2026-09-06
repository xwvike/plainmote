package web

import (
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

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
			switch outcome {
			case "success":
				return "on"
			case "expired", "exhausted":
				return "wa"
			case "invalid", "missing_token", "upstream_error", "revoked":
				return "no"
			default:
				return "off"
			}
		},
		"remainText": remainText,
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
		User:       user,
		CSRF:       csrfValue(r),
		Active:     "resources",
		BaseURL:    a.baseURL(r),
		MaxContent: a.cfg.MaxContent,
	}
}

func (a *App) basePageWithError(r *http.Request, user User, message string) pageData {
	data := a.basePage(r, user)
	data.Error = message
	return data
}
