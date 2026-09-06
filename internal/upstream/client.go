package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// defaultMaxBytes is used when a caller does not set a ceiling. It matches the
// service's own default so the two limits agree out of the box.
const defaultMaxBytes = 4 << 20

const maxRedirects = 3

type Client struct {
	client       *http.Client
	allowPrivate bool
	maxBytes     int64
}

func New(allowPrivate bool, maxBytes int64) *Client {
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	u := &Client{allowPrivate: allowPrivate, maxBytes: maxBytes}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// A process-wide HTTP proxy would hide the actual upstream connection from
	// the address check below. Upstream resources therefore connect directly.
	transport.Proxy = nil
	if !allowPrivate {
		transport.DialContext = validatedDialContext
	}
	u.client = &http.Client{
		Timeout:   15 * time.Second,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > maxRedirects {
				return errors.New("too many redirects")
			}
			return ValidateURL(req.URL, u.allowPrivate)
		},
	}
	return u
}

func ValidateURL(parsed *url.URL, allowPrivate bool) error {
	if parsed == nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.User != nil {
		return errors.New("upstream URL must be HTTP(S) without user information")
	}
	if allowPrivate {
		return nil
	}
	host := parsed.Hostname()
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") || strings.EqualFold(host, "metadata.google.internal") {
		return errors.New("local upstream hosts are not allowed")
	}
	if ip := net.ParseIP(host); ip != nil {
		if isPrivateIP(ip) {
			return errors.New("private upstream addresses are not allowed")
		}
		return nil
	}
	return nil
}

func isPrivateIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsInterfaceLocalMulticast()
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
		if isPrivateIP(ip) {
			return nil, errors.New("upstream resolved to a private address")
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
		if isPrivateIP(resolved.IP) {
			return nil, errors.New("upstream host resolves to a private address")
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
	if err := ValidateURL(parsed, u.allowPrivate); err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "text/plain, application/json, text/yaml, text/*;q=0.8, */*;q=0.1")
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
