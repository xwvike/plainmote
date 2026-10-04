package web

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"plainmote/internal/store"
)

// End-to-end encrypted resources, from the page's side. Everything that
// reaches these handlers was encrypted - or, to turn encryption off,
// decrypted - in the browser by sealed.js; they store it, check its shape and
// answer in JSON, since the script that sent it decides where to go next.
//
//	GET  /resources/sealed        the account's encrypted names, still encrypted
//	POST /resources/sealed        a new encrypted resource
//	POST /resources/{id}/sealed   action=save, seal or unseal

const sealedPath = "sealed"

// sealedRequestLimit bounds one request: turning encryption on or off sends
// the current content and every earlier version at once.
func (a *App) sealedRequestLimit() int64 {
	return int64(store.HistoryKeep+2) * (a.cfg.MaxContent + 64<<10)
}

func (a *App) sealedFail(w http.ResponseWriter, r *http.Request, status int, code string, err error) {
	message := ""
	if err != nil {
		text, failed := a.writeErrorText("sealed resource", err)
		if failed >= 500 {
			status = failed
		}
		message = localizePageError(requestLanguage(r).Locale, text)
	}
	a.keyringFail(w, status, code, message)
}

// readSealedForm parses one sealed request and checks it came from this
// site, with the session's CSRF token.
func (a *App) readSealedForm(w http.ResponseWriter, r *http.Request, sessionID string) bool {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return false
	}
	if !a.sameOriginPost(r) {
		writePlainError(w, http.StatusForbidden, "cross-site post")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, a.sealedRequestLimit())
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		a.keyringFail(w, http.StatusRequestEntityTooLarge, "too_large", "")
		return false
	}
	if !a.checkCSRF(r, sessionID) {
		writePlainError(w, http.StatusForbidden, "invalid csrf token")
		return false
	}
	return true
}

// part is a file field of the form, or nil when there is none.
func (a *App) part(r *http.Request, name string) []byte {
	files := r.MultipartForm.File[name]
	if len(files) == 0 {
		return nil
	}
	file, err := files[0].Open()
	if err != nil {
		return nil
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, a.cfg.MaxContent+64<<10+1))
	if err != nil || int64(len(data)) > a.cfg.MaxContent+64<<10 {
		return nil
	}
	return data
}

func base64Field(r *http.Request, name string) []byte {
	value, err := base64.RawURLEncoding.DecodeString(r.FormValue(name))
	if err != nil {
		return nil
	}
	return value
}

// versionFields gathers the earlier versions a request carries, named
// version_<n>_<field>.
func versionNumbers(r *http.Request) []int {
	seen := map[int]bool{}
	var numbers []int
	collect := func(key string) {
		if !strings.HasPrefix(key, "version_") {
			return
		}
		rest := strings.TrimPrefix(key, "version_")
		number, err := strconv.Atoi(rest[:max(strings.IndexByte(rest, '_'), 0)])
		if err == nil && number > 0 && !seen[number] {
			seen[number] = true
			numbers = append(numbers, number)
		}
	}
	for key := range r.MultipartForm.Value {
		collect(key)
	}
	for key := range r.MultipartForm.File {
		collect(key)
	}
	return numbers
}

