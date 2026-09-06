package web

import (
	"embed"
	"html/template"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"plainmote/internal/auth"
	"plainmote/internal/store"
	"plainmote/internal/upstream"
)

//go:embed templates/*.html static/style.css static/logo.png
var webAssets embed.FS

// staticAssets is the whole set of files served under /static/. Serving from an
// explicit list rather than the embedded tree keeps the content type off
// extension guessing, and means a file added to the directory is not exposed
// until it is named here.
var staticAssets = map[string]string{
	"style.css": "text/css; charset=utf-8",
	"logo.png":  "image/png",
}

type App struct {
	cfg       Config
	db        *store.Store
	upstream  *upstream.Client
	github    *auth.GitHub
	templates *template.Template
	handler   http.Handler
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
}

func New(cfg Config, db *store.Store, source *upstream.Client, github *auth.GitHub) *App {
	app := &App{cfg: cfg, db: db, upstream: source, github: github}
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
	Resources  []Resource
	Pager      pager
	IsNew      bool
	NewKind    string

	// ContentText is the body loaded for the editor. It is only filled in for
	// a resource small and textual enough to show, so a large or binary one is
	// never pulled into memory just to render a page.
	ContentText         string
	LogResource         string
	LogOutcome          string
	Outcomes            []string
	Resource            Resource
	Shares              []linkView
	ShareOpen           bool
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
	mux.HandleFunc("/", a.handleDashboardRoot)
	return mux
}

func (a *App) handleStatic(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/static/")
	contentType, ok := staticAssets[name]
	if !ok {
		writePlainError(w, http.StatusNotFound, "not found")
		return
	}
	data, err := webAssets.ReadFile("static/" + name)
	if err != nil {
		writePlainError(w, http.StatusNotFound, "not found")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(data)
}

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok\n")
}
