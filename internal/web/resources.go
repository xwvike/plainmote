package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"plainmote/internal/store"
)

// The terms a new share starts with. The lifetime is what limits it, not a use
// count: a link sent through a chat app is fetched first by that app's preview
// crawler, so a single-use default was spent before the recipient ever saw it.
// An hour keeps a forwarded link from staying useful for long, and lands on a
// preset the settings dialog can show as selected. Both are one click away
// from being changed.
const (
	defaultShareTTL  = time.Hour
	defaultShareUses = 0
)

func (a *App) handleResources(w http.ResponseWriter, r *http.Request) {
	user, sessionID, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/resources/")
	if path == "new" || path == "new/" {
		a.handleNewResource(w, r, user, sessionID)
		return
	}
	if path == sealedPath {
		a.handleSealedIndex(w, r, user, sessionID)
		return
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		a.handleDashboard(w, r, user)
		return
	}
	resourceID := parts[0]
	switch {
	case len(parts) == 1:
		a.handleResource(w, r, user, sessionID, resourceID)
	case len(parts) == 2 && parts[1] == "raw":
		a.handleRawPreview(w, r, user, resourceID)
	case len(parts) == 2 && parts[1] == "share":
		a.handleShare(w, r, user, sessionID, resourceID)
	case len(parts) == 2 && parts[1] == sealedPath:
		a.handleSealedAction(w, r, user, sessionID, resourceID)
	case len(parts) >= 2 && parts[1] == "versions":
		a.handleVersions(w, r, user, sessionID, resourceID, parts[2:])
	default:
		writePlainError(w, http.StatusNotFound, "not found")
	}
}

// handleNewResource renders the same page as an existing resource, with empty
// fields: creating and editing are one screen.
// Resources come in two kinds, and which one you are making is settled by the
// button you pressed rather than by a control inside the form: an upload keeps
// a copy here, a remote link keeps only a reference.
// A resource is either kept here or referenced elsewhere. Uploading is not a
// third kind: a chosen file is read into the editor and saved from there, the
// same as text that was typed, so it is a way of filling the box rather than a
// thing to decide before opening it. "upload" is still accepted on the way in,
// because links to the old form exist.
const (
	kindLocal  = "local"
	kindRemote = "remote"
)

// actionPreview asks the server to read the address and show what comes back,
// and to do nothing else. Pulling an upstream to look at it is not the same
// intent as keeping it, and on the new-resource screen there is nothing to
// keep yet - the form has not been filled in.
const actionPreview = "preview"

// actionDelete removes the resource for good. It is reached from a confirm
// dialog rather than the save bar, because nothing here undoes it.
const actionDelete = "delete"

// actionKeep makes a quick share an ordinary resource.
const actionKeep = "keep"

func newResourceKind(value string) string {
	if value == kindRemote {
		return kindRemote
	}
	return kindLocal
}

func (a *App) handleNewResource(w http.ResponseWriter, r *http.Request, user User, sessionID string) {
	if r.Method == http.MethodGet {
		data := a.basePage(r, user)
		data.IsNew = true
		data.NewKind = newResourceKind(r.URL.Query().Get("kind"))
		var err error
		if data.HasKeyring, err = a.hasKeyring(r, user.ID); err != nil {
			a.renderError(w, http.StatusInternalServerError, err)
			return
		}
		a.renderTemplate(w, r, http.StatusOK, "resource.html", data)
		return
	}
	if r.Method != http.MethodPost || !a.checkCSRF(r, sessionID) {
		writePlainError(w, http.StatusForbidden, "invalid request")
		return
	}
	form, err := readResourceForm(w, r, a.cfg.MaxContent)
	if err == nil && r.FormValue("action") == actionPreview {
		data := a.basePage(r, user)
		data.IsNew = true
		data.NewKind = newResourceKind(r.FormValue("kind"))
		data.Resource = Resource{Name: form.Name, Filename: form.Filename, OriginURL: form.OriginURL}
		a.previewUpstream(r.Context(), &data, form.OriginURL)
		a.renderTemplate(w, r, http.StatusOK, "resource.html", data)
		return
	}
	if err == nil {
		var resource Resource
		resource, err = a.db.CreateResource(r.Context(), user.ID, form.Name, form.Filename, form.Content, form.ContentEncoding, form.OriginURL)
		if err == nil {
			http.Redirect(w, r, "/resources/"+resource.ID+"?created=1", http.StatusSeeOther)
			return
		}
	}
	text, status := a.writeErrorText("create resource", err)
	body, recovered := editableText(form.Content, form.Filename, form.ContentEncoding)
	if form.Uploaded && !recovered {
		// The only body that cannot be handed back. Saying so beats a page
		// that silently comes back without the file that was just read.
		text += "（该内容无法在页面中保留，请重新选择文件）"
	}
	data := a.basePageWithError(r, user, text)
	data.IsNew = true
	data.NewKind = newResourceKind(r.FormValue("kind"))
	data.Resource = Resource{Name: form.Name, Filename: form.Filename, ContentSize: int64(len(form.Content)), ContentEncoding: form.ContentEncoding, OriginURL: form.OriginURL}
	data.ContentEncoding = form.ContentEncoding
	data.ContentEOL = form.ContentEOL
	data.ContentText = body
	a.renderTemplate(w, r, status, "resource.html", data)
}

