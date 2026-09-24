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

// deliveryPrefix serves shares; each request for resource bytes consumes one
// use. The token selects the share; the trailing filename is decorative.
const deliveryPrefix = "/d/"

// Raw content is sandboxed with scripts and frames blocked. Same-origin media
// remains allowed for authenticated raw previews, including Chromium's native
// player. The public Blob player uses its own policy in renderMediaPlayer.
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

// embeddedCrossSite reports a request that another site's page made for one
// of its elements - an <img>, <video>, <iframe>, a fetch() - rather than a
// person opening the link. A share address is something to open or download;
// letting other pages load it as a subresource turns it into free hosting,
// with every page view costing a use, a log row and a storage read here.
//
// It relies on Fetch Metadata, which browsers send and nothing else does. A
// request without it - curl, a chat app building a preview, an old browser -
// is not what embedding looks like and is let through; for the old browser,
// Cross-Origin-Resource-Policy on the response keeps the bytes off the page.
func embeddedCrossSite(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") != "cross-site" {
		return false
	}
	return r.Header.Get("Sec-Fetch-Mode") != "navigate" || r.Header.Get("Sec-Fetch-Dest") != "document"
}

func (a *App) handlePublic(w http.ResponseWriter, r *http.Request) {
	w.Header().Add("Vary", "Accept")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	// Refused before the token is even read: an embed costs no use, no log row
	// and no storage read, only a count in the per-minute probe line.
	if embeddedCrossSite(r) {
		a.recordProbe(probeEmbed)
		writePlainError(w, http.StatusForbidden, "this link cannot be embedded in another site")
		return
	}
	token, ok := splitDeliveryPath(r.URL.Path)
	if !ok {
		a.recordProbe(probeMalformed)
		writePlainError(w, http.StatusNotFound, "not found")
		return
	}
	// HTML negotiation affects presentation only. Every request for bytes below
	// consumes one use, regardless of Cookie, Range or other client headers.
	if wantsMediaPlayer(r) {
		media, err := a.db.IsMediaShare(r.Context(), token)
		if err != nil {
			a.serverError(w, "identify media share", err)
			return
		}
		if media {
			a.renderMediaPlayer(w, r)
			return
		}
	}
	meta := a.requestMetadata(r)
	result, err := a.db.ConsumeToken(r.Context(), token, meta, time.Now().UTC())
	if err != nil {
		a.serverError(w, "consume token", err)
		return
	}
	if !result.Allowed {
		if result.Reason == store.ReasonInvalid {
			a.recordProbe(probeUnknown)
		}
		writePlainError(w, http.StatusUnauthorized, "link is not valid")
		return
	}
	resource := result.Resource
	contentType := resource.ContentType
	filenameType := contentType
	var body io.ReadCloser
	var size int64
	if resource.Remote() {
		fetched, upstreamType, err := a.upstream.Fetch(r.Context(), resource.OriginURL)
		if err != nil {
			a.recordAccess(r, result, store.OutcomeUpstreamError, meta, http.StatusBadGateway, err.Error())
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
			a.serverError(w, "open content", err)
			return
		}
	}
	defer body.Close()
	a.recordAccess(r, result, store.OutcomeSuccess, meta, http.StatusOK, "link accepted")
	w.Header().Set("Content-Type", contentType)
	disposition := contentDisposition(deliveryFilename(resource, filenameType))
	if (!store.TextLike(contentType) && !strings.HasPrefix(contentType, "image/")) || r.URL.Query().Get("download") == "1" {
		disposition = strings.Replace(disposition, "inline;", "attachment;", 1)
	}
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", deliveredContentSecurityPolicy)
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("Referrer-Policy", "no-referrer")
	// Public delivery is always full-body. In particular, Range never opens an
	// exemption from accounting, and a retry is a new counted request.
	w.Header().Set("Accept-Ranges", "none")
	if size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	w.WriteHeader(http.StatusOK)
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

func redactSensitiveQuery(rawQuery string) string {
	if rawQuery == "" {
		return ""
	}
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return "[redacted]"
	}
	for _, key := range []string{"token", "grant"} {
		if values.Has(key) {
			values.Set(key, "[redacted]")
		}
	}
	return values.Encode()
}

func redactSensitiveURL(rawURL string) string {
	if rawURL == "" {
		return ""
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "[redacted]"
	}
	parsed.Path = redactDeliveryPath(parsed.Path)
	parsed.RawPath = ""
	parsed.RawQuery = redactSensitiveQuery(parsed.RawQuery)
	return parsed.String()
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
	return store.RequestMeta{
		RemoteIP:       a.clientIP(r),
		RemoteAddr:     r.RemoteAddr,
		Host:           r.Host,
		Query:          redactSensitiveQuery(r.URL.RawQuery),
		Proto:          r.Proto,
		UserAgent:      r.UserAgent(),
		Referer:        redactSensitiveURL(r.Referer()),
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
