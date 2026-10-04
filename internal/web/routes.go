package web

import (
	"context"
	"crypto/ed25519"
	"encoding/xml"
	"html/template"
	"io"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"time"

	"plainmote/internal/auth"
	"plainmote/internal/store"
)

type App struct {
	cfg       Config
	db        *store.Store
	upstream  upstreamFetcher
	github    *auth.GitHub
	templates *template.Template
	handler   http.Handler
	probes    probeLog
	admin     *adminGate
	limits    *apiLimits
	cli       *cliIndex
}

type upstreamFetcher interface {
	Fetch(context.Context, string) ([]byte, string, error)
}

type User = store.User
type Resource = store.Resource
type Link = store.Link
type AccessLog = store.AccessLog

type Config struct {
	PublicURL      string
	SessionTTL     time.Duration
	MaxContent     int64
	AllowedIDs     map[string]bool
	TrustedProxies []netip.Prefix
	// CloudflareLocation believes the visitor location headers Cloudflare
	// adds, when they come from a trusted proxy.
	CloudflareLocation bool
	RegistrationMode   auth.RegistrationMode
	AnonymousEnabled   bool
	LogRetention       time.Duration
	Operator           string
	ContactEmail       string
	SourceURL          string
	BlobEndpoint       string
	// The admin interface exists only when there is a key to call it with.
	AdminKeys    []ed25519.PublicKey
	AdminOrigins []string
	Version      string
	Revision     string
	StartedAt    time.Time
	// CLIDir holds the command line builds the install page offers; empty,
	// or without builds in it, the page says there are none.
	CLIDir string
}

func New(cfg Config, db *store.Store, source upstreamFetcher, github *auth.GitHub) *App {
	app := &App{cfg: cfg, db: db, upstream: source, github: github}
	if len(cfg.AdminKeys) > 0 {
		app.admin = newAdminGate(cfg.AdminKeys, cfg.AdminOrigins)
	}
	app.templates = app.templateSet()
	app.handler = app.routes()
	return app
}

func (a *App) Handler() http.Handler { return a.handler }

