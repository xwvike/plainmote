package web

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"plainmote/internal/store"
)

const developersPath = "/developers"

// developerPages are the pages for someone who would rather type, in the
// order the side navigation lists them: the command line, the HTTP API,
// quick shares with curl where the deployment takes them, and the MCP server
// for AI apps, which every deployment has.
func (a *App) developerPages() []string {
	pages := []string{"cli", "api"}
	if a.cfg.AnonymousEnabled {
		pages = append(pages, "curl")
	}
	return append(pages, "mcp")
}

// developersMovedPages are earlier addresses of a developer page: the page
// for AI assistants became the MCP server's, which kept its llms.txt part.
var developersMovedPages = map[string]string{"ai": "mcp"}

// handleDevelopers serves /developers/<page>; /developers itself leads to
// the first. The pages are found by search where the deployment has public
// pages at all, in every language at an address of its own.
func (a *App) handleDevelopers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	prefix := ""
	if route, ok := routeLocale(r); ok {
		prefix = route.prefix
	}
	page := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, prefix), developersPath)
	if page == "" || page == "/" {
		http.Redirect(w, r, prefix+developersPath+"/cli", http.StatusFound)
		return
	}
	page = strings.TrimPrefix(page, "/")
	if moved, ok := developersMovedPages[page]; ok {
		http.Redirect(w, r, prefix+developersPath+"/"+moved, http.StatusMovedPermanently)
		return
	}
	if !slices.Contains(a.developerPages(), page) {
		writePlainError(w, http.StatusNotFound, "not found")
		return
	}
	path := developersPath + "/" + page
	locale := requestLanguage(r).Locale
	data := pageData{Active: "developers", Section: "dev-" + page, SignInURL: "/login", BaseURL: a.baseURL(r), Canonical: localePrefix(locale) + path}
	data.Indexable = a.indexable(data.Canonical)
	data.Alternates = localeAlternates(data.BaseURL, path)
	if user, _, ok := a.currentUser(r); ok {
		data.User, data.SignedIn, data.CSRF = user, true, csrfValue(r)
	}
	switch page {
	case "cli":
		w.Header().Add("Vary", "User-Agent")
		w.Header().Add("Vary", "Sec-CH-UA-Platform")
		data.CLIBinaries = a.cliBinaries()
		data.CLIVersion = a.cfg.Version
		data.CLIPlatform = clientPlatform(r)
		data.CLILang = cliLanguage(locale)
	case "api":
		data.APIDocsURL = apiDocsURL(a.cfg.SourceURL, locale)
	case "curl":
		data.PasteTTL = pasteDefaultTTL
		data.MaxPaste = store.AnonymousMaxBytes
	case "mcp":
		data.MCPClients = mcpClients(a.mcpResource(r))
	}
	a.renderTemplate(w, r, http.StatusOK, "developers.html", data)
}

// apiDocsURL is the API reference in the source, where the source is on
// GitHub and so its address is known; elsewhere the page goes without.
func apiDocsURL(source, locale string) string {
	parsed, err := url.Parse(source)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "github.com" {
		return ""
	}
	file := "api.md"
	if strings.HasPrefix(locale, "zh") {
		file = "api.zh-CN.md"
	}
	return strings.TrimRight(source, "/") + "/blob/main/docs/" + file
}

// mcpClient is one app on the MCP server page: how to add the server to it,
// as notes (message keys) and the commands or configuration they lead to.
type mcpClient struct {
	Name  string
	Parts []mcpClientPart
}

type mcpClientPart struct {
	Note  string
	Code  string
	Shell bool
	// Link installs the server in one click where the app has such links.
	Link string
}

// mcpClients are the apps the page shows, each with what it takes to add
// the server at address to it. Every one of them signs in through the browser on its own,
// so none needs a token in its configuration.
func mcpClients(address string) []mcpClient {
	cursor := mcpConfig(map[string]any{"mcpServers": map[string]any{"plainmote": map[string]string{"url": address}}})
	cursorLink := "https://cursor.com/en/install-mcp?name=plainmote&config=" +
		url.QueryEscape(base64.StdEncoding.EncodeToString(mcpCompact(map[string]string{"url": address})))
	return []mcpClient{
		{"Claude Code", []mcpClientPart{
			{Code: "claude mcp add --transport http --scope user plainmote " + address, Shell: true},
			{Note: "dev_mcp_claude_code"},
		}},
		{"Codex", []mcpClientPart{
			{Code: "codex mcp add plainmote --url " + address, Shell: true},
			{Note: "dev_mcp_codex"},
		}},
		{"Zed", []mcpClientPart{
			{Note: "dev_mcp_zed", Code: mcpConfig(map[string]any{"context_servers": map[string]any{"plainmote": map[string]string{"url": address}}})},
		}},
		{"VS Code", []mcpClientPart{
			{Code: "code --add-mcp '" + string(mcpCompact(map[string]string{"name": "plainmote", "type": "http", "url": address})) + "'", Shell: true},
			{Note: "dev_mcp_vscode", Code: mcpConfig(map[string]any{"servers": map[string]any{"plainmote": map[string]string{"type": "http", "url": address}}})},
		}},
		{"opencode", []mcpClientPart{
			{Note: "dev_mcp_opencode", Code: mcpConfig(map[string]any{"mcp": map[string]any{"plainmote": map[string]string{"type": "remote", "url": address}}})},
			{Note: "dev_mcp_opencode_auth", Code: "opencode mcp auth plainmote", Shell: true},
		}},
		{"Cursor", []mcpClientPart{
			{Note: "dev_mcp_cursor_link", Link: cursorLink},
			{Note: "dev_mcp_cursor", Code: cursor},
		}},
		{"Antigravity", []mcpClientPart{
			{Note: "dev_mcp_antigravity", Code: mcpConfig(map[string]any{"mcpServers": map[string]any{"plainmote": map[string]string{"serverUrl": address}}})},
		}},
		{"Claude", []mcpClientPart{
			{Note: "dev_mcp_claude", Code: address},
		}},
		{"ChatGPT", []mcpClientPart{
			{Note: "dev_mcp_chatgpt", Code: address},
		}},
	}
}

// mcpConfig is configuration as an app's documentation writes it: indented
// by two spaces, with the address as typed rather than escaped for HTML.
func mcpConfig(value any) string {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(value)
	return strings.TrimSuffix(out.String(), "\n")
}

func mcpCompact(value any) []byte {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
	return bytes.TrimSuffix(out.Bytes(), []byte("\n"))
}
