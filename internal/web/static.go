package web

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// webAssets contains the templates and browser assets shipped with the Web
// service. The explicit staticTypes list below remains the public boundary.
//
//go:embed templates/*.html static/style.css static/noscript.css static/media.css static/media.js static/logo.png static/favicon.ico static/icon-192.png static/apple-touch-icon.png static/editor.js static/upload.js static/resource.js static/parts.js static/copy.js static/time.js static/e2ee.js static/e2ee-share.js static/e2ee-open.js static/keys.js static/vault.js static/lock.js static/keyring.js static/seal.js static/unlock.js static/sealed.js static/sealed-view.js static/diff.js static/sealed-names.js static/zip.js static/export.js static/vendor/codemirror.js
var webAssets embed.FS

var staticTypes = map[string]string{
	"style.css":            "text/css; charset=utf-8",
	"noscript.css":         "text/css; charset=utf-8",
	"media.css":            "text/css; charset=utf-8",
	"media.js":             "text/javascript; charset=utf-8",
	"logo.png":             "image/png",
	"favicon.ico":          "image/x-icon",
	"icon-192.png":         "image/png",
	"apple-touch-icon.png": "image/png",
	"editor.js":            "text/javascript; charset=utf-8",
	"upload.js":            "text/javascript; charset=utf-8",
	"resource.js":          "text/javascript; charset=utf-8",
	"parts.js":             "text/javascript; charset=utf-8",
	"copy.js":              "text/javascript; charset=utf-8",
	"time.js":              "text/javascript; charset=utf-8",
	"e2ee.js":              "text/javascript; charset=utf-8",
	"e2ee-share.js":        "text/javascript; charset=utf-8",
	"e2ee-open.js":         "text/javascript; charset=utf-8",
	"keys.js":              "text/javascript; charset=utf-8",
	"vault.js":             "text/javascript; charset=utf-8",
	"lock.js":              "text/javascript; charset=utf-8",
	"keyring.js":           "text/javascript; charset=utf-8",
	"seal.js":              "text/javascript; charset=utf-8",
	"unlock.js":            "text/javascript; charset=utf-8",
	"sealed.js":            "text/javascript; charset=utf-8",
	"sealed-view.js":       "text/javascript; charset=utf-8",
	"diff.js":              "text/javascript; charset=utf-8",
	"sealed-names.js":      "text/javascript; charset=utf-8",
	"zip.js":               "text/javascript; charset=utf-8",
	"export.js":            "text/javascript; charset=utf-8",
	"vendor/codemirror.js": "text/javascript; charset=utf-8",
}

type staticAsset struct {
	contentType string
	body        []byte
	gzipped     []byte
	etag        string
}

var staticAssets = loadStaticAssets()

// staticVersion names this build's set of assets. Pages link to them under
// /static/v/<version>/, so a deploy that changes any of them changes every
// address: the proxy in front may hold a stylesheet for hours whatever the
// service asks, and new markup must never meet an old one. One version for
// the whole set, rather than one per file, is what keeps a module's relative
// imports in the same release as the module.
var staticVersion = func() string {
	names := make([]string, 0, len(staticAssets))
	for name := range staticAssets {
		names = append(names, name)
	}
	slices.Sort(names)
	digest := sha256.New()
	for _, name := range names {
		fmt.Fprintf(digest, "%s %s\n", name, staticAssets[name].etag)
	}
	return fmt.Sprintf("%x", digest.Sum(nil)[:6])
}()

// staticVersionPrefix is where the current assets are linked from.
var staticVersionPrefix = staticPrefix + "v/" + staticVersion + "/"

// assetPath is the address a page links an asset by.
func assetPath(name string) string {
	return staticVersionPrefix + name
}

func loadStaticAssets() map[string]staticAsset {
	assets := make(map[string]staticAsset, len(staticTypes))
	for name, contentType := range staticTypes {
		body, err := webAssets.ReadFile("static/" + name)
		if err != nil {
			panic("static asset " + name + ": " + err.Error())
		}
		digest := sha256.Sum256(body)
		assets[name] = staticAsset{
			contentType: contentType,
			body:        body,
			gzipped:     compressStatic(body),
			etag:        fmt.Sprintf(`W/"%x"`, digest[:12]),
		}
	}
	return assets
}

func compressStatic(body []byte) []byte {
	var buffer bytes.Buffer
	writer, err := gzip.NewWriterLevel(&buffer, gzip.BestCompression)
	if err != nil {
		return nil
	}
	if _, err := writer.Write(body); err != nil {
		return nil
	}
	if err := writer.Close(); err != nil {
		return nil
	}
	if buffer.Len() >= len(body)*9/10 {
		return nil
	}
	return bytes.Clone(buffer.Bytes())
}

// acceptsEncoding implements the q-value part of Accept-Encoding needed by
// this service. An explicit coding overrides a wildcard, and malformed quality
// values are refused rather than turning a client's q=0 into an opt-in.
func acceptsEncoding(header, wanted string) bool {
	explicit, wildcard := -1.0, -1.0
	for item := range strings.SplitSeq(header, ",") {
		parts := strings.Split(item, ";")
		coding := strings.TrimSpace(parts[0])
		quality := 1.0
		for _, parameter := range parts[1:] {
			name, raw, found := strings.Cut(parameter, "=")
			if !found || !strings.EqualFold(strings.TrimSpace(name), "q") {
				continue
			}
			value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
			if err != nil || value < 0 || value > 1 {
				quality = 0
			} else {
				quality = value
			}
		}
		switch {
		case strings.EqualFold(coding, wanted):
			explicit = quality
		case coding == "*":
			wildcard = quality
		}
	}
	if explicit >= 0 {
		return explicit > 0
	}
	return wildcard > 0
}

func etagMatches(header, etag string) bool {
	for candidate := range strings.SplitSeq(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || candidate == etag {
			return true
		}
	}
	return false
}

// faviconPath is where browsers and search engines look for a site's icon
// whether or not a page names one. It is the same file as any static asset.
const faviconPath = "/favicon.ico"

func (a *App) handleFavicon(w http.ResponseWriter, r *http.Request) {
	icon := r.Clone(r.Context())
	icon.URL.Path = staticPrefix + "favicon.ico"
	a.handleStatic(w, icon)
}

func (a *App) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name := strings.TrimPrefix(r.URL.Path, staticPrefix)
	// A versioned address of this build never changes content, so it may be
	// kept for good. Any other - the bare name, or the version an older page
	// still links - is answered with what is current, which is why it has to
	// be checked again on every use.
	cache := "public, no-cache"
	if rest, ok := strings.CutPrefix(name, "v/"); ok {
		version, file, _ := strings.Cut(rest, "/")
		if version == staticVersion {
			cache = "public, max-age=31536000, immutable"
		}
		name = file
	}
	asset, ok := staticAssets[name]
	if !ok {
		writePlainError(w, http.StatusNotFound, "not found")
		return
	}
	body := asset.body
	w.Header().Set("Content-Type", asset.contentType)
	w.Header().Set("Cache-Control", cache)
	w.Header().Set("ETag", asset.etag)
	if asset.gzipped != nil {
		w.Header().Set("Vary", "Accept-Encoding")
		if acceptsEncoding(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			body = asset.gzipped
		}
	}
	if etagMatches(r.Header.Get("If-None-Match"), asset.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}
