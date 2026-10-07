package store

import (
	"bytes"
	"encoding/json"
	"mime"
	"net/http"
	"path"
	"regexp"
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

// byExtension maps the delivery filename's suffix to a type. The owner types
// the filename deliberately, so it decides between formats the bytes cannot
// tell apart - YAML from TOML, an AAC stream from nothing in particular. It
// does not outvote bytes that plainly say something else: a file replaced by
// another keeps its old name, and a PNG called movie.mp4 is still a PNG. Text
// suffixes are decoded with the resource's recorded encoding or the detector
// before they are admitted to the editor.
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

// DetectContentType works out how a resource should be served. Callers that
// already know the source encoding use DetectContent so that the same decision
// also records how the editor must decode and re-encode the bytes.
func DetectContentType(publicPath string, content []byte) string {
	contentType, _ := DetectContent(publicPath, content, "")
	return contentType
}

// DetectContent returns both the safe media type and the detected text
// encoding. Binary resources have an empty encoding.
func DetectContent(publicPath string, content []byte, encodingHint string) (string, string) {
	if extension := strings.ToLower(path.Ext(publicPath)); extension != "" {
		if known, ok := byExtension[extension]; ok {
			if !TextLike(known) {
				return binaryByName(known, content)
			}
			// A text-looking suffix cannot hide a recognised binary signature.
			if magic := sniffMagic(content); magic != "" {
				return magic, ""
			}
			if _, encodingName, err := detectAndDecodeText(content, encodingHint); err == nil {
				return known, encodingName
			}
			return typeBinary, ""
		}
	}
	// No usable extension: let the standard library read the file's own magic
	// bytes, which is what it is good at. It only gets a say when it
	// recognises something specific.
	if magic := sniffMagic(content); magic != "" {
		return magic, ""
	}
	decoded, encodingName, err := detectAndDecodeText(content, encodingHint)
	if err != nil {
		return typeBinary, ""
	}
	return sniffContentType([]byte(decoded)), encodingName
}

// binaryByName settles a binary suffix against the bytes. A signature the
// bytes carry wins when it names a different kind of thing: an image where
// the name says video, an archive where it says PDF. Audio and video are left
// to the name - one container holds either, and an M4A sniffs as MP4 - as
// are formats with no signature to read. Bytes that would run in a browser
// are named nothing, and bytes that are plainly UTF-8 text are text.
func binaryByName(known string, content []byte) (string, string) {
	magic := sniffMagic(content)
	switch {
	case magic == typeBinary:
		return typeBinary, ""
	case magic != "" && signatureOverrules(known, magic):
		return magic, ""
	case magic == "" && len(content) > 0 && utf8.Valid(content) && !bytes.ContainsRune(content, 0):
		return sniffContentType(content), "utf-8"
	}
	return known, ""
}

func signatureOverrules(known, magic string) bool {
	if known == magic {
		return false
	}
	gzip := func(t string) string {
		if t == "application/x-gzip" {
			return "application/gzip"
		}
		return t
	}
	knownFamily, _, _ := strings.Cut(known, "/")
	magicFamily, _, _ := strings.Cut(magic, "/")
	media := func(family string) bool { return family == "audio" || family == "video" }
	switch {
	case knownFamily != magicFamily:
		return !(media(knownFamily) && media(magicFamily))
	case knownFamily == "image":
		// Image signatures are exact; a JPEG saved over logo.png is a JPEG.
		return true
	case knownFamily == "application":
		return gzip(known) != gzip(magic)
	}
	return false
}

// ExtensionType is the type a filename's suffix names, or "" for none.
func ExtensionType(name string) string {
	return byExtension[strings.ToLower(path.Ext(name))]
}

// RefitFilename keeps a filename unless the content now under it makes it
// wrong: a suffix naming a different kind of thing than the bytes are. Then
// the other name is used - the replacement file's own, or the one a restored
// version had - so the image that replaced a video is not handed out as
// movie.mp4. A name with no suffix this knows, Dockerfile or app.conf.bak,
// is the owner's and stays.
func RefitFilename(name, other string, content []byte) string {
	byName := ExtensionType(name)
	if other == "" || byName == "" {
		return name
	}
	if detected, _ := DetectContent(name, content, ""); detected == byName {
		return name
	}
	return other
}

// TextFilename reports whether a filename explicitly denotes a format that
// can be opened in the text editor. It lets old rows marked as opaque be
// reconsidered now that legacy encodings are supported.
func TextFilename(publicPath string) bool {
	known, ok := byExtension[strings.ToLower(path.Ext(publicPath))]
	return ok && TextLike(known)
}

// ContentTypeWithEncoding adds the standard charset label used by HTTP while
// keeping BOM preservation details in the separate resource metadata.
func ContentTypeWithEncoding(contentType, encodingName string) string {
	canonical, ok := normalizeTextEncoding(encodingName)
	if !ok || !TextLike(contentType) {
		return contentType
	}
	// Preserve the long-standing UTF-8 response types. text/plain and text/csv
	// already carry their charset, while adding a parameter to application/json
	// and the structured text types needlessly changes their public contract.
	if canonical == "utf-8" || canonical == "utf-8bom" {
		return contentType
	}
	charset := canonical
	switch canonical {
	case "utf-8bom":
		charset = "utf-8"
	case "utf-16le-bom":
		charset = "utf-16le"
	case "utf-16be-bom":
		charset = "utf-16be"
	case "utf-32le-bom":
		charset = "utf-32le"
	case "utf-32be-bom":
		charset = "utf-32be"
	}
	base, parameters, err := mime.ParseMediaType(contentType)
	if err != nil {
		return contentType
	}
	parameters["charset"] = charset
	return mime.FormatMediaType(base, parameters)
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
	// Neutral on purpose: the name at the end of an encrypted share's address
	// says nothing about it, not even that it is encrypted.
	EncryptedContentType: "file",
	SealedContentType:    "file",
	typeJSON:             "file.json",
	typeYAML:             "file.yaml",
	typeTOML:             "file.toml",
	typeXML:              "file.xml",
	typeCSV:              "file.csv",
	typeBinary:           "file.bin",
	typeText:             "file.txt",
}

// subtypeRule is the shape a media subtype must have before it is used as a
// file extension. It arrives in an upstream header rather than from the owner,
// so it is held to a far narrower set than a filename is.
var subtypeRule = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]{0,63}$`)

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
	if found && subtypeRule.MatchString(subtype) {
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
