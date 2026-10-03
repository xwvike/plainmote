package web

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"plainmote/internal/store"
)

const (
	pasteEncryptedPath = "/paste/encrypted"
	accountE2EEPath    = "/account/e2ee"
)

// decryptPagePolicy is the strictest policy any page here carries. The page
// holds the key and the plaintext, so it loads nothing from anywhere else -
// not even the web font - and can talk only to this origin, from which it
// fetches the ciphertext once. blob: is the decrypted image or media it shows,
// made here from bytes it already holds.
const decryptPagePolicy = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' blob:; media-src blob:; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'"

// renderDecryptPage answers a browser opening an encrypted share. The page
// carries no content and costs no use; its script fetches the ciphertext -
// that request is the counted one - and decrypts it with the key after the #
// in the address, which the browser never sends here.
func (a *App) renderDecryptPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", decryptPagePolicy)
	a.renderTemplate(w, r, http.StatusOK, "decrypt.html", pageData{
		MediaSource: r.URL.EscapedPath() + "?raw=1",
	})
}

// e2eeEnabled is whether this visitor's quick shares are to be encrypted: a
// signed-in account that turned it on, on a deployment with quick shares.
func (a *App) e2eeEnabled(r *http.Request, userID string) bool {
	if !a.cfg.AnonymousEnabled || userID == "" {
		return false
	}
	enabled, err := a.db.E2EEEnabled(r.Context(), userID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read e2ee setting: %v\n", err)
		return false
	}
	return enabled
}

// handleEncryptedPaste takes the ciphertext the home page's script produced.
// It is the browser's endpoint only: a signed-in session, its CSRF token, the
// account's setting on, and an envelope of the expected shape. The answer is
// the result page's address; the page adds the key to it on its side.
func (a *App) handleEncryptedPaste(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !a.sameOriginPost(r) {
		writePlainError(w, http.StatusForbidden, "cross-site post")
		return
	}
	locale := requestLanguage(r).Locale
	w.Header().Add("Vary", "Accept-Language")
	// Every refusal is a sentence for the page to show as it is.
	user, sessionID, ok := a.currentUser(r)
	if !ok {
		writePlainError(w, http.StatusUnauthorized, translate(locale, "e2ee_signed_out"))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, store.EncryptedMaxBytes+8<<10)
	if err := r.ParseMultipartForm(store.EncryptedMaxBytes + 8<<10); err != nil {
		writePlainError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(translate(locale, "e2ee_too_large"), store.BytesText(store.AnonymousMaxBytes)))
		return
	}
	defer r.MultipartForm.RemoveAll()
	if !a.checkCSRF(r, sessionID) {
		writePlainError(w, http.StatusForbidden, translate(locale, "e2ee_page_stale"))
		return
	}
	if !a.e2eeEnabled(r, user.ID) {
		writePlainError(w, http.StatusForbidden, translate(locale, "e2ee_turned_off"))
		return
	}
	files := r.MultipartForm.File["envelope"]
	if len(files) != 1 {
		writePlainError(w, http.StatusBadRequest, translate(locale, "e2ee_bad_envelope"))
		return
	}
	part, err := files[0].Open()
	if err != nil {
		writePlainError(w, http.StatusBadRequest, translate(locale, "e2ee_bad_envelope"))
		return
	}
	defer part.Close()
	envelope, err := io.ReadAll(io.LimitReader(part, store.EncryptedMaxBytes+1))
	if err != nil {
		writePlainError(w, http.StatusBadRequest, translate(locale, "e2ee_bad_envelope"))
		return
	}
	ttl, _ := parsePasteTTL(r.FormValue("ttl"))
	resource, _, err := a.db.CreateEncryptedPaste(r.Context(), user.ID, envelope, ttl, time.Now().UTC())
	if err != nil {
		if !store.IsRefusal(err) {
			fmt.Fprintf(os.Stderr, "create encrypted paste: %v\n", err)
			writePlainError(w, http.StatusInternalServerError, translate(locale, "e2ee_failed"))
			return
		}
		writePlainError(w, http.StatusBadRequest, localizePageError(locale, err.Error()))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"result": pasteResultPrefix + resource.ID})
}

// handleAccountE2EE turns encrypted quick shares on or off, from the account
// page's form.
func (a *App) handleAccountE2EE(w http.ResponseWriter, r *http.Request) {
	user, sessionID, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !a.checkCSRF(r, sessionID) {
		writePlainError(w, http.StatusForbidden, "invalid csrf token")
		return
	}
	if err := a.db.SetE2EE(r.Context(), user.ID, r.FormValue("e2ee") == "on"); err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	http.Redirect(w, r, accountPath, http.StatusSeeOther)
}
