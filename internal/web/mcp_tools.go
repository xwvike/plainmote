package web

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"plainmote/internal/store"
)

// The MCP tools. Each answers for the token's own account only: a resource
// is looked up with that account as its owner, so another account's resource
// - by id, by its page address or by one of its share links - is not found,
// in exactly the words a resource that does not exist is not found.
//
// Encrypted resources are listed, as placeholders that say only that they
// exist, and are never read: the service holds ciphertext and no key.
const (
	mcpListPage  = 50
	mcpReadChunk = 256 << 10
	mcpRefMax    = 2048
	// The name an encrypted resource is listed under: its own name is
	// inside the ciphertext.
	mcpEncryptedName = "Encrypted resource"
	mcpEncryptedText = "This resource is end-to-end encrypted. It can only be decrypted in the browser or in PlainMote CLI, not through this server."
)

type mcpTool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	Annotations map[string]any `json:"annotations"`
	scope       string
	run         func(a *App, x mcpExchange, args json.RawMessage) (mcpOutcome, error)
}

// mcpOutcome is what a tool call produced: structured data with the text a
// model reads, or a problem the model can act on.
type mcpOutcome struct {
	structured any
	text       []string
	problem    string
}

func mcpProblem(format string, args ...any) (mcpOutcome, error) {
	return mcpOutcome{problem: fmt.Sprintf(format, args...)}, nil
}

func (o mcpOutcome) result() map[string]any {
	if o.problem != "" {
		return map[string]any{"content": []map[string]string{{"type": "text", "text": o.problem}}, "isError": true}
	}
	content := make([]map[string]string, 0, len(o.text)+1)
	for _, text := range o.text {
		content = append(content, map[string]string{"type": "text", "text": text})
	}
	if len(content) == 0 {
		encoded, _ := json.Marshal(o.structured)
		content = append(content, map[string]string{"type": "text", "text": string(encoded)})
	}
	return map[string]any{"content": content, "structuredContent": o.structured}
}

var readOnlyTool = map[string]any{"readOnlyHint": true, "openWorldHint": false}

var resourceRefSchema = map[string]any{
	"type": "string",
	"description": "The resource: its id, the address of its page or of one of its share links on this server, or its exact name or filename. " +
		"A share link's address only names the resource: using it here neither uses the link up nor needs it to still work.",
}

// The tools in the order they are listed: reading, then writing, then
// sharing.
var mcpTools = append(append(mcpReadTools, mcpWriteTools...), mcpShareTools...)

var mcpReadTools = []*mcpTool{
	{
		Name:  "list_resources",
		Title: "List resources",
		Description: "List the resources in this PlainMote account, most recently updated first: name, filename, type, size, version and update time. " +
			"Filter by words in the name or filename, or by kind. End-to-end encrypted resources appear as placeholders with only an id, size and time.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query":  map[string]any{"type": "string", "description": "Words to look for in the name or filename."},
				"kind":   map[string]any{"type": "string", "enum": []string{"resource", "quick_share"}, "description": "Only resources, or only quick shares not yet kept."},
				"cursor": map[string]any{"type": "string", "description": "The next_cursor of the previous page."},
			},
			"additionalProperties": false,
		},
		Annotations: readOnlyTool,
		scope:       store.OAuthScopeRead,
		run:         (*App).mcpListResources,
	},
	{
		Name:  "read_resource",
		Title: "Read a resource",
		Description: "Read the text of a resource, now or at an earlier version. Text longer than 256 KiB comes in parts: pass next_offset back as offset for the next. " +
			"Binary content is not returned, only its description. Encrypted resources cannot be read here.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"resource": resourceRefSchema,
				"version":  map[string]any{"type": "integer", "minimum": 1, "description": "An earlier version to read, from list_versions. Leave out for the current content."},
				"offset":   map[string]any{"type": "integer", "minimum": 0, "description": "Where to continue: the next_offset of the previous part."},
			},
			"required":             []string{"resource"},
			"additionalProperties": false,
		},
		Annotations: readOnlyTool,
		scope:       store.OAuthScopeRead,
		run:         (*App).mcpReadResource,
	},
	{
		Name:        "list_versions",
		Title:       "List versions",
		Description: "List a resource's versions, current first: version number, size and when it was saved. Read one with read_resource and its version.",
		InputSchema: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"resource": resourceRefSchema},
			"required":             []string{"resource"},
			"additionalProperties": false,
		},
		Annotations: readOnlyTool,
		scope:       store.OAuthScopeRead,
		run:         (*App).mcpListVersions,
	},
}

