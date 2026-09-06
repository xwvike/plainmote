package config

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Listen               string
	DatabaseURL          string
	BlobEndpoint         string
	BlobBucket           string
	BlobRegion           string
	BlobAccessKey        string
	BlobSecretKey        string
	PublicURL            string
	AllowPrivateUpstream bool
	TrustedProxies       []netip.Prefix
	SessionTTL           time.Duration
	MaxContent           int64
	GitHubID             string
	GitHubSecret         string
	AllowedIDs           map[string]bool
	TokenKey             []byte
}

// Load reads deployment settings from the process environment. The service
// has no configuration file and no writable local state.
func Load() (Config, error) {
	cfg := Config{
		Listen:        env("PLAINMOTE_LISTEN", ":8964"),
		DatabaseURL:   strings.TrimSpace(os.Getenv("DATABASE_URL")),
		BlobEndpoint:  strings.TrimSpace(os.Getenv("PLAINMOTE_BLOB_ENDPOINT")),
		BlobBucket:    strings.TrimSpace(os.Getenv("PLAINMOTE_BLOB_BUCKET")),
		BlobRegion:    env("PLAINMOTE_BLOB_REGION", "auto"),
		BlobAccessKey: strings.TrimSpace(os.Getenv("PLAINMOTE_BLOB_ACCESS_KEY")),
		BlobSecretKey: strings.TrimSpace(os.Getenv("PLAINMOTE_BLOB_SECRET_KEY")),
		PublicURL:     strings.TrimRight(strings.TrimSpace(os.Getenv("PLAINMOTE_PUBLIC_URL")), "/"),
		GitHubID:      strings.TrimSpace(os.Getenv("GITHUB_CLIENT_ID")),
		GitHubSecret:  strings.TrimSpace(os.Getenv("GITHUB_CLIENT_SECRET")),
		AllowedIDs:    parseAllowedIDs(os.Getenv("GITHUB_ALLOWED_IDS")),
	}

	for _, required := range []struct{ name, value string }{
		{"DATABASE_URL", cfg.DatabaseURL},
		{"PLAINMOTE_PUBLIC_URL", cfg.PublicURL},
		{"PLAINMOTE_BLOB_ENDPOINT", cfg.BlobEndpoint},
		{"PLAINMOTE_BLOB_BUCKET", cfg.BlobBucket},
		{"PLAINMOTE_BLOB_ACCESS_KEY", cfg.BlobAccessKey},
		{"PLAINMOTE_BLOB_SECRET_KEY", cfg.BlobSecretKey},
		{"PLAINMOTE_TOKEN_KEY", strings.TrimSpace(os.Getenv("PLAINMOTE_TOKEN_KEY"))},
		{"GITHUB_CLIENT_ID", cfg.GitHubID},
		{"GITHUB_CLIENT_SECRET", cfg.GitHubSecret},
		{"GITHUB_ALLOWED_IDS", strings.TrimSpace(os.Getenv("GITHUB_ALLOWED_IDS"))},
	} {
		if required.value == "" {
			return Config{}, fmt.Errorf("%s is required", required.name)
		}
	}

	parsedPublicURL, err := url.Parse(cfg.PublicURL)
	if err != nil || parsedPublicURL.Scheme == "" || parsedPublicURL.Host == "" ||
		(parsedPublicURL.Scheme != "http" && parsedPublicURL.Scheme != "https") ||
		parsedPublicURL.Path != "" || parsedPublicURL.RawQuery != "" || parsedPublicURL.Fragment != "" {
		return Config{}, errors.New("PLAINMOTE_PUBLIC_URL must be an absolute HTTP(S) origin without a path")
	}

	cfg.AllowPrivateUpstream, err = parseBool("PLAINMOTE_ALLOW_PRIVATE_UPSTREAM", false)
	if err != nil {
		return Config{}, err
	}
	cfg.TrustedProxies, err = parseTrustedProxies(os.Getenv("PLAINMOTE_TRUSTED_PROXIES"))
	if err != nil {
		return Config{}, err
	}
	cfg.SessionTTL, err = parseDuration("PLAINMOTE_SESSION_TTL", 30*24*time.Hour)
	if err != nil {
		return Config{}, err
	}
	maxContentMiB, err := parsePositiveInt("PLAINMOTE_MAX_CONTENT_MIB", 4)
	if err != nil {
		return Config{}, err
	}
	cfg.MaxContent = int64(maxContentMiB) << 20
	cfg.TokenKey, err = parseSecretKey(os.Getenv("PLAINMOTE_TOKEN_KEY"))
	if err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func env(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func parseBool(name string, fallback bool) (bool, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", name, err)
	}
	return parsed, nil
}

func parseDuration(name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration", name)
	}
	return parsed, nil
}

func parsePositiveInt(name string, fallback int) (int, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return parsed, nil
}

func parseTrustedProxies(value string) ([]netip.Prefix, error) {
	items := strings.Split(value, ",")
	prefixes := make([]netip.Prefix, 0, len(items))
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(item)
		if err != nil {
			address, addressErr := netip.ParseAddr(item)
			if addressErr != nil {
				return nil, fmt.Errorf("invalid trusted proxy %q: use an IP address or CIDR", item)
			}
			prefix = netip.PrefixFrom(address, address.BitLen())
		}
		prefix = prefix.Masked()
		if key := prefix.String(); !seen[key] {
			seen[key] = true
			prefixes = append(prefixes, prefix)
		}
	}
	return prefixes, nil
}

func parseAllowedIDs(value string) map[string]bool {
	allowed := map[string]bool{}
	for item := range strings.SplitSeq(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			allowed[item] = true
		}
	}
	return allowed
}

func parseSecretKey(raw string) ([]byte, error) {
	value := strings.TrimSpace(raw)
	if decoded, err := hex.DecodeString(value); err == nil && len(decoded) == 32 {
		return decoded, nil
	}
	if decoded, err := base64.StdEncoding.DecodeString(value); err == nil && len(decoded) == 32 {
		return decoded, nil
	}
	if len(value) == 32 {
		return []byte(value), nil
	}
	return nil, errors.New("PLAINMOTE_TOKEN_KEY must be 32 bytes, 64 hex characters, or base64")
}
