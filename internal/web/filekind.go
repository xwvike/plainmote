package web

import (
	"path"
	"strings"

	"plainmote/internal/store"
)

// fileKindView describes how a resource is classified visually in the Finder-style
// file browser: an icon Category (for restrained palette/glyph styling), a short
// uppercase extension Badge printed on the document icon, and a human-readable
// localized Kind Label shown in List view.
type fileKindView struct {
	Category string
	Badge    string
	Label    string
}

func fileKindForResource(locale string, r store.Resource) fileKindView {
	if r.Sealed() || r.Encrypted() {
		return fileKindView{
			Category: "locked",
			Badge:    "",
			Label:    translate(locale, "kind_file_encrypted"),
		}
	}
	return fileKindFromMeta(locale, r.Filename, r.ContentType)
}

func fileKindFromMeta(locale, filename, contentType string) fileKindView {
	base := strings.ToLower(path.Base(strings.TrimSpace(filename)))
	ext := strings.TrimPrefix(strings.ToLower(path.Ext(base)), ".")
	mime := strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))

	shortBadge := func(fallback string) string {
		if len(ext) >= 1 && len(ext) <= 4 && isASCIIAlnum(ext) {
			return strings.ToUpper(ext)
		}
		return fallback
	}

	// Well-known extensionless filenames.
	switch base {
	case "makefile", "gnumakefile":
		return fileKindView{Category: "code", Badge: "MAKE", Label: translate(locale, "kind_file_code")}
	case "dockerfile":
		return fileKindView{Category: "config", Badge: "DOCK", Label: translate(locale, "kind_file_config")}
	}

	switch ext {
	case "yaml", "yml":
		return fileKindView{Category: "config", Badge: "YAML", Label: translate(locale, "kind_file_yaml")}
	case "json", "jsonc", "jsonl":
		return fileKindView{Category: "config", Badge: "JSON", Label: translate(locale, "kind_file_json")}
	case "toml", "ini", "conf", "cfg", "env", "properties", "hcl", "tf", "service", "caddyfile", "nginx":
		return fileKindView{Category: "config", Badge: shortBadge("CONF"), Label: translate(locale, "kind_file_config")}
	case "md", "markdown", "mdx":
		return fileKindView{Category: "doc", Badge: "MD", Label: translate(locale, "kind_file_markdown")}
	case "log":
		return fileKindView{Category: "doc", Badge: "LOG", Label: translate(locale, "kind_file_log")}
	case "go", "py", "js", "mjs", "cjs", "ts", "tsx", "jsx", "rs", "c", "h", "cc", "cpp", "hpp", "java", "kt", "swift",
		"rb", "php", "sh", "bash", "zsh", "fish", "ps1", "sql", "html", "htm", "css", "scss", "less", "xml", "lua", "zig", "vue", "svelte":
		return fileKindView{Category: "code", Badge: shortBadge("CODE"), Label: translate(locale, "kind_file_code")}
	case "pdf":
		return fileKindView{Category: "pdf", Badge: "PDF", Label: translate(locale, "kind_file_pdf")}
	case "zip", "tar", "gz", "tgz", "bz2", "xz", "7z", "rar", "zst":
		return fileKindView{Category: "archive", Badge: shortBadge("ZIP"), Label: translate(locale, "kind_file_archive")}
	case "txt", "csv", "tsv", "rtf":
		return fileKindView{Category: "doc", Badge: shortBadge("TXT"), Label: translate(locale, "kind_file_text")}
	}

	switch {
	case mime == "application/pdf":
		return fileKindView{Category: "pdf", Badge: "PDF", Label: translate(locale, "kind_file_pdf")}
	case strings.HasPrefix(mime, "image/"):
		return fileKindView{Category: "image", Badge: shortBadge("IMG"), Label: translate(locale, "kind_file_image")}
	case strings.HasPrefix(mime, "video/"):
		return fileKindView{Category: "video", Badge: shortBadge("VID"), Label: translate(locale, "kind_file_video")}
	case strings.HasPrefix(mime, "audio/"):
		return fileKindView{Category: "audio", Badge: shortBadge("AUD"), Label: translate(locale, "kind_file_audio")}
	case mime == "application/json":
		return fileKindView{Category: "config", Badge: "JSON", Label: translate(locale, "kind_file_json")}
	case mime == "application/yaml" || mime == "text/yaml" || mime == "application/x-yaml":
		return fileKindView{Category: "config", Badge: "YAML", Label: translate(locale, "kind_file_yaml")}
	case mime == "application/zip" || mime == "application/gzip" || mime == "application/x-tar" ||
		mime == "application/x-7z-compressed" || mime == "application/x-rar-compressed":
		return fileKindView{Category: "archive", Badge: shortBadge("ZIP"), Label: translate(locale, "kind_file_archive")}
	case strings.HasPrefix(mime, "text/") || store.TextLike(contentType):
		return fileKindView{Category: "doc", Badge: shortBadge("TXT"), Label: translate(locale, "kind_file_text")}
	default:
		return fileKindView{Category: "file", Badge: shortBadge("FILE"), Label: translate(locale, "kind_file_binary")}
	}
}

func isASCIIAlnum(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}
