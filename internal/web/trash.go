package web

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"plainmote/internal/store"
)

const (
	trashPath        = "trash"
	actionPurge      = "purge"
	actionEmptyTrash = "empty"
)

func (a *App) handleTrashPost(w http.ResponseWriter, r *http.Request, user User, sessionID string) {
	if !a.checkCSRF(r, sessionID) {
		writePlainError(w, http.StatusForbidden, "invalid csrf token")
		return
	}
	ctx := r.Context()
	now := time.Now().UTC()
	switch r.FormValue("action") {
	case actionRestore:
		id := strings.TrimSpace(r.FormValue("id"))
		restored, err := a.db.RestoreResource(ctx, user.ID, id, now)
		if errors.Is(err, store.ErrNotFound) {
			http.Redirect(w, r, accountTrashPath, http.StatusSeeOther)
			return
		}
		if err != nil {
			text, status := a.writeErrorText("restore resource", err)
			a.renderAccount(w, r, user, "trash", text, false, status)
			return
		}
		http.Redirect(w, r, accountTrashPath+"?restored="+url.QueryEscape(restored.ID), http.StatusSeeOther)
	case actionPurge:
		id := strings.TrimSpace(r.FormValue("id"))
		err := a.db.PurgeResource(ctx, user.ID, id)
		if errors.Is(err, store.ErrNotFound) {
			http.Redirect(w, r, accountTrashPath, http.StatusSeeOther)
			return
		}
		if err != nil {
			text, status := a.writeErrorText("purge resource", err)
			a.renderAccount(w, r, user, "trash", text, false, status)
			return
		}
		http.Redirect(w, r, accountTrashPath+"?purged=1", http.StatusSeeOther)
	case actionEmptyTrash:
		removed, err := a.db.EmptyTrash(ctx, user.ID)
		if err != nil {
			text, status := a.writeErrorText("empty trash", err)
			a.renderAccount(w, r, user, "trash", text, false, status)
			return
		}
		http.Redirect(w, r, accountTrashPath+"?emptied="+strconv.FormatInt(removed, 10), http.StatusSeeOther)
	default:
		writePlainError(w, http.StatusBadRequest, "invalid action")
	}
}

// fillAccountTrash loads the trash section inside account.html. It returns
// false when it has already answered the request (redirect or error).
func (a *App) fillAccountTrash(w http.ResponseWriter, r *http.Request, user User, data *pageData) bool {
	ctx := r.Context()
	query := r.URL.Query()
	size := normalizePageSize(query.Get("size"))
	page, err := strconv.Atoi(strings.TrimSpace(query.Get("page")))
	if err != nil || page < 1 {
		page = 1
	}
	resources, total, err := a.db.ListTrash(ctx, user.ID, size, (page-1)*size)
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return false
	}
	if last := lastPage(total, size); page > last {
		http.Redirect(w, r, trashURL(size, last), http.StatusSeeOther)
		return false
	}
	data.Resources = resources
	data.TrashCount = total
	data.TrashDays = int(store.TrashRetention / (24 * time.Hour))
	data.Pager = buildPager(page, size, total, pageSizes, func(number int) string {
		return trashURL(size, number)
	})
	if purgeID := strings.TrimSpace(query.Get("purge")); purgeID != "" {
		if target, err := a.db.TrashedResourceForOwner(ctx, user.ID, purgeID); err == nil {
			data.PurgeTarget = &target
		}
	}
	if query.Get("empty") == "1" && total > 0 {
		data.EmptyOpen = true
	}
	if restoredID := strings.TrimSpace(query.Get("restored")); restoredID != "" {
		if restored, err := a.db.ResourceForOwner(ctx, user.ID, restoredID); err == nil {
			data.RestoredResource = &restored
		}
	}
	data.Purged = query.Get("purged") == "1"
	if emptied, err := strconv.Atoi(strings.TrimSpace(query.Get("emptied"))); err == nil && emptied > 0 {
		data.Emptied = emptied
	}
	return true
}

func trashURL(size, page int) string {
	values := url.Values{}
	if size != defaultPageSize {
		values.Set("size", strconv.Itoa(size))
	}
	if page > 1 {
		values.Set("page", strconv.Itoa(page))
	}
	if len(values) == 0 {
		return accountTrashPath
	}
	return accountTrashPath + "?" + values.Encode()
}

func trashPurgeAt(resource Resource) *time.Time {
	if resource.DeletedAt == nil {
		return nil
	}
	at := resource.DeletedAt.Add(store.TrashRetention)
	if resource.ExpiresAt != nil && resource.ExpiresAt.Before(at) {
		at = *resource.ExpiresAt
	}
	return &at
}
