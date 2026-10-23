package web

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"plainmote/internal/store"
)

// The MCP endpoint: one address, POST only, a single JSON answer per
// request. Two eras of the protocol are served side by side. A request that
// carries its protocol version in _meta is answered by the current revision,
// statelessly. One that opens with initialize is answered by the earlier
// revision the client asked for - still without a session: nothing here
// remembers one request for the next, so there is nothing to hand out or to
// end.
//
// The caller is the account its access token was issued for, and nothing
// else: no tool takes an account, an owner or a token as an argument, and
// every lookup goes through the same ownership checks the pages use.
const (
	mcpModernVersion = "2026-07-28"
	mcpLegacyVersion = "2025-11-25"
	mcpMetaVersion   = "io.modelcontextprotocol/protocolVersion"
	mcpMetaCaps      = "io.modelcontextprotocol/clientCapabilities"
	mcpMetaServer    = "io.modelcontextprotocol/serverInfo"
	// How long a client may keep the tool list and the server's description.
	mcpListTTL = 5 * time.Minute
)

// mcpSupportedVersions is what an unsupported version is told to use.
var mcpSupportedVersions = []string{mcpModernVersion, mcpLegacyVersion}

// mcpLegacyVersions are the initialize-era revisions answered as asked. They
// differ in nothing the tools here use.
var mcpLegacyVersions = map[string]bool{mcpLegacyVersion: true, "2025-06-18": true, "2025-03-26": true}

