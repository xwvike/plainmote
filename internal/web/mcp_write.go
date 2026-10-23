package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"plainmote/internal/store"
)

// The tools that change something. Writing needs the write scope; making or
// ending a share link needs share, which a token never has unless its owner
// ticked it on the consent page.
//
// A link is the one thing here that reaches past the account: anyone holding
// its address can open what it points at. So the tool says so in its
// description and annotations, which lead clients to ask before calling it;
// it refuses encrypted resources, whose key it does not have; it is limited
// per authorization; every link it makes carries the app's name, which the
// resource page shows beside it; and every one ends within mcpShareMax. An app
// can be talked into a call by what it reads, and a link that never ends
// would keep handing out every later save to whoever got it. A link without
// an end is made on the resource's page.
const (
	mcpNameMaxRunes  = 200
	mcpShareDefault  = 7 * 24 * time.Hour
	mcpShareMax      = 30 * 24 * time.Hour
	mcpShareMaxUses  = 1_000_000
	mcpShareNameRune = 100
)

var mcpWriteAnnotations = map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": false, "openWorldHint": false}

var mcpWriteTools = []*mcpTool{
	{
		Name:  "create_resource",
		Title: "Create a resource",
		Description: "Create a new text resource in this PlainMote account, such as a configuration file, a log or a note. " +
			"It stays private until a share link is made for it. The account's storage quota and the size limit apply.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"content":  map[string]any{"type": "string", "description": "The text to keep."},
				"filename": map[string]any{"type": "string", "description": "A filename such as nginx.conf. Its extension decides how the text is typed."},
				"name":     map[string]any{"type": "string", "description": "The name shown in lists. The filename if left out."},
				"tags":     mcpTagsSchema("Tags to put on it, such as tmp."),
			},
			"required":             []string{"content"},
			"additionalProperties": false,
		},
		OutputSchema: mcpResourceOut,
		Annotations:  mcpWriteAnnotations,
		scope:        store.OAuthScopeWrite,
		run:          (*App).mcpCreateResource,
	},
	{
		Name:  "update_resource",
		Title: "Save a new version",
		Description: "Save the whole new text of a resource as its next version; earlier versions stay in its history. " +
			"For a change to part of it, edit_resource sends only that part. " +
			"base_version must be the version the change was made from, as read_resource gave it: if the resource was saved since, " +
			"nothing is saved and it has to be read again. Text identical to the current text is never a conflict: it changes nothing. " +
			"The file keeps its encoding and line endings.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"resource":     resourceRefSchema,
				"base_version": map[string]any{"type": "integer", "minimum": 1, "description": "The version the new text was made from."},
				"content":      map[string]any{"type": "string", "description": "The whole new text."},
				"name":         map[string]any{"type": "string", "description": "A new name. Unchanged if left out."},
				"filename":     map[string]any{"type": "string", "description": "A new filename. Unchanged if left out."},
			},
			"required":             []string{"resource", "base_version", "content"},
			"additionalProperties": false,
		},
		OutputSchema: mcpSaveOut,
		Annotations:  mcpWriteAnnotations,
		scope:        store.OAuthScopeWrite,
		run:          (*App).mcpUpdateResource,
	},
	{
		Name:  "edit_resource",
		Title: "Edit a resource",
		Description: "Change part of a text resource without sending all of it: each edit replaces old_text, which must appear exactly once, with new_text, " +
			"and the result is saved as the next version. Edits apply in order, each to the text the ones before it left. " +
			"base_version must be the version the edits were made against, as read_resource gave it: if the resource was saved since, nothing is saved. " +
			"Nothing is saved either if an old_text is missing, or appears more than once without replace_all. " +
			"Line breaks match whether the file uses LF or CRLF, and the file keeps its encoding and line endings.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"resource":     resourceRefSchema,
				"base_version": map[string]any{"type": "integer", "minimum": 1, "description": "The version the edits were made against."},
				"edits": map[string]any{
					"type": "array", "minItems": 1, "maxItems": mcpEditMax,
					"description": "The replacements, applied in order.",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"old_text":    map[string]any{"type": "string", "description": "Text to replace, copied exactly, with enough around it to be unique."},
							"new_text":    map[string]any{"type": "string", "description": "What replaces it; empty to delete it."},
							"replace_all": map[string]any{"type": "boolean", "description": "Replace every occurrence rather than requiring exactly one."},
						},
						"required":             []string{"old_text", "new_text"},
						"additionalProperties": false,
					},
				},
			},
			"required":             []string{"resource", "base_version", "edits"},
			"additionalProperties": false,
		},
		OutputSchema: mcpSaveOut,
		Annotations:  mcpWriteAnnotations,
		scope:        store.OAuthScopeWrite,
		run:          (*App).mcpEditResource,
	},
	{
		Name:  "tag_resource",
		Title: "Tag a resource",
		Description: "Add tags to a resource or remove them; its content and version stay as they are. " +
			"A tag is free text of at most 24 characters; one differing only in case from a tag the account uses is that tag. " +
			"Encrypted resources and quick shares can be tagged too.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"resource": resourceRefSchema,
				"add":      mcpTagsSchema("Tags to put on it."),
				"remove":   mcpTagsSchema("Tags to take off it."),
			},
			"required":             []string{"resource"},
			"additionalProperties": false,
		},
		OutputSchema: mcpTagResourceOut,
		Annotations:  map[string]any{"readOnlyHint": false, "destructiveHint": false, "idempotentHint": true, "openWorldHint": false},
		scope:        store.OAuthScopeWrite,
		run:          (*App).mcpTagResource,
	},
}

