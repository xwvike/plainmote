package web

import (
	"archive/zip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"plainmote/internal/store"
)

// The settings, a page each: the account itself, how it signs in, its
// master password, what is signed in to it, and its data.
const (
	accountPath         = "/account"
	accountSignInPath   = "/account/sign-in"
	accountSecurityPath = "/account/security"
	accountDevicesPath  = "/account/devices"
	accountDataPath     = "/account/data"
)

// The forms those pages post to.
const (
	accountExportPath = "/account/export"
	accountDeletePath = "/account/delete"
	accountTokensPath = "/account/tokens"
	// accountIdentitiesPath links and unlinks sign-in methods.
	accountIdentitiesPath = "/account/identities"
)

// accountSections names each settings page by its path.
var accountSections = map[string]string{
	accountPath:         "account",
	accountSignInPath:   "sign-in",
	accountSecurityPath: "security",
	accountDevicesPath:  "devices",
	accountDataPath:     "data",
}

// exportWriteTimeout replaces the server's 30-second write timeout for the one
// response that is a whole account: every body in the plan, over whatever
// connection the owner happens to be on.
const exportWriteTimeout = 15 * time.Minute

func (a *App) handleAccount(w http.ResponseWriter, r *http.Request) {
	user, _, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	a.renderAccount(w, r, user, accountSections[r.URL.Path], "", false, http.StatusOK)
}

// renderAccount is one settings page, reading only what that page shows.
func (a *App) renderAccount(w http.ResponseWriter, r *http.Request, user User, section, pageError string, deleteOpen bool, status int) {
	ctx := r.Context()
	now := time.Now().UTC()
	account, err := a.db.Account(ctx, user.ID)
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	data := a.basePage(r, user)
	data.Active = "account"
	data.Section = section
	data.Account = account
	data.Error = pageError
	data.DeleteOpen = deleteOpen
	switch section {
	case "account":
		if data.Quota, err = a.db.QuotaForUser(ctx, user.ID, now); err != nil {
			a.renderError(w, http.StatusInternalServerError, err)
			return
		}
		if data.APITokens, err = a.db.ListAPITokens(ctx, user.ID, now); err != nil {
			a.renderError(w, http.StatusInternalServerError, err)
			return
		}
		if data.Export, err = a.db.ExportAllowance(ctx, user.ID, now); err != nil {
			a.renderError(w, http.StatusInternalServerError, err)
			return
		}
	case "sign-in":
		data.SignIns = a.signIns(account.Identities)
		if pageError == "" {
			data.Notice, data.Error = identityFlash(r)
		}
	case "security":
		data.KeyringLockChoices = store.KeyringLockChoices
		if keyring, err := a.db.Keyring(ctx, user.ID); err == nil {
			data.Keyring = &keyring
		} else if !errors.Is(err, store.ErrNotFound) {
			a.renderError(w, http.StatusInternalServerError, err)
			return
		}
		if !a.countSealed(w, r, user, &data) {
			return
		}
	case "devices":
		if data.APITokens, err = a.db.ListAPITokens(ctx, user.ID, now); err != nil {
			a.renderError(w, http.StatusInternalServerError, err)
			return
		}
		// The revoke dialog opens over the list, for a token that is still on it.
		if id := r.URL.Query().Get("revoke"); id != "" {
			for _, token := range data.APITokens {
				if token.ID == id {
					data.RevokeToken = token
				}
			}
		}
	case "data":
		if data.Export, err = a.db.ExportAllowance(ctx, user.ID, now); err != nil {
			a.renderError(w, http.StatusInternalServerError, err)
			return
		}
		data.ExportLimit = store.ExportLimit
		data.ExportWindow = int(store.ExportWindow / time.Hour)
		if !a.countSealed(w, r, user, &data) {
			return
		}
	}
	a.renderTemplate(w, r, status, "account.html", data)
}

