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
	Pager            pager
	IsNew            bool
	NewKind          string

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

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok\n")
}