func mcpTagsSchema(description string) map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": description}
}

var mcpShareTools = []*mcpTool{
	{
		Name:  "share_resource",
		Title: "Create a share link",
		Description: "Create a public share link for a resource. ANYONE who gets the address can open the content, without signing in, " +
			"until the link expires, is used up or is revoked. Only do this when the user has asked for a link. " +
			"Encrypted resources cannot be shared here. Links expire after 7 days unless expires_in says otherwise, and after 30 days at most; " +
			"a link that never expires can only be made by the owner on the resource's page.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"resource":   resourceRefSchema,
				"name":       map[string]any{"type": "string", "description": "A note for the owner's list of links, such as who it is for."},
				"expires_in": map[string]any{"type": "string", "description": "How long the link works, such as 1h, 24h or 7d, at most 30d. 7d if left out."},
				"max_uses":   map[string]any{"type": "integer", "minimum": 0, "description": "How many times it can be opened; 0 or left out for no limit."},
			},
			"required":             []string{"resource"},
			"additionalProperties": false,
		},
		OutputSchema: mcpShareResourceOut,
		Annotations:  map[string]any{"readOnlyHint": false, "destructiveHint": true, "idempotentHint": false, "openWorldHint": true},
		scope:        store.OAuthScopeShare,
		run:          (*App).mcpShareResource,
	},
	{
		Name:  "unshare_resource",
		Title: "Revoke share links",
		Description: "Revoke a resource's share links so their addresses stop working: one link, given by its id or address, " +
			"or every live link of the resource when link is left out.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"resource": resourceRefSchema,
				"link":     map[string]any{"type": "string", "description": "The link_id or address share_resource returned. Every link of the resource if left out."},
			},
			"required":             []string{"resource"},
			"additionalProperties": false,
		},
		OutputSchema: mcpUnshareResourceOut,
		Annotations:  map[string]any{"readOnlyHint": false, "destructiveHint": true, "idempotentHint": true, "openWorldHint": false},
		scope:        store.OAuthScopeShare,
		run:          (*App).mcpUnshareResource,
	},
}

