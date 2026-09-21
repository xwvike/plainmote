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
const pasteResultPrefix = "/paste/"
const pasteSavePath = "/paste/save"

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
	// With the box turned off there is nothing at the root for someone without
	// an account to do, and for someone with one the list is what they came
	// for. A page whose only content is a button to somewhere else is a step,
	// not a destination, and routing past it costs one line here.
	if !a.cfg.AnonymousEnabled {
		if _, _, ok := a.currentUser(r); !ok {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		http.Redirect(w, r, dashboardPath, http.StatusSeeOther)
		return
	}
	a.renderHome(w, r, a.homePage(r), http.StatusOK)
}

// homePage is the page as it stands with nothing submitted. Signing in does
// not change what the box creates; it only lets the result be adopted by the
// account afterward when the person explicitly asks to keep it.
func (a *App) homePage(r *http.Request) pageData {
	data := pageData{
		Active:       "home",
		Indexable:    true,
		BaseURL:      a.baseURL(r),
		SignInURL:    "/login",
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

	creatorID := ""
	if user, _, ok := a.currentUser(r); ok {
		creatorID = user.ID
	}
	resource, _, err := a.db.CreateAnonymousPasteFor(r.Context(), creatorID, filename, []byte(content), ttl, time.Now().UTC())
	if err != nil {
		if errors.Is(err, store.ErrInternal) {
			fmt.Fprintf(os.Stderr, "create paste: %v\n", err)
			a.refusePasteWith(w, r, content, filename, ttlValue, "服务暂时无法完成该操作，请稍后重试。", http.StatusInternalServerError)
			return
		}
		a.refusePasteWith(w, r, content, filename, ttlValue, err.Error(), http.StatusBadRequest)
		return
	}

	http.Redirect(w, r, pasteResultPrefix+resource.ID, http.StatusSeeOther)
}

func (a *App) handlePasteResult(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.AnonymousEnabled {
		writePlainError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	resourceID := strings.TrimPrefix(r.URL.Path, pasteResultPrefix)
	if resourceID == "" || strings.Contains(resourceID, "/") {
		writePlainError(w, http.StatusNotFound, "not found")
		return
	}
	now := time.Now().UTC()
	resource, link, err := a.db.AnonymousPasteResult(r.Context(), resourceID, now)
	if errors.Is(err, store.ErrNotFound) {
		writePlainError(w, http.StatusGone, "临时分享已失效")
		return
	}
	if err != nil {
		a.serverError(w, "read paste result", err)
		return
	}
	data := a.pasteResultPage(r, resource, link)
	data.SignInURL = "/login?next=" + url.QueryEscape(r.URL.RequestURI())
	if user, _, ok := a.currentUser(r); ok {
		if _, _, err := a.db.ClaimableAnonymousPaste(r.Context(), user.ID, resourceID, now); err == nil {
			data.PasteClaimable = true
		} else if !errors.Is(err, store.ErrNotFound) {
			a.serverError(w, "read paste claim", err)
			return
		}
	}
	a.renderHome(w, r, data, http.StatusOK)
}

func (a *App) pasteResultPage(r *http.Request, resource Resource, link Link) pageData {
	data := a.homePage(r)
	data.Indexable = false
	data.PasteURL = a.baseURL(r) + shareAddress(link.Token, deliveryFilename(resource, resource.ContentType))
	data.PasteResourceID = resource.ID
	data.PasteExpires = *link.ExpiresAt
	data.PasteFilename = resource.Filename
	data.PasteSize = resource.ContentSize
	return data
}

func (a *App) handleSavePaste(w http.ResponseWriter, r *http.Request) {
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
	resourceID := strings.TrimSpace(r.FormValue("resource_id"))
	now := time.Now().UTC()
	resource, link, err := a.db.ClaimableAnonymousPaste(r.Context(), user.ID, resourceID, now)
	if errors.Is(err, store.ErrNotFound) {
		writePlainError(w, http.StatusGone, "临时分享已失效，无法保存")
		return
	}
	if err != nil {
		a.serverError(w, "read paste claim", err)
		return
	}
	claimed, err := a.db.ClaimAnonymousPaste(r.Context(), user.ID, resourceID, now)
	if err != nil {
		data := a.pasteResultPage(r, resource, link)
		data.PasteClaimable = true
		if errors.Is(err, store.ErrNotFound) {
			data.Error = "临时分享已失效，无法保存。"
			a.renderHome(w, r, data, http.StatusGone)
			return
		}
		message, status := a.writeErrorText("save quick share", err)
		data.Error = message
		a.renderHome(w, r, data, status)
		return
	}
	http.Redirect(w, r, "/resources/"+claimed.ID, http.StatusSeeOther)
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
