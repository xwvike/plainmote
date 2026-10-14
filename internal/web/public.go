package web

import (
	"bytes"
	"context"
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

// deliveredPolicy is the policy for one response. An image opened on its own
// is shown by the browser's image viewer, which centres it on a dark ground
// with inline styles; without them Chrome leaves it in the top left corner.
// The body is an image, not markup, so there is nothing an allowed style
// could come from but the browser itself - and scripts stay forbidden.
func deliveredPolicy(contentType string) string {
	if strings.HasPrefix(contentType, "image/") {
		return deliveredContentSecurityPolicy + "; style-src 'unsafe-inline'"
	}
	return deliveredContentSecurityPolicy
}

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
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
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
	// HTML negotiation affects presentation only. Every request answered below
	// consumes one use, regardless of Cookie, Range or other client headers - a
	// HEAD, and a conditional request answered 304, included.
	if r.Method == http.MethodGet && wantsMediaPlayer(r) {
		shell, err := a.db.ShareShell(r.Context(), token)
		if err != nil {
			a.serverError(w, "identify share presentation", err)
			return
		}
		switch shell {
		case store.ShellMedia:
			a.renderMediaPlayer(w, r)
			return
		case store.ShellEncrypted:
			a.renderDecryptPage(w, r)
			return
		}
	}
	meta := a.requestMetadata(r)
	result, err := a.db.ConsumeTokenAnswering(r.Context(), token, meta, time.Now().UTC(), func(resource store.Resource) store.Answer {
		_, _, answer := deliveryAnswer(r, resource)
		return answer
	})
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
	// From here the use is spent and recorded as delivered; each way the
	// delivery can still end without sending the content corrects that row.
	resource := result.Resource
	contentType, etag, answer := deliveryAnswer(r, resource)
	w.Header().Set("ETag", etag)
	w.Header().Set("Last-Modified", resource.UpdatedAt.UTC().Format(http.TimeFormat))
	w.Header().Set("Cache-Control", "no-store")
	if answer.Status == http.StatusNotModified {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body := io.NopCloser(bytes.NewReader(nil))
	size := resource.ContentSize
	if r.Method != http.MethodHead {
		body, size, err = a.db.OpenContent(r.Context(), resource)
		if err != nil {
			a.amendAccess(r, result, store.OutcomeSuccess, http.StatusInternalServerError, "stored content could not be read", false)
			a.serverError(w, "open content", err)
			return
		}
	}
	// An encrypted resource goes out with what the address's key needs to
	// open it: the content key wrapped for this link, and the encrypted
	// metadata, ahead of the encrypted content. Ciphertext to every client;
	// a browser was given the decryption page first.
	if resource.Sealed() {
		prefix, err := store.SealedBundle(resource.ID, result.LinkSealedKey, resource.SealedMeta)
		if err != nil {
			a.amendAccess(r, result, store.OutcomeSuccess, http.StatusInternalServerError, "sealed bundle", false)
			a.serverError(w, "sealed bundle", err)
			return
		}
		body = struct {
			io.Reader
			io.Closer
		}{io.MultiReader(bytes.NewReader(prefix), body), body}
		size += int64(len(prefix))
	}
	defer body.Close()
	w.Header().Set("Content-Type", contentType)
	disposition := contentDisposition(deliveryFilename(resource, resource.ContentType))
	if !shownInline(contentType) || r.URL.Query().Get("download") == "1" {
		disposition = strings.Replace(disposition, "inline;", "attachment;", 1)
	}
	w.Header().Set("Content-Disposition", disposition)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", deliveredPolicy(contentType))
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	w.Header().Set("Referrer-Policy", "no-referrer")
	// Public delivery is always full-body. In particular, Range never opens an
	// exemption from accounting, and a retry is a new counted request.
	w.Header().Set("Accept-Ranges", "none")
	if size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, body)
	}
}

// deliveryAnswer is how a use of a link to resource is answered: the type it
// goes out as, its validator, and whether the answer sends no content - a
// 304 when the caller already holds this version, or the headers of a HEAD.
// It reads the resource row alone, so it can be settled before the use is
// recorded.
func deliveryAnswer(r *http.Request, resource store.Resource) (string, string, store.Answer) {
	contentType := store.ContentTypeWithEncoding(resource.ContentType, resource.ContentEncoding)
	etag := storedETag(resource, contentType)
	switch {
	case notModified(r, etag, resource.UpdatedAt):
		return contentType, etag, store.Answer{Status: http.StatusNotModified, Detail: "not modified"}
	case r.Method == http.MethodHead:
		return contentType, etag, store.Answer{Status: http.StatusOK, Detail: "headers only"}
	}
	return contentType, etag, store.Answer{}
}

// xmlType is XML in any of its media types. A browser renders the XHTML and
// SVG an XML document may hold as pages of their own.
func xmlType(contentType string) bool {
	base, _, _ := strings.Cut(contentType, ";")
	base = strings.ToLower(strings.TrimSpace(base))
	return base == "application/xml" || base == "text/xml" || strings.HasSuffix(base, "+xml")
}

// shownInline is content a browser may display where the link is opened:
// text and pictures. XML is text, but is saved rather than shown, so that
// whatever page it holds is never rendered on this origin, sandboxed or not.
func shownInline(contentType string) bool {
	if xmlType(contentType) {
		return false
	}
	return store.TextLike(contentType) || strings.HasPrefix(contentType, "image/")
}

// amendAccess corrects the row ConsumeToken wrote for this use: no content
// went out. The correction is made even if the caller has gone - it is the
// record that must be right - and a failure leaves the row overstating the
// delivery, never missing it.
func (a *App) amendAccess(r *http.Request, result store.ConsumeResult, outcome string, status int, detail string, delivered bool) {
	if result.AccessID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	if err := a.db.AmendAccess(ctx, result.AccessID, outcome, status, detail, delivered); err != nil {
		fmt.Fprintf(os.Stderr, "amend %s access for resource %s: %v\n", outcome, result.Resource.ID, err)
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
		if r.Header.Get("CF-Connecting-IP") != "" || r.Header.Get("X-Forwarded-For") != "" {
			a.untrustedProxy.Do(func() {
				fmt.Fprintf(os.Stderr, "%s sent forwarding headers but is not in PLAINMOTE_TRUSTED_PROXIES: "+
					"if it is the proxy in front of this service, every client is counted as that one address, "+
					"by the rate limits and in the access history\n", direct)
			})
		}
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
		Location:       a.visitorLocation(r),
	}
}
