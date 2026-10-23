package web

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"plainmote/internal/store"
)

// handleLLMs serves /llms.txt: the service described for a language model, in
// the llmstxt.org shape - a title, a one-line summary, then sections. It says
// only what this deployment offers: no quick share section where the box is
// off, no command line where the image carries no builds, no page links where
// the pages do not exist, and nothing at all where neither does.
func (a *App) handleLLMs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if len(a.indexablePages()) == 0 {
		writePlainError(w, http.StatusNotFound, "not found")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", crawlerFileCache)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = io.WriteString(w, a.llmsText())
}

func (a *App) llmsText() string {
	base := strings.TrimRight(a.cfg.PublicURL, "/")
	var text strings.Builder
	text.WriteString("# PlainMote\n\n")
	text.WriteString("> PlainMote keeps and shares configs, logs, notes and other files, byte for byte as given. Every save keeps a version; each recipient or machine gets a link of its own, with its own expiry and use limit, revocable on its own, and every access through it is recorded. It works from a browser, a terminal, an HTTP API and an MCP server for AI apps.\n\n")
	if a.cfg.SourceURL != "" {
		fmt.Fprintf(&text, "Source code (AGPL-3.0): %s\n\n", a.cfg.SourceURL)
	}

	if a.cfg.AnonymousEnabled {
		fmt.Fprintf(&text, `## Quick share (no account)

Send text or a file and get a link back, on one line:

`+"```"+`
cmd | curl -F 'content=<-' %[1]s/paste
curl -F content=@app.log %[1]s/paste
curl --data-binary @app.log '%[1]s/paste?ttl=1d&filename=app.log'
`+"```"+`

- `+"`ttl`"+`: how long the link works: `+"`10m`"+`, `+"`1h`"+`, `+"`1d`"+`, `+"`7d`"+` or `+"`30d`"+`. Default `+"`%[2]s`"+`.
- `+"`filename`"+`: the name at the end of the link. A form field, or a query parameter with `+"`--data-binary`"+`.
- At most %[3]s, stored byte for byte. UTF-8 text is served as `+"`text/plain; charset=utf-8`"+`; an image, audio or video (recognised by its bytes) as its own type; anything else as `+"`application/octet-stream`"+`, for download.
- Success is `+"`201`"+` with the link; a refusal is `+"`400`"+` with a one-line reason.
- `+"`curl %[1]s/paste`"+` prints this usage.

`, base, pasteDefaultTTL, legalBytes(store.AnonymousMaxBytes))
	}

	fmt.Fprintf(&text, `## Share links

- A link looks like `+"`%s/d/<token>/<filename>`"+`. The token is the credential: anyone holding the link can fetch the content while the link is valid, with no sign-in. A plain GET returns the raw bytes.
- Responses carry an `+"`ETag`"+` (and `+"`Last-Modified`"+` for stored content): a client that polls a link can send `+"`If-None-Match`"+` and gets `+"`304`"+` with no body while the content is unchanged. `+"`HEAD`"+` is answered too. Every request answered, `+"`304`"+` and `+"`HEAD`"+` included, counts as one use of the link and is recorded.
- A link to end-to-end encrypted content carries its key after `+"`#`"+`, which is never sent to the server, or is opened with a four-character code. A GET returns only ciphertext, as `+"`%s`"+`; a browser opening the full link decrypts it.
`, base, store.SealedContentType)

	text.WriteString("\n## Accounts\n\n")
	if providers := a.providerLabels(); providers != "" {
		fmt.Fprintf(&text, "- Sign-in is with %s; the service sets no sign-in password of its own.\n", providers)
	}
	fmt.Fprintf(&text, `- An account keeps resources up to %s each. A save that changes the content keeps the version it replaced, to compare or restore. Resources can carry tags: free text of at most 24 characters, one spelling per account whatever the case.
- Each recipient gets a separate link with its own expiry and use limit, revoked on its own; the owner reads every access.
- End-to-end encryption is optional, under a master password only the owner knows. Encrypted content, with its name and filename, is encrypted on the client; the server stores ciphertext and keys it cannot unwrap.
`, legalBytes(a.cfg.MaxContent))

	if len(a.cliBinaries()) > 0 {
		fmt.Fprintf(&text, `
## Command line

`+"`plainmote`"+` lists, reads, edits and uploads an account's resources from a terminal or a script. The install script points it at this server.

`+"```"+`
curl -fsSL %[1]s/cli | sh       # macOS, Linux
irm %[1]s/cli.ps1 | iex         # Windows PowerShell
`+"```"+`

- `+"`plainmote login`"+` signs the device in: it prints a code, which the account owner enters at `+"`%[1]s/cli/device`"+` in a signed-in browser. `+"`--read-only`"+` asks for a token that cannot write. `+"`PLAINMOTE_TOKEN`"+` takes the place of the saved sign-in.
- `+"`plainmote ls [keyword] [--tag <tag>]`"+`, `+"`cat <resource>`"+`, `+"`edit <resource>`"+`, `+"`push <file|-> [--to <resource>] [--tag <tag>]`"+`, `+"`tag <resource> <tag>...`"+`, `+"`untag <resource> <tag>...`"+`, `+"`tags`"+`%[2]s. A `+"`<resource>`"+` is an ID, an ID prefix of at least six characters, or an exact name or filename.
- %[3]s `+"`--encrypt`"+`. Encrypted resources are opened with the master password, asked for at the terminal or read from `+"`PLAINMOTE_MASTER_PASSWORD`"+`; the command line keeps no key.
- `+"`plainmote help`"+` lists every command and option.
`, base, a.llmsShareCommand(), a.llmsShareEncrypt())
	}

	fmt.Fprintf(&text, `
## HTTP API

- Base `+"`%[1]s/api/v1/`"+`. JSON, except resource content, which is raw bytes. Errors are `+"`{\"error\": \"<code>\", \"message\": \"...\"}`"+`.
- Authentication is a personal access token (`+"`pmt_...`"+`) in `+"`Authorization: Bearer`"+`, obtained by device authorization (RFC 8628): `+"`POST /api/v1/device/code`"+` with `+"`{\"scope\": \"read\"|\"write\", \"device\": \"...\"}`"+`, the owner approves the user code at `+"`%[1]s/cli/device`"+`, then poll `+"`POST /api/v1/device/token`"+` with `+"`{\"device_code\": \"...\"}`"+`. A token lasts 90 days; the owner can revoke it under Settings › Authorized apps.
- Reading: `+"`GET /me`"+`; `+"`GET /resources?q=&tag=`"+` lists, `+"`GET /resources/{id}`"+` describes, `+"`GET /resources/{id}/content`"+` returns the raw bytes; `+"`GET /tags`"+` lists the account's tags with their counts.
- Writing, with a `+"`write`"+` token: `+"`PUT /resources/{id}/content`"+` with `+"`If-Match: \"v<version>\"`"+` saves a new version (`+"`412`"+` on a conflict); `+"`POST /resources`"+` creates one from a multipart `+"`content`"+` file, with `+"`tag`"+` fields to tag it; `+"`PATCH /resources/{id}/tags`"+` with `+"`{\"add\": [...], \"remove\": [...]}`"+` changes its tags%[2]s.
- The API cannot delete resources, manage share links or read access records. Encrypted content is returned as ciphertext, for the client to decrypt.
- `+"`%[1]s/developers/cli`"+` and `+"`%[1]s/developers/api`"+` present the command line and the API to people.
`, base, a.llmsQuickShareAPI())

	fmt.Fprintf(&text, `
## MCP server

- `+"`%[1]s%[2]s`"+` is a remote MCP server over Streamable HTTP, for AI apps such as Claude, ChatGPT and Cursor. An app connecting to it is sent through OAuth authorization: the account owner signs in on this site and allows or denies it. An authorization covers every tool; none deletes anything. Apps register dynamically or by a client ID metadata document.
- Tools: `+"`list_resources`"+`, `+"`list_tags`"+`, `+"`read_resource`"+`, `+"`list_versions`"+` and `+"`diff_versions`"+` (read); `+"`create_resource`"+`, `+"`edit_resource`"+`, `+"`update_resource`"+` and `+"`tag_resource`"+` (write; an edit replaces parts of the text, an update the whole of it, and both name the version they were based on and are refused if the resource has moved on); `+"`share_resource`"+` and `+"`unshare_resource`"+` (share).
- Encrypted resources appear only as placeholders: no tool decrypts them or asks for the master password. Unencrypted content an app reads is sent to that app and its model provider.
- An authorization lasts until it goes unused for 30 days, and the owner can revoke it at any time. `+"`%[1]s/developers/mcp`"+` explains how to add the server to each app.
`, base, mcpPath)

	if a.cfg.ContactEmail != "" {
		text.WriteString("\n## Pages\n\n")
		for _, page := range []struct{ path, title, note string }{
			{"/about", "About", "what the service is"},
			{"/privacy", "Privacy Policy", "what is collected and kept"},
			{"/terms", "Terms of Service", "conditions and acceptable use"},
			{"/contact", "Contact", "questions, abuse reports and data requests"},
		} {
			fmt.Fprintf(&text, "- [%s](%s%s): %s\n", page.title, base, page.path, page.note)
		}
	}
	return text.String()
}

func (a *App) providerLabels() string {
	var labels []string
	for _, p := range a.providers() {
		labels = append(labels, p.label)
	}
	return strings.Join(labels, " or ")
}

func (a *App) llmsShareCommand() string {
	if !a.cfg.AnonymousEnabled {
		return ""
	}
	return ", `share <file|-> [--ttl 10m|1h|1d|7d|30d]` (a quick share; prints its link)"
}

func (a *App) llmsShareEncrypt() string {
	if !a.cfg.AnonymousEnabled {
		return "`push` takes"
	}
	return "`push` and `share` take"
}

func (a *App) llmsQuickShareAPI() string {
	if !a.cfg.AnonymousEnabled {
		return ""
	}
	return "; `POST /quick-shares` with `content` and optional `filename` and `ttl` makes a quick share and returns `share_url`"
}
