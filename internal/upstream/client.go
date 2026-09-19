package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// defaultMaxBytes is used when a caller does not set a ceiling. It matches the
// service's own default so the two limits agree out of the box.
const defaultMaxBytes = 4 << 20

const maxRedirects = 3

type Client struct {
	client   *http.Client
	maxBytes int64
}

func New(maxBytes int64) *Client {
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	u := &Client{maxBytes: maxBytes}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// A process-wide HTTP proxy would hide the actual upstream connection from
	// the address check below. Upstream resources therefore connect directly.
	transport.Proxy = nil
	transport.DialContext = validatedDialContext
	u.client = &http.Client{
		Timeout:   15 * time.Second,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > maxRedirects {
				return errors.New("too many redirects")
			}
			return ValidateURL(req.URL)
		},
	}
	return u
}

func ValidateURL(parsed *url.URL) error {
	if parsed == nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil {
		return errors.New("upstream URL must be HTTP(S) without user information")
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || host == "metadata.google.internal" {
		return errors.New("local upstream hosts are not allowed")
	}
	if ip := net.ParseIP(host); ip != nil {
		if isNonPublicIP(ip) {
			return errors.New("non-public upstream addresses are not allowed")
		}
		return nil
	}
	return nil
}

var additionalNonPublicPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

// IsPrivate only covers RFC 1918 and IPv6 ULA space. Upstream access also
// rejects addresses that are commonly routed inside deployments despite not
// carrying that label, including shared address space used by overlay networks.
func isNonPublicIP(ip net.IP) bool {
	if !ip.IsGlobalUnicast() || ip.IsPrivate() {
		return true
	}
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	address = address.Unmap()
	for _, prefix := range additionalNonPublicPrefixes {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

var upstreamDialer = net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}

// validatedDialContext resolves once, validates exactly those addresses, and
// dials the validated IP directly. This closes the DNS-rebinding gap between a
// preflight lookup and net/http's connection lookup.
func validatedDialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("parse upstream address: %w", err)
	}
	if ip := net.ParseIP(host); ip != nil {
		if isNonPublicIP(ip) {
			return nil, errors.New("upstream resolved to a non-public address")
		}
		return upstreamDialer.DialContext(ctx, network, address)
	}

	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("resolve upstream host: %w", err)
	}
	if len(addresses) == 0 {
		return nil, errors.New("upstream host has no address")
	}
	for _, resolved := range addresses {
		if isNonPublicIP(resolved.IP) {
			return nil, errors.New("upstream host resolves to a non-public address")
		}
	}

	var dialErrors []error
	for _, resolved := range addresses {
		connection, err := upstreamDialer.DialContext(ctx, network, net.JoinHostPort(resolved.String(), port))
		if err == nil {
			return connection, nil
		}
		dialErrors = append(dialErrors, err)
	}
	return nil, fmt.Errorf("connect upstream: %w", errors.Join(dialErrors...))
}

func (u *Client) Fetch(ctx context.Context, rawURL string) ([]byte, string, error) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, "", fmt.Errorf("parse upstream URL: %w", err)
	}
	if err := ValidateURL(parsed); err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, "", err
	}
	// Ask for the resource, not for a preferred shape of it. An Accept header
	// that ranks text formats is content negotiation, and a server that honours
	// it answers with something other than what the address returns by default
	// - GitHub's API, for one, drops from indented JSON to a single compact
	// line. This service forwards what an address serves; stating a preference
	// here would put a different document in front of every consumer of the
	// share link.
	req.Header.Set("Accept", "*/*")
	resp, err := u.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("fetch upstream: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("upstream returned HTTP %d", resp.StatusCode)
	}
	reader := io.LimitReader(resp.Body, u.maxBytes+1)
	body, err := io.ReadAll(reader)
	if err != nil {
		return nil, "", fmt.Errorf("read upstream response: %w", err)
	}
	if int64(len(body)) > u.maxBytes {
		return nil, "", fmt.Errorf("上游内容超过 %d MiB 上限", u.maxBytes>>20)
	}
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = http.DetectContentType(body)
	}
	return body, contentType, nil
}