func mcpToolList() []*mcpTool { return mcpTools }

func mcpToolNamed(name string) *mcpTool {
	for _, tool := range mcpTools {
		if tool.Name == name {
			return tool
		}
	}
	return nil
}

func (a *App) mcpCallTool(w http.ResponseWriter, x mcpExchange, req rpcRequest, params rpcParams) {
	tool := mcpToolNamed(params.Name)
	if tool == nil {
		a.mcpFail(w, req, http.StatusOK, rpcInvalidParams, "Unknown tool: "+clipText(params.Name, 100), nil)
		return
	}
	if !x.access.Has(tool.scope) {
		// The client can ask its user for more: the challenge names the
		// scope that would do, as the step-up flow expects.
		wanted, _ := store.NormalizeOAuthScope(x.access.Scope + " " + tool.scope)
		w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope", scope="`+wanted+
			`", resource_metadata="`+a.baseURL(x.r)+wellKnownResource+mcpPath+`"`)
		a.mcpFail(w, req, http.StatusForbidden, rpcInsufficientScope,
			"This app was not given the "+tool.scope+" permission that "+tool.Name+" needs. Its user can allow it by connecting the app again.", nil)
		return
	}
	outcome, err := tool.run(a, x, params.Arguments)
	if err != nil {
		// Only the tool and the error: never its arguments, which can hold
		// the content being saved.
		fmt.Fprintf(os.Stderr, "mcp %s: %v\n", tool.Name, err)
		a.mcpFail(w, req, http.StatusInternalServerError, rpcInternalError, "the service could not complete this request", nil)
		return
	}
	a.mcpAnswer(w, x, req, outcome.result())
}

// mcpArguments reads a call's arguments into into, saying what is wrong in a
// way a model can correct.
func mcpArguments(args json.RawMessage, into any) string {
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	if err := json.Unmarshal(args, into); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) && typeErr.Field != "" {
			want := "string"
			if typeErr.Type.Kind() != reflect.String {
				want = "whole number"
			}
			return fmt.Sprintf("Invalid arguments: %s must be a %s.", typeErr.Field, want)
		}
		return "Invalid arguments: they must be an object as the tool's input schema describes."
	}
	return ""
}