// JSON-RPC error codes: the standard ones, the two the MCP specification
// defines that are used here, and one of this server's own, outside the
// reserved range, for a token without the scope a tool needs.
const (
	rpcParseError         = -32700
	rpcInvalidRequest     = -32600
	rpcMethodNotFound     = -32601
	rpcInvalidParams      = -32602
	rpcInternalError      = -32603
	rpcHeaderMismatch     = -32020
	rpcUnsupportedVersion = -32022
	rpcInsufficientScope  = -31403
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcParams struct {
	Meta            map[string]json.RawMessage `json:"_meta"`
	Name            string                     `json:"name"`
	URI             string                     `json:"uri"`
	Arguments       json.RawMessage            `json:"arguments"`
	ProtocolVersion string                     `json:"protocolVersion"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// mcpExchange is one request being answered: who sent it, in which era.
type mcpExchange struct {
	r      *http.Request
	user   User
	access store.OAuthAccess
	modern bool
}

func (a *App) handleMCP(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin != "" && !a.mcpOrigin(r, origin) {
		mcpWrite(w, http.StatusForbidden, rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: rpcInvalidRequest, Message: "this origin may not call the MCP endpoint"}})
		return
	}
	if origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Expose-Headers", "WWW-Authenticate")
		w.Header().Add("Vary", "Origin")
		if r.Method == http.MethodOptions {
			w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, Accept, MCP-Protocol-Version, Mcp-Method, Mcp-Name")
			w.Header().Set("Access-Control-Max-Age", "600")
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	user, access, ok := a.mcpUser(w, r)
	if !ok {
		return
	}
	limit := 2*a.cfg.MaxContent + 64<<10
	allowUpload(w, limit)
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			mcpWrite(w, http.StatusRequestEntityTooLarge, rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: rpcInvalidRequest, Message: "request is too large"}})
			return
		}
		writePlainError(w, http.StatusBadRequest, "could not read the request")
		return
	}
	body = bytes.TrimSpace(body)
	if len(body) > 0 && body[0] == '[' {
		mcpWrite(w, http.StatusBadRequest, rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: rpcInvalidRequest, Message: "batches are not supported"}})
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		mcpWrite(w, http.StatusBadRequest, rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: rpcParseError, Message: "the body is not a JSON-RPC message"}})
		return
	}
	if req.ID == nil {
		// A notification: there is nothing to answer, in either era.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if !rpcValidID(req.ID) || req.JSONRPC != "2.0" || req.Method == "" {
		mcpWrite(w, http.StatusBadRequest, rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: rpcInvalidRequest, Message: "not a JSON-RPC 2.0 request"}})
		return
	}
	var params rpcParams
	if len(req.Params) > 0 && !bytes.Equal(req.Params, []byte("null")) {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			a.mcpFail(w, req, http.StatusBadRequest, rpcInvalidParams, "params must be an object with fields of the right types", nil)
			return
		}
	}
	x := mcpExchange{r: r, user: user, access: access}
	var metaVersion string
	if raw, found := params.Meta[mcpMetaVersion]; found {
		if json.Unmarshal(raw, &metaVersion) != nil || metaVersion == "" {
			a.mcpFail(w, req, http.StatusBadRequest, rpcInvalidParams, "_meta "+mcpMetaVersion+" must be a version string", nil)
			return
		}
	}
	if metaVersion != "" {
		x.modern = true
		if !a.mcpModernHeaders(w, r, req, params, metaVersion) {
			return
		}
		if metaVersion != mcpModernVersion {
			a.mcpFail(w, req, http.StatusBadRequest, rpcUnsupportedVersion, "Unsupported protocol version",
				map[string]any{"supported": mcpSupportedVersions, "requested": metaVersion})
			return
		}
		if caps, found := params.Meta[mcpMetaCaps]; !found || !json.Valid(caps) || caps[0] != '{' {
			a.mcpFail(w, req, http.StatusBadRequest, rpcInvalidParams, "_meta "+mcpMetaCaps+" is required", nil)
			return
		}
	} else if req.Method != "initialize" {
		switch version := r.Header.Get("MCP-Protocol-Version"); {
		case version == "" || mcpLegacyVersions[version]:
			// Before 2025-06-18 there was no header; such a client is
			// answered as 2025-03-26, which nothing here tells apart.
		case version == mcpModernVersion:
			a.mcpFail(w, req, http.StatusBadRequest, rpcInvalidParams, "_meta "+mcpMetaVersion+" is required", nil)
			return
		default:
			a.mcpFail(w, req, http.StatusBadRequest, rpcUnsupportedVersion, "Unsupported protocol version",
				map[string]any{"supported": mcpSupportedVersions, "requested": version})
			return
		}
	}
	switch {
	case req.Method == "initialize" && !x.modern:
		version := mcpLegacyVersion
		if mcpLegacyVersions[params.ProtocolVersion] {
			version = params.ProtocolVersion
		}
		a.mcpAnswer(w, x, req, map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      a.mcpServerInfo(),
			"instructions":    mcpInstructions,
		})
	case req.Method == "server/discover" && x.modern:
		a.mcpAnswer(w, x, req, map[string]any{
			"supportedVersions": mcpSupportedVersions,
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"instructions":      mcpInstructions,
			"ttlMs":             mcpListTTL.Milliseconds(),
			"cacheScope":        "public",
		})
	case req.Method == "ping":
		a.mcpAnswer(w, x, req, map[string]any{})
	case req.Method == "tools/list":
		// Every tool is listed whatever the token allows, so the list is the
		// same for everyone; a call beyond the token's scope is what fails.
		result := map[string]any{"tools": mcpToolList()}
		if x.modern {
			result["ttlMs"] = mcpListTTL.Milliseconds()
			result["cacheScope"] = "public"
		}
		a.mcpAnswer(w, x, req, result)
	case req.Method == "tools/call":
		a.mcpCallTool(w, x, req, params)
	default:
		status := http.StatusOK
		if x.modern {
			status = http.StatusNotFound
		}
		a.mcpFail(w, req, status, rpcMethodNotFound, "Method not found: "+clipText(req.Method, 100), nil)
	}
}

// mcpModernHeaders checks what the current revision mirrors into headers
// against the body, so that nothing routing on one can be shown the other.
func (a *App) mcpModernHeaders(w http.ResponseWriter, r *http.Request, req rpcRequest, params rpcParams, metaVersion string) bool {
	mismatch := func(message string) bool {
		a.mcpFail(w, req, http.StatusBadRequest, rpcHeaderMismatch, "Header mismatch: "+message, nil)
		return false
	}
	if r.Header.Get("MCP-Protocol-Version") != metaVersion {
		return mismatch("MCP-Protocol-Version does not match the protocol version in _meta")
	}
	if r.Header.Get("Mcp-Method") != req.Method {
		return mismatch("Mcp-Method does not match the request's method")
	}
	var name string
	switch req.Method {
	case "tools/call", "prompts/get":
		name = params.Name
	case "resources/read":
		name = params.URI
	default:
		return true
	}
	header := r.Header.Get("Mcp-Name")
	if value, ok := mcpHeaderValue(header); header == "" || !ok || value != name {
		return mismatch("Mcp-Name does not match the name in the request")
	}
	return true
}

// mcpOrigin admits a browser page from this site, or from a program on the
// caller's own computer, such as the MCP Inspector. Servers send no Origin.
func (a *App) mcpOrigin(r *http.Request, origin string) bool {
	if strings.EqualFold(origin, a.baseURL(r)) {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "http" || parsed.Path != "" {
		return false
	}
	switch parsed.Hostname() {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// mcpChallenge is the 401 that sends a client to discover how to sign in.
func (a *App) mcpChallenge(w http.ResponseWriter, r *http.Request, invalid bool, description string) {
	challenge := `Bearer resource_metadata="` + a.baseURL(r) + wellKnownResource + mcpPath + `", scope="` + store.OAuthScopeRead + `"`
	code := "unauthorized"
	if invalid {
		challenge += `, error="invalid_token"`
		code = "invalid_token"
	}
	w.Header().Set("WWW-Authenticate", challenge)
	a.writeAPI(w, http.StatusUnauthorized, oauthError{Error: code, Description: description})
}

// mcpUser authenticates a request by its access token, which must have been
// issued for this endpoint. A command-line token is not one.
func (a *App) mcpUser(w http.ResponseWriter, r *http.Request) (User, store.OAuthAccess, bool) {
	now := time.Now().UTC()
	ip := a.clientIP(r)
	if a.limits.authFailures.blocked(addressKey(ip), now) {
		w.Header().Set("Retry-After", "60")
		a.writeAPI(w, http.StatusTooManyRequests, oauthError{Error: "rate_limited", Description: "too many failed attempts; try again in a minute"})
		return User{}, store.OAuthAccess{}, false
	}
	header := r.Header.Get("Authorization")
	if len(header) < 7 || !strings.EqualFold(header[:7], "Bearer ") || strings.TrimSpace(header[7:]) == "" {
		a.mcpChallenge(w, r, false, "authorization is required")
		return User{}, store.OAuthAccess{}, false
	}
	user, access, err := a.db.OAuthAccessUser(r.Context(), strings.TrimSpace(header[7:]), ip, now)
	if errors.Is(err, store.ErrNotFound) || (err == nil && access.Resource != a.mcpResource(r)) {
		a.limits.authFailures.allow(addressKey(ip), now)
		a.mcpChallenge(w, r, true, "the access token is not valid here, has expired or was revoked")
		return User{}, store.OAuthAccess{}, false
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcp authenticate: %v\n", err)
		writePlainError(w, http.StatusInternalServerError, "the service could not complete this request")
		return User{}, store.OAuthAccess{}, false
	}
	if !a.limits.mcpCalls.allow(access.GrantID, now) || !a.limits.mcpAddresses.allow(addressKey(ip), now) {
		w.Header().Set("Retry-After", "60")
		a.writeAPI(w, http.StatusTooManyRequests, oauthError{Error: "rate_limited", Description: "too many requests; slow down"})
		return User{}, store.OAuthAccess{}, false
	}
	return user, access, true
}

func (a *App) mcpServerInfo() map[string]any {
	version := a.cfg.Version
	if version == "" {
		version = "dev"
	}
	return map[string]any{"name": "plainmote", "title": "PlainMote", "version": version}
}

// mcpAnswer sends a result. The current revision types every result and
// names the server on each, since no handshake did.
func (a *App) mcpAnswer(w http.ResponseWriter, x mcpExchange, req rpcRequest, result map[string]any) {
	if x.modern {
		result["resultType"] = "complete"
		result["_meta"] = map[string]any{mcpMetaServer: a.mcpServerInfo()}
	}
	mcpWrite(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result})
}

func (a *App) mcpFail(w http.ResponseWriter, req rpcRequest, status, code int, message string, data any) {
	id := req.ID
	if !rpcValidID(id) {
		id = nil
	}
	mcpWrite(w, status, rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message, Data: data}})
}

func mcpWrite(w http.ResponseWriter, status int, value any) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		fmt.Fprintf(os.Stderr, "mcp encode: %v\n", err)
		writePlainError(w, http.StatusInternalServerError, "the service could not complete this request")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(out.Bytes())
}

// rpcValidID reports an id MCP allows: a string or an integer, never null.
func rpcValidID(id json.RawMessage) bool {
	if len(id) == 0 {
		return false
	}
	if id[0] == '"' {
		var text string
		return json.Unmarshal(id, &text) == nil
	}
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(id))
	decoder.UseNumber()
	if decoder.Decode(&number) != nil {
		return false
	}
	_, err := number.Int64()
	return err == nil
}

// mcpHeaderValue reads a mirrored header, undoing the base64 form a value
// that is not plain ASCII travels in.
func mcpHeaderValue(value string) (string, bool) {
	if inner, found := strings.CutPrefix(value, "=?base64?"); found {
		inner, found = strings.CutSuffix(inner, "?=")
		if !found {
			return "", false
		}
		decoded, err := base64.StdEncoding.DecodeString(inner)
		if err != nil {
			decoded, err = base64.RawStdEncoding.DecodeString(inner)
		}
		return string(decoded), err == nil
	}
	for i := 0; i < len(value); i++ {
		if c := value[i]; (c < 0x20 && c != '\t') || c > 0x7e {
			return "", false
		}
	}
	return value, true
}

// clipText shortens what a caller sent before it is echoed in a message.
func clipText(value string, max int) string {
	if len(value) <= max {
		return value
	}
	cut := max
	for cut > 0 && value[cut]&0xc0 == 0x80 {
		cut--
	}
	return value[:cut] + "…"
}

const mcpInstructions = "PlainMote keeps this account's resources - configuration files, logs, notes and other files - with their version history. " +
	"Use list_resources to find a resource, read_resource to read its text (long text comes in parts: pass next_offset back as offset), " +
	"list_versions for its history and diff_versions to see what changed between two versions. Resources carry the user's own tags, such as tmp or surge: list_tags shows them, list_resources " +
	"filters by one, and tag_resource adds or removes them. To change part of a resource, read it, then replace that part with edit_resource and the " +
	"version you read as base_version; update_resource saves a whole new text the same way, and create_resource makes a new one. share_resource makes a public link that anyone with the " +
	"address can open: make one only when the user asks for it. End-to-end encrypted resources appear only as placeholders: they can be decrypted in the browser " +
	"or in PlainMote CLI, never through this server. Never ask the user for their master password."
