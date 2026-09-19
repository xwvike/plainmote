package web

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"plainmote/internal/store"
)

// deliveryPrefix is the one path the service serves resources under. The
// token after it is the whole address: there is no user-chosen path any more,
// so nothing about a link says what it points at or who owns it.
const deliveryPrefix = "/d/"

const deliveryCookie = "plainmote_delivery"

// Browsers turn a top-level audio or video response into a generated media
// document whose <source> points back at the same URL. The source needs this
// one permission to load. The sandbox must retain the response's origin too:
// Chromium puts crossorigin=anonymous on its generated player, and an opaque
// sandbox origin would turn that same URL into a cross-origin request. Scripts,
// frames and every other subresource remain blocked.
const deliveredContentSecurityPolicy = "sandbox allow-same-origin; default-src 'none'; media-src 'self'"

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
		a.recordProbe(probeMalformed)
		writePlainError(w, http.StatusNotFound, "not found")
		return
	}
	now := time.Now().UTC()
	meta := a.requestMetadata(r)
	var grant string
	if cookie, cookieErr := r.Cookie(deliveryCookie); cookieErr == nil {
		grant = cookie.Value
	}
	result, resumed, err := a.db.ResumeDelivery(r.Context(), token, grant, now)
	if err == nil && !resumed {
		result, err = a.db.ConsumeToken(r.Context(), token, meta, now)
	}
	if err != nil {
		a.serverError(w, "consume token", err)
		return
	}
	if !result.Allowed {
		// A token nobody issued belongs to no resource, so the refusal has no
		// owner to read it. It is counted rather than stored; every refusal
		// that does belong to a link was already written by ConsumeToken.
		if result.Reason == store.ReasonInvalid {
			a.recordProbe(probeUnknown)
		}
		writePlainError(w, http.StatusUnauthorized, "link is not valid")
		return
	}
	if !resumed {
		grant, expires, err := a.db.IssueDeliveryGrant(token, result.LinkID, now)
		if err != nil {
			a.serverError(w, "issue delivery grant", err)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name: deliveryCookie, Value: grant, Path: deliveryPrefix + token,
			Expires: expires, MaxAge: int(time.Until(expires).Seconds()),
			HttpOnly: true,
			Secure:   strings.HasPrefix(strings.ToLower(a.baseURL(r)), "https://"),
			SameSite: http.SameSiteLaxMode,
		})
	}
	resource := result.Resource

	contentType := resource.ContentType
	filenameType := contentType
	var body io.ReadCloser
	var size int64
	var servedRange *byteRange
	if resource.Remote() {
		// Nothing is cached: every request goes back to the upstream, and its
		// content type is passed through unchanged. A failure is hard - the
		// token has already been spent, so the miss is logged separately.
		fetched, upstreamType, err := a.upstream.Fetch(r.Context(), resource.OriginURL)
		if err != nil {
			a.recordAccess(r, result, store.OutcomeUpstreamError, meta, http.StatusBadGateway, err.Error())
			writePlainError(w, http.StatusBadGateway, "upstream unavailable")
			return
		}
		contentType, size = store.SafeContentType(upstreamType), int64(len(fetched))
		filenameType = contentType
		servedRange, err = parseByteRange(r.Header.Get("Range"), size)
		if err != nil {
			a.recordAccess(r, result, store.OutcomeSuccess, meta, http.StatusRequestedRangeNotSatisfiable, "link accepted; invalid byte range")
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
			writePlainError(w, http.StatusRequestedRangeNotSatisfiable, "requested range is not satisfiable")
			return
		}
		if servedRange != nil {
			fetched = fetched[servedRange.start : servedRange.end+1]
		}
		body = io.NopCloser(bytes.NewReader(fetched))
	} else {
		contentType = store.ContentTypeWithEncoding(contentType, resource.ContentEncoding)
		size = resource.ContentSize
		servedRange, err = parseByteRange(r.Header.Get("Range"), size)
		if err != nil {
			a.recordAccess(r, result, store.OutcomeSuccess, meta, http.StatusRequestedRangeNotSatisfiable, "link accepted; invalid byte range")
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
			writePlainError(w, http.StatusRequestedRangeNotSatisfiable, "requested range is not satisfiable")
			return
		}
		if servedRange == nil {
			body, size, err = a.db.OpenContent(r.Context(), resource)
		} else {
			var opened int64
			body, opened, err = a.db.OpenContentRange(r.Context(), resource, servedRange.start, servedRange.end)
			if err == nil && opened != servedRange.length() {
				_ = body.Close()
				err = fmt.Errorf("blob range length %d, want %d", opened, servedRange.length())
			}
		}
		if err != nil {
			a.serverError(w, "open content", err)
			return
		}
	}
	defer body.Close()
	// The use was already spent when the token was consumed, so refusing to
	// deliver here would cost the caller the resource without buying back the
	// record it failed to write. Delivery goes ahead; the miss goes to stderr.
	status := http.StatusOK
	responseSize := size
	if servedRange != nil {
		status = http.StatusPartialContent
		responseSize = servedRange.length()
	}
	a.recordAccess(r, result, store.OutcomeSuccess, meta, status, "link accepted")
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", contentDisposition(deliveryFilename(resource, filenameType)))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", deliveredContentSecurityPolicy)
	w.Header().Set("Accept-Ranges", "bytes")
	if servedRange != nil {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", servedRange.start, servedRange.end, size))
	}
	if responseSize > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(responseSize, 10))
	}
	w.WriteHeader(status)
	// Streamed rather than buffered, so a large object costs a copy buffer
	// instead of its whole size in memory.
	_, _ = io.Copy(w, body)
}