type pageData struct {
	User       User
	CSRF       string
	Active     string
	BaseURL    string
	MaxContent int64
	Error      string
	LoginURL   string
	SignInURL  string
	Locale     string
	Language   languageView
	Theme      themeView
	SourceURL  string
	// Canonical is the page's own address, as a path; Alternates are the same
	// page in its other languages. LocalePrefix is the language segment of a
	// language's own address ("" elsewhere), which the home links keep, and
	// LegalPrefix is where the footer finds the legal pages in this language.
	Canonical    string
	Alternates   []alternate
	LocalePrefix string
	LegalPrefix  string
	// The command line: the builds the install page lists, and the device
	// page's step - "code", "confirm", "done" or "denied" - with the sign-in
	// being decided and its code as shown.
	CLIBinaries []cliBinary
	CLIVersion  string
	CLIPlatform string
	CLILang     string
	DeviceStep  string
	DeviceGrant store.DeviceGrant
	DeviceCode  string
	// APITokens are the account page's signed-in command lines, and
	// RevokeToken the one its revoke dialog is about.
	APITokens   []store.APIToken
	RevokeToken store.APIToken
	// E2EE is whether this account's quick shares are encrypted in the browser.
	E2EE bool
	// Keyring is the account's master password, if it has one, and
	// KeyringLockChoices the idle times it may be kept unlocked.
	Keyring            *store.Keyring
	KeyringLockChoices []int
	RegistrationMode   auth.RegistrationMode
	Resources          []Resource
	Quota              store.UserQuota
	Pager              pager
	IsNew              bool
	NewKind            string
	SignedIn           bool
	// Indexable is set by the one page that is meant to be found. The response
	// header carries the same exception; both have to agree or the meta tag
	// quietly undoes it.
	Indexable bool
	// Anonymous is whether this deployment takes pastes from anyone at all.
	// With it off the home page is what the service is, and nothing more.
	Anonymous bool
	// LegalLinks is whether the about, privacy, terms and contact pages exist.
	LegalLinks bool
	Legal      legalView
	// Account is the account page's subject; Notice is a one-line
	// confirmation, used by the sign-in page after an account is deleted.
	Account store.Account
	Notice  string
	// Export is the account's standing against the export limit, which the
	// account page states and the export handler enforces.
	Export       store.ExportAllowance
	ExportLimit  int
	ExportWindow int

	// The home page, both halves of it: the form as it was submitted when a
	// paste was refused, and the address when one was made.
	PasteContent    string
	PasteFilename   string
	PasteTTL        string
	PasteChoices    []ttlChoice
	PasteURL        string
	PasteResourceID string
	PasteClaimable  bool
	// PasteOwned is a signed-in quick share, already in its creator's
	// resources.
	PasteOwned     bool
	PasteExpires   time.Time
	PasteEncrypted bool
	PasteGauge     template.CSS
	PasteEnd       template.CSS
	PasteSize      int64
	MaxPaste       int64

	// ContentText is the body loaded for the editor. It is only filled in for
	// a resource small and textual enough to show, so a large or non-text one is
	// never pulled into memory just to render a page.
	ContentText         string
	ContentEncoding     string
	ContentEOL          string
	TextEncodings       []encodingOption
	LogResource         string
	LogOutcome          string
	Outcomes            []string
	Resource            Resource
	MediaSource         string
	MediaAutoSave       bool
	Shares              []linkView
	EndedShares         []linkView
	ShareOpen           bool
	DeleteOpen          bool
	FocusShare          linkView
	AccessLogs          []AccessLog
	UpstreamType        string
	UpstreamText        string
	UpstreamTextPreview bool
	UpstreamError       string

	// Version history. HistoryCount is the number of earlier versions;
	// Versions the list, current first; HistoryGone the range of numbers the
	// retention rules removed, zero when none.
	HistoryCount  int
	HistoryBytes  int64
	HistoryKeep   int
	HistoryDays   int
	Versions      []versionRow
	HistoryGone   [2]int
	RemoveVersion int
	// The version page: the version shown, whether it is text, and its
	// neighbours among what is kept (NextVersion may be the current one).
	Viewed      store.Version
	ViewedText  bool
	PrevVersion int
	NextVersion int
	RestoreOpen bool
	// The comparison page.
	DiffChoices []int
	DiffFrom    versionBody
	DiffTo      versionBody
	Diff        textDiff
	DiffNote    string
	DiffFull    bool
	// Both sides, when they are shown next to each other rather than diffed.
	DiffSides []versionBody
	// A save refused because the content moved on while it was being edited,
	// and the version the edit was made against.
	Conflict     *store.VersionConflict
	ConflictBase int
	// One-line confirmations on the resource page after a creation, a save,
	// a restore, and a save that took earlier versions' room. SavedVersion is
	// the version a save made, 0 when it left the content as it was.
	// UndoVersion is what restoring back would bring back, 0 when that is
	// not on offer.
	Created      bool
	Saved        bool
	SavedVersion int
	RestoredFrom int
	UndoVersion  int
	Trimmed      int
	// Kept confirms a quick share just kept as a resource.
	Kept bool
}

// pager carries everything the dashboard footer needs; every link is a plain
// href so the list stays usable without JavaScript.
type pager struct {
	Query   string
	Kind    string
	Size    int
	Sizes   []int
	Total   int
	From    int
	To      int
	Links   []pageLink
	PrevURL string
	NextURL string
}

type pageLink struct {
	Num     int
	URL     string
	Current bool
}

type encodingOption struct {
	Value string
	Label string
}

// ttlChoice is one entry in the home page's lifetime picker. The values are
// minutes, which is the only unit an anonymous paste is ever measured in.
type ttlChoice struct {
	Value string
	// Duration is what Value stands for. Only the quick share picker sets it;
	// the share settings dialog reads its values as Go durations.
	Duration time.Duration
}

