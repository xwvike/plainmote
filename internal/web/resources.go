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
			http.Redirect(w, r, "/resources/"+resource.ID, http.StatusSeeOther)
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
		if uploaded, err := readUploadedFile(r, maxBytes); err != nil {
			return form, err
		} else if uploaded != nil {
			form.Content = uploaded
			form.ContentGiven = true
			form.Uploaded = true
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

func readUploadedFile(r *http.Request, maxBytes int64) ([]byte, error) {
	file, header, err := r.FormFile("upload")
	if errors.Is(err, http.ErrMissingFile) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read upload: %w", err)
	}
	defer file.Close()
	if header.Size > maxBytes {
		return nil, errors.New("uploaded file is too large")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read upload: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, errors.New("uploaded file is too large")
	}
	if len(data) == 0 {
		return nil, nil
	}
	return data, nil
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
				a.renderResourcePage(w, r, user, resource, text, status, nil)
				return
			}
			http.Redirect(w, r, dashboardPath, http.StatusSeeOther)
			return
		}
		form, err := readResourceForm(w, r, a.cfg.MaxContent)
		if err != nil {
			a.renderResourcePage(w, r, user, resource, err.Error(), http.StatusBadRequest, nil)
			return
		}
		if r.FormValue("action") == actionPreview {
			// Render against what is in the form rather than what is stored, so
			// a pasted address can be read before deciding to keep it. The
			// stored row is untouched either way.
			pending := resource
			pending.Name, pending.Filename, pending.OriginURL = form.Name, form.Filename, form.OriginURL
			a.renderResourcePage(w, r, user, pending, "", http.StatusOK, nil)
			return
		}
		content := form.Content
		if !form.ContentGiven {
			// A nil body tells the store to retain the current object.
			content = nil
		}
		if err := a.db.UpdateResource(r.Context(), user.ID, resourceID, form.Name, form.Filename, content, form.ContentEncoding, form.OriginURL); err != nil {
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
			a.renderResourcePage(w, r, user, pending, text, status, pendingBody)
			return
		}
		http.Redirect(w, r, "/resources/"+resourceID, http.StatusSeeOther)
		return
	}
	if r.Method != http.MethodGet {
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	a.renderResourcePage(w, r, user, resource, r.URL.Query().Get("error"), http.StatusOK, nil)
}

// writeErrorText is what the owner of a resource is shown when a write is
// refused. A quota refusal or a bad field is about their own request and says
// so in full; anything else is the service failing, and its text names database
// relations and object keys, so it goes to stderr with a status to match.
func (a *App) writeErrorText(what string, err error) (string, int) {
	if errors.Is(err, store.ErrInternal) {
		fmt.Fprintf(os.Stderr, "%s: %v\n", what, err)
		return "服务暂时无法完成该操作，请稍后重试。", http.StatusInternalServerError
	}
	return err.Error(), http.StatusBadRequest
}