// mcpStoreProblem turns what the store refused into words a model can act
// on. Anything else is the service failing, and is returned as an error.
func mcpStoreProblem(err error) (mcpOutcome, error) {
	if errors.Is(err, store.ErrQuotaExceeded) || store.IsRefusal(err) {
		return mcpProblem("%s", localizePageError("en", err.Error()))
	}
	return mcpOutcome{}, err
}

func (a *App) mcpCreateResource(x mcpExchange, args json.RawMessage) (mcpOutcome, error) {
	var in struct {
		Content  *string  `json:"content"`
		Filename string   `json:"filename"`
		Name     string   `json:"name"`
		Tags     []string `json:"tags"`
	}
	if problem := mcpArguments(args, &in); problem != "" {
		return mcpProblem("%s", problem)
	}
	if in.Content == nil {
		return mcpProblem("Invalid arguments: content is required.")
	}
	if problem := a.mcpTextProblem(*in.Content, in.Name); problem != "" {
		return mcpProblem("%s", problem)
	}
	if err := checkTags(in.Tags); err != nil {
		return mcpStoreProblem(err)
	}
	resource, err := a.db.CreateResource(x.r.Context(), x.user.ID, in.Name, in.Filename, []byte(*in.Content), "")
	if err != nil {
		return mcpStoreProblem(err)
	}
	if len(in.Tags) > 0 {
		if resource.Tags, err = a.db.SetResourceTags(x.r.Context(), x.user.ID, resource.ID, in.Tags); err != nil {
			return mcpStoreProblem(err)
		}
	}
	item := a.mcpResourceItem(x.r, resource)
	return mcpOutcome{structured: item, text: []string{fmt.Sprintf("Created %s (id %s) as version 1. It is private; its page is %s.",
		mcpLabel(resource), resource.ID, item.URL)}}, nil
}

// mcpTextProblem checks text sent to be kept: its size, and a name a list can
// show.
func (a *App) mcpTextProblem(content, name string) string {
	if int64(len(content)) > a.cfg.MaxContent {
		return fmt.Sprintf("The content is %d bytes; a resource can hold at most %d.", len(content), a.cfg.MaxContent)
	}
	if utf8.RuneCountInString(name) > mcpNameMaxRunes {
		return fmt.Sprintf("Invalid arguments: name can be at most %d characters.", mcpNameMaxRunes)
	}
	return ""
}

func (a *App) mcpUpdateResource(x mcpExchange, args json.RawMessage) (mcpOutcome, error) {
	var in struct {
		Resource    string  `json:"resource"`
		BaseVersion int     `json:"base_version"`
		Content     *string `json:"content"`
		Name        *string `json:"name"`
		Filename    *string `json:"filename"`
	}
	if problem := mcpArguments(args, &in); problem != "" {
		return mcpProblem("%s", problem)
	}
	if in.BaseVersion < 1 {
		return mcpProblem("Invalid arguments: base_version is required: the version the new text was made from, as read_resource gave it.")
	}
	if in.Content == nil {
		return mcpProblem("Invalid arguments: content is required: the whole new text.")
	}
	resource, problem, err := a.mcpResolve(x, in.Resource)
	if err != nil || problem != "" {
		return mcpOutcome{problem: problem}, err
	}
	name, filename := resource.Name, resource.Filename
	if in.Name != nil {
		name = *in.Name
	}
	if in.Filename != nil {
		filename = *in.Filename
	}
	if problem := mcpUnwritable(resource); problem != "" {
		return mcpProblem("%s", problem)
	}
	// The new text is written the way the file is: in its encoding, with its
	// line endings, as the editor on the resource page does.
	eol := store.EOLLF
	if stored, err := a.db.ReadContent(x.r.Context(), resource); err == nil {
		if text, _, err := store.DecodeText(stored, resource.ContentEncoding); err == nil {
			eol = store.DetectEOL(text)
		}
	}
	return a.mcpSaveText(x, resource, name, filename, *in.Content, eol, in.BaseVersion)
}

// mcpEditMax bounds the replacements one call makes.
const mcpEditMax = 100

