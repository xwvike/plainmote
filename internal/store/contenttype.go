package store

import (
	"encoding/json"
	"net/http"
	"path"
	"strings"
	"unicode/utf8"
)

// Content types this service is willing to serve. Nothing here is executed by
// a browser: a resource must never be able to become a script on this origin,
// so text/html and friends are deliberately absent and unreachable.
const (
	typeText   = "text/plain; charset=utf-8"
	typeJSON   = "application/json"
	typeYAML   = "application/yaml"
	typeTOML   = "application/toml"
	typeXML    = "application/xml"
	typeCSV    = "text/csv; charset=utf-8"
	typeBinary = "application/octet-stream"
)

// unsafeTypeWords name the families a browser will execute. Detection may
// never land on one: a resource is served from the same origin as the admin
// pages, so a file that could become a script would be a way in.
var unsafeTypeWords = []string{"html", "javascript", "ecmascript", "svg", "xhtml"}

func executableType(contentType string) bool {
	lowered := strings.ToLower(contentType)
	for _, word := range unsafeTypeWords {
		if strings.Contains(lowered, word) {
			return true
		}
	}
	return false
}

// SafeContentType keeps an upstream response from becoming executable content
// on the same origin as the signed-in interface. Known non-executable types
// pass through unchanged; browser-executable or empty values become downloads.
func SafeContentType(contentType string) string {
	contentType = strings.TrimSpace(contentType)
	if contentType == "" || executableType(contentType) {
		return typeBinary
	}
	return contentType
}

// byExtension maps the delivery filename's suffix to a type. The filename is the
// strongest signal available and the owner types it deliberately, so it wins
// over sniffing the bytes - including the plain-text suffixes, so naming a
// file .txt keeps it text even when the body happens to parse as JSON.
var byExtension = map[string]string{
	".json": typeJSON,
	".yaml": typeYAML,
	".yml":  typeYAML,
	".toml": typeTOML,
	".xml":  typeXML,
	".csv":  typeCSV,

	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".avif": "image/avif",
	".bmp":  "image/bmp",
	".ico":  "image/x-icon",

	".mp3":  "audio/mpeg",
	".m4a":  "audio/mp4",
	".aac":  "audio/aac",
	".ogg":  "audio/ogg",
	".opus": "audio/ogg",
	".flac": "audio/flac",
	".wav":  "audio/wav",

	".mp4":  "video/mp4",
	".webm": "video/webm",
	".mov":  "video/quicktime",

	".pdf": "application/pdf",
	".zip": "application/zip",
	".gz":  "application/gzip",

	".txt":        typeText,
	".text":       typeText,
	".md":         typeText,
	".log":        typeText,
	".conf":       typeText,
	".cfg":        typeText,
	".ini":        typeText,
	".env":        typeText,
	".list":       typeText,
	".rules":      typeText,
	".properties": typeText,
	".pem":        typeText,
	".crt":        typeText,
	".key":        typeText,
	".pub":        typeText,
	".sh":         typeText,
	".service":    typeText,
}

// DetectContentType works out how a resource should be served: first from the
// delivery filename's extension, then from the shape of the content. Anything it
// cannot place confidently is plain text, which is safe for every consumer.
func DetectContentType(publicPath string, content []byte) string {
	if extension := strings.ToLower(path.Ext(publicPath)); extension != "" {
		if known, ok := byExtension[extension]; ok {
			return known
		}
	}
	// No usable extension: let the standard library read the file's own magic
	// bytes, which is what it is good at. It only gets a say when it
	// recognises something specific.
	if magic := sniffMagic(content); magic != "" {
		return magic
	}
	if !utf8.Valid(content) {
		return typeBinary
	}
	return sniffContentType(content)
}

// sniffMagic asks net/http what the bytes look like, and returns "" unless the
// answer is both confident and safe to serve.
func sniffMagic(content []byte) string {
	detected := http.DetectContentType(content)
	if executableType(detected) {
		// Something that would run in a browser: refuse to name it, and let
		// the caller fall through to a type that cannot.
		return typeBinary
	}
	base, _, _ := strings.Cut(detected, ";")
	switch base = strings.TrimSpace(base); {
	case strings.HasPrefix(base, "image/"),
		strings.HasPrefix(base, "audio/"),
		strings.HasPrefix(base, "video/"),
		strings.HasPrefix(base, "font/"),
		base == "application/pdf",
		base == "application/zip",
		base == "application/x-gzip",
		base == "application/gzip":
		return base
	}
	return ""
}

// sniffContentType only claims the formats it can recognise unambiguously.
// YAML and the various key=value config dialects all look alike, so guessing
// between them would be worse than plain text.
func sniffContentType(content []byte) string {
	trimmed := strings.TrimSpace(string(content))
	if trimmed == "" {
		return typeText
	}
	switch trimmed[0] {
	case '{', '[':
		if json.Valid([]byte(trimmed)) {
			return typeJSON
		}
	case '<':
		if strings.HasPrefix(trimmed, "<?xml") {
			return typeXML
		}
	case '-':
		// A YAML document marker is the one unambiguous YAML signal.
		if strings.HasPrefix(trimmed, "---\n") || trimmed == "---" || strings.HasPrefix(trimmed, "--- ") {
			return typeYAML
		}
	}
	return typeText
}

// fallbackNames is the reverse of byExtension: what to call a file when its
// owner did not name it. Only the types DetectContentType can produce appear
// here, so every resource has an answer.
var fallbackNames = map[string]string{
	typeJSON:   "file.json",
	typeYAML:   "file.yaml",
	typeTOML:   "file.toml",
	typeXML:    "file.xml",
	typeCSV:    "file.csv",
	typeBinary: "file.bin",
	typeText:   "file.txt",
}

// FallbackFilename names a resource whose owner left the filename empty, so
// that saving it still produces something openable instead of a token.
func FallbackFilename(contentType string) string {
	if name, ok := fallbackNames[contentType]; ok {
		return name
	}
	// A remote resource carries whatever the upstream said, which may be
	// anything; take the type's own subtype as the extension when it looks
	// usable, and fall back to plain text otherwise.
	base, _, _ := strings.Cut(contentType, ";")
	_, subtype, found := strings.Cut(strings.TrimSpace(base), "/")
	if found && filenameRule.MatchString("f."+subtype) {
		return "file." + subtype
	}
	return "file.txt"
}

// TextLike reports whether a content type is something a person can sensibly
// edit in a textarea. Everything else has to be replaced by uploading again:
// a form round-trip would rewrite the bytes it cannot represent.
func TextLike(contentType string) bool {
	base, _, _ := strings.Cut(contentType, ";")
	base = strings.TrimSpace(strings.ToLower(base))
	if strings.HasPrefix(base, "text/") {
		return true
	}
	switch base {
	case "application/json", "application/yaml", "application/toml", "application/xml":
		return true
	}
	return false
}
