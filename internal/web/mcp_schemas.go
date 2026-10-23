package web

// The shape of each tool's structured result, as its outputSchema, so that a
// client knows the field names before the first call. A field left out of
// required is one the result leaves out when it does not apply.

var (
	mcpStringOut  = map[string]any{"type": "string"}
	mcpIntegerOut = map[string]any{"type": "integer"}
	mcpBooleanOut = map[string]any{"type": "boolean"}
	mcpTimeOut    = map[string]any{"type": "string", "format": "date-time"}
	mcpStringsOut = map[string]any{"type": "array", "items": mcpStringOut}
)

func mcpObjectOut(required []string, properties map[string]any) map[string]any {
	return map[string]any{"type": "object", "properties": properties, "required": required}
}

func mcpDescribed(schema map[string]any, description string) map[string]any {
	described := map[string]any{"description": description}
	for key, value := range schema {
		described[key] = value
	}
	return described
}

var mcpResourceOut = mcpObjectOut([]string{"id", "name", "size", "updated_at", "url"}, map[string]any{
	"id":          mcpStringOut,
	"name":        mcpStringOut,
	"filename":    mcpStringOut,
	"type":        mcpDescribed(mcpStringOut, "The content type, such as text/plain."),
	"size":        mcpDescribed(mcpIntegerOut, "Bytes stored."),
	"version":     mcpIntegerOut,
	"updated_at":  mcpTimeOut,
	"kind":        map[string]any{"type": "string", "enum": []string{"resource", "quick_share"}},
	"text":        mcpDescribed(mcpBooleanOut, "Whether read_resource returns it as text."),
	"encrypted":   mcpDescribed(mcpBooleanOut, "End-to-end encrypted: listed as a placeholder, never read here."),
	"taken_down":  mcpBooleanOut,
	"expires_at":  mcpDescribed(mcpTimeOut, "When a quick share ends."),
	"share_links": mcpDescribed(mcpIntegerOut, "How many of its share links work now."),
	"url":         mcpDescribed(mcpStringOut, "Its page."),
	"tags":        mcpStringsOut,
})

var mcpListResourcesOut = mcpObjectOut([]string{"resources", "total"}, map[string]any{
	"resources":   map[string]any{"type": "array", "items": mcpResourceOut},
	"total":       mcpDescribed(mcpIntegerOut, "How many resources match, on every page."),
	"next_cursor": mcpDescribed(mcpStringOut, "Pass as cursor for the next page; absent on the last."),
})

var mcpListTagsOut = mcpObjectOut([]string{"tags"}, map[string]any{
	"tags": map[string]any{"type": "array", "items": mcpObjectOut([]string{"name", "count"}, map[string]any{
		"name":  mcpStringOut,
		"count": mcpDescribed(mcpIntegerOut, "How many resources carry it."),
	})},
})

var mcpReadResourceOut = mcpObjectOut([]string{"id", "type", "version", "current_version", "url"}, map[string]any{
	"id":              mcpStringOut,
	"name":            mcpStringOut,
	"filename":        mcpStringOut,
	"type":            mcpStringOut,
	"encoding":        mcpDescribed(mcpStringOut, "The text encoding of the file, such as utf-8."),
	"version":         mcpDescribed(mcpIntegerOut, "The version read."),
	"current_version": mcpDescribed(mcpIntegerOut, "The version that is current now: the base_version for update_resource or edit_resource."),
	"offset":          mcpDescribed(mcpIntegerOut, "Where this part starts, in bytes of UTF-8 text."),
	"length":          mcpDescribed(mcpIntegerOut, "The length of the whole text, in bytes of UTF-8 text: an offset near it reads the end, such as the last lines of a log."),
	"text":            mcpStringOut,
	"next_offset":     mcpDescribed(mcpIntegerOut, "Pass as offset to read the next part; absent once the text is all read."),
	"binary":          mcpDescribed(mcpBooleanOut, "The content is not text and is not returned."),
	"size":            mcpDescribed(mcpIntegerOut, "Bytes stored, for content that is not text."),
	"url":             mcpStringOut,
})

var mcpListVersionsOut = mcpObjectOut([]string{"id", "name", "current_version", "versions", "url"}, map[string]any{
	"id":              mcpStringOut,
	"name":            mcpStringOut,
	"current_version": mcpIntegerOut,
	"versions": map[string]any{"type": "array", "items": mcpObjectOut([]string{"version", "size", "saved_at"}, map[string]any{
		"version":       mcpIntegerOut,
		"size":          mcpIntegerOut,
		"size_delta":    mcpDescribed(mcpIntegerOut, "Bytes more or fewer than the version kept before it; absent on the oldest."),
		"sha256":        mcpDescribed(mcpStringOut, "SHA-256 of the stored bytes, in hex; absent for some versions saved long ago."),
		"saved_at":      mcpTimeOut,
		"current":       mcpBooleanOut,
		"restored_from": mcpDescribed(mcpIntegerOut, "The earlier version this one brought back."),
	})},
	"url": mcpStringOut,
})

var mcpDiffVersionsOut = mcpObjectOut([]string{"id", "from", "to", "added", "removed", "url"}, map[string]any{
	"id":        mcpStringOut,
	"from":      mcpIntegerOut,
	"to":        mcpIntegerOut,
	"added":     mcpDescribed(mcpIntegerOut, "Lines only to has."),
	"removed":   mcpDescribed(mcpIntegerOut, "Lines only from has."),
	"diff":      mcpDescribed(mcpStringOut, "The unified diff; absent when too_large."),
	"too_large": mcpDescribed(mcpBooleanOut, "Too many changes to compare line by line."),
	"truncated": mcpDescribed(mcpBooleanOut, "The diff was cut short."),
	"url":       mcpStringOut,
})

var mcpSaveOut = mcpObjectOut([]string{"id", "name", "filename", "version", "new_version", "url"}, map[string]any{
	"id":              mcpStringOut,
	"name":            mcpStringOut,
	"filename":        mcpStringOut,
	"version":         mcpDescribed(mcpIntegerOut, "The version the resource is at now."),
	"new_version":     mcpDescribed(mcpBooleanOut, "False when the text was what it already held, and nothing was saved."),
	"history_trimmed": mcpDescribed(mcpIntegerOut, "Earlier versions removed to make room."),
	"replacements":    mcpDescribed(mcpIntegerOut, "Occurrences replaced, by edit_resource."),
	"url":             mcpStringOut,
})

var mcpTagResourceOut = mcpObjectOut([]string{"id", "tags"}, map[string]any{
	"id":   mcpStringOut,
	"tags": mcpDescribed(mcpStringsOut, "Every tag it carries now."),
})

var mcpShareResourceOut = mcpObjectOut([]string{"link_id", "url", "resource_id", "max_uses"}, map[string]any{
	"link_id":     mcpStringOut,
	"url":         mcpDescribed(mcpStringOut, "The share link's address."),
	"resource_id": mcpStringOut,
	"max_uses":    mcpDescribed(mcpIntegerOut, "0 for no limit."),
	"expires_at":  mcpTimeOut,
})

var mcpUnshareResourceOut = mcpObjectOut([]string{"resource_id", "revoked"}, map[string]any{
	"resource_id": mcpStringOut,
	"link_id":     mcpStringOut,
	"revoked":     mcpDescribed(mcpIntegerOut, "How many links were revoked."),
})
