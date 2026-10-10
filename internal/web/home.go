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

// lifetimeChoices is the one scale of lifetimes the service offers: on the
// quick share box, and in a share link's settings, where "never" and a custom
// lifetime follow it. The values are what a terminal sends as ?ttl= too.
var lifetimeChoices = []ttlChoice{
	{Value: "10m", Duration: 10 * time.Minute},
	{Value: "1h", Duration: time.Hour},
	{Value: "1d", Duration: 24 * time.Hour},
	{Value: "7d", Duration: 7 * 24 * time.Hour},
	{Value: "30d", Duration: 30 * 24 * time.Hour},
}

// lifetimeChoice finds the stop on the scale a value names.
func lifetimeChoice(value string) (ttlChoice, bool) {
	for _, choice := range lifetimeChoices {
		if choice.Value == value {
			return choice, true
		}
	}
	return ttlChoice{}, false
}

// lifetimeValue is the stop on the scale a duration is, if it is one.
func lifetimeValue(d time.Duration) (string, bool) {
	for _, choice := range lifetimeChoices {
		if choice.Duration == d {
			return choice.Value, true
		}
	}
	return "", false
}

// pasteDefaultTTL is the preselected choice, the one standing for the store's
// default, so the page and a request that names no lifetime cannot disagree.
var pasteDefaultTTL = func() string {
	if value, ok := lifetimeValue(store.AnonymousDefaultTTL); ok {
		return value
	}
	panic("the default quick share lifetime is not one of the choices")
}()

// pasteFormMaxBytes is what the handler will read at all. The store enforces
// the real limit on the body; this one is about not reading a request that
// cannot possibly be within it. The slack is the rest of a multipart form.
const pasteFormMaxBytes = store.AnonymousMaxBytes + 64<<10

func (a *App) handleHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		writePlainError(w, http.StatusNotFound, "not found")
		return
	}
	a.serveHome(w, r)
}

