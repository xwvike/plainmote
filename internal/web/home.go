package web

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"plainmote/internal/store"
)

// pastePath is where the open endpoint lives. It is its own path rather than a
// POST to "/" so the one unauthenticated write in the service can be rate
// limited at the edge by a single rule, without touching the reads.
const pastePath = "/paste"

// pasteTTLChoices are the lifetimes the page offers. Minutes only: this is a
// handoff, and store.AnonymousMaxTTL refuses anything longer whatever arrives.
var pasteTTLChoices = []ttlChoice{
	{Value: "1", Label: "1 分钟"},
	{Value: "5", Label: "5 分钟"},
	{Value: "10", Label: "10 分钟"},
	{Value: "30", Label: "30 分钟"},
}

// pasteFormMaxBytes is what the handler will read at all. The store enforces
// the real limit on the body; this one is about not reading a request that
// cannot possibly be within it.
const pasteFormMaxBytes = store.AnonymousMaxBytes + 8<<10

func (a *App) handleHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writePlainError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	a.renderHome(w, r, a.homePage(r), http.StatusOK)
}

// homePage is the page as it stands with nothing submitted. The account, if
// there is one, only changes the top bar and one line of copy: what the box
// does is the same either way, and saying so is the point.
func (a *App) homePage(r *http.Request) pageData {
	data := pageData{
		Active:       "home",
		Indexable:    true,
		BaseURL:      a.baseURL(r),
		PasteTTL:     pasteTTLChoices[0].Value,
		PasteChoices: pasteTTLChoices,
		MaxPaste:     store.AnonymousMaxBytes,
	}
	if user, _, ok := a.currentUser(r); ok {
		data.User, data.SignedIn, data.CSRF = user, true, csrfValue(r)
	}
	return data
}

func (a *App) renderHome(w http.ResponseWriter, r *http.Request, data pageData, status int) {
	a.renderTemplate(w, status, "home.html", data)
}

func (a *App) handlePaste(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.AnonymousEnabled {
		writePlainError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// A browser tells us when a form came from somewhere else. There is no
	// session here to hang a CSRF token off, and this is the stateless form of
	// the same check: it stops another site driving visitors' browsers into
	// this endpoint, which would spread writes across addresses the edge is
	// rate limiting one by one. A request with no Origin - curl, an old
	// browser - is not what that attack looks like, and is let through.
	if !a.sameOriginPost(r) {
		writePlainError(w, http.StatusForbidden, "cross-site post")
		return
	}
	if r.ContentLength > pasteFormMaxBytes {
		a.refusePaste(w, r, "", "", fmt.Sprintf("内容最大 %s", store.BytesText(store.AnonymousMaxBytes)))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, pasteFormMaxBytes)
	if err := r.ParseForm(); err != nil {
		a.refusePaste(w, r, "", "", fmt.Sprintf("内容最大 %s", store.BytesText(store.AnonymousMaxBytes)))
		return
	}

	content := r.FormValue("content")
	filename := strings.TrimSpace(r.FormValue("filename"))
	ttl, ttlValue := parsePasteTTL(r.FormValue("ttl"))

	resource, link, err := a.db.CreateAnonymousPaste(r.Context(), filename, []byte(content), ttl, time.Now().UTC())
	if err != nil {
		if errors.Is(err, store.ErrInternal) {
			fmt.Fprintf(os.Stderr, "create paste: %v\n", err)
			a.refusePasteWith(w, r, content, filename, ttlValue, "服务暂时无法完成这次操作，请稍后再试。", http.StatusInternalServerError)
			return
		}
		a.refusePasteWith(w, r, content, filename, ttlValue, err.Error(), http.StatusBadRequest)
		return
	}

	data := a.homePage(r)
	data.PasteURL = a.baseURL(r) + shareAddress(link.Token, deliveryFilename(resource, resource.ContentType))
	data.PasteExpires = *link.ExpiresAt
	data.PasteFilename = resource.Filename
	data.PasteSize = resource.ContentSize
	a.renderHome(w, r, data, http.StatusOK)
}

// refusePaste hands the submission back. A refused paste that comes back with
// an empty box has thrown away what the visitor wrote, and they have nowhere
// else to get it from.
func (a *App) refusePaste(w http.ResponseWriter, r *http.Request, content, filename, message string) {
	a.refusePasteWith(w, r, content, filename, pasteTTLChoices[0].Value, message, http.StatusBadRequest)
}

func (a *App) refusePasteWith(w http.ResponseWriter, r *http.Request, content, filename, ttl, message string, status int) {
	data := a.homePage(r)
	data.Error = message
	data.PasteContent = content
	data.PasteFilename = filename
	data.PasteTTL = ttl
	a.renderHome(w, r, data, status)
}

// parsePasteTTL reads the picker. Anything unrecognised becomes the default
// rather than an error: the store is what enforces the range, and a visitor
// should not lose a paste to a value they never chose.
func parsePasteTTL(value string) (time.Duration, string) {
	for _, choice := range pasteTTLChoices {
		if choice.Value == value {
			minutes, _ := strconv.Atoi(choice.Value)
			return time.Duration(minutes) * time.Minute, choice.Value
		}
	}
	return store.AnonymousDefaultTTL, pasteTTLChoices[0].Value
}

func (a *App) sameOriginPost(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" || origin == "null" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(parsed.Host, r.Host)
}
