package web

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// webAssets contains the templates and browser assets shipped with the Web
// service. The explicit staticTypes list below remains the public boundary.
//
//go:embed templates/*.html static/style.css static/media.css static/media.js static/logo.png static/editor.js static/upload.js static/resource.js static/time.js static/vendor/codemirror.js
var webAssets embed.FS

var staticTypes = map[string]string{
	"style.css":            "text/css; charset=utf-8",
	"media.css":            "text/css; charset=utf-8",
	"media.js":             "text/javascript; charset=utf-8",
	"logo.png":             "image/png",
	"editor.js":            "text/javascript; charset=utf-8",
	"upload.js":            "text/javascript; charset=utf-8",
	"resource.js":          "text/javascript; charset=utf-8",
	"time.js":              "text/javascript; charset=utf-8",
	"vendor/codemirror.js": "text/javascript; charset=utf-8",
}

type staticAsset struct {
	contentType string
	body        []byte
	gzipped     []byte
	etag        string
}

var staticAssets = loadStaticAssets()

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

func (a *App) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/static/")
	asset, ok := staticAssets[name]
	if !ok {
		writePlainError(w, http.StatusNotFound, "not found")
		return
	}
	body := asset.body
	w.Header().Set("Content-Type", asset.contentType)
	w.Header().Set("Cache-Control", "public, no-cache")
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
