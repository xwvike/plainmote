package web

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"plainmote/internal/store"
)

const (
	foldersPath = "folders"
	movePath    = "move"
	renamePath  = "rename"
	viewCookie  = "plainmote_view"
	viewIcon    = "icon"
	viewList    = "list"
)

func modalModeFromRequest(r *http.Request) string {
	mode := strings.TrimSpace(r.URL.Query().Get("modal"))
	if mode == "" && r.Method == http.MethodPost {
		mode = strings.TrimSpace(r.FormValue("modal"))
	}
	switch mode {
	case "edit", "share", "versions":
		return mode
	default:
		return ""
	}
}

func appendModalParam(target, mode string) string {
	if mode == "" {
		return target
	}
	if strings.Contains(target, "?") {
		return target + "&modal=" + url.QueryEscape(mode)
	}
	return target + "?modal=" + url.QueryEscape(mode)
}

// folderOption represents one entry in the "Move to…" folder selector, ordered
// depth-first so the hierarchy reads naturally inside a plain <select>.
type folderOption struct {
	ID       string
	ParentID string
	Name     string
	Label    string
	Depth    int
}

func buildFolderOptions(folders []store.Folder) []folderOption {
	if len(folders) == 0 {
		return nil
	}
	children := make(map[string][]store.Folder, len(folders))
	for _, f := range folders {
		children[f.ParentID] = append(children[f.ParentID], f)
	}
	for k := range children {
		slices.SortFunc(children[k], func(a, b store.Folder) int {
			if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
				return c
			}
			return strings.Compare(a.ID, b.ID)
		})
	}
	var out []folderOption
	var walk func(parentID string, depth int)
	walk = func(parentID string, depth int) {
		if depth > store.FolderMaxDepth+1 {
			return
		}
		for _, f := range children[parentID] {
			prefix := strings.Repeat("— ", depth)
			out = append(out, folderOption{
				ID:       f.ID,
				ParentID: f.ParentID,
				Name:     f.Name,
				Label:    prefix + f.Name,
				Depth:    depth,
			})
			walk(f.ID, depth+1)
		}
	}
	walk("", 0)
	return out
}

// resolveViewMode reads ?view=icon|list (updating the preference cookie when
// explicitly given), otherwise falls back to the saved cookie, defaulting to
// the Finder-style icon grid.
func (a *App) resolveViewMode(w http.ResponseWriter, r *http.Request) string {
	q := strings.TrimSpace(r.URL.Query().Get("view"))
	if q == viewIcon || q == viewList {
		http.SetCookie(w, &http.Cookie{
			Name:     viewCookie,
			Value:    q,
			Path:     "/",
			MaxAge:   365 * 24 * 3600,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
			Secure:   strings.HasPrefix(a.baseURL(r), "https://"),
		})
		return q
	}
	if c, err := r.Cookie(viewCookie); err == nil && (c.Value == viewIcon || c.Value == viewList) {
		return c.Value
	}
	return viewIcon
}

func folderDashboardURL(folderID string, extra url.Values) string {
	values := url.Values{}
	for k, vs := range extra {
		for _, v := range vs {
			values.Add(k, v)
		}
	}
	if folderID != "" {
		values.Set("folder", folderID)
	}
	if len(values) == 0 {
		return dashboardPath
	}
	return dashboardPath + "?" + values.Encode()
}

func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