func (a *App) mcpEditResource(x mcpExchange, args json.RawMessage) (mcpOutcome, error) {
	var in struct {
		Resource    string `json:"resource"`
		BaseVersion int    `json:"base_version"`
		Edits       []struct {
			Old        *string `json:"old_text"`
			New        *string `json:"new_text"`
			ReplaceAll bool    `json:"replace_all"`
		} `json:"edits"`
	}
	if problem := mcpArguments(args, &in); problem != "" {
		return mcpProblem("%s", problem)
	}
	if in.BaseVersion < 1 {
		return mcpProblem("Invalid arguments: base_version is required: the version the edits were made against, as read_resource gave it.")
	}
	if len(in.Edits) == 0 || len(in.Edits) > mcpEditMax {
		return mcpProblem("Invalid arguments: edits must hold from 1 to %d replacements.", mcpEditMax)
	}
	resource, problem, err := a.mcpResolve(x, in.Resource)
	if err != nil || problem != "" {
		return mcpOutcome{problem: problem}, err
	}
	if problem := mcpUnwritable(resource); problem != "" {
		return mcpProblem("%s", problem)
	}
	if resource.Version != in.BaseVersion {
		return mcpConflict(resource, in.BaseVersion, resource.Version)
	}
	stored, err := a.db.ReadContent(x.r.Context(), resource)
	if err != nil {
		return mcpOutcome{}, err
	}
	text, _, err := store.DecodeText(stored, resource.ContentEncoding)
	if err != nil {
		return mcpProblem("%s is not readable as text, so it cannot be edited here.", mcpLabel(resource))
	}
	// Matching is on line breaks as LF, whatever the file uses, and the file
	// gets its own back when it is written.
	eol := store.DetectEOL(text)
	text = store.ApplyEOL(text, store.EOLLF)
	replaced := 0
	for i, edit := range in.Edits {
		if edit.Old == nil || edit.New == nil {
			return mcpProblem("Invalid arguments: edit %d needs old_text and new_text.", i+1)
		}
		old, replacement := store.ApplyEOL(*edit.Old, store.EOLLF), store.ApplyEOL(*edit.New, store.EOLLF)
		if old == "" {
			return mcpProblem("Invalid arguments: edit %d has an empty old_text. To write the whole text, use update_resource.", i+1)
		}
		switch count := strings.Count(text, old); {
		case count == 0:
			return mcpProblem("Nothing was saved: the old_text of edit %d is not in version %d of %s%s. "+
				"Copy it exactly as read_resource gave it, spaces and line breaks included.", i+1, resource.Version, mcpLabel(resource), mcpAfterEarlier(i))
		case count > 1 && !edit.ReplaceAll:
			return mcpProblem("Nothing was saved: the old_text of edit %d appears %d times in %s%s. "+
				"Include more of the text around it to pick one, or set replace_all to change every one.", i+1, count, mcpLabel(resource), mcpAfterEarlier(i))
		case edit.ReplaceAll:
			text = strings.ReplaceAll(text, old, replacement)
			replaced += count
		default:
			text = strings.Replace(text, old, replacement, 1)
			replaced++
		}
	}
	outcome, err := a.mcpSaveText(x, resource, resource.Name, resource.Filename, text, eol, in.BaseVersion)
	if structured, ok := outcome.structured.(map[string]any); ok {
		structured["replacements"] = replaced
	}
	return outcome, err
}

func mcpAfterEarlier(edit int) string {
	if edit == 0 {
		return ""
	}
	return " as the edits before it left it"
}

// mcpUnwritable says why a resource's text cannot be replaced here, if it
// cannot.
func mcpUnwritable(resource Resource) string {
	switch {
	case mcpEncrypted(resource):
		return mcpEncryptedText
	case resource.QuickShare():
		return fmt.Sprintf("%s is a quick share, which is read only until it is kept as a resource on its page.", mcpLabel(resource))
	case !store.TextLike(resource.ContentType):
		return fmt.Sprintf("%s holds %s content, which cannot be replaced with text here.", mcpLabel(resource), resource.ContentType)
	}
	return ""
}

