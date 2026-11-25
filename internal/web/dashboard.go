package web

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"plainmote/internal/store"
)

// dashboardPath is where an account's own resources live now that the root is
// the page anyone can open.
const dashboardPath = "/resources/"

const (
	defaultPageSize = 10
	// Finder browser loads a generous directory batch instead of forcing
	// traditional 10-item pages; overflow batches load seamlessly on scroll.
	finderBatchSize = 100
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
	size := finderBatchSize
	if rawSize := strings.TrimSpace(query.Get("size")); rawSize != "" {
		size = normalizePageSize(rawSize)
	}
	page, err := strconv.Atoi(strings.TrimSpace(query.Get("page")))
	if err != nil || page < 1 {
		page = 1
	}
	folderID := strings.TrimSpace(query.Get("folder"))
	viewMode := a.resolveViewMode(w, r)

	filter := store.ResourceFilter{Query: search, Kind: query.Get("kind"), Tags: resourceTags(query["tag"])}
	if filter.Kind != store.KindResource && filter.Kind != store.KindQuickShare {
		filter.Kind = ""
	}

	browsingFolder := search == "" && len(filter.Tags) == 0 && filter.Kind != store.KindQuickShare
	var crumbs []store.Folder
	var childFolders []store.Folder
	if browsingFolder {
		if folderID != "" {
			crumbs, err = a.db.FolderBreadcrumbs(r.Context(), user.ID, folderID)
			if errors.Is(err, store.ErrNotFound) {
				http.Redirect(w, r, dashboardPath, http.StatusSeeOther)
				return
			}
			if err != nil {
				a.renderError(w, http.StatusInternalServerError, err)
				return
			}
		}
		filter.UseFolder = true
		filter.FolderID = folderID
		childFolders, err = a.db.ListChildFolders(r.Context(), user.ID, folderID)
		if err != nil {
			a.renderError(w, http.StatusInternalServerError, err)
			return
		}
	} else {
		folderID = ""
	}

	allFolders, err := a.db.ListAllFolders(r.Context(), user.ID)
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}

	resources, total, err := a.db.ListResourcesBy(r.Context(), user.ID, filter, size, (page-1)*size)
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	if last := lastPage(total, size); page > last {
		http.Redirect(w, r, dashboardURL(filter, size, last), http.StatusSeeOther)
		return
	}
	tags, err := a.db.AccountTags(r.Context(), user.ID)
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	chips := make([]tagChip, 0, len(tags))
	for _, tag := range tags {
		chips = append(chips, tagChip{Name: tag.Name, Count: tag.Count, On: containsFold(filter.Tags, tag.Name)})
	}
	// A tag may have been removed since this URL was saved. Keep it selectable
	// so it can be cleared individually, even if it currently matches nothing.
	for _, selected := range filter.Tags {
		if !slices.ContainsFunc(tags, func(tag store.Tag) bool { return strings.EqualFold(tag.Name, selected) }) {
			chips = append(chips, tagChip{Name: selected, On: true})
		}
	}

	// What is left is part of the list, not a surprise at save time: the
	// account should be able to see it coming.
	quota, err := a.db.QuotaForUser(r.Context(), user.ID, time.Now().UTC())
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}

	pager := buildPager(page, size, total, pageSizes, func(number int) string {
		return dashboardURL(filter, size, number)
	})
	pager.Query = search
	pager.Kind = filter.Kind
	pager.FolderID = folderID
	pager.Tags = filter.Tags
	clear := filter
	clear.Tags = nil
	pager.ClearTagsURL = dashboardURL(clear, size, 1)
	pager.SealedIndexURL = "/resources/sealed?" + url.Values{"kind": {filter.Kind}, "tag": filter.Tags}.Encode()

	viewURL := func(mode string) string {
		u := dashboardURL(filter, size, page)
		if strings.Contains(u, "?") {
			return u + "&view=" + mode
		}
		return u + "?view=" + mode
	}
	newResourceURL := "/resources/new"
	if folderID != "" {
		newResourceURL += "?folder=" + url.QueryEscape(folderID)
	}
	folderTrashed, _ := strconv.Atoi(query.Get("folder_trashed"))
	a.renderTemplate(w, r, http.StatusOK, "dashboard.html", pageData{
		User:            user,
		CSRF:            csrfValue(r),
		Active:          "resources",
		BaseURL:         a.baseURL(r),
		SignedIn:        true,
		Error:           strings.TrimSpace(query.Get("error")),
		Resources:       resources,
		Folders:         childFolders,
		FolderCrumbs:    crumbs,
		FolderOptions:   buildFolderOptions(allFolders),
		CurrentFolderID: folderID,
		BrowsingFolder:  browsingFolder,
		ViewMode:        viewMode,
		IconViewURL:     viewURL(viewIcon),
		ListViewURL:     viewURL(viewList),
		NewResourceURL:  newResourceURL,
		Tags:            chips,
		Quota:           quota,
		Pager:           pager,
		TrashDays:       int(store.TrashRetention / (24 * time.Hour)),
		Trashed:         query.Get("trashed") == "1",
		FolderDeleted:   query.Get("folder_deleted") == "1",
		FolderTrashed:   folderTrashed,
	})
}

func resourceTags(values []string) []string {
	var tags []string
	for _, value := range values {
		tag := truncateBytes(strings.TrimSpace(value), maxQueryLength)
		if tag != "" && !containsFold(tags, tag) {
			tags = append(tags, tag)
		}
	}
	return tags
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

func dashboardURL(filter store.ResourceFilter, size, page int) string {
	values := url.Values{}
	if filter.UseFolder && filter.FolderID != "" {
		values.Set("folder", filter.FolderID)
	}
	if filter.Query != "" {
		values.Set("q", filter.Query)
	}
	if filter.Kind != "" {
		values.Set("kind", filter.Kind)
	}
	for _, tag := range filter.Tags {
		values.Add("tag", tag)
	}
	if size != defaultPageSize && size != finderBatchSize {
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
