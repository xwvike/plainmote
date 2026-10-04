package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"plainmote/internal/store"
)

// versionRow is one line of the history list. The current content is the
// first row; Links are the live links whose latest delivery carried it.
type versionRow struct {
	Number       int
	SavedAt      time.Time
	Size         int64
	Current      bool
	RestoredFrom int
	Links        []string
}

func versionsPath(resourceID string) string { return "/resources/" + resourceID + "/versions" }

func versionPath(resourceID string, number int) string {
	return versionsPath(resourceID) + "/" + strconv.Itoa(number)
}

// handleVersions routes everything under /resources/<id>/versions.
func (a *App) handleVersions(w http.ResponseWriter, r *http.Request, user User, sessionID, resourceID string, rest []string) {
	resource, err := a.db.ResourceForOwner(r.Context(), user.ID, resourceID)
	if errors.Is(err, store.ErrNotFound) {
		writePlainError(w, http.StatusNotFound, "resource not found")
		return
	}
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	switch {
	case len(rest) == 0:
		a.handleVersionList(w, r, user, resource)
	case len(rest) == 1 && rest[0] == "compare":
		a.handleCompare(w, r, user, resource)
	default:
		number, err := strconv.Atoi(rest[0])
		if err != nil || number <= 0 || len(rest) > 2 || len(rest) == 2 && rest[1] != "raw" {
			writePlainError(w, http.StatusNotFound, "not found")
			return
		}
		if number == resource.Version {
			// The current content is the resource; its page is where it lives.
			http.Redirect(w, r, "/resources/"+resource.ID, http.StatusSeeOther)
			return
		}
		version, err := a.db.VersionForOwner(r.Context(), user.ID, resource.ID, number)
		if errors.Is(err, store.ErrNotFound) {
			writePlainError(w, http.StatusNotFound, "version not found")
			return
		}
		if err != nil {
			a.renderError(w, http.StatusInternalServerError, err)
			return
		}
		switch {
		case len(rest) == 2:
			a.handleVersionRaw(w, r, resource, version)
		case r.Method == http.MethodPost:
			a.handleVersionAction(w, r, user, sessionID, resource, version)
		case r.Method == http.MethodGet:
			a.renderVersion(w, r, user, resource, version, r.URL.Query().Get("error"))
		default:
			writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		}
	}
}