// editableText is what a refused save can put back in the editor. A browser
// cannot refill a file input, so a page that comes back without the body has
// thrown the upload away; anything that decodes as text goes back, and the
// caller is told when it could not.
func editableText(content []byte, filename, encoding string) (string, bool) {
	if len(content) == 0 {
		return "", false
	}
	text, encodingName, err := store.DecodeText(content, encoding)
	if err != nil {
		return "", false
	}
	if contentType, _ := store.DetectContent(filename, content, encodingName); !store.TextLike(contentType) {
		return "", false
	}
	return text, true
}

// resourceForm is one submission of the resource screen. An uploaded file wins
// over the textarea, so picking a file and saving replaces the content.
type resourceForm struct {
	Name      string
	Filename  string
	OriginURL string
	Content   []byte
	// ContentEncoding is the source file encoding selected or detected in the
	// editor. Uploaded bytes already use it; textarea content is converted to
	// it before reaching the store.
	ContentEncoding string
	// ContentEOL is the line ending the body is written with. It is not stored:
	// it is detected from the bytes whenever the page is rendered, so there is
	// nothing to migrate and nothing that can go stale against the content.
	ContentEOL string
	Uploaded   bool

	// ContentGiven separates "the editor sent an empty box" from "this form
	// had no content field at all". A binary resource is edited through a
	// preview, not a textarea, so saving its name must leave the bytes alone
	// rather than read the absent field as a deletion.
	ContentGiven bool
}

func readResourceForm(w http.ResponseWriter, r *http.Request, maxBytes int64) (resourceForm, error) {
	var form resourceForm
	limit := maxBytes + 64*1024
	if r.ContentLength > limit {
		return form, errors.New("content is too large")
	}
	// The length check above only sees a declared length. A chunked request
	// declares none, and ParseMultipartForm spills whatever does not fit in
	// memory to a temporary file with no ceiling of its own - on /tmp, which
	// is tmpfs in the production container. Bounding the reader bounds both.
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	multipartForm := strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data")
	if multipartForm {
		if err := r.ParseMultipartForm(maxBytes + 1); err != nil {
			return form, fmt.Errorf("read upload: %w", err)
		}
		defer func() {
			if r.MultipartForm != nil {
				_ = r.MultipartForm.RemoveAll()
			}
		}()
	}
	form = resourceForm{
		Name:            r.FormValue("name"),
		Filename:        strings.TrimSpace(r.FormValue("filename")),
		OriginURL:       strings.TrimSpace(r.FormValue("origin_url")),
		Content:         []byte(r.FormValue("content")),
		ContentEncoding: strings.TrimSpace(r.FormValue("content_encoding")),
		ContentEOL:      store.NormalizeEOLName(r.FormValue("content_eol")),
		ContentGiven:    r.Form["content"] != nil,
	}
	// Only a multipart submission can carry a file; anything else simply has
	// none, which is not an error.
	if multipartForm {
		if uploaded, uploadedAs, err := readUploadedFile(r, maxBytes); err != nil {
			return form, err
		} else if uploaded != nil {
			form.Content = uploaded
			form.ContentGiven = true
			form.Uploaded = true
			form.Filename = store.RefitFilename(form.Filename, uploadedAs, uploaded)
		}
	}
	if form.OriginURL == "" && form.ContentGiven && !form.Uploaded {
		// Line endings first: the submitted value has been through a textarea,
		// which the browser normalises to CRLF on the way out no matter what
		// the file actually used.
		body := store.ApplyEOL(string(form.Content), form.ContentEOL)
		encoded, encodingName, err := store.EncodeText(body, form.ContentEncoding)
		if err != nil {
			return form, err
		}
		form.Content = encoded
		form.ContentEncoding = encodingName
	}
	if form.OriginURL == "" && int64(len(form.Content)) > maxBytes {
		return form, errors.New("content is too large")
	}
	return form, nil
}

