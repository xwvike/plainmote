package web

import (
	"mime"
	"net/http"
	"strconv"
	"strings"
)

// This is representation negotiation, never an authorization decision. The
// shell contains no resource data; its one full-body fetch is counted normally.
func wantsMediaPlayer(r *http.Request) bool {
	if r.URL.Query().Get("raw") == "1" || r.URL.Query().Get("download") == "1" {
		return false
	}
	for _, value := range strings.Split(r.Header.Get("Accept"), ",") {
		kind, params, err := mime.ParseMediaType(strings.TrimSpace(value))
		if err != nil || kind != "text/html" {
			continue
		}
		if q, exists := params["q"]; exists {
			quality, err := strconv.ParseFloat(q, 64)
			if err != nil || !(quality > 0 && quality <= 1) {
				continue
			}
		}
		return true
	}
	return false
}

func (a *App) renderMediaPlayer(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src data:; connect-src 'self'; media-src blob:; base-uri 'none'; frame-ancestors 'none'; form-action 'none'")
	// EscapedPath preserves reserved characters in the decorative filename.
	a.renderTemplate(w, r, http.StatusOK, "media.html", pageData{
		MediaSource:   r.URL.EscapedPath() + "?raw=1",
		MediaAutoSave: browserNavigation(r),
	})
}

// browserNavigation is a person opening the address in a browser, told apart
// by the Fetch Metadata only browsers send. It decides whether the player page
// may start the download by itself when scripts are off. A link-preview
// crawler runs no script either and sends none of these headers; handing it a
// redirect it might follow would spend a use of the link before the recipient
// ever opened it, so it gets the page with the button only.
func browserNavigation(r *http.Request) bool {
	return r.Header.Get("Sec-Fetch-Mode") == "navigate" && r.Header.Get("Sec-Fetch-Dest") == "document"
}