func (a *App) handleVersionList(w http.ResponseWriter, r *http.Request, user User, resource Resource) {
	if r.Method != http.MethodGet {
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	versions, err := a.db.ListVersions(r.Context(), user.ID, resource.ID)
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	delivered, err := a.db.LastDelivered(r.Context(), user.ID, resource.ID, time.Now().UTC())
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	data := a.basePage(r, user)
	data.Error = r.URL.Query().Get("error")
	data.Resource = resource
	data.HistoryKeep = store.HistoryKeep
	data.HistoryDays = int(store.HistoryRetention / (24 * time.Hour))

	linksOf := func(number int) []string {
		var names []string
		for _, link := range delivered {
			if link.Version == number {
				names = append(names, link.LinkName)
			}
		}
		return names
	}
	if !resource.Remote() {
		data.Versions = append(data.Versions, versionRow{
			Number: resource.Version, SavedAt: resource.VersionAt, Size: resource.ContentSize,
			Current: true, RestoredFrom: resource.RestoredFrom, Links: linksOf(resource.Version),
		})
	}
	for _, version := range versions {
		data.HistoryBytes += version.ContentSize
		data.Versions = append(data.Versions, versionRow{
			Number: version.Number, SavedAt: version.SavedAt, Size: version.ContentSize,
			RestoredFrom: version.RestoredFrom, Links: linksOf(version.Number),
		})
	}
	data.HistoryCount = len(versions)
	// Numbers below the oldest kept one were removed by the retention rules.
	// Said once, as a range, so the list does not appear to skip for no reason.
	oldest := resource.Version
	if len(versions) > 0 {
		oldest = versions[len(versions)-1].Number
	}
	if oldest > 1 && !resource.Remote() {
		data.HistoryGone = [2]int{1, oldest - 1}
	}
	if remove, err := strconv.Atoi(r.URL.Query().Get("remove")); err == nil {
		for _, version := range versions {
			if version.Number == remove {
				data.RemoveVersion = remove
			}
		}
	}
	a.renderTemplate(w, r, http.StatusOK, "versions.html", data)
}

func (a *App) renderVersion(w http.ResponseWriter, r *http.Request, user User, resource Resource, version store.Version, pageError string) {
	versions, err := a.db.ListVersions(r.Context(), user.ID, resource.ID)
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	data := a.basePage(r, user)
	data.Error = pageError
	data.Resource = resource
	data.Viewed = version
	// Neighbours among what is kept: older is further down the list, newer
	// is further up, and above the newest kept version is the current one.
	data.NextVersion = resource.Version
	for _, other := range versions {
		if other.Number < version.Number && data.PrevVersion == 0 {
			data.PrevVersion = other.Number
		}
		if other.Number > version.Number {
			data.NextVersion = other.Number
		}
	}
	if store.TextLike(version.ContentType) {
		body, err := a.db.ReadVersion(r.Context(), version)
		if err != nil {
			a.renderError(w, http.StatusInternalServerError, err)
			return
		}
		if text, encoding, err := store.DecodeText(body, version.ContentEncoding); err == nil {
			data.ContentText = text
			data.ContentEncoding = encoding
			data.ContentEOL = store.DetectEOL(text)
			data.ViewedText = true
		}
	}
	data.RestoreOpen = r.URL.Query().Get("restore") != ""
	a.renderTemplate(w, r, http.StatusOK, "version.html", data)
}

// handleVersionRaw serves an earlier version's bytes to its owner, for the
// version page to show an image or play a sound. The same guards as the raw
// preview of current content: owner only, never as anything executable.
func (a *App) handleVersionRaw(w http.ResponseWriter, r *http.Request, resource Resource, version store.Version) {
	if r.Method != http.MethodGet {
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	resource.ContentType = version.ContentType
	if version.Filename != "" {
		resource.Filename = version.Filename
	}
	a.serveOwnedBytes(w, r,
		store.ContentTypeWithEncoding(version.ContentType, version.ContentEncoding),
		contentDisposition(deliveryFilename(resource, version.ContentType)),
		version.ContentSize,
		func() (io.ReadCloser, int64, error) { return a.db.OpenVersion(r.Context(), version) },
		func(start, end int64) (io.ReadCloser, int64, error) {
			return a.db.OpenVersionRange(r.Context(), version, start, end)
		})
}

// Actions on one earlier version. Each lands on the page that now shows the
// result: the resource for a restore or a copy, the list for a deletion.
const (
	actionRestore = "restore"
	actionCopy    = "copy"
)

func (a *App) handleVersionAction(w http.ResponseWriter, r *http.Request, user User, sessionID string, resource Resource, version store.Version) {
	if !a.checkCSRF(r, sessionID) {
		writePlainError(w, http.StatusForbidden, "invalid csrf token")
		return
	}
	back := func(target string, err error) {
		text, _ := a.writeErrorText("version action", err)
		http.Redirect(w, r, target+"?"+url.Values{"error": {text}}.Encode(), http.StatusSeeOther)
	}
	switch r.FormValue("action") {
	case actionRestore:
		result, err := a.db.RestoreVersion(r.Context(), user.ID, resource.ID, version.Number)
		if err != nil {
			back(versionPath(resource.ID, version.Number), err)
			return
		}
		values := url.Values{"restored": {strconv.Itoa(version.Number)}}
		if result.Trimmed > 0 {
			values.Set("trimmed", strconv.Itoa(result.Trimmed))
		}
		http.Redirect(w, r, "/resources/"+resource.ID+"?"+values.Encode(), http.StatusSeeOther)
	case actionCopy:
		name := fmt.Sprintf("%s (v%d)", resourceTitle(requestLanguage(r).Locale, resource), version.Number)
		copied, err := a.db.CopyVersion(r.Context(), user.ID, resource.ID, version.Number, name)
		if err != nil {
			back(versionPath(resource.ID, version.Number), err)
			return
		}
		http.Redirect(w, r, "/resources/"+copied.ID, http.StatusSeeOther)
	case actionDelete:
		if err := a.db.DeleteVersion(r.Context(), user.ID, resource.ID, version.Number); err != nil {
			back(versionsPath(resource.ID), err)
			return
		}
		http.Redirect(w, r, versionsPath(resource.ID), http.StatusSeeOther)
	default:
		writePlainError(w, http.StatusBadRequest, "unknown version action")
	}
}

// resourceTitle is what a resource is called on its pages.
func resourceTitle(locale string, resource Resource) string {
	if resource.Name != "" {
		return resource.Name
	}
	if resource.Filename != "" {
		return resource.Filename
	}
	return translate(locale, "untitled_resource")
}

// versionBody is one side of a comparison: an earlier version, or the
// current content when the number is the current one. Kind says how a page
// shows it - text is compared line by line, anything else side by side.
type versionBody struct {
	Number   int
	Filename string
	Current  bool
	Size     int64
	Type     string
	Encoding string
	Bytes    []byte
	Kind     string
	SHA256   string
	Width    int
	Height   int
	// RawURL serves the bytes to the owner; PageURL is where the version is
	// shown on its own.
	RawURL  string
	PageURL string
	// Meta is an encrypted side's encrypted metadata, for the browser.
	Meta []byte
}

func (a *App) versionBody(r *http.Request, user User, resource Resource, number int) (versionBody, error) {
	side := versionBody{Number: number}
	if number == resource.Version {
		body, err := a.db.ReadContent(r.Context(), resource)
		if err != nil {
			return side, err
		}
		side.Current, side.Bytes, side.Filename = true, body, resource.Filename
		side.Size, side.Type, side.Encoding, side.SHA256 = resource.ContentSize, resource.ContentType, resource.ContentEncoding, resource.ContentSHA256
		side.RawURL, side.PageURL = "/resources/"+resource.ID+"/raw", "/resources/"+resource.ID
		side.Meta = resource.SealedMeta
	} else {
		version, err := a.db.VersionForOwner(r.Context(), user.ID, resource.ID, number)
		if err != nil {
			return side, err
		}
		body, err := a.db.ReadVersion(r.Context(), version)
		if err != nil {
			return side, err
		}
		side.Bytes, side.Filename = body, version.Filename
		if side.Filename == "" {
			side.Filename = resource.Filename
		}
		side.Size, side.Type, side.Encoding, side.SHA256 = version.ContentSize, version.ContentType, version.ContentEncoding, version.ContentSHA256
		side.RawURL, side.PageURL = versionPath(resource.ID, number)+"/raw", versionPath(resource.ID, number)
		side.Meta = version.SealedMeta
	}
	// A file is shown as what its bytes are, whatever its row says: a row
	// written before detection read the bytes may name a PNG as a video.
	if !store.TextLike(side.Type) {
		side.Type, _ = store.DetectContent(side.Filename, side.Bytes, "")
	}
	if side.SHA256 == "" {
		sum := sha256.Sum256(side.Bytes)
		side.SHA256 = hex.EncodeToString(sum[:])
	}
	switch {
	case store.TextLike(side.Type):
		side.Kind = "text"
	case strings.HasPrefix(side.Type, "image/"):
		side.Kind = "image"
		side.Width, side.Height = imageSize(side.Bytes)
	case strings.HasPrefix(side.Type, "audio/"):
		side.Kind = "audio"
	case strings.HasPrefix(side.Type, "video/"):
		side.Kind = "video"
	default:
		side.Kind = "file"
	}
	return side, nil
}

// imageSize reads an image's dimensions from its header, 0 × 0 when the
// format is one the standard library does not know and WebP is not it.
func imageSize(data []byte) (int, int) {
	if config, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		return config.Width, config.Height
	}
	if len(data) < 30 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return 0, 0
	}
	le24 := func(b []byte) int { return int(b[0]) | int(b[1])<<8 | int(b[2])<<16 }
	switch string(data[12:16]) {
	case "VP8 ":
		return int(binary.LittleEndian.Uint16(data[26:28]) & 0x3fff), int(binary.LittleEndian.Uint16(data[28:30]) & 0x3fff)
	case "VP8L":
		b := data[21:25]
		return 1 + (int(b[1]&0x3f)<<8 | int(b[0])), 1 + (int(b[3]&0x0f)<<10 | int(b[2])<<2 | int(b[1]&0xc0)>>6)
	case "VP8X":
		return 1 + le24(data[24:27]), 1 + le24(data[27:30])
	}
	return 0, 0
}

