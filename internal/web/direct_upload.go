package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"plainmote/internal/store"
)

const (
	uploadPresignPath = "/uploads/presign"
	uploadJSONHeader  = "X-PlainMote-Upload"

	uploadPurposePaste          = "paste"
	uploadPurposeSealedPaste    = "sealed_paste"
	uploadPurposeResourceCreate = "resource_create"
	uploadPurposeResourceSave   = "resource_save"
	uploadPurposeSealedCreate   = "sealed_create"
	uploadPurposeSealedSave     = "sealed_save"

	presignURLTTL    = 10 * time.Minute
	presignTicketTTL = 15 * time.Minute
)

func (a *App) directUploadEnabled() bool {
	if a.db == nil || !a.db.CanPresign() {
		return false
	}
	return a.cfg.DirectUpload || strings.HasPrefix(strings.ToLower(strings.TrimSpace(a.cfg.BlobEndpoint)), "https://")
}

func (a *App) handleUploadPresign(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !a.sameOriginPost(r) {
		writePlainError(w, http.StatusForbidden, "cross-site post")
		return
	}
	if !a.directUploadEnabled() {
		a.writeAPI(w, http.StatusNotImplemented, map[string]string{"error": "direct_upload_disabled"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	var req struct {
		Purpose    string `json:"purpose"`
		Size       int64  `json:"size"`
		SHA256     string `json:"sha256"`
		ResourceID string `json:"resource_id"`
		CSRF       string `json:"csrf"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		a.writeAPI(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
		return
	}
	locale := requestLanguage(r).Locale
	now := time.Now().UTC()
	if req.Size <= 0 {
		a.writeAPI(w, http.StatusBadRequest, map[string]string{"error": "empty", "message": localizePageError(locale, "内容不能为空")})
		return
	}

	user, sessionID, signedIn := a.currentUser(r)
	var ownerID, resourceID, excludeResourceID string
	addResources := 1

	switch req.Purpose {
	case uploadPurposePaste:
		if !a.cfg.AnonymousEnabled {
			a.writeAPI(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		if req.Size > store.AnonymousMaxBytes {
			msg := fmt.Sprintf("内容最大 %s", store.BytesText(store.AnonymousMaxBytes))
			a.writeAPI(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "too_large", "message": localizePageError(locale, msg)})
			return
		}
		if signedIn {
			if !a.checkCSRFValue(r, sessionID, req.CSRF) {
				a.writeAPI(w, http.StatusForbidden, map[string]string{"error": "csrf", "message": translate(locale, "e2ee_page_stale")})
				return
			}
			ownerID = user.ID
		} else {
			// Spend the anonymous rate limit atomically at presign time so
			// uncommitted uploads cannot bypass IP rate limits and fill the bucket.
			key := addressKey(a.clientIP(r))
			limited := a.limits.pastes
			if limited.allow(key, now) {
				if limited = a.limits.pasteBytes; limited.spend(key, int(req.Size), now) {
					limited = nil
				}
			}
			if limited != nil {
				wait := limited.retryIn(key, now)
				w.Header().Set("Retry-After", strconv.Itoa(int(wait.Round(time.Second)/time.Second)))
				a.writeAPI(w, http.StatusTooManyRequests, map[string]string{
					"error":   "rate_limited",
					"message": fmt.Sprintf(translate(locale, "paste_rate_limited"), waitText(locale, wait)),
				})
				return
			}
			ownerID = store.AnonymousUserID
		}
		resourceID = uuid.NewString()

	case uploadPurposeSealedPaste:
		if !a.cfg.AnonymousEnabled {
			a.writeAPI(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		if !signedIn {
			a.writeAPI(w, http.StatusUnauthorized, map[string]string{"error": "signed_out", "message": translate(locale, "e2ee_signed_out")})
			return
		}
		if !a.checkCSRFValue(r, sessionID, req.CSRF) {
			a.writeAPI(w, http.StatusForbidden, map[string]string{"error": "csrf", "message": translate(locale, "e2ee_page_stale")})
			return
		}
		if has, err := a.hasKeyring(r, user.ID); err != nil || !has {
			a.writeAPI(w, http.StatusForbidden, map[string]string{"error": "no_keyring", "message": translate(locale, "e2ee_turned_off")})
			return
		}
		if uuid.Validate(req.ResourceID) != nil {
			a.writeAPI(w, http.StatusBadRequest, map[string]string{"error": "invalid_resource"})
			return
		}
		if req.Size <= store.SealedHeaderBytes || req.Size > store.AnonymousMaxBytes+store.SealedHeaderBytes {
			a.writeAPI(w, http.StatusRequestEntityTooLarge, map[string]string{
				"error":   "too_large",
				"message": fmt.Sprintf(translate(locale, "e2ee_too_large"), store.BytesText(store.AnonymousMaxBytes)),
			})
			return
		}
		ownerID, resourceID = user.ID, req.ResourceID

	case uploadPurposeResourceCreate:
		if !signedIn || !a.checkCSRFValue(r, sessionID, req.CSRF) {
			a.writeAPI(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return
		}
		if req.Size > a.cfg.MaxContent {
			a.writeAPI(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "too_large", "message": localizePageError(locale, "uploaded file is too large")})
			return
		}
		ownerID, resourceID = user.ID, uuid.NewString()

	case uploadPurposeResourceSave:
		if !signedIn || !a.checkCSRFValue(r, sessionID, req.CSRF) {
			a.writeAPI(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return
		}
		if req.Size > a.cfg.MaxContent {
			a.writeAPI(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "too_large", "message": localizePageError(locale, "uploaded file is too large")})
			return
		}
		res, err := a.db.ResourceForOwner(r.Context(), user.ID, req.ResourceID)
		if errors.Is(err, store.ErrNotFound) {
			a.writeAPI(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		if err != nil {
			a.serverError(w, "presign resource lookup", err)
			return
		}
		if res.QuickShare() || res.Sealed() {
			a.writeAPI(w, http.StatusBadRequest, map[string]string{"error": "refused"})
			return
		}
		ownerID, resourceID, excludeResourceID, addResources = user.ID, res.ID, res.ID, 0

	case uploadPurposeSealedCreate:
		if !signedIn || !a.checkCSRFValue(r, sessionID, req.CSRF) {
			a.writeAPI(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return
		}
		if has, err := a.hasKeyring(r, user.ID); err != nil || !has {
			a.writeAPI(w, http.StatusForbidden, map[string]string{"error": "no_keyring", "message": translate(locale, "e2ee_turned_off")})
			return
		}
		if uuid.Validate(req.ResourceID) != nil {
			a.writeAPI(w, http.StatusBadRequest, map[string]string{"error": "invalid_resource"})
			return
		}
		if req.Size <= store.SealedHeaderBytes || req.Size > a.cfg.MaxContent+store.SealedHeaderBytes {
			a.writeAPI(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "too_large", "message": localizePageError(locale, "uploaded file is too large")})
			return
		}
		ownerID, resourceID = user.ID, req.ResourceID

	case uploadPurposeSealedSave:
		if !signedIn || !a.checkCSRFValue(r, sessionID, req.CSRF) {
			a.writeAPI(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
			return
		}
		res, err := a.db.ResourceForOwner(r.Context(), user.ID, req.ResourceID)
		if errors.Is(err, store.ErrNotFound) {
			a.writeAPI(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		if err != nil {
			a.serverError(w, "presign sealed lookup", err)
			return
		}
		if !res.Sealed() {
			a.writeAPI(w, http.StatusBadRequest, map[string]string{"error": "refused"})
			return
		}
		if req.Size <= store.SealedHeaderBytes || req.Size > a.cfg.MaxContent+store.SealedHeaderBytes {
			a.writeAPI(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "too_large", "message": localizePageError(locale, "uploaded file is too large")})
			return
		}
		ownerID, resourceID, excludeResourceID, addResources = user.ID, res.ID, res.ID, 0

	default:
		a.writeAPI(w, http.StatusBadRequest, map[string]string{"error": "invalid_purpose"})
		return
	}

	presigned, token, _, err := a.db.BeginStagedUpload(
		r.Context(), ownerID, req.Purpose, resourceID, excludeResourceID,
		req.Size, addResources, req.SHA256, presignURLTTL, presignTicketTTL, now,
	)
	if err != nil {
		text, status := a.writeErrorText("begin staged upload", err)
		a.writeAPI(w, status, map[string]string{"error": "refused", "message": localizePageError(locale, text)})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	a.writeAPI(w, http.StatusOK, map[string]any{
		"url":         presigned.URL,
		"headers":     presigned.Headers,
		"token":       token,
		"resource_id": resourceID,
	})
}

func wantsUploadJSON(r *http.Request) bool {
	return r.Header.Get(uploadJSONHeader) == "1"
}

func (a *App) verifyUploadToken(token, purpose, ownerID, resourceID string) (store.UploadTicket, error) {
	ticket, err := a.db.VerifyUploadTicket(token, time.Now().UTC())
	if err != nil {
		return store.UploadTicket{}, err
	}
	if ticket.Purpose != purpose || ticket.OwnerID != ownerID {
		return store.UploadTicket{}, errors.New("上传凭证无效")
	}
	if resourceID != "" && ticket.ResourceID != resourceID {
		return store.UploadTicket{}, errors.New("上传凭证无效")
	}
	return ticket, nil
}
