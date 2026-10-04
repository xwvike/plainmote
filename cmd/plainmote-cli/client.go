package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// client talks to one server's /api/v1, with one token. The token travels in
// the Authorization header and nowhere else.
type client struct {
	server string
	token  string
	http   *http.Client
	// serverVersion is the version the last answer carried.
	serverVersion string
}

// newClient follows no redirects: the API never sends one, and following
// one could carry the token somewhere it was not meant to go - to plain http,
// for one.
func newClient(server, token string) *client {
	return &client{server: server, token: token, http: &http.Client{
		Timeout: 2 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return errors.New("the server answered with a redirect")
		},
	}}
}

// host is how messages name the server a change went to.
func (c *client) host() string {
	if parsed, err := url.Parse(c.server); err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return c.server
}

func userAgent() string {
	return "plainmote-cli/" + version + " (" + runtime.GOOS + "/" + runtime.GOARCH + ")"
}

// apiError is a refusal from the server, as it describes it.
type apiError struct {
	Status         int    `json:"-"`
	Code           string `json:"error"`
	Message        string `json:"message"`
	CurrentVersion int    `json:"current_version"`
	Interval       int    `json:"interval"`
}

func (e *apiError) Error() string {
	switch e.Code {
	case "unauthorized":
		return msg("signin_expired")
	case "read_only":
		return msg("read_only")
	}
	text := e.Message
	if text == "" {
		text = e.Code
	}
	return msg("server_error", e.Status, text)
}

