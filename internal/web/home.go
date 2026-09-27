package web

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
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
	{Value: "1"},
	{Value: "5"},
	{Value: "10"},
	{Value: "30"},
}

// pasteDefaultTTL is the preselected choice, taken from the store's default so
// the page and a request that names no lifetime cannot disagree.
var pasteDefaultTTL = strconv.Itoa(int(store.AnonymousDefaultTTL / time.Minute))

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
		PasteTTL:     pasteDefaultTTL,
		PasteChoices: pasteTTLChoices,
		MaxPaste:     store.AnonymousMaxBytes,
	}
	if user, _, ok := a.currentUser(r); ok {
		data.User, data.SignedIn, data.CSRF = user, true, csrfValue(r)
		data.E2EE = a.e2eeEnabled(r, user.ID)
	}
	return data
}

func (a *App) renderHome(w http.ResponseWriter, r *http.Request, data pageData, status int) {
	a.renderTemplate(w, r, status, "home.html", data)
}

func (a *App) handlePaste(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.AnonymousEnabled {
		writePlainError(w, http.StatusNotFound, "not found")
		return
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		// Opened in a terminal, the endpoint explains itself; opened in a
		// browser, the page with the box is where it lives.
		if wantsHTML(r) {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, a.pasteUsage(r))
		}
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, HEAD, POST")
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
	tooLarge := fmt.Sprintf("内容最大 %s", store.BytesText(store.AnonymousMaxBytes))
	if r.ContentLength > pasteFormMaxBytes {
		a.refusePaste(w, r, "", "", tooLarge)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, pasteFormMaxBytes)
	content, filename, ttlChoice, err := readPasteForm(r)
	if err != nil {
		a.refusePaste(w, r, "", "", tooLarge)
		return
	}
	ttl, ttlValue := parsePasteTTL(ttlChoice)
	if wantsHTML(r) {
		// The page's textarea submits CRLF whatever was typed. What a terminal
		// sends is exactly the file, and is kept as it is.
		content = store.ApplyEOL(content, "lf")
	}

	creatorID := ""
	if user, _, ok := a.currentUser(r); ok {
		creatorID = user.ID
		// An account that turned encryption on never has its box's text
		// stored in the clear because a script did not run. A terminal
		// carries no session and is not what this refuses.
		if wantsHTML(r) && a.e2eeEnabled(r, user.ID) {
			a.refusePasteWith(w, r, content, filename, ttlValue, translate(requestLanguage(r).Locale, "e2ee_plaintext_refused"), http.StatusBadRequest)
			return
		}
	}
	resource, link, err := a.db.CreateAnonymousPasteFor(r.Context(), creatorID, filename, []byte(content), ttl, time.Now().UTC())
	if err != nil {
		if errors.Is(err, store.ErrInternal) {
			fmt.Fprintf(os.Stderr, "create paste: %v\n", err)
			a.refusePasteWith(w, r, content, filename, ttlValue, "服务暂时无法完成该操作，请稍后重试。", http.StatusInternalServerError)
			return
		}
		a.refusePasteWith(w, r, content, filename, ttlValue, err.Error(), http.StatusBadRequest)
		return
	}

	if !wantsHTML(r) {
		// A terminal gets the address and nothing else, so it can be piped on
		// or captured with $(...). The result page is for a browser.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, a.baseURL(r)+shareAddress(link.Token, deliveryFilename(resource, resource.ContentType))+"\n")
		return
	}
	http.Redirect(w, r, pasteResultPrefix+resource.ID, http.StatusSeeOther)
}

// readPasteForm reads the box's own form and what curl sends:
//
//   - a multipart body, from `curl -F 'content=<-'` (so a log can be piped
//     straight in) or `curl -F content=@app.log`, whose part name becomes the
//     filename unless one is given;
//   - a urlencoded form with a content field, from the page or
//     `curl --data-urlencode content@app.log`;
//   - anything else as the content itself, byte for byte. This is what
//     `curl --data-binary @app.log` sends - curl labels it a form, but it has
//     no content field - and what a text/plain body is. Its lifetime and
//     filename come from the query string, since the body has no room for them.
func readPasteForm(r *http.Request) (content, filename, ttl string, err error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return "", "", "", err
	}
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch mediaType {
	case "multipart/form-data":
		r.Body = io.NopCloser(bytes.NewReader(body))
		if err := r.ParseMultipartForm(pasteFormMaxBytes); err != nil {
			return "", "", "", err
		}
		defer r.MultipartForm.RemoveAll()
		content, filename, ttl = r.FormValue("content"), strings.TrimSpace(r.FormValue("filename")), r.FormValue("ttl")
		if files := r.MultipartForm.File["content"]; content == "" && len(files) > 0 {
			part, err := files[0].Open()
			if err != nil {
				return "", "", "", err
			}
			defer part.Close()
			data, err := io.ReadAll(part)
			if err != nil {
				return "", "", "", err
			}
			content = string(data)
			if filename == "" {
				filename = path.Base(files[0].Filename)
			}
		}
		return content, filename, ttl, nil
	case "application/x-www-form-urlencoded":
		if form, err := url.ParseQuery(string(body)); err == nil && form.Has("content") {
			return form.Get("content"), strings.TrimSpace(form.Get("filename")), form.Get("ttl"), nil
		}
	}
	query := r.URL.Query()
	return string(body), strings.TrimSpace(query.Get("filename")), query.Get("ttl"), nil
}

// pasteUsage is what `curl .../paste` prints: every way in, and the limits.
func (a *App) pasteUsage(r *http.Request) string {
	endpoint := a.baseURL(r) + pastePath
	return fmt.Sprintf(`PlainMote quick share: send text, get a link back.

  cmd | curl -F 'content=<-' %[1]s
  curl -F 'content=<app.log' %[1]s
  curl -F content=@app.log %[1]s
  curl --data-binary @app.log '%[1]s?ttl=30&filename=app.log'

ttl       minutes until the link expires: 1, 5, 10 or 30 (default %[2]s)
filename  name at the end of the link (a form field, or a query parameter
          with --data-binary)

The response is the link, on one line. The text must be UTF-8, at most %[3]s;
it is kept byte for byte and served as plain text.
`, endpoint, pasteDefaultTTL, legalBytes(store.AnonymousMaxBytes))
}

// wantsHTML tells a browser from a terminal. A browser submitting the form
// always asks for text/html; curl asks for */* and gets plain text.
func wantsHTML(r *http.Request) bool {
	for _, value := range strings.Split(r.Header.Get("Accept"), ",") {
		if mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(value)); err == nil && mediaType == "text/html" {
			return true
		}
	}
	return false
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
		writeLocalizedError(w, r, http.StatusGone, "error_paste_expired")
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
	data.PasteEncrypted = resource.ContentType == store.EncryptedContentType
	data.PasteGauge = gaugeStyle(link.TermsAt, link.ExpiresAt)
	data.PasteEnd = endStyle(link.ExpiresAt)
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
		writeLocalizedError(w, r, http.StatusGone, "error_paste_expired_save")
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
	a.refusePasteWith(w, r, content, filename, pasteDefaultTTL, message, http.StatusBadRequest)
}

func (a *App) refusePasteWith(w http.ResponseWriter, r *http.Request, content, filename, ttl, message string, status int) {
	if !wantsHTML(r) {
		w.Header().Add("Vary", "Accept-Language")
		writePlainError(w, status, localizePageError(requestLanguage(r).Locale, message))
		return
	}
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
	return store.AnonymousDefaultTTL, pasteDefaultTTL
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