// countSealed is how many encrypted resources the account has, and how many
// of them are quick shares.
func (a *App) countSealed(w http.ResponseWriter, r *http.Request, user User, data *pageData) bool {
	sealed, err := a.db.SealedIndex(r.Context(), user.ID)
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return false
	}
	data.SealedCount = len(sealed)
	for _, entry := range sealed {
		if entry.QuickShare {
			data.SealedQuickShares++
		}
	}
	return true
}

// signInView is one row of the account's sign-in methods: a provider and the
// identity linked there, if any.
type signInView struct {
	Provider  string
	Label     string
	Identity  *store.Identity
	CanLink   bool
	CanUnlink bool
}

func (a *App) signIns(identities []store.Identity) []signInView {
	var views []signInView
	seen := map[string]bool{}
	for i := range identities {
		seen[identities[i].Provider] = true
		views = append(views, signInView{
			Provider: identities[i].Provider, Label: providerLabel(identities[i].Provider),
			Identity: &identities[i], CanUnlink: len(identities) > 1,
		})
	}
	for _, p := range a.providers() {
		if !seen[p.name] {
			views = append(views, signInView{Provider: p.name, Label: p.label, CanLink: true})
		}
	}
	return views
}

// identityFlash is the line the account page shows after a sign-in method
// was linked or unlinked, or could not be.
func identityFlash(r *http.Request) (notice, failure string) {
	locale := requestLanguage(r).Locale
	query := r.URL.Query()
	// Only a provider's own name is said back: anything else in the address
	// would be someone else's words on this page.
	known := func(name string) bool { return name == store.ProviderGitHub || name == store.ProviderGoogle }
	switch {
	case known(query.Get("linked")):
		return fmt.Sprintf(translate(locale, "identity_linked"), providerLabel(query.Get("linked"))), ""
	case known(query.Get("unlinked")):
		return fmt.Sprintf(translate(locale, "identity_unlinked"), providerLabel(query.Get("unlinked"))), ""
	}
	switch query.Get("identity") {
	case "taken":
		return "", translate(locale, "identity_taken")
	case "linked":
		return "", translate(locale, "identity_provider_linked")
	case "last":
		return "", translate(locale, "identity_last")
	}
	return "", ""
}

// handleAccountIdentities starts linking a provider, through the same flow
// as signing in with it, or removes one the account no longer wants.
func (a *App) handleAccountIdentities(w http.ResponseWriter, r *http.Request) {
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
	provider := r.FormValue("provider")
	switch r.FormValue("action") {
	case "link":
		p, ok := a.provider(provider)
		if !ok {
			writePlainError(w, http.StatusBadRequest, "unknown provider")
			return
		}
		a.beginOAuth(w, r, p, "", true)
	case "unlink":
		err := a.db.UnlinkIdentity(r.Context(), user.ID, provider)
		switch {
		case err == nil:
			http.Redirect(w, r, accountSignInPath+"?unlinked="+url.QueryEscape(provider), http.StatusSeeOther)
		case errors.Is(err, store.ErrLastIdentity):
			http.Redirect(w, r, accountSignInPath+"?identity=last", http.StatusSeeOther)
		case errors.Is(err, store.ErrNotFound):
			http.Redirect(w, r, accountSignInPath, http.StatusSeeOther)
		default:
			a.serverError(w, "unlink identity", err)
		}
	default:
		writePlainError(w, http.StatusBadRequest, "unknown action")
	}
}

// handleAccountTokens revokes one signed-in command line. Its next request is
// refused, and the page comes back without it.
func (a *App) handleAccountTokens(w http.ResponseWriter, r *http.Request) {
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
	if r.FormValue("action") != "revoke" {
		writePlainError(w, http.StatusBadRequest, "unknown action")
		return
	}
	// Already gone is what was asked for: another tab may have revoked it.
	if err := a.db.RevokeAPIToken(r.Context(), user.ID, r.FormValue("token")); err != nil && !errors.Is(err, store.ErrNotFound) {
		a.serverError(w, "revoke token", err)
		return
	}
	http.Redirect(w, r, accountDevicesPath, http.StatusSeeOther)
}

