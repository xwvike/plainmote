package web

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"plainmote/internal/store"
)

// dashboardPath is where an account's own resources live now that the root is
// the page anyone can open.
const dashboardPath = "/resources/"

// batchHeader marks an incremental cursor-batch fetch from file-browser.js.
// Unlike X-PlainMote-Fragment (which replaces the whole directory view), a
// batch request queries only the next slice of resources and returns pure item
// markup to append into the active grid or list.
const batchHeader = "X-PlainMote-Batch"

const (
	// Table-view pagination defaults (used by Trash and Access Logs).
	defaultPageSize = 10
	// Finder browser streams directories in keyset-cursor batches instead of
	// numbered OFFSET pages, preventing item skips/duplicates during mutations.
	finderBatchSize = 60
	// Keep search URLs and LIKE patterns bounded without cutting a UTF-8 rune.
	maxQueryLength = 256
	pageWindow     = 7
)

// dashboardBatchLimit defaults to finderBatchSize and may be overridden in tests.
var dashboardBatchLimit = finderBatchSize

var pageSizes = []int{10, 20, 50}

func encodeResourceCursor(c *store.ResourceCursor) string {
	if c == nil || c.CreatedAt.IsZero() || c.ID == "" {
		return ""
	}
	raw := fmt.Sprintf("%d.%s", c.CreatedAt.UTC().UnixMicro(), c.ID)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeResourceCursor(token string) *store.ResourceCursor {
	token = strings.TrimSpace(token)
	if token == "" || len(token) > 128 {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return nil
	}
	usStr, id, ok := strings.Cut(string(raw), ".")
	if !ok {
		return nil
	}
	us, err := strconv.ParseInt(usStr, 10, 64)
	if err != nil || us <= 0 {
		return nil
	}
	parsedID, err := uuid.Parse(id)
	if err != nil {
		return nil
	}
	return &store.ResourceCursor{
		CreatedAt: time.UnixMicro(us).UTC(),
		ID:        parsedID.String(),
	}
}

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
	folderID := strings.TrimSpace(query.Get("folder"))
	viewMode := a.resolveViewMode(w, r)
	after := decodeResourceCursor(query.Get("cursor"))

	filter := store.ResourceFilter{Query: search, Kind: query.Get("kind"), Tags: resourceTags(query["tag"])}
	if filter.Kind != store.KindResource && filter.Kind != store.KindQuickShare {
		filter.Kind = ""
	}

	browsingFolder := search == "" && len(filter.Tags) == 0 && filter.Kind != store.KindQuickShare
	if browsingFolder {
		filter.UseFolder = true
		filter.FolderID = folderID
	} else {
		folderID = ""
	}

	batchLimit := dashboardBatchLimit
	if batchLimit <= 0 {
		batchLimit = finderBatchSize
	}

	// Fast path for incremental cursor streaming: skip folder tree, tag sidebar,
	// breadcrumbs, and storage quota queries completely.
	if r.Header.Get(batchHeader) != "" {
		batch, err := a.db.ListResourcesCursor(r.Context(), user.ID, filter, batchLimit, after)
		if err != nil {
			a.renderError(w, http.StatusInternalServerError, err)
			return
		}
		nextURL := ""
		if batch.NextCursor != nil {
			nextURL = dashboardURL(filter, encodeResourceCursor(batch.NextCursor))
		}
		a.renderTemplate(w, r, http.StatusOK, "dashboard.html#batch", pageData{
			Resources: batch.Resources,
			ViewMode:  viewMode,
			Pager: pager{
				Total:   batch.Total,
				NextURL: nextURL,
			},
		})
		return
	}

	var crumbs []store.Folder
	var childFolders []store.Folder
	if browsingFolder {
		var err error
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
		// Only include folders on the initial view (not when a no-JS client
		// follows a subsequent cursor link).
		if after == nil {
			childFolders, err = a.db.ListChildFolders(r.Context(), user.ID, folderID)
			if err != nil {
				a.renderError(w, http.StatusInternalServerError, err)
				return
			}
		}
	}

	allFolders, err := a.db.ListAllFolders(r.Context(), user.ID)
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}

	batch, err := a.db.ListResourcesCursor(r.Context(), user.ID, filter, batchLimit, after)
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	// If a stale no-JS cursor landed past the end of a non-empty list, bounce
	// cleanly to the top of the current directory/filter.
	if after != nil && len(batch.Resources) == 0 && batch.Total > 0 {
		http.Redirect(w, r, dashboardURL(filter, ""), http.StatusSeeOther)
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

	nextURL := ""
	if batch.NextCursor != nil {
		nextURL = dashboardURL(filter, encodeResourceCursor(batch.NextCursor))
	}
	clear := filter
	clear.Tags = nil
	p := pager{
		Query:          search,
		Kind:           filter.Kind,
		FolderID:       folderID,
		Tags:           filter.Tags,
		ClearTagsURL:   dashboardURL(clear, ""),
		SealedIndexURL: "/resources/sealed?" + url.Values{"kind": {filter.Kind}, "tag": filter.Tags}.Encode(),
		Total:          batch.Total,
		NextURL:        nextURL,
	}

	viewURL := func(mode string) string {
		u := dashboardURL(filter, "")
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
		MaxContent:      a.cfg.MaxContent,
		SignedIn:        true,
		Error:           strings.TrimSpace(query.Get("error")),
		Resources:       batch.Resources,
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
		Pager:           p,
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

func dashboardURL(filter store.ResourceFilter, cursor string) string {
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
	if cursor != "" {
		values.Set("cursor", cursor)
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
