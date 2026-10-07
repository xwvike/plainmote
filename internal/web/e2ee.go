package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	"plainmote/internal/store"
)

const (
	pasteSealedPath = "/paste/sealed"
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

// e2eeEnabled is whether the home box's encryption switch starts on: an
// account with a master password whose last quick share from the box was
// encrypted, on a deployment with quick shares. Without a master password
// there is nothing to encrypt under, and the switch is off.
func (a *App) e2eeEnabled(r *http.Request, userID string) bool {
	if !a.cfg.AnonymousEnabled || userID == "" {
		return false
	}
	enabled, err := a.db.E2EEEnabled(r.Context(), userID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read e2ee setting: %v\n", err)
		return false
	}
	if !enabled {
		return false
	}
	keyring, err := a.hasKeyring(r, userID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read keyring: %v\n", err)
	}
	return keyring
}

// handleSealedPaste takes an encrypted quick share the home page's script
// made under the master password: the content and its metadata encrypted
// under a new content key, that key wrapped by the account key and by the
// link's key, and the link's key - or its code - wrapped by the account key
// for its owner. It is the browser's endpoint only: a signed-in session, its
// CSRF token and the account's setting on. The answer is the result page's
// address; the page adds the link's key to it on its side.
func (a *App) handleSealedPaste(w http.ResponseWriter, r *http.Request) {
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
	limit := int64(store.AnonymousMaxBytes + 64<<10)
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := r.ParseMultipartForm(limit); err != nil {
		writePlainError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf(translate(locale, "e2ee_too_large"), store.BytesText(store.AnonymousMaxBytes)))
		return
	}
	defer r.MultipartForm.RemoveAll()
	if !a.checkCSRF(r, sessionID) {
		writePlainError(w, http.StatusForbidden, translate(locale, "e2ee_page_stale"))
		return
	}
	if has, err := a.hasKeyring(r, user.ID); err != nil || !has {
		writePlainError(w, http.StatusForbidden, translate(locale, "e2ee_turned_off"))
		return
	}
	ttl, _ := parsePasteTTL(r.FormValue("ttl"))
	resource, _, err := a.db.CreateSealedQuickShare(r.Context(), user.ID, r.FormValue("id"), r.FormValue("link_id"),
		store.SealedPart{Content: a.part(r, "content"), Meta: base64Field(r, "meta")},
		base64Field(r, "sealed_key"), base64Field(r, "link_key"), base64Field(r, "owner_key"), ttl, time.Now().UTC())
	if err != nil {
		if !store.IsRefusal(err) && !errors.Is(err, store.ErrNotFound) {
			fmt.Fprintf(os.Stderr, "create sealed quick share: %v\n", err)
			writePlainError(w, http.StatusInternalServerError, translate(locale, "e2ee_failed"))
			return
		}
		writePlainError(w, http.StatusBadRequest, localizePageError(locale, err.Error()))
		return
	}
	a.rememberE2EE(r, user.ID, true)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"result": pasteResultPrefix + resource.ID})
}

// rememberE2EE keeps the choice the box was last sent with, for its switch
// to start at next time. It is written only when it changes; failing to
// remember costs nothing but the default.
func (a *App) rememberE2EE(r *http.Request, userID string, on bool) {
	current, err := a.db.E2EEEnabled(r.Context(), userID)
	if err == nil && current == on {
		return
	}
	if err := a.db.SetE2EE(r.Context(), userID, on); err != nil {
		fmt.Fprintf(os.Stderr, "remember e2ee choice: %v\n", err)
	}
}
