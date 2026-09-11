package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"plainmote/internal/store"
)

// What a freshly opened share dialog hands you: alive long enough to be
// useful, and spent after one fetch, so a link that is forwarded or left in a
// chat log does not keep working. Both are one click away from being widened.
const (
	defaultShareTTL  = 24 * time.Hour
	defaultShareUses = 1
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
		writePlainError(w, http.StatusNotFound, "not found")
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
const (
	kindUpload = "upload"
	kindRemote = "remote"
)

// actionPreview asks the server to read the address and show what comes back,
// and to do nothing else. Pulling an upstream to look at it is not the same
// intent as keeping it, and on the new-resource screen there is nothing to
// keep yet - the form has not been filled in.
const actionPreview = "preview"

func newResourceKind(value string) string {
	if value == kindRemote {
		return kindRemote
	}
	return kindUpload
}

func (a *App) handleNewResource(w http.ResponseWriter, r *http.Request, user User, sessionID string) {
	if r.Method == http.MethodGet {
		data := a.basePage(r, user)
		data.IsNew = true
		data.NewKind = newResourceKind(r.URL.Query().Get("kind"))
		a.renderTemplate(w, http.StatusOK, "resource.html", data)
		return
	}
	if r.Method != http.MethodPost || !a.checkCSRF(r, sessionID) {
		writePlainError(w, http.StatusForbidden, "invalid request")
		return
	}
	form, err := readResourceForm(r, a.cfg.MaxContent)
	if err == nil && r.FormValue("action") == actionPreview {
		data := a.basePage(r, user)
		data.IsNew = true
		data.NewKind = newResourceKind(r.FormValue("kind"))
		data.Resource = Resource{Name: form.Name, Filename: form.Filename, OriginURL: form.OriginURL}
		a.previewUpstream(r.Context(), &data, form.OriginURL)
		a.renderTemplate(w, http.StatusOK, "resource.html", data)
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
	data := a.basePageWithError(r, user, err.Error())
	data.IsNew = true
	data.NewKind = newResourceKind(r.FormValue("kind"))
	data.Resource = Resource{Name: form.Name, Filename: form.Filename, ContentSize: int64(len(form.Content)), ContentEncoding: form.ContentEncoding, OriginURL: form.OriginURL}
	data.ContentEncoding = form.ContentEncoding
	data.ContentEOL = form.ContentEOL
	if !form.Uploaded {
		if text, _, decodeErr := store.DecodeText(form.Content, form.ContentEncoding); decodeErr == nil {
			data.ContentText = text
		}
	}
	a.renderTemplate(w, http.StatusBadRequest, "resource.html", data)
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

func readResourceForm(r *http.Request, maxBytes int64) (resourceForm, error) {
	var form resourceForm
	if r.ContentLength > maxBytes+64*1024 {
		return form, errors.New("content is too large")
	}
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
		form, err := readResourceForm(r, a.cfg.MaxContent)
		if err != nil {
			a.renderResourcePage(w, r, user, resource, err.Error())
			return
		}
		if r.FormValue("action") == actionPreview {
			// Render against what is in the form rather than what is stored, so
			// a pasted address can be read before deciding to keep it. The
			// stored row is untouched either way.
			pending := resource
			pending.Name, pending.Filename, pending.OriginURL = form.Name, form.Filename, form.OriginURL
			a.renderResourcePage(w, r, user, pending, "")
			return
		}
		content := form.Content
		if !form.ContentGiven {
			// A nil body tells the store to retain the current object.
			content = nil
		}
		if err := a.db.UpdateResource(r.Context(), user.ID, resourceID, form.Name, form.Filename, content, form.ContentEncoding, form.OriginURL); err != nil {
			a.renderResourcePage(w, r, user, resource, err.Error())
			return
		}
		http.Redirect(w, r, "/resources/"+resourceID, http.StatusSeeOther)
		return
	}
	if r.Method != http.MethodGet {
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	a.renderResourcePage(w, r, user, resource, r.URL.Query().Get("error"))
}

func (a *App) renderResourcePage(w http.ResponseWriter, r *http.Request, user User, resource Resource, pageError string) {
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
		text, err := a.db.ReadContent(r.Context(), resource)
		if err != nil {
			a.renderError(w, http.StatusInternalServerError, err)
			return
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

	base := a.baseURL(r)
	for _, share := range shares {
		data.Shares = append(data.Shares, linkView{
			Link: share,
			URL:  base + shareAddress(share.Token, deliveryFilename(resource, servedType)),
		})
	}

	// Two ways into the dialog: "分享" mints a link and focuses it, while
	// "查看分享" only lists what already exists and creates nothing.
	if focusID := strings.TrimSpace(r.URL.Query().Get("share")); focusID != "" {
		data.ShareOpen = true
		for _, view := range data.Shares {
			if view.Link.ID == focusID {
				data.FocusShare = view
				break
			}
		}
	} else if r.URL.Query().Get("shares") != "" {
		data.ShareOpen = true
	}

	a.renderTemplate(w, http.StatusOK, "resource.html", data)
}

// previewUpstream reads the address exactly as a public request would and fills
// in whatever of it the page can show. Nothing is stored and nothing is cached:
// this is a live read, and it is the only way the owner sees a remote body.
// Returns the type the address would be served as, or "" if it could not be
// read - callers keep their own fallback in that case.
func (a *App) previewUpstream(ctx context.Context, data *pageData, originURL string) string {
	if strings.TrimSpace(originURL) == "" {
		data.UpstreamError = "先填写远程地址，再点拉取。"
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
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.Header().Set("Cache-Control", "no-store")
	body, size, err := a.db.OpenContent(r.Context(), resource)
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	defer body.Close()
	if size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
}

func (a *App) handleShare(w http.ResponseWriter, r *http.Request, user User, sessionID, resourceID string) {
	if r.Method != http.MethodPost || !a.checkCSRF(r, sessionID) {
		writePlainError(w, http.StatusForbidden, "invalid request")
		return
	}
	// Every share action lands back in the dialog: focused on one link when
	// there is one to focus, otherwise on the list.
	back := func(shareID, message string) {
		values := url.Values{}
		if shareID != "" {
			values.Set("share", shareID)
		} else {
			values.Set("shares", "1")
		}
		if message != "" {
			values.Set("error", message)
		}
		http.Redirect(w, r, "/resources/"+resourceID+"?"+values.Encode(), http.StatusSeeOther)
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
		back(link.ID, "")
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
