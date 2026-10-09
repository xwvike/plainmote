package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Flow is what one sign-in carries from its start to its callback, in a
// cookie: the state that ties the callback to this browser, the nonce the ID
// token must repeat, and the PKCE verifier the code is redeemed with.
type Flow struct {
	State    string
	Nonce    string
	Verifier string
}

func NewFlow() (Flow, error) {
	var parts [3]string
	for i := range parts {
		data := make([]byte, 32)
		if _, err := rand.Read(data); err != nil {
			return Flow{}, err
		}
		parts[i] = base64.RawURLEncoding.EncodeToString(data)
	}
	return Flow{State: parts[0], Nonce: parts[1], Verifier: parts[2]}, nil
}

func (f Flow) Encode() string { return f.State + "." + f.Nonce + "." + f.Verifier }

func ParseFlow(value string) (Flow, bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return Flow{}, false
	}
	return Flow{State: parts[0], Nonce: parts[1], Verifier: parts[2]}, true
}

// Google signs in with Google's OpenID Connect, asking for the account's
// name and picture and nothing else - not its email address.
type Google struct {
	clientID     string
	clientSecret string
	http         *http.Client
}

const (
	googleAuthorize = "https://accounts.google.com/o/oauth2/v2/auth"
	googleToken     = "https://oauth2.googleapis.com/token"
)

func NewGoogle(clientID, clientSecret string) *Google {
	return &Google{
		clientID:     strings.TrimSpace(clientID),
		clientSecret: strings.TrimSpace(clientSecret),
		http:         &http.Client{Timeout: 15 * time.Second},
	}
}

func (g *Google) Configured() bool {
	return g != nil && g.clientID != "" && g.clientSecret != ""
}

func (g *Google) AuthorizationURL(redirectURI string, flow Flow) string {
	challenge := sha256.Sum256([]byte(flow.Verifier))
	query := url.Values{
		"client_id":             {g.clientID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"scope":                 {"openid profile"},
		"state":                 {flow.State},
		"nonce":                 {flow.Nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"code_challenge_method": {"S256"},
		"prompt":                {"select_account"},
	}
	return googleAuthorize + "?" + query.Encode()
}

type googleClaims struct {
	Issuer   string `json:"iss"`
	Audience string `json:"aud"`
	Subject  string `json:"sub"`
	Expires  int64  `json:"exp"`
	Nonce    string `json:"nonce"`
	Name     string `json:"name"`
	Picture  string `json:"picture"`
}

// Authenticate redeems the code for an ID token and reads the account from
// it. The token comes straight from Google's token endpoint over TLS, in
// answer to this client's own secret, which OpenID Connect accepts in place
// of checking its signature; what is checked is that it was issued by Google,
// for this client, for this sign-in, and has not expired.
func (g *Google) Authenticate(ctx context.Context, code, redirectURI string, flow Flow) (Profile, error) {
	if !g.Configured() {
		return Profile{}, errors.New("Google sign-in is not configured")
	}
	form := url.Values{
		"code":          {code},
		"client_id":     {g.clientID},
		"client_secret": {g.clientSecret},
		"redirect_uri":  {redirectURI},
		"grant_type":    {"authorization_code"},
		"code_verifier": {flow.Verifier},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, googleToken, strings.NewReader(form.Encode()))
	if err != nil {
		return Profile{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := g.http.Do(req)
	if err != nil {
		return Profile{}, fmt.Errorf("exchange Google code: %w", err)
	}
	defer resp.Body.Close()
	var payload struct {
		IDToken   string `json:"id_token"`
		Error     string `json:"error"`
		ErrorDesc string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return Profile{}, fmt.Errorf("decode Google token response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || payload.IDToken == "" {
		return Profile{}, fmt.Errorf("Google token endpoint returned HTTP %d %s %s", resp.StatusCode, payload.Error, payload.ErrorDesc)
	}
	claims, err := g.claims(payload.IDToken, flow, time.Now())
	if err != nil {
		return Profile{}, err
	}
	name := strings.TrimSpace(claims.Name)
	if name == "" {
		name = "Google user"
	}
	return Profile{ID: claims.Subject, Login: name, Name: name, AvatarURL: claims.Picture}, nil
}

func (g *Google) claims(token string, flow Flow, now time.Time) (googleClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return googleClaims{}, errors.New("Google ID token is malformed")
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return googleClaims{}, errors.New("Google ID token is malformed")
	}
	var claims googleClaims
	if err := json.Unmarshal(body, &claims); err != nil {
		return googleClaims{}, errors.New("Google ID token is malformed")
	}
	switch {
	case claims.Issuer != "https://accounts.google.com" && claims.Issuer != "accounts.google.com":
		return googleClaims{}, errors.New("Google ID token has the wrong issuer")
	case claims.Audience != g.clientID:
		return googleClaims{}, errors.New("Google ID token is for another client")
	case now.Unix() >= claims.Expires:
		return googleClaims{}, errors.New("Google ID token has expired")
	case subtle.ConstantTimeCompare([]byte(claims.Nonce), []byte(flow.Nonce)) != 1:
		return googleClaims{}, errors.New("Google ID token is for another sign-in")
	case strings.TrimSpace(claims.Subject) == "" || len(claims.Subject) > 255:
		return googleClaims{}, errors.New("Google ID token names no account")
	}
	return claims, nil
}