func mcpConflict(resource Resource, base, current int) (mcpOutcome, error) {
	return mcpProblem("Nothing was saved: %s has been saved since version %d and is now at version %d. "+
		"Read it again with read_resource, make the change to that text, and save with base_version %d.",
		mcpLabel(resource), base, current, current)
}

// mcpSaveText saves text as a resource's next version, in its encoding and
// with the line endings eol names, unless it was saved since base.
func (a *App) mcpSaveText(x mcpExchange, resource Resource, name, filename, text, eol string, base int) (mcpOutcome, error) {
	if problem := a.mcpTextProblem(text, name); problem != "" {
		return mcpProblem("%s", problem)
	}
	encoded, encoding, err := store.EncodeText(store.ApplyEOL(text, eol), resource.ContentEncoding)
	if err != nil {
		return mcpStoreProblem(err)
	}
	result, err := a.db.SaveResource(x.r.Context(), x.user.ID, resource.ID, store.ResourceEdit{
		Name: name, Filename: filename, Content: encoded, ContentEncoding: encoding, BaseVersion: base,
	})
	if conflict := (*store.VersionConflict)(nil); errors.As(err, &conflict) {
		return mcpConflict(resource, base, conflict.Current)
	}
	if errors.Is(err, store.ErrNotFound) {
		return mcpProblem("%s no longer exists.", mcpLabel(resource))
	}
	if err != nil {
		return mcpStoreProblem(err)
	}
	structured := map[string]any{
		"id": resource.ID, "name": name, "filename": filename, "version": result.Version, "new_version": result.NewVersion,
		"url": a.baseURL(x.r) + "/resources/" + resource.ID,
	}
	message := fmt.Sprintf("Saved %s as version %d.", mcpLabel(resource), result.Version)
	if !result.NewVersion {
		message = fmt.Sprintf("The text is what %s already holds; it stays at version %d.", mcpLabel(resource), result.Version)
	}
	if result.Trimmed > 0 {
		structured["history_trimmed"] = result.Trimmed
		message += fmt.Sprintf(" To make room, the %d oldest earlier versions were removed from the account's history.", result.Trimmed)
	}
	return mcpOutcome{structured: structured, text: []string{message}}, nil
}