// linkView pairs a link with the address a viewer copies.
type linkView struct {
	Link      Link
	URL       string
	TTLChoice string
	TTLCustom string
	// For a link that has stopped working: why ("revoked", "expired",
	// "exhausted") and when.
	Ended   string
	EndedAt time.Time
}

func (a *App) routes() http.Handler {
	if a.limits == nil {
		a.limits = newAPILimits()
	}
	if a.cli == nil {
		a.cli = &cliIndex{}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/static/", a.handleStatic)
	mux.HandleFunc(faviconPath, a.handleFavicon)
	mux.HandleFunc("/healthz", a.handleHealth)
	mux.HandleFunc("/login", a.handleLogin)
	mux.HandleFunc("/auth/github", a.handleGitHubLogin)
	mux.HandleFunc("/auth/github/callback", a.handleGitHubCallback)
	mux.HandleFunc("/logout", a.handleLogout)
	mux.HandleFunc("/language", a.handleLanguage)
	mux.HandleFunc("/theme", a.handleTheme)
	mux.HandleFunc("/resources/", a.handleResources)
	mux.HandleFunc("/logs", a.handleLogs)
	mux.HandleFunc(accountPath, a.handleAccount)
	mux.HandleFunc(accountExportPath, a.handleAccountExport)
	mux.HandleFunc(accountDeletePath, a.handleAccountDelete)
	mux.HandleFunc(accountTokensPath, a.handleAccountTokens)
	mux.HandleFunc(accountKeyringPath, a.handleAccountKeyring)
	mux.HandleFunc(deliveryPrefix, a.handlePublic)
	mux.HandleFunc(apiPrefix, a.handleAPI)
	mux.HandleFunc(cliPath, a.handleCLI)
	mux.HandleFunc(cliPath+"/", a.handleCLIFiles)
	mux.HandleFunc(cliPowerShell, a.handleCLIPowerShell)
	mux.HandleFunc("/robots.txt", a.handleRobots)
	mux.HandleFunc("/sitemap.xml", a.handleSitemap)
	mux.HandleFunc("/llms.txt", a.handleLLMs)
	// Registered only with a key to call it by: without one the path does not
	// exist, the same as any other.
	if a.admin != nil {
		mux.HandleFunc(adminPrefix, a.handleAdmin)
		// The bare prefix would otherwise get the mux's redirect to the
		// subtree, which a deployment without the interface does not give:
		// answer it the way that deployment does.
		mux.HandleFunc(strings.TrimSuffix(adminPrefix, "/"), func(w http.ResponseWriter, r *http.Request) {
			writePlainError(w, http.StatusNotFound, "not found")
		})
	}
	// Not registered at all when it is off, so the endpoint does not exist
	// rather than existing and refusing.
	if a.cfg.AnonymousEnabled {
		mux.HandleFunc(pastePath, a.handlePaste)
		mux.HandleFunc(pasteResultPrefix, a.handlePasteResult)
		mux.HandleFunc(pasteSavePath, a.handleSavePaste)
		mux.HandleFunc(pasteEncryptedPath, a.handleEncryptedPaste)
		mux.HandleFunc(accountE2EEPath, a.handleAccountE2EE)
	}
	if a.cfg.ContactEmail != "" {
		for _, page := range legalPages {
			mux.HandleFunc("/"+page, a.handleLegal)
		}
	}
	for _, public := range publicLocales {
		mux.HandleFunc(public.prefix+"/", a.handleLocalized)
	}
	mux.HandleFunc("/", a.handleHome)
	return a.noIndex(mux)
}

// indexablePages are the paths meant to be found: the home page, where it is a
// page - with the box off it redirects into the half of the service that needs
// an account - and the about, privacy, terms and contact pages where they
// exist. Everything else needs a session or is somebody's secret. robots.txt,
// the sitemap and the X-Robots-Tag header are all derived from this list, so
// they cannot disagree about what may be indexed.
func (a *App) indexablePages() []string {
	var pages []string
	if a.cfg.AnonymousEnabled {
		pages = append(pages, "/")
	}
	if a.cfg.ContactEmail != "" {
		for _, page := range legalPages {
			pages = append(pages, "/"+page)
		}
	}
	return append(pages, a.localizedPaths()...)
}

func (a *App) indexable(path string) bool {
	return slices.Contains(a.indexablePages(), path)
}

// crawlerFileCache keeps robots.txt and the sitemap cacheable, briefly: a CDN
// honours it too, and a day of the previous rules outlasts any change to them.
const crawlerFileCache = "public, max-age=3600"

// staticPrefix holds the stylesheets, scripts and logo. They are not pages, but
// a crawler needs them to render the pages that are, and the logo is the icon
// a search result shows.
const staticPrefix = "/static/"

func (a *App) handleRobots(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", crawlerFileCache)
	if r.Method == http.MethodHead {
		return
	}
	var body strings.Builder
	body.WriteString("User-agent: *\n")
	pages := a.indexablePages()
	// "$" anchors the end, so /about is allowed and /about?x is not; the
	// longest matching rule wins, so these outrank the blanket Disallow.
	for _, page := range pages {
		body.WriteString("Allow: " + page + "$\n")
	}
	if len(pages) > 0 {
		// The sitemap too: the blanket Disallow below would otherwise keep a
		// crawler from reading the very file this points it to.
		body.WriteString("Allow: /sitemap.xml$\n")
		body.WriteString("Allow: /llms.txt$\n")
		body.WriteString("Allow: " + staticPrefix + "\n")
		body.WriteString("Allow: " + faviconPath + "$\n")
	}
	body.WriteString("Disallow: /\n")
	if len(pages) > 0 {
		body.WriteString("\nSitemap: " + strings.TrimRight(a.cfg.PublicURL, "/") + "/sitemap.xml\n")
	}
	_, _ = io.WriteString(w, body.String())
}

// handleSitemap lists the indexable pages. With none there is no sitemap.
func (a *App) handleSitemap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	pages := a.indexablePages()
	if len(pages) == 0 {
		writePlainError(w, http.StatusNotFound, "not found")
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", crawlerFileCache)
	if r.Method == http.MethodHead {
		return
	}
	base := strings.TrimRight(a.cfg.PublicURL, "/")
	var body strings.Builder
	body.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	body.WriteString(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9" xmlns:xhtml="http://www.w3.org/1999/xhtml">` + "\n")
	// Every address with its language versions beside it, as the pages
	// themselves declare them.
	for _, page := range pages {
		body.WriteString("  <url><loc>")
		_ = xml.EscapeText(&body, []byte(base+page))
		body.WriteString("</loc>")
		for _, link := range a.pageAlternates(base, page) {
			body.WriteString(`<xhtml:link rel="alternate" hreflang="` + link.Hreflang + `" href="`)
			_ = xml.EscapeText(&body, []byte(link.URL))
			body.WriteString(`"/>`)
		}
		body.WriteString("</url>\n")
	}
	body.WriteString("</urlset>\n")
	_, _ = io.WriteString(w, body.String())
}

// noIndex is the half of this that does not depend on a crawler asking first.
// robots.txt is a request; X-Robots-Tag travels with the response, so it also
// covers an address someone else published - a share link pasted into a public
// issue is a leak to be contained, not a page to be discovered. noarchive
// matters as much as noindex here: a cached copy would outlive a revoked link.
func (a *App) noIndex(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Indexable pages go without it, and so do static assets: a stylesheet
		// or a logo is not a page to keep out of an index, and the logo is what
		// a search result shows as the site's icon.
		if !a.indexable(r.URL.Path) && !strings.HasPrefix(r.URL.Path, staticPrefix) && r.URL.Path != faviconPath {
			w.Header().Set("X-Robots-Tag", "noindex, nofollow, noarchive")
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok\n")
}