// compareNote says why a comparison has no rows.
const (
	compareSame        = "compare_same"
	compareEncoding    = "compare_encoding_only"
	compareFilesDiffer = "compare_files_differ"
	compareTooLarge    = "compare_too_large"
)

func (a *App) handleCompare(w http.ResponseWriter, r *http.Request, user User, resource Resource) {
	if r.Method != http.MethodGet {
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if resource.Remote() {
		writePlainError(w, http.StatusNotFound, "a remote resource has no versions")
		return
	}
	versions, err := a.db.ListVersions(r.Context(), user.ID, resource.ID)
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	choices := []int{resource.Version}
	for _, version := range versions {
		choices = append(choices, version.Number)
	}
	query := r.URL.Query()
	from, to := resource.Version, resource.Version
	if len(versions) > 0 {
		from = versions[0].Number
	}
	if value, err := strconv.Atoi(query.Get("from")); err == nil {
		from = value
	}
	if value, err := strconv.Atoi(query.Get("to")); err == nil {
		to = value
	}
	before, err := a.versionBody(r, user, resource, from)
	if err == nil {
		var after versionBody
		after, err = a.versionBody(r, user, resource, to)
		if err == nil {
			data := a.basePage(r, user)
			data.Resource = resource
			data.DiffChoices = choices
			data.DiffFrom, data.DiffTo = before, after
			data.DiffFull = query.Get("full") != ""
			// Encrypted sides are compared in the browser, which alone can
			// read them.
			if resource.Sealed() {
				data.SealedCompare = true
			} else {
				data.DiffNote = compareVersions(&data, before, after)
			}
			a.renderTemplate(w, r, http.StatusOK, "compare.html", data)
			return
		}
	}
	if errors.Is(err, store.ErrNotFound) {
		writePlainError(w, http.StatusNotFound, "version not found")
		return
	}
	a.renderError(w, http.StatusInternalServerError, err)
}

// compareVersions fills in a line diff when both sides are text, and puts
// the sides next to each other when either is not; the key it returns is the
// sentence that stands in for, or goes above, what is shown.
func compareVersions(data *pageData, before, after versionBody) string {
	if before.Kind == "text" && after.Kind == "text" {
		beforeText, _, errBefore := store.DecodeText(before.Bytes, before.Encoding)
		afterText, _, errAfter := store.DecodeText(after.Bytes, after.Encoding)
		if errBefore == nil && errAfter == nil {
			diff := lineDiff(beforeText, afterText, data.DiffFull)
			switch {
			case diff.TooLarge:
				return compareTooLarge
			case diff.Added+diff.Removed == 0 && bytes.Equal(before.Bytes, after.Bytes):
				return compareSame
			case diff.Added+diff.Removed == 0:
				return compareEncoding
			}
			data.Diff = diff
			return ""
		}
	}
	// A file is not read line by line: an image is looked at, a sound played,
	// and anything else is the same bytes or not.
	data.DiffSides = []versionBody{before, after}
	if bytes.Equal(before.Bytes, after.Bytes) {
		return compareSame
	}
	return compareFilesDiffer
}

// versionNumber reads a positive version number from a form field.
func versionNumber(value string) int {
	number, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || number < 0 {
		return 0
	}
	return number
}
