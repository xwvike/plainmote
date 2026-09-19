package web

import (
	"html/template"
	"io"
	"net/http"
	"net/netip"
	"time"

	"plainmote/internal/auth"
	"plainmote/internal/store"
	"plainmote/internal/upstream"
)

type App struct {
	cfg       Config
	db        *store.Store
	upstream  *upstream.Client
	github    *auth.GitHub
	templates *template.Template
	handler   http.Handler
	probes    probeLog
}

type User = store.User
type Resource = store.Resource
type Link = store.Link
type AccessLog = store.AccessLog

type Config struct {
	PublicURL        string
	SessionTTL       time.Duration
	MaxContent       int64
	AllowedIDs       map[string]bool
	TrustedProxies   []netip.Prefix
	RegistrationMode auth.RegistrationMode
}

func New(cfg Config, db *store.Store, source *upstream.Client, github *auth.GitHub) *App {
	app := &App{cfg: cfg, db: db, upstream: source, github: github}
	app.templates = app.templateSet()
	app.handler = app.routes()
	return app
}

func (a *App) Handler() http.Handler { return a.handler }

type pageData struct {
	User             User
	CSRF             string
	Active           string
	BaseURL          string
	MaxContent       int64
	Error            string
	LoginURL         string
	RegistrationMode auth.RegistrationMode
	Resources        []Resource
	Quota            store.UserQuota
	Pager            pager
	IsNew            bool
	NewKind          string
	SignedIn         bool
	// Indexable is set by the one page that is meant to be found. The response
	// header carries the same exception; both have to agree or the meta tag
	// quietly undoes it.
	Indexable bool

	// The home page, both halves of it: the form as it was submitted when a
	// paste was refused, and the address when one was made.
	PasteContent  string
	PasteFilename string
	PasteTTL      string
	PasteChoices  []ttlChoice
	PasteURL      string
	PasteExpires  time.Time
	PasteSize     int64
	MaxPaste      int64

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
	Shares              []linkView
	ShareOpen           bool
	DeleteOpen          bool
	FocusShare          linkView
	AccessLogs          []AccessLog
	UpstreamType        string
	UpstreamText        string
	UpstreamTextPreview bool
	UpstreamError       string
}

// pager carries everything the dashboard footer needs; every link is a plain
// href so the list stays usable without JavaScript.
type pager struct {
	Query   string
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
	Label string
}

// linkView pairs a link with the address a viewer copies.
type linkView struct {
	Link Link
	URL  string
}

func (a *App) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/static/", a.handleStatic)
	mux.HandleFunc("/healthz", a.handleHealth)
	mux.HandleFunc("/login", a.handleLogin)
	mux.HandleFunc("/auth/github", a.handleGitHubLogin)
	mux.HandleFunc("/auth/github/callback", a.handleGitHubCallback)
	mux.HandleFunc("/logout", a.handleLogout)
	mux.HandleFunc("/resources/", a.handleResources)
	mux.HandleFunc("/logs", a.handleLogs)
	mux.HandleFunc(deliveryPrefix, a.handlePublic)
	mux.HandleFunc("/robots.txt", a.handleRobots)
	mux.HandleFunc(pastePath, a.handlePaste)
	mux.HandleFunc("/", a.handleHome)
	return noIndex(mux)
}

// robotsTxt keeps crawlers off the whole service. There is nothing here to
// find: every page needs a session, and the one public path serves secrets.
const robotsTxt = "User-agent: *\nAllow: /$\nDisallow: /\n"

func (a *App) handleRobots(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.WriteString(w, robotsTxt)
}

// noIndex is the half of this that does not depend on a crawler asking first.
// robots.txt is a request; X-Robots-Tag travels with the response, so it also
// covers an address someone else published - a share link pasted into a public
// issue is a leak to be contained, not a page to be discovered. noarchive
// matters as much as noindex here: a cached copy would outlive a revoked link.
func noIndex(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The home page is the one thing here that is meant to be found. Every
		// other path is either behind a session or is somebody's secret.
		if r.URL.Path != "/" {
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