// readUploadedFile returns the chosen file's bytes and the name it was
// chosen under.
func readUploadedFile(r *http.Request, maxBytes int64) ([]byte, string, error) {
	file, header, err := r.FormFile("upload")
	if errors.Is(err, http.ErrMissingFile) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("read upload: %w", err)
	}
	defer file.Close()
	if header.Size > maxBytes {
		return nil, "", errors.New("uploaded file is too large")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("read upload: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, "", errors.New("uploaded file is too large")
	}
	if len(data) == 0 {
		return nil, "", nil
	}
	return data, uploadName(header.Filename), nil
}

func (a *App) handleResource(w http.ResponseWriter, r *http.Request, user User, sessionID, resourceID string) {
	resource, err := a.db.ResourceForOwner(r.Context(), user.ID, resourceID)
	if errors.Is(err, store.ErrNotFound) {
		writePlainError(w, http.StatusNotFound, "resource not found")
		return
	}
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	if r.Method == http.MethodPost {
		if !a.checkCSRF(r, sessionID) {
			writePlainError(w, http.StatusForbidden, "invalid csrf token")
			return
		}
		if r.FormValue("action") == actionDelete {
			if err := a.db.DeleteResource(r.Context(), user.ID, resourceID); err != nil {
				text, status := a.writeErrorText("delete resource", err)
				a.renderResourcePage(w, r, user, resource, text, status, nil, nil)
				return
			}
			http.Redirect(w, r, dashboardPath, http.StatusSeeOther)
			return
		}
		if r.FormValue("action") == actionKeep {
			if err := a.db.KeepQuickShare(r.Context(), user.ID, resourceID, time.Now().UTC()); err != nil {
				if errors.Is(err, store.ErrNotFound) {
					writePlainError(w, http.StatusNotFound, "resource not found")
					return
				}
				text, status := a.writeErrorText("keep quick share", err)
				a.renderResourcePage(w, r, user, resource, text, status, nil, nil)
				return
			}
			http.Redirect(w, r, "/resources/"+resourceID+"?kept=1", http.StatusSeeOther)
			return
		}
		// Deleting and keeping are all a quick share takes. The store refuses
		// the rest too; this keeps a stale form from reaching it.
		if resource.QuickShare() {
			a.renderResourcePage(w, r, user, resource, translate(requestLanguage(r).Locale, "quick_share_read_only"), http.StatusBadRequest, nil, nil)
			return
		}
		form, err := readResourceForm(w, r, a.cfg.MaxContent)
		if err != nil {
			a.renderResourcePage(w, r, user, resource, err.Error(), http.StatusBadRequest, nil, nil)
			return
		}
		if r.FormValue("action") == actionPreview {
			// Render against what is in the form rather than what is stored, so
			// a pasted address can be read before deciding to keep it. The
			// stored row is untouched either way.
			pending := resource
			pending.Name, pending.Filename, pending.OriginURL = form.Name, form.Filename, form.OriginURL
			a.renderResourcePage(w, r, user, pending, "", http.StatusOK, nil, nil)
			return
		}
		content := form.Content
		if !form.ContentGiven {
			// A nil body tells the store to retain the current object.
			content = nil
		}
		base := versionNumber(r.FormValue("base_version"))
		result, err := a.db.SaveResource(r.Context(), user.ID, resourceID, store.ResourceEdit{
			Name: form.Name, Filename: form.Filename, Content: content,
			ContentEncoding: form.ContentEncoding, OriginURL: form.OriginURL, BaseVersion: base,
		})
		if conflict := (*store.VersionConflict)(nil); errors.As(err, &conflict) {
			// Nothing was written, and nothing is lost: the page comes back
			// with the submission in the editor, says what happened, and the
			// next save is made against the version that is current - which
			// is how "save anyway" works, with no flag of its own.
			pending := resource
			pending.Name, pending.Filename = form.Name, form.Filename
			pending.ContentEncoding = form.ContentEncoding
			a.renderResourcePage(w, r, user, pending, "", http.StatusConflict, content, func(data *pageData) {
				data.Conflict, data.ConflictBase = conflict, base
			})
			return
		}
		if err != nil {
			text, status := a.writeErrorText("update resource", err)
			// The refused page comes back carrying the submission, not the
			// stored row: the fields as they were typed, and the body as it
			// was sent. Bytes that cannot go in a textarea are the one thing
			// that cannot come back, and the page says so instead of quietly
			// showing the old version in their place.
			pending := resource
			pending.Name, pending.Filename, pending.OriginURL = form.Name, form.Filename, form.OriginURL
			pendingBody := content
			if pendingBody != nil {
				pending.ContentEncoding = form.ContentEncoding
				if _, ok := editableText(pendingBody, form.Filename, form.ContentEncoding); !ok {
					pendingBody = nil
					text += "（该内容无法在页面中保留，请重新选择文件）"
				}
			}
			a.renderResourcePage(w, r, user, pending, text, status, pendingBody, nil)
			return
		}
		// saved=0 is a save that changed only the name or the address: the
		// content, and so the version, stayed where it was.
		values := url.Values{"saved": {"0"}}
		if result.NewVersion {
			values.Set("saved", strconv.Itoa(result.Version))
		}
		if result.Trimmed > 0 {
			values.Set("trimmed", strconv.Itoa(result.Trimmed))
		}
		http.Redirect(w, r, "/resources/"+resourceID+"?"+values.Encode(), http.StatusSeeOther)
		return
	}
	if r.Method != http.MethodGet {
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	a.renderResourcePage(w, r, user, resource, r.URL.Query().Get("error"), http.StatusOK, nil, nil)
}

// writeErrorText is what the person whose request failed is told, and with
// which status. A refusal - about their own request: a bad field, a quota
// reached - is told in full. Anything else is the service failing, however it
// got here, and its text, which can name tables, hosts and object keys, goes
// to stderr behind one sentence. Nothing is shown merely because nobody
// marked it internal.
func (a *App) writeErrorText(what string, err error) (string, int) {
	if store.IsRefusal(err) {
		return err.Error(), http.StatusBadRequest
	}
	fmt.Fprintf(os.Stderr, "%s: %v\n", what, err)
	return serviceFailure, http.StatusInternalServerError
}

// serviceFailure is what a failure of the service is called on a page.
const serviceFailure = "服务暂时无法完成该操作，请稍后重试。"

// pendingBody is the body the user just submitted, for a save that was
// refused: their work only exists in that request, so re-reading the stored
// object would quietly replace it with the version they were editing away
// from. nil means there is nothing pending and the stored object is the truth.
//
// adjust, when given, sets whatever else the page is to say before it renders.
func (a *App) renderResourcePage(w http.ResponseWriter, r *http.Request, user User, resource Resource, pageError string, status int, pendingBody []byte, adjust func(*pageData)) {
	now := time.Now().UTC()
	shares, ended, err := a.listShares(r.Context(), user.ID, resource.ID, now)
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	data := a.basePage(r, user)
	data.Error = pageError
	// A request from parts.js renders everything but the editor, so it does
	// not read the body - except for a resource with no filename, whose body
	// or upstream type is what names it in the addresses of its links.
	parts := wantsParts(r)
	if !resource.Remote() {
		if data.HistoryCount, err = a.db.HistoryCount(r.Context(), user.ID, resource.ID); err != nil {
			a.renderError(w, http.StatusInternalServerError, err)
			return
		}
	}
	query := r.URL.Query()
	// The confirmations are read from the address the action redirected to,
	// and only believed while they are still true: a restore notice for
	// content that has since been saved over would be wrong.
	if restored := versionNumber(query.Get("restored")); restored > 0 && restored == resource.RestoredFrom {
		data.RestoredFrom = restored
		if resource.Version > 1 {
			if _, err := a.db.VersionForOwner(r.Context(), user.ID, resource.ID, resource.Version-1); err == nil {
				data.UndoVersion = resource.Version - 1
			}
		}
	}
	// A creation is believed while nothing has been saved over it, and a
	// numbered save while its version is still the current one.
	data.Created = query.Get("created") == "1" && resource.Version == 1
	if query.Has("saved") && data.RestoredFrom == 0 {
		if saved := versionNumber(query.Get("saved")); saved == 0 || (saved == resource.Version && !resource.Remote()) {
			data.Saved, data.SavedVersion = true, saved
		}
	}
	data.Trimmed = versionNumber(query.Get("trimmed"))
	data.Kept = query.Get("kept") == "1" && !resource.QuickShare()
	data.Sealed = query.Get("sealed") == "1" && resource.Sealed()
	data.Unsealed = query.Get("unsealed") == "1" && !resource.Sealed()
	if !parts {
		if data.HasKeyring, err = a.hasKeyring(r, user.ID); err != nil {
			a.renderError(w, http.StatusInternalServerError, err)
			return
		}
		if !resource.Remote() && !resource.QuickShare() {
			if data.HistoryJSON, err = a.historyJSON(r, user.ID, resource.ID); err != nil {
				a.renderError(w, http.StatusInternalServerError, err)
				return
			}
		}
	}
	// Reconsider only old opaque local rows with an explicitly textual name.
	// A known image whose filename happens to end in .txt, and remote resources
	// with no stored body, must keep their existing handling.
	recoverOpaqueText := !resource.Remote() && resource.ContentType == "application/octet-stream" && store.TextFilename(resource.Filename)
	if (resource.Editable() || recoverOpaqueText) && (!parts || resource.Filename == "") {
		text := pendingBody
		if text == nil {
			var err error
			text, err = a.db.ReadContent(r.Context(), resource)
			if err != nil {
				a.renderError(w, http.StatusInternalServerError, err)
				return
			}
		}
		decoded, encodingName, decodeErr := store.DecodeText(text, resource.ContentEncoding)
		if decodeErr == nil {
			contentType, detectedEncoding := store.DetectContent(resource.Filename, text, encodingName)
			if store.TextLike(contentType) {
				resource.ContentType = contentType
				resource.ContentEncoding = detectedEncoding
				data.ContentText = decoded
				data.ContentEOL = store.DetectEOL(decoded)
			}
		} else {
			resource.ContentType = "application/octet-stream"
			resource.ContentEncoding = ""
		}
	}
	data.Resource = resource
	data.ContentEncoding = resource.ContentEncoding

	// The type a request would be served with decides what an unnamed
	// resource is called in its own address, so it has to be known before the
	// links are rendered. For a remote resource that means asking upstream,
	// which is the same fetch the preview needs.
	servedType := resource.ContentType
	if resource.Remote() && (!parts || resource.Filename == "") {
		if fetched := a.previewUpstream(r.Context(), &data, resource.OriginURL); fetched != "" {
			servedType = fetched
		}
	}

	data.Shares = buildShareViews(data.BaseURL, resource, servedType, shares, now)
	data.EndedShares = buildShareViews(data.BaseURL, resource, servedType, ended, now)

	// The list is on the page, so the only thing left to open is one share's
	// terms. A stale id opens nothing rather than an empty dialog.
	if focusID := strings.TrimSpace(r.URL.Query().Get("share")); focusID != "" {
		for _, view := range data.Shares {
			if view.Link.ID == focusID && !view.Link.Unreadable {
				data.FocusShare = view
				data.ShareOpen = true
				break
			}
		}
	} else if r.URL.Query().Get("delete") != "" {
		data.DeleteOpen = true
	}
	if adjust != nil {
		adjust(&data)
	}

	a.renderTemplate(w, r, status, "resource.html", data)
}

// Links that stopped working stay on the page for a week, the latest few, so
// the owner sees a revocation or an expiry land; older ones are in the access
// history.
const (
	endedShareWindow = 7 * 24 * time.Hour
	endedShareLimit  = 5
)

func (a *App) listShares(ctx context.Context, userID, resourceID string, now time.Time) (live, ended []Link, err error) {
	if live, err = a.db.ListShares(ctx, userID, resourceID, now); err != nil {
		return nil, nil, err
	}
	ended, err = a.db.ListEndedShares(ctx, userID, resourceID, now, now.Add(-endedShareWindow), endedShareLimit)
	return live, ended, err
}

func buildShareViews(base string, resource Resource, servedType string, shares []Link, now time.Time) []linkView {
	views := make([]linkView, 0, len(shares))
	for _, share := range shares {
		ttlChoice, ttlCustom := shareTTLForm(share, now)
		address := ""
		if !share.Unreadable {
			address = base + shareAddress(share.Token, deliveryFilename(resource, servedType))
		}
		ended, endedAt := share.Ending(now)
		views = append(views, linkView{
			Link:      share,
			URL:       address,
			TTLChoice: ttlChoice,
			TTLCustom: ttlCustom,
			Ended:     ended,
			EndedAt:   endedAt,
		})
	}
	return views
}

// shareTTLForm maps the stored deadline back to the choices in the settings
// dialog: a stop on the lifetime scale when the terms were one, the custom
// box with what is left otherwise.
func shareTTLForm(link Link, now time.Time) (choice, custom string) {
	if link.ExpiresAt == nil {
		return "never", ""
	}
	// Counted from when the terms began, not from the link's creation: a link
	// whose terms were changed to 24 hours is a 24-hour link.
	if value, ok := lifetimeValue(link.ExpiresAt.Sub(link.TermsAt)); ok {
		return value, ""
	}
	return "custom", shareDurationInput(link.ExpiresAt.Sub(now))
}

// shareDurationInput writes what is left as something a person reads and can
// type back: days, hours, minutes, and seconds only under a minute - "5d12h44m",
// not "477827s". Rounded up, so re-applying it never shortens the link.
func shareDurationInput(duration time.Duration) string {
	if duration <= 0 {
		return ""
	}
	unit := time.Second
	if duration >= time.Minute {
		unit = time.Minute
	}
	if remainder := duration % unit; remainder != 0 {
		duration += unit - remainder
	}
	var text strings.Builder
	for _, part := range []struct {
		size time.Duration
		mark string
	}{{24 * time.Hour, "d"}, {time.Hour, "h"}, {time.Minute, "m"}, {time.Second, "s"}} {
		if count := duration / part.size; count > 0 {
			text.WriteString(strconv.FormatInt(int64(count), 10) + part.mark)
			duration -= count * part.size
		}
	}
	return text.String()
}

// previewUpstream reads the address exactly as a public request would and fills
// in whatever of it the page can show. Nothing is stored and nothing is cached:
// this is a live read, and it is the only way the owner sees a remote body.
// Returns the type the address would be served as, or "" if it could not be
// read - callers keep their own fallback in that case.
func (a *App) previewUpstream(ctx context.Context, data *pageData, originURL string) string {
	if strings.TrimSpace(originURL) == "" {
		data.UpstreamError = "请先填写远程地址。"
		return ""
	}
	body, contentType, err := a.upstream.Fetch(ctx, originURL)
	if err != nil {
		data.UpstreamError = err.Error()
		return ""
	}
	servedType := store.SafeContentType(contentType)
	data.UpstreamType = servedType
	if store.TextLike(servedType) {
		_, parameters, _ := mime.ParseMediaType(servedType)
		if decoded, _, decodeErr := store.DecodeText(body, parameters["charset"]); decodeErr == nil {
			data.UpstreamText = decoded
			data.UpstreamTextPreview = true
		}
	}
	return servedType
}

// handleRawPreview serves a resource's own bytes to its owner, so the editing
// page can show an image or play a sound without inlining megabytes of base64
// into the HTML. It is signed-in only and never renders as anything a browser
// would execute: the type comes from detection, which cannot produce one, and
// nosniff stops the browser second-guessing it.
func (a *App) handleRawPreview(w http.ResponseWriter, r *http.Request, user User, resourceID string) {
	if r.Method != http.MethodGet {
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	resource, err := a.db.ResourceForOwner(r.Context(), user.ID, resourceID)
	if errors.Is(err, store.ErrNotFound) {
		writePlainError(w, http.StatusNotFound, "resource not found")
		return
	}
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	if resource.Remote() {
		writePlainError(w, http.StatusNotFound, "remote resources have no stored bytes")
		return
	}
	a.serveOwnedBytes(w, r,
		store.ContentTypeWithEncoding(resource.ContentType, resource.ContentEncoding),
		`inline; filename="`+deliveryFilename(resource, resource.ContentType)+`"`,
		resource.ContentSize,
		func() (io.ReadCloser, int64, error) { return a.db.OpenContent(r.Context(), resource) },
		func(start, end int64) (io.ReadCloser, int64, error) {
			return a.db.OpenContentRange(r.Context(), resource, start, end)
		})
}

// serveOwnedBytes sends stored bytes to their owner for a page to show, whole
// or as one byte range - a video cannot be sought without ranges, and Safari
// will not play one at all. The type comes from detection, which never
// produces anything a browser would execute, and nosniff and the sandbox keep
// it that way.
func (a *App) serveOwnedBytes(w http.ResponseWriter, r *http.Request, contentType, disposition string, size int64,
	open func() (io.ReadCloser, int64, error), openRange func(start, end int64) (io.ReadCloser, int64, error)) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", deliveredPolicy(contentType))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Accept-Ranges", "bytes")
	requestedRange, err := parseByteRange(r.Header.Get("Range"), size)
	if err != nil {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		writePlainError(w, http.StatusRequestedRangeNotSatisfiable, "requested range is not satisfiable")
		return
	}
	var body io.ReadCloser
	if requestedRange == nil {
		body, size, err = open()
	} else {
		var opened int64
		body, opened, err = openRange(requestedRange.start, requestedRange.end)
		if err == nil && opened != requestedRange.length() {
			_ = body.Close()
			err = fmt.Errorf("blob range length %d, want %d", opened, requestedRange.length())
		}
	}
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	defer body.Close()
	status := http.StatusOK
	responseSize := size
	if requestedRange != nil {
		status = http.StatusPartialContent
		responseSize = requestedRange.length()
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", requestedRange.start, requestedRange.end, size))
	}
	if responseSize > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(responseSize, 10))
	}
	w.WriteHeader(status)
	_, _ = io.Copy(w, body)
}

