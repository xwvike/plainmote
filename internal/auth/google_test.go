package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func idToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	body, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	encode := base64.RawURLEncoding.EncodeToString
	return encode([]byte(`{"alg":"RS256"}`)) + "." + encode(body) + "." + encode([]byte("signature"))
}

func validClaims(flow Flow, now time.Time) map[string]any {
	return map[string]any{
		"iss": "https://accounts.google.com", "aud": "client-id", "sub": "1234567890",
		"exp": now.Add(time.Hour).Unix(), "nonce": flow.Nonce,
		"name": "Alice Example", "picture": "https://example.test/a.png",
	}
}

func TestGoogleAuthorizationURL(t *testing.T) {
	client := NewGoogle("client-id", "client-secret")
	flow, err := NewFlow()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(client.AuthorizationURL("https://cfg.test/auth/google/callback", flow))
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	challenge := sha256.Sum256([]byte(flow.Verifier))
	switch {
	case parsed.Host != "accounts.google.com":
		t.Fatalf("host %q", parsed.Host)
	case query.Get("scope") != "openid profile":
		t.Fatalf("scope %q: only the name and picture are asked for, never the email address", query.Get("scope"))
	case query.Get("state") != flow.State || query.Get("nonce") != flow.Nonce:
		t.Fatalf("state or nonce missing: %s", parsed)
	case query.Get("code_challenge_method") != "S256" || query.Get("code_challenge") != base64.RawURLEncoding.EncodeToString(challenge[:]):
		t.Fatalf("PKCE challenge does not match the verifier: %s", parsed)
	case strings.Contains(parsed.RawQuery, flow.Verifier):
		t.Fatal("the PKCE verifier must never leave the server")
	case query.Has("prompt"):
		t.Fatalf("prompt %q makes Google ask on every sign-in", query.Get("prompt"))
	}
}

func TestFlowRoundTrips(t *testing.T) {
	flow, err := NewFlow()
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewFlow()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := ParseFlow(flow.Encode()); !ok || got != flow {
		t.Fatalf("ParseFlow(Encode()) = %+v %v", got, ok)
	}
	if flow.State == other.State || flow.Nonce == other.Nonce || flow.Verifier == other.Verifier {
		t.Fatal("two flows share a value")
	}
	for _, bad := range []string{"", "a.b", "a.b.c.d", ".b.c", "a..c", "a.b."} {
		if _, ok := ParseFlow(bad); ok {
			t.Fatalf("ParseFlow(%q) accepted a malformed flow", bad)
		}
	}
}

// The ID token decides which account a sign-in reaches, so every claim that
// ties it to this client and this sign-in must hold.
func TestGoogleIDTokenChecks(t *testing.T) {
	client := NewGoogle("client-id", "client-secret")
	flow, err := NewFlow()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	claims, err := client.claims(idToken(t, validClaims(flow, now)), flow, now)
	if err != nil || claims.Subject != "1234567890" {
		t.Fatalf("a valid token was refused: %+v %v", claims, err)
	}
	if _, err := client.claims(idToken(t, map[string]any{
		"iss": "accounts.google.com", "aud": "client-id", "sub": "1", "exp": now.Add(time.Minute).Unix(), "nonce": flow.Nonce,
	}), flow, now); err != nil {
		t.Fatalf("Google's issuer without the scheme was refused: %v", err)
	}
	for _, tc := range []struct {
		name  string
		claim string
		value any
	}{
		{"another issuer", "iss", "https://accounts.example.test"},
		{"another client", "aud", "someone-else"},
		{"several audiences", "aud", []string{"client-id", "someone-else"}},
		{"expired", "exp", now.Add(-time.Second).Unix()},
		{"expiring now", "exp", now.Unix()},
		{"no expiry", "exp", nil},
		{"another sign-in", "nonce", "not-this-flow"},
		{"no nonce", "nonce", nil},
		{"no subject", "sub", ""},
		{"blank subject", "sub", "   "},
		{"subject too long", "sub", strings.Repeat("1", 256)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			values := validClaims(flow, now)
			if tc.value == nil {
				delete(values, tc.claim)
			} else {
				values[tc.claim] = tc.value
			}
			if got, err := client.claims(idToken(t, values), flow, now); err == nil {
				t.Fatalf("accepted %+v", got)
			}
		})
	}
	for _, malformed := range []string{"", "a.b", "a.!!!.c", "a." + base64.RawURLEncoding.EncodeToString([]byte("not json")) + ".c"} {
		if _, err := client.claims(malformed, flow, now); err == nil {
			t.Fatalf("accepted the malformed token %q", malformed)
		}
	}
}

func TestGoogleAuthenticateRedeemsTheCodeWithItsVerifier(t *testing.T) {
	client := NewGoogle("client-id", "client-secret")
	flow, err := NewFlow()
	if err != nil {
		t.Fatal(err)
	}
	token := idToken(t, validClaims(flow, time.Now()))
	var sent url.Values
	client.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != googleToken || r.Method != http.MethodPost {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL)
		}
		body, _ := io.ReadAll(r.Body)
		sent, _ = url.ParseQuery(string(body))
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"id_token":"` + token + `"}`))}, nil
	})}
	profile, err := client.Authenticate(t.Context(), "the-code", "https://cfg.test/auth/google/callback", flow)
	if err != nil {
		t.Fatal(err)
	}
	if profile.ID != "1234567890" || profile.Name != "Alice Example" {
		t.Fatalf("profile %+v", profile)
	}
	if sent.Get("code") != "the-code" || sent.Get("code_verifier") != flow.Verifier || sent.Get("client_secret") != "client-secret" ||
		sent.Get("redirect_uri") != "https://cfg.test/auth/google/callback" {
		t.Fatalf("token request %v", sent)
	}

	other, err := NewFlow()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Authenticate(t.Context(), "the-code", "https://cfg.test/auth/google/callback", other); err == nil {
		t.Fatal("a token issued for another sign-in's nonce was accepted")
	}

	client.http = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader(`{"error":"invalid_grant"}`))}, nil
	})}
	if _, err := client.Authenticate(t.Context(), "used-code", "https://cfg.test/auth/google/callback", flow); err == nil {
		t.Fatal("a refused code signed someone in")
	}
	if _, err := NewGoogle("", "").Authenticate(t.Context(), "code", "https://cfg.test/", flow); err == nil {
		t.Fatal("an unconfigured client signed someone in")
	}
}