func (a *App) mcpShareResource(x mcpExchange, args json.RawMessage) (mcpOutcome, error) {
	var in struct {
		Resource  string `json:"resource"`
		Name      string `json:"name"`
		ExpiresIn string `json:"expires_in"`
		MaxUses   int    `json:"max_uses"`
	}
	if problem := mcpArguments(args, &in); problem != "" {
		return mcpProblem("%s", problem)
	}
	ttl := mcpShareDefault
	switch value := strings.ToLower(strings.TrimSpace(in.ExpiresIn)); value {
	case "":
	case "never":
		return mcpProblem("A link made by an app lasts at most 30 days. A link that never expires can only be made by the owner, on the resource's page.")
	default:
		parsed, err := parseShareDuration(value)
		if err != nil || parsed <= 0 || parsed > mcpShareMax {
			return mcpProblem("Invalid arguments: expires_in must be a duration such as 1h, 24h or 7d, at most 30d.")
		}
		ttl = parsed
	}
	if in.MaxUses < 0 || in.MaxUses > mcpShareMaxUses {
		return mcpProblem("Invalid arguments: max_uses must be 0 for no limit, or a number of uses up to %d.", mcpShareMaxUses)
	}
	if utf8.RuneCountInString(in.Name) > mcpShareNameRune {
		return mcpProblem("Invalid arguments: name can be at most %d characters.", mcpShareNameRune)
	}
	resource, problem, err := a.mcpResolve(x, in.Resource)
	if err != nil || problem != "" {
		return mcpOutcome{problem: problem}, err
	}
	if mcpEncrypted(resource) {
		return mcpProblem("This resource is end-to-end encrypted. A link to it can only be made in the browser, which holds its key.")
	}
	if !a.limits.mcpShares.allow(x.access.GrantID, time.Now().UTC()) {
		return mcpProblem("This app has made too many share links in the last hour. Try again later, or make the link on the resource's page.")
	}
	link, err := a.db.CreateAppShare(x.r.Context(), x.user.ID, resource.ID, strings.TrimSpace(in.Name), ttl, in.MaxUses, x.access.ClientName)
	if errors.Is(err, store.ErrNotFound) {
		return mcpProblem("%s no longer exists.", mcpLabel(resource))
	}
	if err != nil {
		return mcpStoreProblem(err)
	}
	address := a.baseURL(x.r) + shareAddress(link.Token, deliveryFilename(resource, resource.ContentType))
	structured := map[string]any{"link_id": link.ID, "url": address, "resource_id": resource.ID, "max_uses": link.MaxUses}
	terms := "until it is revoked"
	if link.ExpiresAt != nil {
		structured["expires_at"] = link.ExpiresAt
		terms = "until " + link.ExpiresAt.UTC().Format(time.RFC3339)
	}
	if link.MaxUses > 0 {
		terms += fmt.Sprintf(", at most %d times", link.MaxUses)
	}
	return mcpOutcome{structured: structured, text: []string{fmt.Sprintf(
		"Anyone with this address can open %s %s: %s\nIt can be revoked with unshare_resource (link_id %s) or on the resource's page.",
		mcpLabel(resource), terms, address, link.ID)}}, nil
}

func (a *App) mcpUnshareResource(x mcpExchange, args json.RawMessage) (mcpOutcome, error) {
	var in struct {
		Resource string `json:"resource"`
		Link     string `json:"link"`
	}
	if problem := mcpArguments(args, &in); problem != "" {
		return mcpProblem("%s", problem)
	}
	resource, problem, err := a.mcpResolve(x, in.Resource)
	if err != nil || problem != "" {
		return mcpOutcome{problem: problem}, err
	}
	ctx := x.r.Context()
	now := time.Now().UTC()
	live, err := a.db.ListShares(ctx, x.user.ID, resource.ID, now)
	if err != nil {
		return mcpOutcome{}, err
	}
	ref := strings.TrimSpace(in.Link)
	if ref == "" {
		if err := a.db.RevokeShares(ctx, x.user.ID, resource.ID); err != nil {
			return mcpOutcome{}, err
		}
		return mcpOutcome{structured: map[string]any{"resource_id": resource.ID, "revoked": len(live)},
			text: []string{fmt.Sprintf("Revoked %d share links of %s; their addresses no longer open it.", len(live), mcpLabel(resource))}}, nil
	}
	linkID := ""
	if uuid.Validate(ref) == nil {
		linkID = strings.ToLower(ref)
	} else if parsed, err := url.Parse(ref); err == nil && parsed.Host != "" {
		if token, ok := splitDeliveryPath(parsed.Path); ok {
			for _, link := range live {
				if !link.Unreadable && link.Token == token {
					linkID = link.ID
				}
			}
		}
	}
	notFound := fmt.Sprintf("%s has no live share link matching %q.", mcpLabel(resource), clipText(ref, 200))
	if linkID == "" {
		return mcpProblem("%s", notFound)
	}
	if err := a.db.RevokeLink(ctx, x.user.ID, resource.ID, linkID); errors.Is(err, store.ErrNotFound) {
		return mcpProblem("%s", notFound)
	} else if err != nil {
		return mcpOutcome{}, err
	}
	return mcpOutcome{structured: map[string]any{"resource_id": resource.ID, "link_id": linkID, "revoked": 1},
		text: []string{fmt.Sprintf("Revoked the link %s of %s; its address no longer opens it.", linkID, mcpLabel(resource))}}, nil
}