// recordAccess writes one delivery event. The record matters, but it is not
// what the caller asked for: a failure here is reported to stderr and the
// response carries on, because the work it describes has already happened.
func (a *App) recordAccess(r *http.Request, result store.ConsumeResult, outcome string, meta store.RequestMeta, status int, detail string) {
	resource := result.Resource
	event := store.AccessEvent{
		OwnerID: resource.OwnerID, ResourceID: resource.ID,
		ResourceName: resource.Name, ResourceFile: resource.Filename,
		LinkID: result.LinkID, LinkName: result.LinkName,
		Outcome: outcome, Status: status, Detail: detail,
	}
	if err := a.db.RecordAccess(r.Context(), event, meta); err != nil {
		fmt.Fprintf(os.Stderr, "record %s access for resource %s: %v\n", outcome, resource.ID, err)
	}
}

// contentDisposition names the file on the way out. A header field is ASCII,
// so a name that is not gets both forms RFC 6266 describes: the quoted one
// with everything unrepresentable folded to an underscore, for a client that
// reads only that, and the extended one beside it, which every current browser
// and curl prefer.
//
// The quoted form escapes rather than relies on the name having been narrowed:
// a quote or a backslash is a perfectly ordinary character in a filename, and
// keeping the header well formed is this function's job, not the validator's.
// Control characters cannot reach here - validateFilename refuses them - but
// they are folded too, so a header line can never be ended early from here.
func contentDisposition(filename string) string {
	var ascii strings.Builder
	for _, r := range filename {
		switch {
		case r == '"' || r == '\\':
			ascii.WriteByte('\\')
			ascii.WriteRune(r)
		case r < 0x20 || r > 0x7e:
			ascii.WriteByte('_')
		default:
			ascii.WriteRune(r)
		}
	}
	fallback := ascii.String()
	value := `inline; filename="` + fallback + `"`
	if fallback != filename {
		value += "; filename*=UTF-8''" + encodeExtendedValue(filename)
	}
	return value
}

// encodeExtendedValue percent-encodes everything outside RFC 5987's attr-char
// set. url.PathEscape leaves several of those bytes alone, and a delimiter
// surviving into a header parameter is exactly what this must not allow.
func encodeExtendedValue(value string) string {
	const safe = "!#$&+-.^_`|~"
	var out strings.Builder
	for _, b := range []byte(value) {
		switch {
		case b >= '0' && b <= '9', b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z',
			strings.IndexByte(safe, b) >= 0:
			out.WriteByte(b)
		default:
			out.WriteString(fmt.Sprintf("%%%02X", b))
		}
	}
	return out.String()
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
