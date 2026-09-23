package web

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"plainmote/internal/store"
)

const (
	accountPath       = "/account"
	accountExportPath = "/account/export"
	accountDeletePath = "/account/delete"
)

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
	a.renderAccount(w, r, user, "", false, http.StatusOK)
}

func (a *App) renderAccount(w http.ResponseWriter, r *http.Request, user User, pageError string, deleteOpen bool, status int) {
	account, err := a.db.Account(r.Context(), user.ID)
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	now := time.Now().UTC()
	allowance, err := a.db.ExportAllowance(r.Context(), user.ID, now)
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	quota, err := a.db.QuotaForUser(r.Context(), user.ID, now)
	if err != nil {
		a.renderError(w, http.StatusInternalServerError, err)
		return
	}
	data := a.basePage(r, user)
	data.Active = "account"
	data.Account = account
	data.Export = allowance
	data.Quota = quota
	data.ExportLimit = store.ExportLimit
	data.ExportWindow = int(store.ExportWindow / time.Hour)
	data.Error = pageError
	data.DeleteOpen = deleteOpen
	a.renderTemplate(w, r, status, "account.html", data)
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
		a.renderAccount(w, r, user, translate(requestLanguage(r).Locale, "delete_account_mismatch"), false, http.StatusBadRequest)
		return
	}
	if r.FormValue("final") != "1" {
		a.renderAccount(w, r, user, "", true, http.StatusOK)
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
	resources, links, err := a.db.ExportResources(ctx, user.ID)
	if err != nil {
		a.serverError(w, "export resources", err)
		return
	}

	// Spent last, once nothing is left that could fail before the download
	// starts: a refusal from an earlier step should not cost an export.
	now := time.Now().UTC()
	if _, err := a.db.ReserveExport(ctx, user.ID, now); err != nil {
		if errors.Is(err, store.ErrExportLimit) {
			a.renderAccount(w, r, user, translate(requestLanguage(r).Locale, "export_limit_reached"), false, http.StatusTooManyRequests)
			return
		}
		a.serverError(w, "reserve export", err)
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(now.Add(exportWriteTimeout))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="plainmote-%s-%s.zip"`, exportFilePart(account.User.Login), now.Format("20060102")))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	archive := zip.NewWriter(w)
	if err := a.writeExport(r, archive, account, resources, links, now); err != nil {
		fmt.Fprintf(os.Stderr, "export account %s: %v\n", user.ID, err)
		return
	}
	if err := archive.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "export account %s: %v\n", user.ID, err)
	}
}

type exportAccount struct {
	GitHubID   string    `json:"github_id"`
	Login      string    `json:"login"`
	Name       string    `json:"name"`
	AvatarURL  string    `json:"avatar_url"`
	CreatedAt  time.Time `json:"created_at"`
	ExportedAt time.Time `json:"exported_at"`
}

type exportResource struct {
	ID              string       `json:"id"`
	Name            string       `json:"name"`
	Filename        string       `json:"filename"`
	ContentType     string       `json:"content_type"`
	ContentEncoding string       `json:"content_encoding,omitempty"`
	Size            int64        `json:"size"`
	OriginURL       string       `json:"origin_url,omitempty"`
	File            string       `json:"file,omitempty"`
	CreatedAt       time.Time    `json:"created_at"`
	UpdatedAt       time.Time    `json:"updated_at"`
	Links           []exportLink `json:"links"`
}

type exportLink struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	MaxUses    int        `json:"max_uses"`
	UsedCount  int        `json:"used_count"`
	ExpiresAt  *time.Time `json:"expires_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	CreatedAt  time.Time  `json:"created_at"`
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
	FirstAt        time.Time `json:"first_at"`
	OccurredAt     time.Time `json:"occurred_at"`
}

func (a *App) writeExport(r *http.Request, archive *zip.Writer, account store.Account, resources []Resource, links []store.ExportLink, now time.Time) error {
	ctx := r.Context()
	if err := writeExportJSON(archive, "account.json", now, exportAccount{
		GitHubID: account.User.GitHubID, Login: account.User.Login, Name: account.User.Name,
		AvatarURL: account.User.AvatarURL, CreatedAt: account.CreatedAt, ExportedAt: now,
	}); err != nil {
		return err
	}

	byResource := make(map[string][]exportLink, len(resources))
	for _, link := range links {
		byResource[link.ResourceID] = append(byResource[link.ResourceID], exportLink{
			ID: link.ID, Name: link.Name, MaxUses: link.MaxUses, UsedCount: link.UsedCount,
			ExpiresAt: link.ExpiresAt, RevokedAt: link.RevokedAt, LastUsedAt: link.LastUsedAt, CreatedAt: link.CreatedAt,
		})
	}
	described := make([]exportResource, 0, len(resources))
	for _, resource := range resources {
		item := exportResource{
			ID: resource.ID, Name: resource.Name, Filename: resource.Filename,
			ContentType: resource.ContentType, ContentEncoding: resource.ContentEncoding,
			Size: resource.ContentSize, OriginURL: resource.OriginURL,
			CreatedAt: resource.CreatedAt, UpdatedAt: resource.UpdatedAt,
			Links: byResource[resource.ID],
		}
		if item.Links == nil {
			item.Links = []exportLink{}
		}
		if resource.ContentKey != "" {
			item.File = exportBodyPath(resource)
		}
		described = append(described, item)
	}
	if err := writeExportJSON(archive, "resources.json", now, described); err != nil {
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
			RemoteIP: entry.RemoteIP, RemoteAddr: entry.RemoteAddr, Method: entry.Method, Host: entry.Host, Path: entry.Path,
			Query: entry.Query, Proto: entry.Proto, TLS: entry.TLS, UserAgent: entry.UserAgent, Referer: entry.Referer,
			Forwarded: entry.Forwarded, XForwardedFor: entry.XForwardedFor, CFConnectingIP: entry.CFConnectingIP,
			CFRay: entry.CFRay, ContentLength: entry.ContentLength, Hits: entry.Hits, FirstAt: entry.FirstAt, OccurredAt: entry.OccurredAt,
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

	// Remote resources are described, not fetched: the export is what this
	// service holds, and requesting every upstream on the owner's behalf would
	// be a different thing entirely.
	for _, resource := range resources {
		if resource.ContentKey == "" {
			continue
		}
		if err := a.writeExportBody(r, archive, resource); err != nil {
			return fmt.Errorf("resource %s: %w", resource.ID, err)
		}
	}
	return nil
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

func writeExportJSON(archive *zip.Writer, name string, now time.Time, value any) error {
	entry, err := archive.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: now})
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(entry)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

// exportBodyPath puts each body in a directory named after its resource, so
// two resources with the same filename cannot overwrite each other when the
// archive is unpacked. The filename has already been validated not to contain
// a separator; the check is repeated because a path inside an archive is
// resolved by whatever tool unpacks it, not by this service.
func exportBodyPath(resource Resource) string {
	name := resource.Filename
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		name = "content"
	}
	return "files/" + resource.ID + "/" + name
}

// exportFilePart keeps the download name to characters every browser and
// filesystem accept. GitHub logins already are, so this only matters if that
// ever changes.
func exportFilePart(value string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, value)
}