func (a *App) handleSealedIndex(w http.ResponseWriter, r *http.Request, user User, sessionID string) {
	if r.Method == http.MethodPost {
		a.handleSealedCreate(w, r, user, sessionID)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, POST")
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	entries, err := a.db.SealedIndex(r.Context(), user.ID)
	if err != nil {
		a.serverError(w, "list sealed resources", err)
		return
	}
	type entry struct {
		ID         string `json:"id"`
		SealedKey  string `json:"sealed_key"`
		SealedMeta string `json:"sealed_meta"`
	}
	out := make([]entry, 0, len(entries))
	encode := base64.RawURLEncoding.EncodeToString
	for _, e := range entries {
		out = append(out, entry{ID: e.ID, SealedKey: encode(e.SealedKey), SealedMeta: encode(e.SealedMeta)})
	}
	w.Header().Set("Cache-Control", "no-store")
	a.writeAPI(w, http.StatusOK, map[string]any{"resources": out})
}

func (a *App) handleSealedCreate(w http.ResponseWriter, r *http.Request, user User, sessionID string) {
	if !a.readSealedForm(w, r, sessionID) {
		return
	}
	resource, err := a.db.CreateSealedResource(r.Context(), user.ID, r.FormValue("id"),
		store.SealedPart{Content: a.part(r, "content"), Meta: base64Field(r, "meta")}, base64Field(r, "sealed_key"))
	if err != nil {
		a.sealedFail(w, r, http.StatusBadRequest, "refused", err)
		return
	}
	a.writeAPI(w, http.StatusCreated, map[string]string{"location": "/resources/" + resource.ID + "?created=1"})
}

func (a *App) handleSealedAction(w http.ResponseWriter, r *http.Request, user User, sessionID, resourceID string) {
	if !a.readSealedForm(w, r, sessionID) {
		return
	}
	base := versionNumber(r.FormValue("base_version"))
	var err error
	answer := map[string]any{}
	switch r.FormValue("action") {
	case "save":
		var result store.SaveResult
		result, err = a.db.SaveSealedResource(r.Context(), user.ID, resourceID, base,
			store.SealedPart{Content: a.part(r, "content"), Meta: base64Field(r, "meta")})
		if err == nil {
			saved := "0"
			if result.NewVersion {
				saved = strconv.Itoa(result.Version)
			}
			location := "/resources/" + resourceID + "?saved=" + saved
			if result.Trimmed > 0 {
				location += "&trimmed=" + strconv.Itoa(result.Trimmed)
			}
			answer["location"] = location
		}
	case "seal":
		versions := map[int]store.SealedPart{}
		for _, number := range versionNumbers(r) {
			prefix := fmt.Sprintf("version_%d_", number)
			versions[number] = store.SealedPart{Content: a.part(r, prefix+"content"), Meta: base64Field(r, prefix+"meta")}
		}
		var revoked int
		revoked, err = a.db.SealResource(r.Context(), user.ID, resourceID, base,
			store.SealedPart{Content: a.part(r, "content"), Meta: base64Field(r, "meta")}, versions, base64Field(r, "sealed_key"))
		if err == nil {
			answer["location"] = "/resources/" + resourceID + "?sealed=1"
			answer["revoked"] = revoked
		}
	case "unseal":
		versions := map[int]store.PlainPart{}
		for _, number := range versionNumbers(r) {
			prefix := fmt.Sprintf("version_%d_", number)
			versions[number] = store.PlainPart{Content: a.part(r, prefix+"content"), Filename: strings.TrimSpace(r.FormValue(prefix + "filename"))}
		}
		err = a.db.UnsealResource(r.Context(), user.ID, resourceID, base, store.PlainPart{
			Content: a.part(r, "content"), Name: strings.TrimSpace(r.FormValue("name")), Filename: strings.TrimSpace(r.FormValue("filename")),
		}, versions)
		if err == nil {
			answer["location"] = "/resources/" + resourceID + "?unsealed=1"
		}
	default:
		a.keyringFail(w, http.StatusBadRequest, "bad_request", "")
		return
	}
	var conflict *store.VersionConflict
	switch {
	case errors.As(err, &conflict):
		w.Header().Set("Content-Type", "application/json")
		a.writeAPI(w, http.StatusConflict, map[string]any{"error": "conflict", "current_version": conflict.Current,
			"message": localizePageError(requestLanguage(r).Locale, conflict.Error())})
	case errors.Is(err, store.ErrNotFound):
		a.keyringFail(w, http.StatusNotFound, "not_found", "")
	case err != nil:
		a.sealedFail(w, r, http.StatusBadRequest, "refused", err)
	default:
		a.writeAPI(w, http.StatusOK, answer)
	}
}

// createSealedShare makes a link to an encrypted resource with the keys the
// browser made for it.
func (a *App) createSealedShare(r *http.Request, user User, resourceID string) error {
	ttl, err := shareTTL(r)
	if err != nil {
		ttl = defaultShareTTL
	}
	maxUses, err := shareUses(r)
	if err != nil {
		maxUses = defaultShareUses
	}
	_, err = a.db.CreateSealedShare(r.Context(), user.ID, resourceID, r.FormValue("link_id"), strings.TrimSpace(r.FormValue("name")),
		ttl, maxUses, base64Field(r, "sealed_key"), base64Field(r, "owner_key"))
	return err
}

func (a *App) hasKeyring(r *http.Request, userID string) (bool, error) {
	_, err := a.db.Keyring(r.Context(), userID)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// historyJSON describes a resource's earlier versions for sealed.js: their
// numbers, and what each needs to be read - its filename and encoding while
// in plaintext, its encrypted metadata once encrypted.
func (a *App) historyJSON(r *http.Request, userID, resourceID string) (string, error) {
	versions, err := a.db.ListVersions(r.Context(), userID, resourceID)
	if err != nil {
		return "", err
	}
	type entry struct {
		Number   int    `json:"n"`
		Filename string `json:"f,omitempty"`
		Type     string `json:"t,omitempty"`
		Encoding string `json:"e,omitempty"`
		Meta     string `json:"m,omitempty"`
	}
	out := make([]entry, 0, len(versions))
	for _, v := range versions {
		e := entry{Number: v.Number, Filename: v.Filename, Type: v.ContentType, Encoding: v.ContentEncoding}
		if len(v.SealedMeta) > 0 {
			e = entry{Number: v.Number, Meta: base64.RawURLEncoding.EncodeToString(v.SealedMeta)}
		}
		out = append(out, e)
	}
	encoded, err := json.Marshal(out)
	return string(encoded), err
}