// pendingBody is the body the user just submitted, for a save that was
// refused: their work only exists in that request, so re-reading the stored
// object would quietly replace it with the version they were editing away
// from. nil means there is nothing pending and the stored object is the truth.
func (a *App) renderResourcePage(w http.ResponseWriter, r *http.Request, user User, resource Resource, pageError string, status int, pendingBody []byte) {
	now := time.Now().UTC()
	shares, err := a.db.ListShares(r.Context(), user.ID, resource.ID, now)
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	data := a.basePage(r, user)
	data.Error = pageError
	// Reconsider only old opaque local rows with an explicitly textual name.
	// A known image whose filename happens to end in .txt, and remote resources
	// with no stored body, must keep their existing handling.
	recoverOpaqueText := !resource.Remote() && resource.ContentType == "application/octet-stream" && store.TextFilename(resource.Filename)
	if resource.Editable() || recoverOpaqueText {
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
	if resource.Remote() {
		if fetched := a.previewUpstream(r.Context(), &data, resource.OriginURL); fetched != "" {
			servedType = fetched
		}
	}

	data.Shares = buildShareViews(data.BaseURL, resource, servedType, shares, now)

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

	a.renderTemplate(w, r, status, "resource.html", data)
}

func buildShareViews(base string, resource Resource, servedType string, shares []Link, now time.Time) []linkView {
	views := make([]linkView, 0, len(shares))
	for _, share := range shares {
		ttlChoice, ttlCustom := shareTTLForm(share, now)
		address := ""
		if !share.Unreadable {
			address = base + shareAddress(share.Token, deliveryFilename(resource, servedType))
		}
		views = append(views, linkView{
			Link:      share,
			URL:       address,
			TTLChoice: ttlChoice,
			TTLCustom: ttlCustom,
		})
	}
	return views
}

// shareTTLForm maps the stored deadline back to the choices in the settings
// dialog. Quick shares use minute-sized lifetimes, so after one is adopted it
// belongs in the custom choice instead of pretending to be a 24-hour share.
func shareTTLForm(link Link, now time.Time) (choice, custom string) {
	if link.ExpiresAt == nil {
		return "never", ""
	}
	ttl := link.ExpiresAt.Sub(link.CreatedAt)
	switch ttl {
	case time.Hour:
		return "1h", ""
	case 24 * time.Hour:
		return "24h", ""
	case 7 * 24 * time.Hour:
		return "168h", ""
	default:
		return "custom", shareDurationInput(link.ExpiresAt.Sub(now))
	}
}

func shareDurationInput(duration time.Duration) string {
	if remainder := duration % time.Second; remainder != 0 {
		duration += time.Second - remainder
	}
	switch {
	case duration > 0 && duration%(24*time.Hour) == 0:
		return strconv.FormatInt(int64(duration/(24*time.Hour)), 10) + "d"
	case duration > 0 && duration%time.Hour == 0:
		return strconv.FormatInt(int64(duration/time.Hour), 10) + "h"
	case duration > 0 && duration%time.Minute == 0:
		return strconv.FormatInt(int64(duration/time.Minute), 10) + "m"
	case duration > 0 && duration%time.Second == 0:
		return strconv.FormatInt(int64(duration/time.Second), 10) + "s"
	default:
		return duration.String()
	}
}

// renderShareFragment refreshes the part of a resource page changed by a
// share action. It deliberately does not read the resource body, so creating a
// link for a large object never reloads the editor or pulls that object again.
func (a *App) renderShareFragment(w http.ResponseWriter, r *http.Request, user User, resourceID string) {
	resource, err := a.db.ResourceForOwner(r.Context(), user.ID, resourceID)
	if errors.Is(err, store.ErrNotFound) {
		writePlainError(w, http.StatusNotFound, "resource not found")
		return
	}
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	shares, err := a.db.ListShares(r.Context(), user.ID, resource.ID, time.Now().UTC())
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	data := a.basePage(r, user)
	data.Resource = resource
	servedType := resource.ContentType
	// The filename is decorative, but preserve the full-page address for the
	// unusual remote resource whose owner left it blank.
	if resource.Remote() && resource.Filename == "" {
		if fetched := a.previewUpstream(r.Context(), &data, resource.OriginURL); fetched != "" {
			servedType = fetched
		}
	}
	data.Shares = buildShareViews(data.BaseURL, resource, servedType, shares, time.Now().UTC())
	a.renderTemplate(w, r, http.StatusOK, "share-fragment", data)
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
	w.Header().Set("Content-Type", store.ContentTypeWithEncoding(resource.ContentType, resource.ContentEncoding))
	w.Header().Set("Content-Disposition", `inline; filename="`+deliveryFilename(resource, resource.ContentType)+`"`)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", deliveredContentSecurityPolicy)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Accept-Ranges", "bytes")
	size := resource.ContentSize
	requestedRange, err := parseByteRange(r.Header.Get("Range"), size)
	if err != nil {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		writePlainError(w, http.StatusRequestedRangeNotSatisfiable, "requested range is not satisfiable")
		return
	}
	var body io.ReadCloser
	if requestedRange == nil {
		body, size, err = a.db.OpenContent(r.Context(), resource)
	} else {
		var opened int64
		body, opened, err = a.db.OpenContentRange(r.Context(), resource, requestedRange.start, requestedRange.end)
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
	// Every share action lands back on the resource, where the list is. A
	// refusal reopens the share it was about so the message has something to
	// be next to; success just shows the list, which now holds the result.
	back := func(shareID, message string) {
		target := "/resources/" + resourceID
		values := url.Values{}
		if message != "" {
			values.Set("error", message)
			if shareID != "" {
				values.Set("share", shareID)
			}
		}
		if len(values) > 0 {
			target += "?" + values.Encode()
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
	}

	switch r.FormValue("action") {
	case "", "create":
		// Pressing 分享 mints the link straight away: the dialog it opens is
		// meant to already hold something you can send.
		link, err := a.db.CreateShare(r.Context(), user.ID, resourceID, strings.TrimSpace(r.FormValue("name")), defaultShareTTL, defaultShareUses)
		if err != nil {
			back("", err.Error())
			return
		}
		_ = link
		if r.Header.Get("X-PlainMote-Fragment") == "shares" {
			a.renderShareFragment(w, r, user, resourceID)
			return
		}
		back("", "")
	case "update":
		shareID := r.FormValue("share_id")
		ttl, err := shareTTL(r)
		if err != nil {
			back(shareID, err.Error())
			return
		}
		maxUses, err := shareUses(r)
		if err != nil {
			back(shareID, err.Error())
			return
		}
		if err := a.db.UpdateShare(r.Context(), user.ID, resourceID, shareID, strings.TrimSpace(r.FormValue("name")), ttl, maxUses); err != nil {
			back(shareID, err.Error())
			return
		}
		back(shareID, "")
	case "revoke":
		if err := a.db.RevokeLink(r.Context(), user.ID, resourceID, r.FormValue("share_id")); err != nil {
			back("", err.Error())
			return
		}
		back("", "")
	case "revoke_all":
		if err := a.db.RevokeShares(r.Context(), user.ID, resourceID); err != nil {
			back("", err.Error())
			return
		}
		back("", "")
	default:
		writePlainError(w, http.StatusBadRequest, "unknown share action")
	}
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

// dayDuration matches a plain number of days. Go's time.ParseDuration stops at
// hours, but the presets already offer 7 days, so typing "30d" has to work
// rather than forcing someone to write 720h.
var dayDuration = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)d$`)

func parseShareDuration(value string) (time.Duration, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if match := dayDuration.FindStringSubmatch(value); match != nil {
		days, err := strconv.ParseFloat(match[1], 64)
		if err != nil {
			return 0, err
		}
		return time.Duration(days * float64(24*time.Hour)), nil
	}
	return time.ParseDuration(value)
}