func (a *App) handleShare(w http.ResponseWriter, r *http.Request, user User, sessionID, resourceID string) {
	if r.Method != http.MethodPost || !a.checkCSRF(r, sessionID) {
		writePlainError(w, http.StatusForbidden, "invalid request")
		return
	}
	// Every share action lands back on the resource, where the list is, and
	// shows the list, which now holds the result - or the reason it does not.
	// A refused change of terms is the exception: see refuseShareUpdate.
	back := func(message string) {
		target := "/resources/" + resourceID
		if message != "" {
			target += "?" + url.Values{"error": {message}}.Encode()
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
	}
	// The message travels in the address, so a failure of the service itself
	// is named by one sentence; what failed underneath goes to the log.
	refused := func(what string, err error) {
		text, _ := a.writeErrorText(what, err)
		back(text)
	}

	switch r.FormValue("action") {
	case "", "create":
		// A link to an encrypted resource arrives with the keys the browser
		// made for it, under the id they are bound to.
		if r.FormValue("link_id") != "" {
			if err := a.createSealedShare(r, user, resourceID); err != nil {
				refused("create sealed share", err)
				return
			}
			back("")
			return
		}
		// Pressing 分享 mints the link straight away: the dialog it opens is
		// meant to already hold something you can send.
		if _, err := a.db.CreateShare(r.Context(), user.ID, resourceID, strings.TrimSpace(r.FormValue("name")), defaultShareTTL, defaultShareUses); err != nil {
			refused("create share", err)
			return
		}
		back("")
	case "update":
		shareID := r.FormValue("share_id")
		ttl, err := shareTTL(r)
		if err != nil {
			a.refuseShareUpdate(w, r, user, resourceID, shareID, err.Error(), http.StatusBadRequest)
			return
		}
		maxUses, err := shareUses(r)
		if err != nil {
			a.refuseShareUpdate(w, r, user, resourceID, shareID, err.Error(), http.StatusBadRequest)
			return
		}
		if err := a.db.UpdateShare(r.Context(), user.ID, resourceID, shareID, strings.TrimSpace(r.FormValue("name")), ttl, maxUses); err != nil {
			text, status := a.writeErrorText("update share", err)
			a.refuseShareUpdate(w, r, user, resourceID, shareID, text, status)
			return
		}
		back("")
	case "revoke":
		if err := a.db.RevokeLink(r.Context(), user.ID, resourceID, r.FormValue("share_id")); err != nil {
			refused("revoke share", err)
			return
		}
		back("")
	case "delete":
		if err := a.db.DeleteEndedLink(r.Context(), user.ID, resourceID, r.FormValue("share_id"), time.Now().UTC()); err != nil {
			refused("delete share", err)
			return
		}
		back("")
	case "revoke_all":
		if err := a.db.RevokeShares(r.Context(), user.ID, resourceID); err != nil {
			refused("revoke shares", err)
			return
		}
		back("")
	default:
		writePlainError(w, http.StatusBadRequest, "unknown share action")
	}
}

// refuseShareUpdate answers a refused change of terms with the dialog still
// open, holding what was typed and saying what was wrong with it. Redirecting
// would reopen the dialog with the stored terms and lose the entry that was
// being corrected. A share that has gone meanwhile has no dialog to reopen,
// and the message goes on the page instead.
func (a *App) refuseShareUpdate(w http.ResponseWriter, r *http.Request, user User, resourceID, shareID, message string, status int) {
	resource, err := a.db.ResourceForOwner(r.Context(), user.ID, resourceID)
	if errors.Is(err, store.ErrNotFound) {
		writePlainError(w, http.StatusNotFound, "resource not found")
		return
	}
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	uses, usesErr := strconv.Atoi(strings.TrimSpace(r.FormValue("uses")))
	a.renderResourcePage(w, r, user, resource, message, status, nil, func(data *pageData) {
		for _, view := range data.Shares {
			if view.Link.ID != shareID || view.Link.Unreadable {
				continue
			}
			view.Link.Name = r.FormValue("name")
			view.TTLChoice, view.TTLCustom = r.FormValue("ttl"), r.FormValue("ttl_custom")
			if usesErr == nil {
				view.Link.MaxUses = uses
			}
			data.FocusShare, data.ShareOpen, data.DeleteOpen = view, true, false
			return
		}
	})
}

// shareUses reads the use-count radios. A missing value means no limit, which
// is the widest option and so the only safe default for an unset group.
func shareUses(r *http.Request) (int, error) {
	choice := strings.TrimSpace(r.FormValue("uses"))
	if choice == "" {
		return 0, nil
	}
	parsed, err := strconv.Atoi(choice)
	if err != nil {
		return 0, errors.New("使用次数无法识别")
	}
	switch parsed {
	case 0, 1, 2, 10:
		return parsed, nil
	default:
		return 0, errors.New("使用次数无法识别")
	}
}

// shareTTL reads the dialog's preset radios, falling back to the custom box.
// A zero duration means the share never expires.
func shareTTL(r *http.Request) (time.Duration, error) {
	choice := r.FormValue("ttl")
	if choice == "never" {
		return 0, nil
	}
	if stop, ok := lifetimeChoice(choice); ok {
		return stop.Duration, nil
	}
	// A Go duration, as the stops used to be posted.
	if choice != "custom" {
		parsed, err := time.ParseDuration(choice)
		if err != nil || parsed <= 0 {
			return 0, errors.New("存活时长无法识别")
		}
		return parsed, nil
	}
	parsed, err := parseShareDuration(r.FormValue("ttl_custom"))
	if err != nil || parsed <= 0 {
		return 0, errors.New("自定义时长无法识别，单位 s / m / h / d，例如 90m")
	}
	return parsed, nil
}

// dayDuration takes a leading number of days off a duration. Go's
// time.ParseDuration stops at hours, but the presets already offer 7 days, so
// "30d" has to work - and so does "5d12h", which is how the box shows what is
// left of a link.
var dayDuration = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)d(.*)$`)

func parseShareDuration(value string) (time.Duration, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	match := dayDuration.FindStringSubmatch(value)
	if match == nil {
		return time.ParseDuration(value)
	}
	days, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		return 0, err
	}
	total := time.Duration(days * float64(24*time.Hour))
	if match[2] != "" {
		rest, err := time.ParseDuration(match[2])
		if err != nil {
			return 0, err
		}
		total += rest
	}
	return total, nil
}
