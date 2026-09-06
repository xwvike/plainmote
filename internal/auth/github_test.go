package auth

import (
	"net/url"
	"testing"
)

func TestGitHubAuthorizationURL(t *testing.T) {
	client := NewGitHub("client-id", "client-secret")
	parsed, err := url.Parse(client.AuthorizationURL("https://config.example/auth/github/callback", "state-value"))
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if parsed.Host != "github.com" || query.Get("client_id") != "client-id" || query.Get("state") != "state-value" || query.Get("scope") != "read:user" {
		t.Fatalf("unexpected authorization URL: %s", parsed.String())
	}
}

func TestGitHubStateIsOpaque(t *testing.T) {
	client := NewGitHub("client-id", "client-secret")
	first, err := client.NewState()
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.NewState()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 32 || first == second {
		t.Fatalf("unexpected OAuth states: %q %q", first, second)
	}
}