type resource struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Filename  string    `json:"filename"`
	Size      int64     `json:"size"`
	Type      string    `json:"type"`
	Encoding  string    `json:"encoding"`
	Version   int       `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
	Remote    bool      `json:"remote"`
	Editable  bool      `json:"editable"`
	Encrypted bool      `json:"encrypted"`
	TakenDown bool      `json:"taken_down"`
	// ExpiresAt marks a quick share: read only until kept as a resource.
	ExpiresAt *time.Time `json:"expires_at"`
	URL       string     `json:"url"`
}

// label is how a resource is named in messages.
func (r resource) label() string {
	switch {
	case r.Name != "":
		return clean(r.Name)
	case r.Filename != "":
		return clean(r.Filename)
	}
	return r.ID
}

func (c *client) do(ctx context.Context, method, path string, body io.Reader, header map[string]string) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, method, c.server+path, body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", userAgent())
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	for key, value := range header {
		request.Header.Set(key, value)
	}
	response, err := c.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%s", msg("request_failed", c.server, err))
	}
	if v := response.Header.Get("X-PlainMote-Version"); v != "" {
		c.serverVersion = v
	}
	return response, nil
}

// readError turns a refusal into an *apiError, whatever shape it came in.
func readError(response *http.Response) error {
	defer response.Body.Close()
	failure := &apiError{Status: response.StatusCode}
	data, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if json.Unmarshal(data, failure) != nil || failure.Code == "" {
		failure.Code = "http_" + strconv.Itoa(response.StatusCode)
		failure.Message = strings.TrimSpace(string(data))
	}
	return failure
}

func (c *client) getJSON(ctx context.Context, path string, into any) error {
	response, err := c.do(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return readError(response)
	}
	defer response.Body.Close()
	return json.NewDecoder(response.Body).Decode(into)
}

func (c *client) postJSON(ctx context.Context, path string, value, into any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	response, err := c.do(ctx, http.MethodPost, path, bytes.NewReader(data), map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusOK {
		return readError(response)
	}
	defer response.Body.Close()
	return json.NewDecoder(response.Body).Decode(into)
}

func (c *client) list(ctx context.Context, query string) ([]resource, error) {
	var answer struct{ Resources []resource }
	err := c.getJSON(ctx, "/api/v1/resources?q="+url.QueryEscape(query), &answer)
	return answer.Resources, err
}

// resolve finds the one resource a reference names. None is an error, and so
// is more than one: the candidates are listed rather than one picked.
func (c *client) resolve(ctx context.Context, ref string) (resource, error) {
	var answer struct{ Resources []resource }
	if err := c.getJSON(ctx, "/api/v1/resources?ref="+url.QueryEscape(ref), &answer); err != nil {
		return resource{}, err
	}
	switch len(answer.Resources) {
	case 0:
		return resource{}, fmt.Errorf("%s", msg("not_found", ref))
	case 1:
		return answer.Resources[0], nil
	}
	var text strings.Builder
	text.WriteString(msg("ambiguous", ref))
	for _, r := range answer.Resources {
		fmt.Fprintf(&text, "\n  %s  %s  %s", r.ID, r.label(), r.Filename)
	}
	return resource{}, errors.New(text.String())
}

// content is a resource's stored bytes with the version and encoding they
// belong to.
type content struct {
	Body     []byte
	Version  int
	Encoding string
}

func (c *client) read(ctx context.Context, id string) (content, error) {
	response, err := c.do(ctx, http.MethodGet, "/api/v1/resources/"+url.PathEscape(id)+"/content", nil, nil)
	if err != nil {
		return content{}, err
	}
	if response.StatusCode != http.StatusOK {
		return content{}, readError(response)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return content{}, err
	}
	number, _ := strconv.Atoi(response.Header.Get("X-PlainMote-Resource-Version"))
	return content{Body: body, Version: number, Encoding: response.Header.Get("X-PlainMote-Encoding")}, nil
}

type saveResult struct {
	Version    int  `json:"version"`
	NewVersion bool `json:"new_version"`
	Trimmed    int  `json:"trimmed"`
}

// write saves body as the resource's next version. base is the version it was
// edited from; 0 saves over whatever is current.
func (c *client) write(ctx context.Context, id string, body []byte, base int, encoding string) (saveResult, error) {
	header := map[string]string{"Content-Type": "application/octet-stream", "If-Match": "*"}
	if base > 0 {
		header["If-Match"] = `"v` + strconv.Itoa(base) + `"`
	}
	if encoding != "" {
		header["X-PlainMote-Encoding"] = encoding
	}
	response, err := c.do(ctx, http.MethodPut, "/api/v1/resources/"+url.PathEscape(id)+"/content", bytes.NewReader(body), header)
	if err != nil {
		return saveResult{}, err
	}
	if response.StatusCode != http.StatusOK {
		return saveResult{}, readError(response)
	}
	defer response.Body.Close()
	var result saveResult
	return result, json.NewDecoder(response.Body).Decode(&result)
}

func (c *client) create(ctx context.Context, name, filename string, body []byte) (resource, error) {
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	_ = writer.WriteField("name", name)
	_ = writer.WriteField("filename", filename)
	part, err := writer.CreateFormFile("content", "content")
	if err != nil {
		return resource{}, err
	}
	if _, err := part.Write(body); err != nil {
		return resource{}, err
	}
	if err := writer.Close(); err != nil {
		return resource{}, err
	}
	response, err := c.do(ctx, http.MethodPost, "/api/v1/resources", &form, map[string]string{"Content-Type": writer.FormDataContentType()})
	if err != nil {
		return resource{}, err
	}
	if response.StatusCode != http.StatusCreated {
		return resource{}, readError(response)
	}
	defer response.Body.Close()
	var made resource
	return made, json.NewDecoder(response.Body).Decode(&made)
}

// quickShare is a quick share just made: the resource it is kept as, and
// the one link to it.
type quickShare struct {
	resource
	ShareURL string `json:"share_url"`
}

func (c *client) share(ctx context.Context, filename, ttl string, body []byte) (quickShare, error) {
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	_ = writer.WriteField("filename", filename)
	if ttl != "" {
		_ = writer.WriteField("ttl", ttl)
	}
	part, err := writer.CreateFormFile("content", "content")
	if err != nil {
		return quickShare{}, err
	}
	if _, err := part.Write(body); err != nil {
		return quickShare{}, err
	}
	if err := writer.Close(); err != nil {
		return quickShare{}, err
	}
	response, err := c.do(ctx, http.MethodPost, "/api/v1/quick-shares", &form, map[string]string{"Content-Type": writer.FormDataContentType()})
	if err != nil {
		return quickShare{}, err
	}
	if response.StatusCode != http.StatusCreated {
		return quickShare{}, readError(response)
	}
	defer response.Body.Close()
	var made quickShare
	return made, json.NewDecoder(response.Body).Decode(&made)
}

type me struct {
	Login         string    `json:"login"`
	Scope         string    `json:"scope"`
	ExpiresAt     time.Time `json:"expires_at"`
	Device        string    `json:"device"`
	ServerVersion string    `json:"server_version"`
}

func (c *client) whoami(ctx context.Context) (me, error) {
	var answer me
	return answer, c.getJSON(ctx, "/api/v1/me", &answer)
}

// revoke ends the token this client holds, on the server.
func (c *client) revoke(ctx context.Context) error {
	response, err := c.do(ctx, http.MethodDelete, "/api/v1/token", nil, nil)
	if err != nil {
		return err
	}
	if response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusUnauthorized {
		return readError(response)
	}
	response.Body.Close()
	return nil
}
