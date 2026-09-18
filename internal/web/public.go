package web

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"plainmote/internal/store"
)

// deliveryPrefix is the one path the service serves resources under. The
// token after it is the whole address: there is no user-chosen path any more,
// so nothing about a link says what it points at or who owns it.
const deliveryPrefix = "/d/"

// shareAddress builds the address a link is handed out as: the token routes,
// and the filename rides along so whoever saves it gets a sensible name. The
// tail is never empty, because plain `curl -O` names the file after the last
// path segment and would otherwise write out the token.
func shareAddress(token, filename string) string {
	return deliveryPrefix + url.PathEscape(token) + "/" + url.PathEscape(filename)
}

// deliveryFilename is what a resource is called on the way out: its own name
// when it has one, otherwise something derived from what is about to be sent.
func deliveryFilename(resource store.Resource, contentType string) string {
	if resource.Filename != "" {
		return resource.Filename
	}
	return store.FallbackFilename(contentType)
}

// splitDeliveryPath pulls the token out of /d/<token> or /d/<token>/<filename>.
// The trailing name is decoration for whoever saves the file and takes no part
// in routing, so any name resolves.
func splitDeliveryPath(urlPath string) (token string, ok bool) {
	rest := strings.TrimPrefix(urlPath, deliveryPrefix)
	if rest == "" || rest == urlPath {
		return "", false
	}
	if cut := strings.IndexByte(rest, '/'); cut >= 0 {
		rest = rest[:cut]
	}
	if !store.ValidShareToken(rest) {
		return "", false
	}
	return rest, true
}

func (a *App) handlePublic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	token, ok := splitDeliveryPath(r.URL.Path)
	if !ok {
		writePlainError(w, http.StatusNotFound, "not found")
		return
	}
	meta := a.requestMetadata(r)
	result, err := a.db.ConsumeToken(r.Context(), token, meta, time.Now().UTC())
	if err != nil {
		writePlainError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !result.Allowed {
		writePlainError(w, http.StatusUnauthorized, "link is not valid")
		return
	}
	resource := result.Resource

	contentType := resource.ContentType
	filenameType := contentType
	var body io.ReadCloser
	var size int64
	if resource.Remote() {
		// Nothing is cached: every request goes back to the upstream, and its
		// content type is passed through unchanged. A failure is hard - the
		// token has already been spent, so the miss is logged separately.
		fetched, upstreamType, err := a.upstream.Fetch(r.Context(), resource.OriginURL)
		if err != nil {
			_ = a.db.RecordAccess(r.Context(), resource.ID, result.LinkID, result.LinkName, "upstream_error", meta, http.StatusBadGateway, err.Error())
			writePlainError(w, http.StatusBadGateway, "upstream unavailable")
			return
		}
		contentType, size = store.SafeContentType(upstreamType), int64(len(fetched))
		filenameType = contentType
		body = io.NopCloser(bytes.NewReader(fetched))
	} else {
		contentType = store.ContentTypeWithEncoding(contentType, resource.ContentEncoding)
		body, size, err = a.db.OpenContent(r.Context(), resource)
		if err != nil {
			writePlainError(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	defer body.Close()
	if err := a.db.RecordAccess(r.Context(), resource.ID, result.LinkID, result.LinkName, "success", meta, http.StatusOK, "link accepted"); err != nil {
		writePlainError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", contentType)
	// The name is restricted to letters, digits and . _ - + @ on the way in,
	// so it cannot break out of the quotes or the header.
	w.Header().Set("Content-Disposition", `inline; filename="`+deliveryFilename(resource, filenameType)+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	if size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	w.WriteHeader(http.StatusOK)
	// Streamed rather than buffered, so a large object costs a copy buffer
	// instead of its whole size in memory.
	_, _ = io.Copy(w, body)
}

// redactDeliveryPath keeps the token out of the audit log. It used to ride in
// the query string, which requestMetadata already scrubs; now it is a path
// segment and needs the same treatment.
func redactDeliveryPath(urlPath string) string {
	token, ok := splitDeliveryPath(urlPath)
	if !ok {
		return urlPath
	}
	return strings.Replace(urlPath, token, "[redacted]", 1)
}

func remoteIP(r *http.Request) string {
	host := r.RemoteAddr
	if parsed, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = parsed
	}
	return host
}

// behindTrustedProxy only trusts explicitly configured peers. A private source
// is not enough by itself: another container on the same network can set the
// same forwarding headers as nginx or cloudflared.
func (a *App) behindTrustedProxy(r *http.Request) bool {
	ip, err := netip.ParseAddr(remoteIP(r))
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	for _, prefix := range a.cfg.TrustedProxies {
		if prefix.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIP resolves who actually made the request. Without it the audit log
// records the proxy on every hit: the Docker gateway behind nginx, or
// 127.0.0.1 behind a tunnel.
func (a *App) clientIP(r *http.Request) string {
	direct := remoteIP(r)
	if !a.behindTrustedProxy(r) {
		return direct
	}
	if cf, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get("CF-Connecting-IP"))); err == nil {
		return cf.Unmap().String()
	}
	// The last entry is the one the nearest proxy appended, so it is the only
	// part of the chain a client cannot forge.
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		parts := strings.Split(forwarded, ",")
		candidate := strings.TrimSpace(parts[len(parts)-1])
		if address, err := netip.ParseAddr(candidate); err == nil {
			return address.Unmap().String()
		}
	}
	return direct
}

func (a *App) requestMetadata(r *http.Request) store.RequestMeta {
	query := r.URL.Query()
	if query.Has("token") {
		query.Set("token", "[redacted]")
	}
	return store.RequestMeta{
		RemoteIP:       a.clientIP(r),
		RemoteAddr:     r.RemoteAddr,
		Host:           r.Host,
		Query:          query.Encode(),
		Proto:          r.Proto,
		UserAgent:      r.UserAgent(),
		Referer:        r.Referer(),
		Forwarded:      r.Header.Get("Forwarded"),
		XForwardedFor:  r.Header.Get("X-Forwarded-For"),
		CFConnectingIP: r.Header.Get("CF-Connecting-IP"),
		CFRay:          r.Header.Get("CF-Ray"),
		ContentLength:  r.Header.Get("Content-Length"),
		TLS:            r.TLS != nil,
		Method:         r.Method,
		Path:           redactDeliveryPath(r.URL.Path),
	}
}
