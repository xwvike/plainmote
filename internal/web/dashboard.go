package web

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// dashboardPath is where an account's own resources live now that the root is
// the page anyone can open.
const dashboardPath = "/resources/"

const (
	defaultPageSize = 10
	// Keep search URLs and LIKE patterns bounded without cutting a UTF-8 rune.
	maxQueryLength = 256
	pageWindow     = 7
)

var pageSizes = []int{10, 20, 50}

// handleDashboard lists the account's own resources. It sits under /resources/
// rather than at the root, because the root is the one page in this service
// that anyone is meant to be able to open.
func (a *App) handleDashboard(w http.ResponseWriter, r *http.Request, user User) {
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

	// What is left is part of the list, not a surprise at save time: the
	// account should be able to see it coming.
	quota, err := a.db.QuotaForUser(r.Context(), user.ID, time.Now().UTC())
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}

	pager := buildPager(page, size, total, pageSizes, func(number int) string {
		return dashboardURL(search, size, number)
	})
	pager.Query = search
	a.renderTemplate(w, r, http.StatusOK, "dashboard.html", pageData{
		User:      user,
		CSRF:      csrfValue(r),
		Active:    "resources",
		BaseURL:   a.baseURL(r),
		SignedIn:  true,
		Resources: resources,
		Quota:     quota,
		Pager:     pager,
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
		return dashboardPath
	}
	return dashboardPath + "?" + values.Encode()
}

func buildPager(page, size, total int, sizes []int, pageURL func(int) string) pager {
	last := lastPage(total, size)
	result := pager{Size: size, Sizes: sizes, Total: total}
	if total > 0 {
		result.From = (page-1)*size + 1
		result.To = page * size
		if result.To > total {
			result.To = total
		}
	}
	if page > 1 {
		result.PrevURL = pageURL(page - 1)
	}
	if page < last {
		result.NextURL = pageURL(page + 1)
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
			URL:     pageURL(number),
			Current: number == page,
		})
	}
	return result
}