// serveHome is the home page at / and at each language's own address.
func (a *App) serveHome(w http.ResponseWriter, r *http.Request) {
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
	data := a.homePage(r)
	data.Canonical = "/"
	if route, ok := routeLocale(r); ok {
		data.Canonical = route.prefix + "/"
	}
	data.Alternates = localeAlternates(data.BaseURL, "/")
	a.renderHome(w, r, data, http.StatusOK)
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
		PasteChoices: lifetimeChoices,
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
	form, err := readPasteForm(r)
	if err != nil {
		a.refusePaste(w, r, "", "", tooLarge)
		return
	}
	content, filename := string(form.content), form.filename
	ttl, ttlValue := parsePasteTTL(form.ttl)
	if wantsHTML(r) && !form.uploaded {
		// The page's textarea submits CRLF whatever was typed. What a terminal
		// sends, and a file the page uploads, is exactly the file, and is kept
		// as it is.
		content = store.ApplyEOL(content, "lf")
	}
	// A refusal puts the box back as it was; a file cannot go back into it.
	boxContent := content
	if form.uploaded {
		boxContent = ""
	}

	// Signed in, the quick share is the account's own: listed with its
	// resources and its visits in its access history. A terminal carries no
	// session, so what it sends here is anonymous.
	var resource Resource
	var link Link
	if user, _, ok := a.currentUser(r); ok {
		// The box's text is never stored in the clear because the script
		// that was to encrypt it did not run.
		if form.encrypt {
			a.refusePasteWith(w, r, boxContent, filename, ttlValue, translate(requestLanguage(r).Locale, "e2ee_plaintext_refused"), http.StatusBadRequest)
			return
		}
		// Sent from the box with its switch off: the box starts that way
		// next time.
		if wantsHTML(r) {
			a.rememberE2EE(r, user.ID, false)
		}
		resource, link, err = a.db.CreateQuickShare(r.Context(), user.ID, filename, []byte(content), ttl, time.Now().UTC())
	} else {
		resource, link, err = a.db.CreateAnonymousPaste(r.Context(), filename, []byte(content), ttl, time.Now().UTC())
	}
	if err != nil {
		if !store.IsRefusal(err) {
			fmt.Fprintf(os.Stderr, "create paste: %v\n", err)
			a.refusePasteWith(w, r, boxContent, filename, ttlValue, serviceFailure, http.StatusInternalServerError)
			return
		}
		a.refusePasteWith(w, r, boxContent, filename, ttlValue, err.Error(), http.StatusBadRequest)
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

// pasteForm is one submission to the open endpoint.
type pasteForm struct {
	content  []byte
	filename string
	ttl      string
	// uploaded is content that arrived as a file part. A file's bytes are
	// kept exactly; only what came from the box has its line endings undone.
	uploaded bool
	// encrypt is the box's encryption switch, on: the page's script was to
	// encrypt this and send it elsewhere, and did not.
	encrypt bool
}

// readPasteForm reads the box's own form and what curl sends:
//
//   - a multipart body: the page's, whose chosen file (the file field) wins
//     over the box, or curl's `-F 'content=<-'` (so a log can be piped
//     straight in) and `-F content=@app.log`. A file part's name becomes the
//     filename unless one is given;
//   - a urlencoded form with a content field, from
//     `curl --data-urlencode content@app.log`;
//   - anything else as the content itself, byte for byte. This is what
//     `curl --data-binary @app.log` sends - curl labels it a form, but it has
//     no content field - and what a text/plain body is. Its lifetime and
//     filename come from the query string, since the body has no room for them.
func readPasteForm(r *http.Request) (pasteForm, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return pasteForm{}, err
	}
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	switch mediaType {
	case "multipart/form-data":
		r.Body = io.NopCloser(bytes.NewReader(body))
		if err := r.ParseMultipartForm(pasteFormMaxBytes); err != nil {
			return pasteForm{}, err
		}
		defer r.MultipartForm.RemoveAll()
		form := pasteForm{content: []byte(r.FormValue("content")), filename: strings.TrimSpace(r.FormValue("filename")), ttl: r.FormValue("ttl"), encrypt: r.FormValue("encrypt") != ""}
		// A page with no file chosen still sends the field, empty.
		for _, field := range []string{"file", "content"} {
			files := r.MultipartForm.File[field]
			if len(files) == 0 || files[0].Size == 0 {
				continue
			}
			part, err := files[0].Open()
			if err != nil {
				return pasteForm{}, err
			}
			data, err := io.ReadAll(part)
			part.Close()
			if err != nil {
				return pasteForm{}, err
			}
			form.content, form.uploaded = data, true
			if form.filename == "" {
				form.filename = uploadName(files[0].Filename)
			}
			break
		}
		return form, nil
	case "application/x-www-form-urlencoded":
		if form, err := url.ParseQuery(string(body)); err == nil && form.Has("content") {
			return pasteForm{content: []byte(form.Get("content")), filename: strings.TrimSpace(form.Get("filename")), ttl: form.Get("ttl"), encrypt: form.Get("encrypt") != ""}, nil
		}
	}
	query := r.URL.Query()
	return pasteForm{content: body, filename: strings.TrimSpace(query.Get("filename")), ttl: query.Get("ttl")}, nil
}

// uploadName is the last segment of what a client called its file. Some
// send a whole path, with either separator.
func uploadName(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	return strings.TrimSpace(name)
}

// pasteUsage is what `curl .../paste` prints: every way in, and the limits.
func (a *App) pasteUsage(r *http.Request) string {
	endpoint := a.baseURL(r) + pastePath
	return fmt.Sprintf(`PlainMote quick share: send text or a file, get a link back.

  cmd | curl -F 'content=<-' %[1]s
  curl -F 'content=<app.log' %[1]s
  curl -F content=@app.log %[1]s
  curl --data-binary @app.log '%[1]s?ttl=1d&filename=app.log'

ttl       how long the link works: 10m, 1h, 1d, 7d or 30d (default %[2]s)
filename  name at the end of the link (a form field, or a query parameter
          with --data-binary)

The response is the link, on one line. At most %[3]s, kept byte for byte:
UTF-8 text is served as plain text, images, audio and video as themselves,
anything else as a download.
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
	// A signed-in quick share's result is its creator's alone; an anonymous
	// one's is whoever holds this address.
	user, _, signedIn := a.currentUser(r)
	if signedIn {
		resource, link, err := a.db.QuickShareLink(r.Context(), user.ID, resourceID, now)
		if err == nil {
			data := a.pasteResultPage(r, resource, link)
			data.PasteOwned = true
			a.renderHome(w, r, data, http.StatusOK)
			return
		}
		if !errors.Is(err, store.ErrNotFound) {
			a.serverError(w, "read quick share", err)
			return
		}
	}
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
	if signedIn {
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
	data.PasteEncrypted = resource.Encrypted() || resource.Sealed()
	data.PasteSealed = resource.Sealed()
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

// parsePasteTTL reads the picker, or ?ttl= from a terminal. A bare number is
// minutes, as every lifetime was once, so scripts written then keep working;
// the store refuses one out of range. Anything else unrecognised becomes the
// default rather than an error: a visitor should not lose a paste to a value
// they never chose.
func parsePasteTTL(value string) (time.Duration, string) {
	value = strings.TrimSpace(value)
	if choice, ok := lifetimeChoice(value); ok {
		return choice.Duration, choice.Value
	}
	if minutes, err := strconv.Atoi(value); err == nil && minutes > 0 {
		return time.Duration(minutes) * time.Minute, value
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
