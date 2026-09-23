package web

import (
	"net/url"
	"testing"
)

// TestSafeNextOnlyStaysOnSite covers the redirects that follow login and the
// language switch. What matters is not what the string looks like but where a
// browser goes when handed it, so every refused case is one a browser resolves
// to another host.
func TestSafeNextOnlyStaysOnSite(t *testing.T) {
	for _, keep := range []string{
		"/", "/resources/", "/resources/abc?share=1", "/paste/abc", "/logs?outcome=expired",
		"/%5Cevil.com", // encoded, so a browser keeps it as a path segment
	} {
		if got := safeNext(keep); got != keep {
			t.Errorf("%q must be kept, got %q", keep, got)
		}
	}
	for _, refuse := range []string{
		"", "resources", "https://evil.com", "//evil.com", "///evil.com",
		"/\\evil.com", "/\\\\evil.com", "\\\\evil.com",
		"/\t/evil.com", "/\n/evil.com", "/\r/evil.com", "/\x00", "/\x7f",
		"/\\/evil.com",
	} {
		if got := safeNext(refuse); got != "" {
			t.Errorf("%q must be refused, got %q", refuse, got)
		}
	}
}

// TestSafeNextNeverLeavesTheOrigin checks the property rather than a list:
// whatever safeNext lets through resolves to this origin.
func TestSafeNextNeverLeavesTheOrigin(t *testing.T) {
	base, _ := url.Parse("https://plainmote.example")
	for _, candidate := range []string{
		"/", "/a", "/\\evil.com", "/\t/evil.com", "//evil.com", "/%09/evil.com",
		"/./evil.com", "/../evil.com", "/;evil.com", "/@evil.com", "/:evil.com",
	} {
		kept := safeNext(candidate)
		if kept == "" {
			continue
		}
		resolved, err := base.Parse(kept)
		if err != nil || resolved.Host != base.Host {
			t.Errorf("%q passed and resolves to %v", candidate, resolved)
		}
	}
}
