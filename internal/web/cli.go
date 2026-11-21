package web

import (
	"errors"
	"net/http"
	"time"

	"plainmote/internal/store"
)

// handleDevicePage is where an HTTP API device authorization (RFC 8628) is
// approved: a user code is typed in, the request it belongs to is shown, and
// the account owner allows or denies it. It needs a signed-in session, and
// every step that changes something is a CSRF-checked POST.
func (a *App) handleDevicePage(w http.ResponseWriter, r *http.Request) {
	user, sessionID, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	data := a.basePage(r, user)
	data.Active = "account"
	data.DeviceStep = "code"
	render := func(status int) { a.renderTemplate(w, r, status, "device.html", data) }
	if r.Method == http.MethodGet {
		// A code in the address is ignored on purpose. Typing it is the step
		// that makes sure the person approving is the one who saw it.
		render(http.StatusOK)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !a.checkCSRF(r, sessionID) {
		writePlainError(w, http.StatusForbidden, "invalid csrf token")
		return
	}
	locale := requestLanguage(r).Locale
	now := time.Now().UTC()
	ip := a.clientIP(r)
	refuse := func() {
		a.limits.codeFailures.allow(user.ID, now)
		a.limits.codeAddress.allow(addressKey(ip), now)
		data.Error = translate(locale, "device_code_invalid")
		render(http.StatusBadRequest)
	}
	if a.limits.codeFailures.blocked(user.ID, now) || a.limits.codeAddress.blocked(addressKey(ip), now) {
		data.Error = translate(locale, "device_code_locked")
		render(http.StatusTooManyRequests)
		return
	}
	code, valid := store.NormalizeUserCode(r.FormValue("code"))
	if !valid {
		refuse()
		return
	}
	switch r.FormValue("action") {
	case "lookup":
		grant, err := a.db.PendingDeviceGrant(r.Context(), code, now)
		if errors.Is(err, store.ErrNotFound) {
			refuse()
			return
		}
		if err != nil {
			a.serverError(w, "read device grant", err)
			return
		}
		data.DeviceStep, data.DeviceGrant, data.DeviceCode = "confirm", grant, store.FormatUserCode(code)
		render(http.StatusOK)
	case "approve", "deny":
		approve := r.FormValue("action") == "approve"
		grant, err := a.db.PendingDeviceGrant(r.Context(), code, now)
		if err == nil && grant.ID == r.FormValue("grant") {
			err = a.db.DecideDeviceGrant(r.Context(), user.ID, grant.ID, code, approve, now)
		} else if err == nil {
			err = store.ErrNotFound
		}
		if errors.Is(err, store.ErrNotFound) {
			refuse()
			return
		}
		if err != nil {
			a.serverError(w, "decide device grant", err)
			return
		}
		data.DeviceGrant = grant
		data.DeviceStep = "denied"
		if approve {
			data.DeviceStep = "done"
		}
		render(http.StatusOK)
	default:
		writePlainError(w, http.StatusBadRequest, "unknown action")
	}
}
