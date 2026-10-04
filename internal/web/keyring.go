package web

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"plainmote/internal/store"
)

// accountKeyringPath is the master password's keyring. The browser reads it
// to unlock and writes it when the password is set or changed; everything
// that passes through is wrapped, and unwrapping happens only in the browser.
const accountKeyringPath = "/account/keyring"

type keyringView struct {
	KDF               string `json:"kdf"`
	Iterations        int    `json:"iterations"`
	Salt              string `json:"salt"`
	WrappedByPassword string `json:"wrapped_by_password"`
	WrappedByRecovery string `json:"wrapped_by_recovery"`
	LockMinutes       int    `json:"lock_minutes"`
	Version           int    `json:"version"`
}

func keyringJSON(k store.Keyring) keyringView {
	encode := base64.RawURLEncoding.EncodeToString
	return keyringView{
		KDF: k.KDF, Iterations: k.Iterations, Salt: encode(k.Salt),
		WrappedByPassword: encode(k.WrappedByPassword), WrappedByRecovery: encode(k.WrappedByRecovery),
		LockMinutes: k.LockMinutes, Version: k.Version,
	}
}

// handleAccountKeyring answers the account page's script. A read needs the
// session; every write needs the session, its CSRF token and a request from
// this site. The auto-lock time is also a plain form, so it works without the
// script, and comes back to the account page.
func (a *App) handleAccountKeyring(w http.ResponseWriter, r *http.Request) {
	user, sessionID, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	switch r.Method {
	case http.MethodGet:
		keyring, err := a.db.Keyring(r.Context(), user.ID)
		if errors.Is(err, store.ErrNotFound) {
			a.keyringFail(w, http.StatusNotFound, "not_found", "")
			return
		}
		if err != nil {
			a.serverError(w, "read keyring", err)
			return
		}
		a.writeAPI(w, http.StatusOK, keyringJSON(keyring))
		return
	case http.MethodPost:
	default:
		w.Header().Set("Allow", "GET, POST")
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
	now := time.Now().UTC()
	locale := requestLanguage(r).Locale
	if r.FormValue("action") == "lock" {
		minutes, _ := strconv.Atoi(r.FormValue("lock_minutes"))
		// Only a crafted request names a time the form does not offer; it
		// changes nothing and comes back to the same page.
		if err := a.db.SetKeyringLock(r.Context(), user.ID, minutes); err != nil && !store.IsRefusal(err) && !errors.Is(err, store.ErrNotFound) {
			a.serverError(w, "set keyring lock", err)
			return
		}
		http.Redirect(w, r, accountPath+"#keyring", http.StatusSeeOther)
		return
	}

	decode := func(field string) []byte {
		value, err := base64.RawURLEncoding.DecodeString(r.FormValue(field))
		if err != nil {
			return nil
		}
		return value
	}
	version, _ := strconv.Atoi(r.FormValue("version"))
	iterations, _ := strconv.Atoi(r.FormValue("iterations"))
	var keyring store.Keyring
	var err error
	switch r.FormValue("action") {
	case "create":
		keyring, err = a.db.CreateKeyring(r.Context(), user.ID, store.Keyring{
			KDF: r.FormValue("kdf"), Iterations: iterations, Salt: decode("salt"),
			WrappedByPassword: decode("wrapped_by_password"), WrappedByRecovery: decode("wrapped_by_recovery"),
		}, now)
	case "password":
		keyring, err = a.db.ChangeKeyringPassword(r.Context(), user.ID, version, r.FormValue("kdf"), iterations, decode("salt"), decode("wrapped_by_password"), now)
	case "recovery":
		keyring, err = a.db.ChangeKeyringRecovery(r.Context(), user.ID, version, decode("wrapped_by_recovery"), now)
	default:
		a.keyringFail(w, http.StatusBadRequest, "bad_request", "")
		return
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		a.keyringFail(w, http.StatusNotFound, "not_found", "")
	case errors.Is(err, store.ErrKeyringChanged):
		a.keyringFail(w, http.StatusConflict, "conflict", localizePageError(locale, err.Error()))
	case err != nil:
		text, status := a.writeErrorText("write keyring", err)
		a.keyringFail(w, status, "refused", localizePageError(locale, text))
	default:
		a.writeAPI(w, http.StatusOK, keyringJSON(keyring))
	}
}

func (a *App) keyringFail(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "message": message})
}