// handleFolders handles POST /resources/folders for creating, renaming, moving,
// and deleting folders.
func (a *App) handleFolders(w http.ResponseWriter, r *http.Request, user User, sessionID string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !a.sameOriginPost(r) {
		writePlainError(w, http.StatusForbidden, "cross-site post")
		return
	}
	if !a.checkCSRF(r, sessionID) {
		writePlainError(w, http.StatusForbidden, "invalid csrf token")
		return
	}

	action := strings.TrimSpace(r.FormValue("action"))
	returnFolder := strings.TrimSpace(r.FormValue("return_folder"))
	fail := func(what string, err error) {
		text, status := a.writeErrorText(what, err)
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
			text = "not found"
		}
		if wantsJSON(r) {
			a.writeAPI(w, status, map[string]string{"message": localizePageError(requestLanguage(r).Locale, text)})
			return
		}
		http.Redirect(w, r, folderDashboardURL(returnFolder, url.Values{"error": {text}}), http.StatusSeeOther)
	}
	okRedirect := func(targetFolder string, extra url.Values) {
		loc := folderDashboardURL(targetFolder, extra)
		if wantsJSON(r) {
			a.writeAPI(w, http.StatusOK, map[string]string{"location": loc})
			return
		}
		http.Redirect(w, r, loc, http.StatusSeeOther)
	}

	switch action {
	case "create":
		parentID := strings.TrimSpace(r.FormValue("parent_id"))
		name := r.FormValue("name")
		folder, err := a.db.CreateFolder(r.Context(), user.ID, parentID, name)
		if err != nil {
			fail("create folder", err)
			return
		}
		if wantsJSON(r) {
			a.writeAPI(w, http.StatusCreated, map[string]string{
				"id":       folder.ID,
				"location": folderDashboardURL(parentID, nil),
			})
			return
		}
		okRedirect(parentID, nil)

	case "rename":
		folderID := strings.TrimSpace(r.FormValue("folder_id"))
		name := r.FormValue("name")
		folder, err := a.db.RenameFolder(r.Context(), user.ID, folderID, name)
		if err != nil {
			fail("rename folder", err)
			return
		}
		dest := returnFolder
		if dest == "" {
			dest = folder.ParentID
		}
		okRedirect(dest, nil)

	case "move":
		folderID := strings.TrimSpace(r.FormValue("folder_id"))
		targetID := strings.TrimSpace(r.FormValue("target_folder_id"))
		if err := a.db.MoveFolder(r.Context(), user.ID, folderID, targetID); err != nil {
			fail("move folder", err)
			return
		}
		okRedirect(returnFolder, nil)

	case "delete":
		folderID := strings.TrimSpace(r.FormValue("folder_id"))
		folder, err := a.db.FolderForOwner(r.Context(), user.ID, folderID)
		if err != nil {
			fail("delete folder", err)
			return
		}
		trashed, _, err := a.db.TrashFolder(r.Context(), user.ID, folderID, time.Now().UTC())
		if err != nil {
			fail("delete folder", err)
			return
		}
		dest := returnFolder
		if dest == folderID {
			dest = folder.ParentID
		}
		extra := url.Values{"folder_deleted": {"1"}}
		if trashed > 0 {
			extra.Set("folder_trashed", strconv.Itoa(trashed))
		}
		okRedirect(dest, extra)

	default:
		writePlainError(w, http.StatusBadRequest, "unknown folder action")
	}
}

// handleMoveResource handles POST /resources/move to place a resource into a
// folder (or back to the root when target_folder_id is empty).
func (a *App) handleMoveResource(w http.ResponseWriter, r *http.Request, user User, sessionID string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !a.sameOriginPost(r) {
		writePlainError(w, http.StatusForbidden, "cross-site post")
		return
	}
	if !a.checkCSRF(r, sessionID) {
		writePlainError(w, http.StatusForbidden, "invalid csrf token")
		return
	}
	resourceID := strings.TrimSpace(r.FormValue("resource_id"))
	targetID := strings.TrimSpace(r.FormValue("target_folder_id"))
	returnFolder := strings.TrimSpace(r.FormValue("return_folder"))
	if err := a.db.MoveResource(r.Context(), user.ID, resourceID, targetID); err != nil {
		text, status := a.writeErrorText("move resource", err)
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
			text = "not found"
		}
		if wantsJSON(r) {
			a.writeAPI(w, status, map[string]string{"message": localizePageError(requestLanguage(r).Locale, text)})
			return
		}
		http.Redirect(w, r, folderDashboardURL(returnFolder, url.Values{"error": {text}}), http.StatusSeeOther)
		return
	}
	loc := folderDashboardURL(returnFolder, nil)
	if wantsJSON(r) {
		a.writeAPI(w, http.StatusOK, map[string]string{"location": loc})
		return
	}
	http.Redirect(w, r, loc, http.StatusSeeOther)
}

// handleRenameResource handles POST /resources/rename to rename a plaintext
// resource's filename (and unified name) directly from the Finder browser.
func (a *App) handleRenameResource(w http.ResponseWriter, r *http.Request, user User, sessionID string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !a.sameOriginPost(r) {
		writePlainError(w, http.StatusForbidden, "cross-site post")
		return
	}
	if !a.checkCSRF(r, sessionID) {
		writePlainError(w, http.StatusForbidden, "invalid csrf token")
		return
	}
	resourceID := strings.TrimSpace(r.FormValue("resource_id"))
	filename := strings.TrimSpace(r.FormValue("filename"))
	returnFolder := strings.TrimSpace(r.FormValue("return_folder"))
	_, err := a.db.SaveResource(r.Context(), user.ID, resourceID, store.ResourceEdit{
		Name:     filename,
		Filename: filename,
		Content:  nil,
	})
	if err != nil {
		text, status := a.writeErrorText("rename resource", err)
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
			text = "not found"
		}
		if wantsJSON(r) {
			a.writeAPI(w, status, map[string]string{"message": localizePageError(requestLanguage(r).Locale, text)})
			return
		}
		http.Redirect(w, r, folderDashboardURL(returnFolder, url.Values{"error": {text}}), http.StatusSeeOther)
		return
	}
	loc := folderDashboardURL(returnFolder, nil)
	if wantsJSON(r) {
		a.writeAPI(w, http.StatusOK, map[string]string{"location": loc})
		return
	}
	http.Redirect(w, r, loc, http.StatusSeeOther)
}
