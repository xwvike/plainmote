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
// off, no page links where the pages do not exist, and nothing at all where
// neither does.
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
	text.WriteString("> PlainMote turns text and files into share links. Content is kept byte for byte as given; links can expire, be limited to a number of uses and be revoked, and every access is recorded.\n\n")

	if a.cfg.AnonymousEnabled {
		fmt.Fprintf(&text, `## Quick share (no account)

Send text or a file and get a link back, on one line:

`+"```"+`
cmd | curl -F 'content=<-' %[1]s/paste
curl -F content=@app.log %[1]s/paste
curl --data-binary @app.log '%[1]s/paste?ttl=30&filename=app.log'
`+"```"+`

- `+"`ttl`"+`: minutes until the link expires: 1, 5, 10 or 30. Default %[2]s.
- `+"`filename`"+`: the name at the end of the link. A form field, or a query parameter with `+"`--data-binary`"+`.
- At most %[3]s, stored byte for byte. UTF-8 text is served as `+"`text/plain; charset=utf-8`"+`; an image, audio or video (recognised by its bytes) as its own type; anything else as `+"`application/octet-stream`"+`, for download.
- Success is `+"`201`"+` with the link; a refusal is `+"`400`"+` with a one-line reason.
- `+"`curl %[1]s/paste`"+` prints this usage.

`, base, pasteDefaultTTL, legalBytes(store.AnonymousMaxBytes))
	}

	fmt.Fprintf(&text, `## Share links

- A link looks like `+"`%s/d/<token>/<filename>`"+`. The token is the credential: anyone holding the link can fetch the content while the link is valid, with no sign-in. A plain GET returns the raw bytes.
- Signed-in accounts (GitHub sign-in) keep resources up to %s each, give each recipient a separate link with its own expiry and use limit, revoke links one by one and read access records. Accounts have no API yet.
- An account can turn on end-to-end encryption for its quick shares. Such a link carries its key after `+"`#`"+` (or needs a passphrase); a GET returns only the ciphertext, as `+"`"+store.EncryptedContentType+"`"+`, and only a browser opening the full link can decrypt it.
`, base, legalBytes(a.cfg.MaxContent))

	if a.cfg.SourceURL != "" {
		fmt.Fprintf(&text, "- Source code (AGPL-3.0): %s\n", a.cfg.SourceURL)
	}

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