type mcpResourceItem struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Filename   string     `json:"filename,omitempty"`
	Type       string     `json:"type,omitempty"`
	Size       int64      `json:"size"`
	Version    int        `json:"version,omitempty"`
	UpdatedAt  time.Time  `json:"updated_at"`
	Kind       string     `json:"kind,omitempty"`
	Text       bool       `json:"text,omitempty"`
	Encrypted  bool       `json:"encrypted,omitempty"`
	TakenDown  bool       `json:"taken_down,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	ShareLinks int        `json:"share_links,omitempty"`
	URL        string     `json:"url"`
}

func mcpEncrypted(resource Resource) bool {
	return resource.Sealed() || resource.Encrypted()
}

func (a *App) mcpResourceItem(r *http.Request, resource Resource) mcpResourceItem {
	page := a.baseURL(r) + "/resources/" + resource.ID
	if mcpEncrypted(resource) {
		return mcpResourceItem{ID: resource.ID, Name: mcpEncryptedName, Size: resource.ContentSize,
			UpdatedAt: resource.UpdatedAt, Encrypted: true, URL: page}
	}
	kind := "resource"
	if resource.QuickShare() {
		kind = "quick_share"
	}
	return mcpResourceItem{
		ID: resource.ID, Name: resource.Name, Filename: resource.Filename, Type: resource.ContentType,
		Size: resource.ContentSize, Version: resource.Version, UpdatedAt: resource.UpdatedAt, Kind: kind,
		Text:      store.TextLike(resource.ContentType),
		TakenDown: resource.TakenDown, ExpiresAt: resource.ExpiresAt, ShareLinks: resource.LiveShares, URL: page,
	}
}

// mcpLabel names a resource in a message.
func mcpLabel(resource Resource) string {
	switch {
	case mcpEncrypted(resource):
		return mcpEncryptedName + " " + resource.ID
	case resource.Name != "":
		return resource.Name
	case resource.Filename != "":
		return resource.Filename
	}
	return resource.ID
}

func (a *App) mcpListResources(x mcpExchange, args json.RawMessage) (mcpOutcome, error) {
	var in struct {
		Query  string `json:"query"`
		Kind   string `json:"kind"`
		Cursor string `json:"cursor"`
	}
	if problem := mcpArguments(args, &in); problem != "" {
		return mcpProblem("%s", problem)
	}
	kind := ""
	switch in.Kind {
	case "":
	case "resource":
		kind = store.KindResource
	case "quick_share":
		kind = store.KindQuickShare
	default:
		return mcpProblem("Invalid arguments: kind must be resource or quick_share.")
	}
	offset := 0
	if in.Cursor != "" {
		var ok bool
		if offset, ok = mcpCursorOffset(in.Cursor); !ok {
			return mcpProblem("Invalid arguments: cursor is not one this tool returned. Leave it out to start from the first page.")
		}
	}
	query := clipText(strings.TrimSpace(in.Query), 200)
	resources, total, err := a.db.ListResourcesOfKind(x.r.Context(), x.user.ID, query, kind, mcpListPage, offset)
	if err != nil {
		return mcpOutcome{}, err
	}
	items := make([]mcpResourceItem, 0, len(resources))
	for _, resource := range resources {
		items = append(items, a.mcpResourceItem(x.r, resource))
	}
	result := map[string]any{"resources": items, "total": total}
	if next := offset + len(resources); len(resources) > 0 && next < total {
		result["next_cursor"] = mcpCursor(next)
	}
	return mcpOutcome{structured: result}, nil
}

// A listing's cursor is where the next page starts, kept opaque so that
// clients pass it back rather than make their own.
func mcpCursor(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("o" + strconv.Itoa(offset)))
}

func mcpCursorOffset(cursor string) (int, bool) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(raw) < 2 || raw[0] != 'o' {
		return 0, false
	}
	offset, err := strconv.Atoi(string(raw[1:]))
	return offset, err == nil && offset >= 0 && offset <= 1_000_000
}

// mcpResolve finds the resource a tool was pointed at, among the account's
// own. The problem it returns is the same whether nothing matches or what
// matches belongs to someone else.
func (a *App) mcpResolve(x mcpExchange, ref string) (Resource, string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Resource{}, "Invalid arguments: resource is required.", nil
	}
	notFound := fmt.Sprintf("No resource in this account matches %q. list_resources shows what there is.", clipText(ref, 200))
	if len(ref) > mcpRefMax {
		return Resource{}, notFound, nil
	}
	ctx := x.r.Context()
	if parsed, err := url.Parse(ref); err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != "" {
		base, _ := url.Parse(a.baseURL(x.r))
		if base == nil || !strings.EqualFold(parsed.Host, base.Host) {
			return Resource{}, notFound, nil
		}
		var resource Resource
		switch {
		case strings.HasPrefix(parsed.Path, "/resources/"):
			id, _, _ := strings.Cut(strings.TrimPrefix(parsed.Path, "/resources/"), "/")
			resource, err = a.db.ResourceForOwner(ctx, x.user.ID, id)
		case strings.HasPrefix(parsed.Path, deliveryPrefix):
			token, ok := splitDeliveryPath(parsed.Path)
			if !ok {
				return Resource{}, notFound, nil
			}
			resource, err = a.db.ShareLinkResource(ctx, x.user.ID, token)
		default:
			return Resource{}, notFound, nil
		}
		if errors.Is(err, store.ErrNotFound) {
			return Resource{}, notFound, nil
		}
		return resource, "", err
	}
	matches, err := a.db.ResolveResources(ctx, x.user.ID, ref)
	if err != nil {
		return Resource{}, "", err
	}
	switch len(matches) {
	case 0:
		return Resource{}, notFound, nil
	case 1:
		return matches[0], "", nil
	}
	names := make([]string, 0, len(matches))
	for i, match := range matches {
		if i == 10 {
			names = append(names, "…")
			break
		}
		names = append(names, fmt.Sprintf("%s (%s)", mcpLabel(match), match.ID))
	}
	return Resource{}, fmt.Sprintf("%q matches more than one resource: %s. Use the id.", clipText(ref, 200), strings.Join(names, ", ")), nil
}

func (a *App) mcpReadResource(x mcpExchange, args json.RawMessage) (mcpOutcome, error) {
	var in struct {
		Resource string `json:"resource"`
		Version  int    `json:"version"`
		Offset   int64  `json:"offset"`
	}
	if problem := mcpArguments(args, &in); problem != "" {
		return mcpProblem("%s", problem)
	}
	if in.Version < 0 || in.Offset < 0 {
		return mcpProblem("Invalid arguments: version and offset cannot be negative.")
	}
	resource, problem, err := a.mcpResolve(x, in.Resource)
	if err != nil || problem != "" {
		return mcpOutcome{problem: problem}, err
	}
	if mcpEncrypted(resource) {
		return mcpProblem("%s", mcpEncryptedText)
	}
	page := a.baseURL(x.r) + "/resources/" + resource.ID
	ctx := x.r.Context()
	version, contentType, encoding, size := resource.Version, resource.ContentType, resource.ContentEncoding, resource.ContentSize
	read := func() ([]byte, error) { return a.db.ReadContent(ctx, resource) }
	openRange := func(start, end int64) (io.ReadCloser, int64, error) {
		return a.db.OpenContentRange(ctx, resource, start, end)
	}
	if in.Version != 0 && in.Version != resource.Version {
		earlier, err := a.db.VersionForOwner(ctx, x.user.ID, resource.ID, in.Version)
		if errors.Is(err, store.ErrNotFound) {
			return mcpProblem("%s has no version %d; list_versions shows the ones it has.", mcpLabel(resource), in.Version)
		}
		if err != nil {
			return mcpOutcome{}, err
		}
		if len(earlier.SealedMeta) > 0 || earlier.ContentType == store.SealedContentType || earlier.ContentType == store.EncryptedContentType {
			return mcpProblem("%s", mcpEncryptedText)
		}
		version, contentType, encoding, size = earlier.Number, earlier.ContentType, earlier.ContentEncoding, earlier.ContentSize
		read = func() ([]byte, error) { return a.db.ReadVersion(ctx, earlier) }
		openRange = func(start, end int64) (io.ReadCloser, int64, error) {
			return a.db.OpenVersionRange(ctx, earlier, start, end)
		}
	}
	if !store.TextLike(contentType) {
		return a.mcpBinary(resource, version, contentType, size, page), nil
	}
	part, ok, err := mcpUTF8Part(encoding, size, in.Offset, openRange)
	if err != nil {
		return mcpOutcome{}, err
	}
	if !ok {
		stored, err := read()
		if err != nil {
			return mcpOutcome{}, err
		}
		text, decodedAs, err := store.DecodeText(stored, encoding)
		if err != nil {
			return a.mcpBinary(resource, version, contentType, size, page), nil
		}
		encoding = decodedAs
		part = textPart(text, in.Offset)
	}
	if part.past {
		return mcpProblem("offset %d is past the end: the text of this version is %d bytes long.", in.Offset, part.length)
	}
	structured := map[string]any{
		"id": resource.ID, "name": resource.Name, "filename": resource.Filename, "type": contentType,
		"encoding": encoding, "version": version, "current_version": resource.Version,
		"offset": part.start, "length": part.length, "text": part.text, "url": page,
	}
	header := fmt.Sprintf("%s · version %d", mcpLabel(resource), version)
	if version == resource.Version {
		header += " (current)"
	} else {
		header += fmt.Sprintf(" (current is %d)", resource.Version)
	}
	if part.start > 0 || part.end < part.length {
		header += fmt.Sprintf(" · bytes %d-%d of %d", part.start, part.end, part.length)
	}
	if part.end < part.length {
		structured["next_offset"] = part.end
		header += fmt.Sprintf(" · to continue, read again with offset %d", part.end)
	}
	return mcpOutcome{structured: structured, text: []string{header, part.text}}, nil
}

// mcpTextPart is one part of a text as read_resource returns it: the bytes
// from start to end of a text length bytes long, cut on character
// boundaries. past is an offset beyond the end.
type mcpTextPart struct {
	text               string
	start, end, length int64
	past               bool
}

// textPart cuts the part of text that begins at offset.
func textPart(text string, offset int64) mcpTextPart {
	length := int64(len(text))
	if offset > length {
		return mcpTextPart{length: length, past: true}
	}
	start := offset
	for start < length && !utf8.RuneStart(text[start]) {
		start++
	}
	end := min(start+mcpReadChunk, length)
	for end < length && !utf8.RuneStart(text[end]) {
		end--
	}
	return mcpTextPart{text: text[start:end], start: start, end: end, length: length}
}

// utf8BOM is what a UTF-8 file may begin with. Decoding drops it, so the text
// read_resource counts in is the stored bytes without it.
const utf8BOM = "\xef\xbb\xbf"

// mcpUTF8Part reads one part of a long UTF-8 text from storage alone, rather
// than the whole of it for every part. The text of stored UTF-8 is its bytes
// less a BOM, so a part is the same range of bytes, a few more fetched to
// find the character boundaries. ok is false when there is no such shortcut
// - a short text, another encoding, bytes that turn out not to be UTF-8 -
// and the whole is then read and decoded instead.
func mcpUTF8Part(encoding string, size, offset int64, openRange func(start, end int64) (io.ReadCloser, int64, error)) (mcpTextPart, bool, error) {
	if !strings.EqualFold(encoding, "utf-8") || size <= mcpReadChunk {
		return mcpTextPart{}, false, nil
	}
	read := func(start, end int64) ([]byte, error) {
		body, _, err := openRange(start, end)
		if err != nil {
			return nil, err
		}
		defer body.Close()
		return io.ReadAll(io.LimitReader(body, end-start+1))
	}
	head, err := read(0, int64(len(utf8BOM))-1)
	if err != nil {
		return mcpTextPart{}, false, err
	}
	skip := int64(0)
	if string(head) == utf8BOM {
		skip = int64(len(utf8BOM))
	}
	length := size - skip
	if offset > length {
		return mcpTextPart{length: length, past: true}, true, nil
	}
	if offset == length {
		return mcpTextPart{start: length, end: length, length: length}, true, nil
	}
	// A part may begin up to three continuation bytes late and end on the
	// byte after it, so that much more is read than the part itself.
	last := min(offset+mcpReadChunk+utf8.UTFMax, length) - 1
	window, err := read(skip+offset, skip+last)
	if err != nil {
		return mcpTextPart{}, false, err
	}
	if int64(len(window)) != last-offset+1 {
		return mcpTextPart{}, false, nil
	}
	at := func(i int64) byte { return window[i-offset] }
	start := offset
	for start <= last && !utf8.RuneStart(at(start)) {
		start++
	}
	if start > last && start < length {
		return mcpTextPart{}, false, nil
	}
	end := min(start+mcpReadChunk, length)
	for end < length && !utf8.RuneStart(at(end)) {
		end--
	}
	text := window[start-offset : end-offset]
	if !utf8.Valid(text) {
		return mcpTextPart{}, false, nil
	}
	return mcpTextPart{text: string(text), start: start, end: end, length: length}, true, nil
}

// mcpBinary describes content that is not text, which no tool returns.
func (a *App) mcpBinary(resource Resource, version int, contentType string, size int64, page string) mcpOutcome {
	return mcpOutcome{
		structured: map[string]any{
			"id": resource.ID, "name": resource.Name, "filename": resource.Filename, "type": contentType,
			"size": size, "version": version, "current_version": resource.Version, "binary": true, "url": page,
		},
		text: []string{fmt.Sprintf("%s · version %d is %s content of %d bytes, which is not returned as text. It can be opened at %s.",
			mcpLabel(resource), version, contentType, size, page)},
	}
}

func (a *App) mcpListVersions(x mcpExchange, args json.RawMessage) (mcpOutcome, error) {
	var in struct {
		Resource string `json:"resource"`
	}
	if problem := mcpArguments(args, &in); problem != "" {
		return mcpProblem("%s", problem)
	}
	resource, problem, err := a.mcpResolve(x, in.Resource)
	if err != nil || problem != "" {
		return mcpOutcome{problem: problem}, err
	}
	earlier, err := a.db.ListVersions(x.r.Context(), x.user.ID, resource.ID)
	if err != nil {
		return mcpOutcome{}, err
	}
	type versionItem struct {
		Version      int       `json:"version"`
		Size         int64     `json:"size"`
		SavedAt      time.Time `json:"saved_at"`
		Current      bool      `json:"current,omitempty"`
		RestoredFrom int       `json:"restored_from,omitempty"`
	}
	versions := make([]versionItem, 0, len(earlier)+1)
	versions = append(versions, versionItem{Version: resource.Version, Size: resource.ContentSize, SavedAt: resource.VersionAt,
		Current: true, RestoredFrom: resource.RestoredFrom})
	for _, v := range earlier {
		versions = append(versions, versionItem{Version: v.Number, Size: v.ContentSize, SavedAt: v.SavedAt, RestoredFrom: v.RestoredFrom})
	}
	name := resource.Name
	if mcpEncrypted(resource) {
		name = mcpEncryptedName
	}
	return mcpOutcome{structured: map[string]any{
		"id": resource.ID, "name": name, "current_version": resource.Version, "versions": versions,
		"url": a.baseURL(x.r) + "/resources/" + resource.ID,
	}}, nil
}