// handleAccountDelete takes two steps: the username typed out, then a dialog
// that names the account and what goes with it. This is the one action here
// that nothing can undo, so neither a click out of habit nor a username typed
// in the wrong tab is enough on its own.
func (a *App) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
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
	if !strings.EqualFold(strings.TrimSpace(r.FormValue("confirm")), user.Login) {
		a.renderAccount(w, r, user, "data", translate(requestLanguage(r).Locale, "delete_account_mismatch"), false, http.StatusBadRequest)
		return
	}
	if r.FormValue("final") != "1" {
		a.renderAccount(w, r, user, "data", "", true, http.StatusOK)
		return
	}
	// Already gone is the outcome that was asked for: a second submission from
	// another tab lands here and should end signed out, not on an error.
	if err := a.db.DeleteAccount(r.Context(), user.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
		a.serverError(w, "delete account", err)
		return
	}
	a.clearSessionCookies(w)
	http.Redirect(w, r, "/login?deleted=1", http.StatusSeeOther)
}

// handleAccountExport streams the account as a ZIP archive. It is a POST with
// the CSRF token, although it changes nothing: every body in the account is
// read on each request, and another site should not be able to start that.
// For the same reason each account gets store.ExportLimit exports per window.
func (a *App) handleAccountExport(w http.ResponseWriter, r *http.Request) {
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
	ctx := r.Context()
	// Everything that can fail with a proper error page is read before the
	// first byte goes out. After that the status is sent and a failure can
	// only cut the archive short.
	account, err := a.db.Account(ctx, user.ID)
	if err != nil {
		a.serverError(w, "export account", err)
		return
	}
	resources, err := a.db.ExportResources(ctx, user.ID)
	if err != nil {
		a.serverError(w, "export resources", err)
		return
	}
	versions, err := a.db.ExportVersions(ctx, user.ID)
	if err != nil {
		a.serverError(w, "export versions", err)
		return
	}

	// Spent last, once nothing is left that could fail before the download
	// starts: a refusal from an earlier step should not cost an export.
	now := time.Now().UTC()
	if _, err := a.db.ReserveExport(ctx, user.ID, now); err != nil {
		if errors.Is(err, store.ErrExportLimit) {
			a.renderAccount(w, r, user, "data", translate(requestLanguage(r).Locale, "export_limit_reached"), false, http.StatusTooManyRequests)
			return
		}
		a.serverError(w, "reserve export", err)
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(now.Add(exportWriteTimeout))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="plainmote-%s-%s.zip"`, exportFilePart(account.User), now.Format("20060102")))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	archive := zip.NewWriter(w)
	if err := a.writeExport(r, archive, account, resources, versions, now); err != nil {
		fmt.Fprintf(os.Stderr, "export account %s: %v\n", user.ID, err)
		return
	}
	if err := archive.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "export account %s: %v\n", user.ID, err)
	}
}

type exportAccessLog struct {
	ID             string    `json:"id"`
	ResourceID     string    `json:"resource_id,omitempty"`
	ResourceName   string    `json:"resource_name"`
	ResourceFile   string    `json:"resource_file"`
	LinkID         string    `json:"link_id,omitempty"`
	LinkName       string    `json:"link_name"`
	Outcome        string    `json:"outcome"`
	Status         int       `json:"status"`
	Detail         string    `json:"detail"`
	RemoteIP       string    `json:"remote_ip"`
	RemoteAddr     string    `json:"remote_addr"`
	Country        string    `json:"country,omitempty"`
	Region         string    `json:"region,omitempty"`
	City           string    `json:"city,omitempty"`
	Method         string    `json:"method"`
	Host           string    `json:"host"`
	Path           string    `json:"path"`
	Query          string    `json:"query"`
	Proto          string    `json:"proto"`
	TLS            bool      `json:"tls"`
	UserAgent      string    `json:"user_agent"`
	Referer        string    `json:"referer"`
	Forwarded      string    `json:"forwarded"`
	XForwardedFor  string    `json:"x_forwarded_for"`
	CFConnectingIP string    `json:"cf_connecting_ip"`
	CFRay          string    `json:"cf_ray"`
	ContentLength  string    `json:"content_length"`
	Hits           int       `json:"hits"`
	Version        int       `json:"version,omitempty"`
	FirstAt        time.Time `json:"first_at"`
	OccurredAt     time.Time `json:"occurred_at"`
}

func (a *App) writeExport(r *http.Request, archive *zip.Writer, account store.Account, resources []Resource, versions []store.Version, now time.Time) error {
	ctx := r.Context()
	if err := writeExportAccount(archive, account, now); err != nil {
		return err
	}

	// The log is written row by row; it is the part with no upper bound.
	logs, err := archive.CreateHeader(&zip.FileHeader{Name: "access_logs.json", Method: zip.Deflate, Modified: now})
	if err != nil {
		return err
	}
	if _, err := io.WriteString(logs, "["); err != nil {
		return err
	}
	first := true
	err = a.db.EachAccessLog(ctx, account.User.ID, func(entry AccessLog) error {
		line, err := json.Marshal(exportAccessLog{
			ID: entry.ID, ResourceID: entry.ResourceID, ResourceName: entry.ResourceName, ResourceFile: entry.ResourceFile,
			LinkID: entry.LinkID, LinkName: entry.LinkName, Outcome: entry.Outcome, Status: entry.Status, Detail: entry.Detail,
			RemoteIP: entry.RemoteIP, RemoteAddr: entry.RemoteAddr,
			Country: entry.Location.Country, Region: entry.Location.Region, City: entry.Location.City, Method: entry.Method, Host: entry.Host, Path: redactDeliveryPath(entry.Path),
			Query: redactSensitiveQuery(entry.Query), Proto: entry.Proto, TLS: entry.TLS, UserAgent: entry.UserAgent, Referer: redactSensitiveURL(entry.Referer),
			Forwarded: entry.Forwarded, XForwardedFor: entry.XForwardedFor, CFConnectingIP: entry.CFConnectingIP,
			CFRay: entry.CFRay, ContentLength: entry.ContentLength, Hits: entry.Hits, Version: entry.Version, FirstAt: entry.FirstAt, OccurredAt: entry.OccurredAt,
		})
		if err != nil {
			return err
		}
		separator := ",\n"
		if first {
			separator, first = "\n", false
		}
		if _, err := io.WriteString(logs, separator); err != nil {
			return err
		}
		_, err = logs.Write(line)
		return err
	})
	if err != nil {
		return err
	}
	if _, err := io.WriteString(logs, "\n]\n"); err != nil {
		return err
	}

	if err := a.writeExportSealed(r, archive, account.User.ID, resources, versions, now); err != nil {
		return err
	}

	// Remote resources become small .url files. The export preserves the saved
	// address without fetching an upstream on the owner's behalf.
	for _, resource := range resources {
		var err error
		switch {
		case resource.OriginURL != "":
			err = writeExportRemote(archive, resource)
		case resource.ContentKey != "":
			err = a.writeExportBody(r, archive, resource)
		}
		if err != nil {
			return fmt.Errorf("resource %s: %w", resource.ID, err)
		}
	}

	// Earlier versions sit beside the current content, one directory each,
	// under the name the resource has now.
	byID := make(map[string]Resource, len(resources))
	for _, resource := range resources {
		byID[resource.ID] = resource
	}
	for _, version := range versions {
		resource, ok := byID[version.ResourceID]
		if !ok {
			continue
		}
		if err := a.writeExportVersion(r, archive, resource, version); err != nil {
			return fmt.Errorf("resource %s version %d: %w", resource.ID, version.Number, err)
		}
	}
	return nil
}

func (a *App) writeExportVersion(r *http.Request, archive *zip.Writer, resource Resource, version store.Version) error {
	body, _, err := a.db.OpenVersion(r.Context(), version)
	if err != nil {
		return err
	}
	defer body.Close()
	// Named as it was named while it was current: a video kept from before an
	// image replaced it is movie.mp4, not photo.png.
	resource.ContentType = version.ContentType
	if version.Filename != "" {
		resource.Filename = version.Filename
	}
	if resource.Sealed() {
		resource.ContentType = store.SealedContentType
	}
	name := path.Base(exportBodyPath(resource))
	entry, err := archive.CreateHeader(&zip.FileHeader{
		Name:     fmt.Sprintf("files/%s/versions/v%d/%s", resource.ID, version.Number, name),
		Method:   zip.Deflate,
		Modified: version.SavedAt,
	})
	if err != nil {
		return err
	}
	_, err = io.Copy(entry, body)
	return err
}

func (a *App) writeExportBody(r *http.Request, archive *zip.Writer, resource Resource) error {
	body, _, err := a.db.OpenContent(r.Context(), resource)
	if err != nil {
		return err
	}
	defer body.Close()
	entry, err := archive.CreateHeader(&zip.FileHeader{Name: exportBodyPath(resource), Method: zip.Deflate, Modified: resource.UpdatedAt})
	if err != nil {
		return err
	}
	_, err = io.Copy(entry, body)
	return err
}

func writeExportAccount(archive *zip.Writer, account store.Account, now time.Time) error {
	entry, err := archive.CreateHeader(&zip.FileHeader{Name: "account.txt", Method: zip.Deflate, Modified: now})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(entry, "Username: %s\nDisplay name: %s\nAvatar URL: %s\nRegistered: %s\nExported: %s\n",
		exportText(account.User.Login), exportText(account.User.Name),
		exportText(account.User.AvatarURL), account.CreatedAt.UTC().Format(time.RFC3339), now.UTC().Format(time.RFC3339))
	if err != nil {
		return err
	}
	for _, identity := range account.Identities {
		if _, err = fmt.Fprintf(entry, "Sign-in: %s %s (%s), linked %s\n", providerLabel(identity.Provider),
			exportText(identity.Subject), exportText(identity.Login), identity.CreatedAt.UTC().Format(time.RFC3339)); err != nil {
			return err
		}
	}
	return nil
}

func writeExportRemote(archive *zip.Writer, resource Resource) error {
	entry, err := archive.CreateHeader(&zip.FileHeader{Name: exportBodyPath(resource), Method: zip.Deflate, Modified: resource.UpdatedAt})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(entry, "[InternetShortcut]\r\nURL=%s\r\n", exportText(resource.OriginURL))
	return err
}

// exportBodyPath puts each body in a directory named after its resource so
// duplicate filenames cannot overwrite each other.
//
// A filename the owner set is kept exactly: Dockerfile, .env and .gitignore
// are what this service is for, and an export that renamed them to
// Dockerfile.txt or env.txt would hand back different files. Only a resource
// with no filename - one typed into the editor - gets a name made up for it,
// from its resource name plus an extension from its type. Either way the name
// is kept to one path segment, since the archive is unpacked by tools this
// service does not control.
func exportBodyPath(resource Resource) string {
	// Encrypted content keeps a fixed name: its own is inside it, and the
	// browser renames it once decrypted (see sealedExportName).
	if resource.Sealed() {
		return "files/" + resource.ID + "/" + sealedExportName
	}
	name := exportSegment(resource.Filename)
	if name == "" {
		name = strings.Trim(exportSegment(resource.Name), " .")
		if name == "" {
			name = "content"
		}
		if resource.OriginURL == "" {
			name += exportExtension(resource.ContentType)
		}
	}
	if resource.OriginURL != "" && !strings.HasSuffix(strings.ToLower(name), ".url") {
		name += ".url"
	}
	return "files/" + resource.ID + "/" + name
}

// exportSegment reduces a name to a single safe path segment: no separators,
// no control characters, and never "." or "..".
func exportSegment(value string) string {
	name := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' {
			return '_'
		}
		return r
	}, strings.TrimSpace(value))
	if name == "." || name == ".." {
		return ""
	}
	return name
}

func exportExtension(contentType string) string {
	mediaType, _, _ := mime.ParseMediaType(contentType)
	if extension, ok := map[string]string{
		"text/plain":               ".txt",
		"text/markdown":            ".md",
		"text/html":                ".html",
		"text/css":                 ".css",
		"text/csv":                 ".csv",
		"application/json":         ".json",
		"application/xml":          ".xml",
		"application/javascript":   ".js",
		"application/pdf":          ".pdf",
		"application/zip":          ".zip",
		"application/gzip":         ".gz",
		"image/png":                ".png",
		"image/jpeg":               ".jpg",
		"image/gif":                ".gif",
		"image/webp":               ".webp",
		"image/svg+xml":            ".svg",
		"audio/mpeg":               ".mp3",
		"audio/ogg":                ".ogg",
		"audio/wav":                ".wav",
		"video/mp4":                ".mp4",
		"video/webm":               ".webm",
		"application/octet-stream": ".bin",
	}[mediaType]; ok {
		return extension
	}
	if strings.HasPrefix(mediaType, "text/") {
		return ".txt"
	}
	return ".bin"
}

func exportText(value string) string {
	value = strings.ReplaceAll(value, "\r", " ")
	return strings.ReplaceAll(value, "\n", " ")
}

// exportFilePart keeps the download name to characters every browser and
// filesystem accept. A name with none of them, such as a Google name in
// another script, gives way to the start of the account ID.
func exportFilePart(user store.User) string {
	kept := false
	part := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			kept = true
			return r
		}
		if r == '-' || r == '_' {
			return r
		}
		return '_'
	}, user.Login)
	if !kept {
		return user.ID[:min(8, len(user.ID))]
	}
	return part
}

// sealedExportName is what an encrypted content is called in an export.
const sealedExportName = "content.sealed"

// writeExportSealed adds what an account's encrypted resources need to be
// opened away from this service: its keyring, still wrapped, and for each
// encrypted resource its wrapped content key and the encrypted metadata of
// it and of each earlier version. Nothing here opens without the master
// password or the recovery key. The account page decrypts the archive in the
// browser when it can; this is what remains when it cannot.
func (a *App) writeExportSealed(r *http.Request, archive *zip.Writer, userID string, resources []Resource, versions []store.Version, now time.Time) error {
	type sealedVersion struct {
		Number int    `json:"n"`
		Meta   string `json:"sealed_meta"`
	}
	type sealedResource struct {
		ID        string          `json:"id"`
		SealedKey string          `json:"sealed_key"`
		Meta      string          `json:"sealed_meta"`
		Versions  []sealedVersion `json:"versions"`
	}
	encode := base64.RawURLEncoding.EncodeToString
	var sealed []sealedResource
	index := map[string]int{}
	for _, resource := range resources {
		if resource.Sealed() {
			index[resource.ID] = len(sealed)
			sealed = append(sealed, sealedResource{ID: resource.ID, SealedKey: encode(resource.SealedKey), Meta: encode(resource.SealedMeta), Versions: []sealedVersion{}})
		}
	}
	if len(sealed) == 0 {
		return nil
	}
	for _, version := range versions {
		if i, ok := index[version.ResourceID]; ok && len(version.SealedMeta) > 0 {
			sealed[i].Versions = append(sealed[i].Versions, sealedVersion{Number: version.Number, Meta: encode(version.SealedMeta)})
		}
	}
	manifest := map[string]any{"format": "https://github.com/xwvike/plainmote/blob/main/docs/encryption.md", "resources": sealed}
	if keyring, err := a.db.Keyring(r.Context(), userID); err == nil {
		manifest["keyring"] = keyringJSON(keyring)
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	entry, err := archive.CreateHeader(&zip.FileHeader{Name: "sealed.json", Method: zip.Deflate, Modified: now})
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(entry)
	encoder.SetIndent("", "  ")
	return encoder.Encode(manifest)
}
