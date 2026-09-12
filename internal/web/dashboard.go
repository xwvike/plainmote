package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	defaultPageSize = 10
	// Keep search URLs and LIKE patterns bounded without cutting a UTF-8 rune.
	maxQueryLength = 256
	pageWindow     = 7
)

var pageSizes = []int{10, 20, 50}

// handleDashboardRoot owns "/" outright: resources are delivered under /d/,
// so nothing else can land here.
func (a *App) handleDashboardRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writePlainError(w, http.StatusNotFound, "not found")
		return
	}
	a.handleDashboard(w, r)
}

func (a *App) handleDashboard(w http.ResponseWriter, r *http.Request) {
	user, _, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	query := r.URL.Query()
	search := strings.TrimSpace(query.Get("q"))
	search = truncateBytes(search, maxQueryLength)
	size := normalizePageSize(query.Get("size"))
	page, err := strconv.Atoi(strings.TrimSpace(query.Get("page")))
	if err != nil || page < 1 {
		page = 1
	}

	resources, total, err := a.db.ListResources(r.Context(), user.ID, search, size, (page-1)*size)
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	if last := lastPage(total, size); page > last {
		http.Redirect(w, r, dashboardURL(search, size, last), http.StatusSeeOther)
		return
	}

	a.renderTemplate(w, http.StatusOK, "dashboard.html", pageData{
		User:      user,
		CSRF:      csrfValue(r),
		Active:    "resources",
		BaseURL:   a.baseURL(r),
		Resources: resources,
		Pager:     buildPager(search, page, size, total),
	})
}

// truncateBytes cuts a string to a byte budget without splitting a character
// in half, which a plain slice would do to any non-ASCII search term.
func truncateBytes(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

func normalizePageSize(value string) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return defaultPageSize
	}
	for _, allowed := range pageSizes {
		if parsed == allowed {
			return parsed
		}
	}
	return defaultPageSize
}

func lastPage(total, size int) int {
	if total <= 0 || size <= 0 {
		return 1
	}
	return (total + size - 1) / size
}

func dashboardURL(search string, size, page int) string {
	values := url.Values{}
	if search != "" {
		values.Set("q", search)
	}
	if size != defaultPageSize {
		values.Set("size", strconv.Itoa(size))
	}
	if page > 1 {
		values.Set("page", strconv.Itoa(page))
	}
	if len(values) == 0 {
		return "/"
	}
	return "/?" + values.Encode()
}

func buildPager(search string, page, size, total int) pager {
	last := lastPage(total, size)
	result := pager{Query: search, Size: size, Sizes: pageSizes, Total: total}
	if total > 0 {
		result.From = (page-1)*size + 1
		result.To = page * size
		if result.To > total {
			result.To = total
		}
	}
	if page > 1 {
		result.PrevURL = dashboardURL(search, size, page-1)
	}
	if page < last {
		result.NextURL = dashboardURL(search, size, page+1)
	}

	first := 1
	if last > pageWindow {
		first = page - pageWindow/2
		if first < 1 {
			first = 1
		}
		if first+pageWindow-1 > last {
			first = last - pageWindow + 1
		}
	}
	for number := first; number <= last && number < first+pageWindow; number++ {
		result.Links = append(result.Links, pageLink{
			Num:     number,
			URL:     dashboardURL(search, size, number),
			Current: number == page,
		})
	}
	return result
}
