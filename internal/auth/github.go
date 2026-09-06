package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type GitHub struct {
	clientID     string
	clientSecret string
	http         *http.Client
}

type Profile struct {
	ID        string
	Login     string
	Name      string
	AvatarURL string
}

type githubProfile struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url"`
}

type accessTokenResponse struct {
	AccessToken string `json:"access_token"`
	Error       string `json:"error"`
	ErrorDesc   string `json:"error_description"`
}

func NewGitHub(clientID, clientSecret string) *GitHub {
	return &GitHub{
		clientID:     strings.TrimSpace(clientID),
		clientSecret: strings.TrimSpace(clientSecret),
		http:         &http.Client{Timeout: 15 * time.Second},
	}
}

func (g *GitHub) Configured() bool {
	return g != nil && g.clientID != "" && g.clientSecret != ""
}

func (g *GitHub) NewState() (string, error) {
	data := make([]byte, 24)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func (g *GitHub) AuthorizationURL(redirectURI, state string) string {
	query := url.Values{
		"client_id":    {g.clientID},
		"redirect_uri": {redirectURI},
		"scope":        {"read:user"},
		"state":        {state},
	}
	return "https://github.com/login/oauth/authorize?" + query.Encode()
}

func (g *GitHub) Authenticate(ctx context.Context, code, redirectURI string) (Profile, error) {
	if !g.Configured() {
		return Profile{}, errors.New("GitHub OAuth is not configured")
	}
	accessToken, err := g.exchangeCode(ctx, code, redirectURI)
	if err != nil {
		return Profile{}, err
	}
	return g.fetchProfile(ctx, accessToken)
}

func (g *GitHub) exchangeCode(ctx context.Context, code, redirectURI string) (string, error) {
	form := url.Values{
		"client_id":     {g.clientID},
		"client_secret": {g.clientSecret},
		"code":          {code},
		"redirect_uri":  {redirectURI},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://github.com/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "plainmote")
	resp, err := g.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("exchange GitHub OAuth code: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("GitHub OAuth returned HTTP %d", resp.StatusCode)
	}
	var payload accessTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", fmt.Errorf("decode GitHub OAuth response: %w", err)
	}
	if payload.AccessToken == "" {
		if payload.ErrorDesc != "" {
			return "", errors.New(payload.ErrorDesc)
		}
		return "", errors.New("GitHub did not return an access token")
	}
	return payload.AccessToken, nil
}

func (g *GitHub) fetchProfile(ctx context.Context, accessToken string) (Profile, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user", nil)
	if err != nil {
		return Profile{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", "plainmote")
	resp, err := g.http.Do(req)
	if err != nil {
		return Profile{}, fmt.Errorf("read GitHub profile: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Profile{}, fmt.Errorf("GitHub profile returned HTTP %d", resp.StatusCode)
	}
	var result githubProfile
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return Profile{}, fmt.Errorf("decode GitHub profile: %w", err)
	}
	if result.ID == 0 || strings.TrimSpace(result.Login) == "" {
		return Profile{}, errors.New("GitHub profile is incomplete")
	}
	return Profile{
		ID:        strconv.FormatInt(result.ID, 10),
		Login:     result.Login,
		Name:      result.Name,
		AvatarURL: result.AvatarURL,
	}, nil
}
